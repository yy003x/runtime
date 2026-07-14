package executor

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"os/signal"
	"strings"
	"sync"
	"syscall"
	"time"
)

type Options struct {
	Argv           []string
	Env            []string
	Dir            string
	Stdin          io.Reader
	Interactive    bool
	ForwardSignals bool
	GracePeriod    time.Duration
	Observer       Observer
}

type Observer struct {
	Started     func(ProcessInfo)
	Stdout      func([]byte)
	Stderr      func([]byte)
	FirstOutput func()
}

type ProcessInfo struct {
	PID  int
	PGID int
}

type Result struct {
	Stdout   string
	Stderr   string
	ExitCode int
	PID      int
	PGID     int
}

func Run(ctx context.Context, options Options) (Result, error) {
	if len(options.Argv) == 0 || strings.TrimSpace(options.Argv[0]) == "" {
		return Result{ExitCode: 1}, fmt.Errorf("executor: empty argv")
	}
	if options.GracePeriod <= 0 {
		options.GracePeriod = 500 * time.Millisecond
	}
	cmd, err := prepareCommand(options)
	if err != nil {
		return Result{ExitCode: 1}, err
	}
	if options.Interactive {
		return runInteractive(ctx, cmd, options)
	}
	return runStreaming(ctx, cmd, options)
}

func prepareCommand(options Options) (*exec.Cmd, error) {
	path, err := resolveExecutable(options.Argv[0], options.Env, options.Dir)
	if err != nil {
		return nil, err
	}
	args := append([]string(nil), options.Argv[1:]...)
	if interpreter, interpreterArgs := resolveEnvShebang(path, options.Env); interpreter != path {
		path = interpreter
		args = append(interpreterArgs, args...)
	}
	cmd := exec.Command(path, args...)
	cmd.Dir = options.Dir
	cmd.Env = options.Env
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	return cmd, nil
}

func runInteractive(ctx context.Context, cmd *exec.Cmd, options Options) (Result, error) {
	cmd.Stdin = options.Stdin
	if cmd.Stdin == nil {
		cmd.Stdin = os.Stdin
	}
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Start(); err != nil {
		return Result{ExitCode: 1}, fmt.Errorf("start %q: %w", options.Argv[0], err)
	}
	info := startedInfo(cmd)
	if options.Observer.Started != nil {
		options.Observer.Started(info)
	}
	if isTerminal(os.Stdin.Fd()) {
		_ = setForegroundPgid(os.Stdin.Fd(), cmd.Process.Pid)
		defer func() { _ = setForegroundPgid(os.Stdin.Fd(), os.Getpid()) }()
	}
	waitErr := wait(ctx, cmd, options.ForwardSignals, options.GracePeriod)
	result := Result{ExitCode: exitCode(waitErr), PID: info.PID, PGID: info.PGID}
	return result, executionError(options.Argv[0], waitErr)
}

func runStreaming(ctx context.Context, cmd *exec.Cmd, options Options) (Result, error) {
	cmd.Stdin = options.Stdin
	stdout, err := cmd.StdoutPipe()
	if err != nil {
		return Result{ExitCode: 1}, fmt.Errorf("create stdout pipe: %w", err)
	}
	stderr, err := cmd.StderrPipe()
	if err != nil {
		return Result{ExitCode: 1}, fmt.Errorf("create stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return Result{ExitCode: 1}, fmt.Errorf("start %q: %w", options.Argv[0], err)
	}
	info := startedInfo(cmd)
	if options.Observer.Started != nil {
		options.Observer.Started(info)
	}

	var stdoutBuffer bytes.Buffer
	var stderrBuffer bytes.Buffer
	var first sync.Once
	notify := func() {
		first.Do(func() {
			if options.Observer.FirstOutput != nil {
				options.Observer.FirstOutput()
			}
		})
	}
	var readers sync.WaitGroup
	readers.Add(2)
	go func() {
		defer readers.Done()
		copyStream(stdout, &stdoutBuffer, options.Observer.Stdout, notify)
	}()
	go func() {
		defer readers.Done()
		copyStream(stderr, &stderrBuffer, options.Observer.Stderr, notify)
	}()

	waitErr := wait(ctx, cmd, options.ForwardSignals, options.GracePeriod)
	readers.Wait()
	notify()
	result := Result{
		Stdout: stdoutBuffer.String(), Stderr: stderrBuffer.String(),
		ExitCode: exitCode(waitErr), PID: info.PID, PGID: info.PGID,
	}
	return result, executionError(options.Argv[0], waitErr)
}

func copyStream(source io.Reader, capture *bytes.Buffer, callback func([]byte), first func()) {
	buffer := make([]byte, 32*1024)
	for {
		read, err := source.Read(buffer)
		if read > 0 {
			chunk := append([]byte(nil), buffer[:read]...)
			first()
			_, _ = capture.Write(chunk)
			if callback != nil {
				callback(chunk)
			}
		}
		if err != nil {
			return
		}
	}
}

func wait(ctx context.Context, cmd *exec.Cmd, forwardSignals bool, grace time.Duration) error {
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()

	var signals chan os.Signal
	if forwardSignals {
		signals = make(chan os.Signal, 1)
		signal.Notify(signals, syscall.SIGINT, syscall.SIGTERM, syscall.SIGHUP)
		defer signal.Stop(signals)
	}
	for {
		select {
		case err := <-done:
			return err
		case received := <-signals:
			if value, ok := received.(syscall.Signal); ok {
				_ = signalProcessGroup(cmd, value)
			}
		case <-ctx.Done():
			_ = signalProcessGroup(cmd, syscall.SIGINT)
			timer := time.NewTimer(grace)
			select {
			case <-done:
				timer.Stop()
			case <-timer.C:
				_ = signalProcessGroup(cmd, syscall.SIGKILL)
				<-done
			}
			return ctx.Err()
		}
	}
}

func startedInfo(cmd *exec.Cmd) ProcessInfo {
	info := ProcessInfo{PID: cmd.Process.Pid, PGID: cmd.Process.Pid}
	if pgid, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		info.PGID = pgid
	}
	return info
}

func signalProcessGroup(cmd *exec.Cmd, value syscall.Signal) error {
	if cmd == nil || cmd.Process == nil {
		return nil
	}
	pgid := cmd.Process.Pid
	if resolved, err := syscall.Getpgid(cmd.Process.Pid); err == nil {
		pgid = resolved
	}
	return syscall.Kill(-pgid, value)
}

func executionError(command string, err error) error {
	if err == nil {
		return nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return nil
	}
	return fmt.Errorf("run %q: %w", command, err)
}

func exitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		return exitErr.ExitCode()
	}
	return 1
}
