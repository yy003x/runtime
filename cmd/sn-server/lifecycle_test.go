package main

import (
	"context"
	"errors"
	"net/http"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"
)

func TestSuperviseServerFailsFastWhenWorkerStops(t *testing.T) {
	readiness := &readinessState{}
	failWorker := make(chan struct{})
	serverStopped := make(chan struct{})
	workerErr := errors.New("claim failed")
	var stopOnce sync.Once
	var shutdownCalls atomic.Int32

	result := make(chan error, 1)
	go func() {
		result <- superviseServer(context.Background(), serverLifecycle{
			Readiness: readiness,
			Tasks: []backgroundTask{
				{
					Name: "worker-1",
					Run: func(context.Context) error {
						<-failWorker
						return workerErr
					},
				},
				{
					Name: "worker-2",
					Run: func(ctx context.Context) error {
						<-ctx.Done()
						return ctx.Err()
					},
				},
			},
			Serve: func() error {
				<-serverStopped
				return http.ErrServerClosed
			},
			Shutdown: func(context.Context) error {
				shutdownCalls.Add(1)
				stopOnce.Do(func() { close(serverStopped) })
				return nil
			},
			ShutdownTimeout: time.Second,
		})
	}()
	waitForReadiness(t, readiness)
	close(failWorker)

	select {
	case err := <-result:
		if !errors.Is(err, workerErr) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop after worker failure")
	}
	if readiness.Ready() {
		t.Fatal("server remained ready after worker failure")
	}
	if shutdownCalls.Load() != 1 {
		t.Fatalf("shutdown calls=%d", shutdownCalls.Load())
	}
}

func TestSuperviseServerTreatsReaperFailureAsFatal(t *testing.T) {
	readiness := &readinessState{}
	serverStopped := make(chan struct{})
	reaperErr := errors.New("reaper store failed")
	var stopOnce sync.Once
	err := superviseServer(context.Background(), serverLifecycle{
		Readiness: readiness,
		Tasks: []backgroundTask{{
			Name: "reaper",
			Run: func(context.Context) error {
				return reaperErr
			},
		}},
		Serve: func() error {
			<-serverStopped
			return http.ErrServerClosed
		},
		Shutdown: func(context.Context) error {
			stopOnce.Do(func() { close(serverStopped) })
			return nil
		},
		ShutdownTimeout: time.Second,
	})
	if !errors.Is(err, reaperErr) || readiness.Ready() {
		t.Fatalf("error=%v ready=%t", err, readiness.Ready())
	}
}

func TestSuperviseServerGracefullyStopsOnContextCancellation(t *testing.T) {
	readiness := &readinessState{}
	serverStopped := make(chan struct{})
	var stopOnce sync.Once
	ctx, cancel := context.WithCancel(context.Background())
	result := make(chan error, 1)
	go func() {
		result <- superviseServer(ctx, serverLifecycle{
			Readiness: readiness,
			Tasks: []backgroundTask{{
				Name: "worker",
				Run: func(ctx context.Context) error {
					<-ctx.Done()
					return ctx.Err()
				},
			}},
			Serve: func() error {
				<-serverStopped
				return http.ErrServerClosed
			},
			Shutdown: func(context.Context) error {
				stopOnce.Do(func() { close(serverStopped) })
				return nil
			},
			ShutdownTimeout: time.Second,
		})
	}()
	waitForReadiness(t, readiness)
	cancel()

	select {
	case err := <-result:
		if err != nil {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor did not stop after context cancellation")
	}
	if readiness.Ready() {
		t.Fatal("server remained ready during shutdown")
	}
}

func TestSuperviseServerReturnsUnexpectedServeFailure(t *testing.T) {
	readiness := &readinessState{}
	serveErr := errors.New("accept failed")
	err := superviseServer(context.Background(), serverLifecycle{
		Readiness: readiness,
		Tasks: []backgroundTask{{
			Name: "worker",
			Run: func(ctx context.Context) error {
				<-ctx.Done()
				return ctx.Err()
			},
		}},
		Serve:           func() error { return serveErr },
		Shutdown:        func(context.Context) error { return nil },
		ShutdownTimeout: time.Second,
	})
	if !errors.Is(err, serveErr) || readiness.Ready() {
		t.Fatalf("error=%v ready=%t", err, readiness.Ready())
	}
}

func TestSuperviseServerBoundsJoinWhenTaskIgnoresCancellation(t *testing.T) {
	readiness := &readinessState{}
	serverStopped := make(chan struct{})
	stuckTask := make(chan struct{})
	ctx, cancel := context.WithCancel(context.Background())
	defer close(stuckTask)
	result := make(chan error, 1)
	go func() {
		result <- superviseServer(ctx, serverLifecycle{
			Readiness: readiness,
			Tasks: []backgroundTask{{
				Name: "stuck-worker",
				Run: func(context.Context) error {
					<-stuckTask
					return nil
				},
			}},
			Serve: func() error {
				<-serverStopped
				return http.ErrServerClosed
			},
			Shutdown: func(context.Context) error {
				close(serverStopped)
				return nil
			},
			ShutdownTimeout: 20 * time.Millisecond,
		})
	}()
	waitForReadiness(t, readiness)
	cancel()
	select {
	case err := <-result:
		if err == nil || !strings.Contains(
			err.Error(), "components did not stop within",
		) {
			t.Fatalf("error=%v", err)
		}
	case <-time.After(time.Second):
		t.Fatal("supervisor waited indefinitely for a stuck worker")
	}
}

func waitForReadiness(t *testing.T, readiness *readinessState) {
	t.Helper()
	deadline := time.Now().Add(time.Second)
	for !readiness.Ready() {
		if time.Now().After(deadline) {
			t.Fatal("server did not become ready")
		}
		time.Sleep(time.Millisecond)
	}
}
