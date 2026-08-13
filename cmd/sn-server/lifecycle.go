package main

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"time"
)

type backgroundTask struct {
	Name string
	Run  func(context.Context) error
}

type serverLifecycle struct {
	Readiness       *readinessState
	Tasks           []backgroundTask
	Serve           func() error
	Shutdown        func(context.Context) error
	ShutdownTimeout time.Duration
}

type backgroundResult struct {
	name     string
	err      error
	stopping bool
}

// superviseServer owns the common lifetime of the HTTP server, durable Run
// workers, and the reaper. Any unexpected component exit makes the execution
// plane unready, cancels its peers, gracefully closes HTTP, and is returned to
// main so the process exits non-zero.
func superviseServer(ctx context.Context, lifecycle serverLifecycle) error {
	if ctx == nil {
		return fmt.Errorf("server context is required")
	}
	if lifecycle.Readiness == nil {
		return fmt.Errorf("server readiness is required")
	}
	if lifecycle.Serve == nil || lifecycle.Shutdown == nil {
		return fmt.Errorf("server Serve and Shutdown functions are required")
	}
	if len(lifecycle.Tasks) == 0 {
		return fmt.Errorf("at least one durable Run worker is required")
	}
	for _, task := range lifecycle.Tasks {
		if task.Name == "" || task.Run == nil {
			return fmt.Errorf("background tasks require a name and Run function")
		}
	}
	if lifecycle.ShutdownTimeout <= 0 {
		lifecycle.ShutdownTimeout = 10 * time.Second
	}

	lifecycle.Readiness.Set(false)
	taskContext, cancelTasks := context.WithCancel(ctx)
	defer cancelTasks()
	startComponents := make(chan struct{})
	taskResults := make(chan backgroundResult, len(lifecycle.Tasks))
	for _, value := range lifecycle.Tasks {
		task := value
		go func() {
			<-startComponents
			err := task.Run(taskContext)
			stopping := taskContext.Err() != nil
			if !stopping {
				lifecycle.Readiness.Set(false)
			}
			taskResults <- backgroundResult{
				name: task.Name, err: err, stopping: stopping,
			}
		}()
	}
	serveDone := make(chan error, 1)
	go func() {
		<-startComponents
		err := lifecycle.Serve()
		lifecycle.Readiness.Set(false)
		serveDone <- err
	}()

	// The listener is already bound by run. Publish readiness only after every
	// background goroutine exists, then let all components enter their loops.
	lifecycle.Readiness.Set(true)
	close(startComponents)

	completedTasks := 0
	serveCompleted := false
	var resultErr error
	select {
	case <-ctx.Done():
	case result := <-taskResults:
		completedTasks++
		resultErr = backgroundTaskError(result)
	case err := <-serveDone:
		serveCompleted = true
		resultErr = unexpectedServeError(err)
	}

	lifecycle.Readiness.Set(false)
	cancelTasks()
	shutdownContext, cancelShutdown := context.WithTimeout(
		context.Background(), lifecycle.ShutdownTimeout,
	)
	defer cancelShutdown()
	shutdownErr := lifecycle.Shutdown(shutdownContext)
	if shutdownErr != nil && !errors.Is(shutdownErr, http.ErrServerClosed) {
		resultErr = errors.Join(
			resultErr, fmt.Errorf("shutdown HTTP server: %w", shutdownErr),
		)
	}

	for completedTasks < len(lifecycle.Tasks) || !serveCompleted {
		select {
		case result := <-taskResults:
			completedTasks++
			if err := backgroundTaskError(result); err != nil {
				resultErr = errors.Join(resultErr, err)
			}
		case err := <-serveDone:
			serveCompleted = true
			if err != nil && !errors.Is(err, http.ErrServerClosed) {
				resultErr = errors.Join(
					resultErr, fmt.Errorf("serve HTTP after shutdown: %w", err),
				)
			}
		case <-shutdownContext.Done():
			return errors.Join(
				resultErr,
				fmt.Errorf(
					"server components did not stop within %s: %w",
					lifecycle.ShutdownTimeout, shutdownContext.Err(),
				),
			)
		}
	}
	return resultErr
}

func backgroundTaskError(result backgroundResult) error {
	if result.stopping {
		return nil
	}
	if result.err == nil {
		return fmt.Errorf("background task %s stopped unexpectedly", result.name)
	}
	return fmt.Errorf("background task %s stopped: %w", result.name, result.err)
}

func unexpectedServeError(err error) error {
	if err == nil || errors.Is(err, http.ErrServerClosed) {
		return fmt.Errorf("HTTP server stopped unexpectedly")
	}
	return fmt.Errorf("serve HTTP: %w", err)
}
