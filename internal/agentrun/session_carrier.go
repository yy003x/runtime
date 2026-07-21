package agentrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agent-runtime/internal/provider"
	providertmux "agent-runtime/internal/provider/tmux"
)

var errCarrierOperationUnsupported = errors.New("carrier operation is not supported")

var terminalLaunchCommandFn = terminalLaunchCommand

type carrierHandle struct {
	ID        string
	Attach    string
	ProcessID int
	Status    map[string]any
}

type sessionCarrier interface {
	Name() string
	ExecutionKind() string
	PrepareProfile(provider.Config) (provider.Config, error)
	Start(context.Context, *Service, provider.Config, Request, SessionOptions, Paths) (carrierHandle, error)
	Alive(context.Context, *Service, Request, map[string]any, Paths) (bool, error)
	Exit(Paths) (code, attempts int, ok bool)
	Capture(context.Context, *Service, Request, map[string]any, int) (string, error)
	Send(context.Context, *Service, provider.Config, Request, map[string]any, string, bool) error
	Interrupt(context.Context, *Service, Request, map[string]any) error
	Stop(context.Context, *Service, Request, map[string]any) error
	Attach(context.Context, *Service, Request, map[string]any) error
	Release(context.Context, *Service, Request, map[string]any) error
}

func (s *Service) resolveSessionCarrier(name string) (sessionCarrier, error) {
	if strings.TrimSpace(name) == "" {
		name = s.DefaultCarrier
	}
	switch name {
	case "tmux":
		return tmuxSessionCarrier{}, nil
	case "terminal":
		if s.TerminalDriver == "" {
			return nil, fmt.Errorf("terminal carrier requires session.terminal.driver=ghostty|iterm2 in configs/runtime.yaml")
		}
		return terminalSessionCarrier{driver: s.TerminalDriver}, nil
	default:
		return nil, fmt.Errorf("carrier must be tmux|terminal")
	}
}

type tmuxSessionCarrier struct{}

func (tmuxSessionCarrier) Name() string          { return "tmux" }
func (tmuxSessionCarrier) ExecutionKind() string { return ExecutionTmux }

func (tmuxSessionCarrier) PrepareProfile(profile provider.Config) (provider.Config, error) {
	return provider.AsTmuxSessionProfile(profile)
}

func (tmuxSessionCarrier) Start(ctx context.Context, service *Service, profile provider.Config, request Request, options SessionOptions, paths Paths) (carrierHandle, error) {
	backend, err := provider.NewTmuxBackend(profile, service.DaemonClient())
	if err != nil {
		return carrierHandle{}, err
	}
	command, err := provider.TmuxShellCommandWithRawArgs(profile, options.RawCLIArgs, nil)
	if err != nil {
		return carrierHandle{}, err
	}
	tmuxConfig := profile.CLI.Tmux
	session, err := backend.StartShellWithOptions(ctx, request.RunID, request.CWD, command, providertmux.StartOptions{
		LogFile: paths.OutputLog, ExitFile: sessionExitFile(paths), WaitForOutput: true,
		RestartMaxAttempts: tmuxConfig.RestartMaxAttempts, RestartDelaySeconds: tmuxConfig.RestartDelaySeconds,
	})
	if err != nil {
		return carrierHandle{}, err
	}
	status := map[string]any{"carrier": "tmux", "carrier_id": session, "tmux_session": session, "alive": true}
	return carrierHandle{ID: session, Attach: "tmux attach-session -t " + session, Status: status}, nil
}

func (tmuxSessionCarrier) Alive(ctx context.Context, service *Service, request Request, status map[string]any, _ Paths) (bool, error) {
	return service.DaemonClient().HasTmux(ctx, request.RunID, carrierID(status))
}

func (tmuxSessionCarrier) Exit(paths Paths) (int, int, bool) {
	return readSessionExit(sessionExitFile(paths))
}

func (tmuxSessionCarrier) Capture(ctx context.Context, service *Service, request Request, status map[string]any, tail int) (string, error) {
	return service.DaemonClient().CaptureTmux(ctx, request.RunID, carrierID(status), tail)
}

func (tmuxSessionCarrier) Send(ctx context.Context, service *Service, profile provider.Config, request Request, status map[string]any, text string, submit bool) error {
	backend, err := provider.NewTmuxBackend(profile, service.DaemonClient())
	if err != nil {
		return err
	}
	return backend.Send(ctx, carrierID(status), text, providertmux.SendOptions{
		Submit: submit, Bracketed: profile.CLI.Tmux.PasteBracketed, Stabilize: submit,
	})
}

func (tmuxSessionCarrier) Interrupt(ctx context.Context, service *Service, request Request, status map[string]any) error {
	return service.DaemonClient().InterruptTmux(ctx, request.RunID, carrierID(status))
}

func (tmuxSessionCarrier) Stop(ctx context.Context, service *Service, request Request, status map[string]any) error {
	return service.DaemonClient().KillTmux(ctx, request.RunID, carrierID(status))
}

func (tmuxSessionCarrier) Attach(ctx context.Context, _ *Service, _ Request, status map[string]any) error {
	return providertmux.Attach(ctx, carrierID(status))
}

func (tmuxSessionCarrier) Release(context.Context, *Service, Request, map[string]any) error {
	return nil
}

type terminalSessionCarrier struct {
	driver string
}

func (terminalSessionCarrier) Name() string          { return "terminal" }
func (terminalSessionCarrier) ExecutionKind() string { return ExecutionTerminal }

func (terminalSessionCarrier) PrepareProfile(profile provider.Config) (provider.Config, error) {
	if profile.Type != provider.TypeCLI || profile.CLI == nil || profile.CLI.Executor != provider.ExecutorCommand {
		return provider.Config{}, fmt.Errorf("terminal carrier requires a cli.executor=command profile")
	}
	return profile, nil
}

func (carrier terminalSessionCarrier) Start(ctx context.Context, service *Service, profile provider.Config, request Request, options SessionOptions, paths Paths) (carrierHandle, error) {
	prepared, err := provider.PrepareInteractiveCLI(profile, options.RawCLIArgs)
	if err != nil {
		return carrierHandle{}, err
	}
	processID := "terminal/" + request.ExecutionID
	environment, acquired, err := provider.InteractiveCLIEnvironment(ctx, profile, service.DaemonClient(), processID)
	if err != nil {
		return carrierHandle{}, err
	}
	release := func() {
		if acquired {
			_ = service.DaemonClient().Release(context.Background(), processID)
		}
	}

	fifoPath := filepath.Join(paths.RunDir, "terminal-input")
	wrapperPath := filepath.Join(paths.RunDir, "terminal-wrapper.sh")
	readyPath := filepath.Join(paths.RunDir, "terminal-ready")
	pidPath := filepath.Join(paths.RunDir, "terminal-pid")
	childPIDPath := filepath.Join(paths.RunDir, "terminal-child-pid")
	for _, path := range []string{fifoPath, readyPath, pidPath, childPIDPath} {
		_ = os.Remove(path)
	}
	if err := syscall.Mkfifo(fifoPath, 0o600); err != nil {
		release()
		return carrierHandle{}, fmt.Errorf("create terminal environment pipe: %w", err)
	}
	cleanup := func() {
		_ = os.Remove(fifoPath)
		_ = os.Remove(wrapperPath)
	}
	wrapper := terminalWrapperScript(fifoPath, readyPath, pidPath, childPIDPath, sessionExitFile(paths))
	if err := os.WriteFile(wrapperPath, []byte(wrapper), 0o700); err != nil {
		cleanup()
		release()
		return carrierHandle{}, fmt.Errorf("write terminal wrapper: %w", err)
	}
	launcher := terminalLaunchCommandFn(ctx, carrier.driver, wrapperPath)
	if output, err := launcher.CombinedOutput(); err != nil {
		cleanup()
		release()
		return carrierHandle{}, fmt.Errorf("launch %s terminal: %w: %s", carrier.driver, err, strings.TrimSpace(string(output)))
	}
	if err := waitForFile(ctx, readyPath, 10*time.Second); err != nil {
		cleanup()
		release()
		return carrierHandle{}, fmt.Errorf("wait for %s terminal: %w", carrier.driver, err)
	}
	payload := terminalPayload(request.CWD, paths.OutputLog, childPIDPath, environment, prepared.Argv)
	if err := writeTerminalFIFO(ctx, fifoPath, payload, 10*time.Second); err != nil {
		cleanup()
		release()
		return carrierHandle{}, err
	}
	_ = os.Remove(wrapperPath)
	pid, err := readPIDFile(pidPath)
	if err != nil {
		release()
		return carrierHandle{}, err
	}
	id := fmt.Sprintf("%s:%d", carrier.driver, pid)
	status := map[string]any{
		"carrier": "terminal", "carrier_id": id, "terminal_driver": carrier.driver,
		"pid": pid, "child_pid_file": childPIDPath, "daemon_process_id": processID,
		"daemon_acquired": acquired, "alive": true,
	}
	return carrierHandle{ID: id, ProcessID: pid, Status: status}, nil
}

func (terminalSessionCarrier) Alive(_ context.Context, _ *Service, _ Request, status map[string]any, paths Paths) (bool, error) {
	if _, _, ok := readSessionExit(sessionExitFile(paths)); ok {
		return false, nil
	}
	pid := intStatus(status, "pid")
	if pid <= 0 {
		return false, nil
	}
	err := syscall.Kill(pid, 0)
	if err == nil || errors.Is(err, syscall.EPERM) {
		return true, nil
	}
	if errors.Is(err, syscall.ESRCH) {
		return false, nil
	}
	return false, err
}

func (terminalSessionCarrier) Exit(paths Paths) (int, int, bool) {
	return readSessionExit(sessionExitFile(paths))
}

func (terminalSessionCarrier) Capture(context.Context, *Service, Request, map[string]any, int) (string, error) {
	return "", nil
}

func (terminalSessionCarrier) Send(context.Context, *Service, provider.Config, Request, map[string]any, string, bool) error {
	return fmt.Errorf("%w: terminal carrier does not support input injection; type in the opened window", errCarrierOperationUnsupported)
}

func (terminalSessionCarrier) Interrupt(_ context.Context, _ *Service, _ Request, status map[string]any) error {
	pid := childPID(status)
	if pid <= 0 {
		pid = intStatus(status, "pid")
	}
	if pid <= 0 {
		return fmt.Errorf("terminal process is unavailable")
	}
	return syscall.Kill(pid, syscall.SIGINT)
}

func (terminalSessionCarrier) Stop(_ context.Context, _ *Service, _ Request, status map[string]any) error {
	pid := intStatus(status, "pid")
	if pid <= 0 {
		return nil
	}
	if err := syscall.Kill(pid, syscall.SIGTERM); err != nil && !errors.Is(err, syscall.ESRCH) {
		return err
	}
	return nil
}

func (terminalSessionCarrier) Attach(context.Context, *Service, Request, map[string]any) error {
	return fmt.Errorf("%w: terminal carrier opens an independent window and cannot be reattached", errCarrierOperationUnsupported)
}

func (terminalSessionCarrier) Release(ctx context.Context, service *Service, _ Request, status map[string]any) error {
	acquired, _ := status["daemon_acquired"].(bool)
	released, _ := status["daemon_released"].(bool)
	processID, _ := status["daemon_process_id"].(string)
	if !acquired || released || processID == "" {
		return nil
	}
	if err := service.DaemonClient().Release(ctx, processID); err != nil {
		return err
	}
	status["daemon_released"] = true
	return nil
}

func terminalWrapperScript(fifoPath, readyPath, pidPath, childPIDPath, exitPath string) string {
	return fmt.Sprintf(`#!/bin/sh
umask 077
input_fifo=%s
exit_file=%s
child_pid_file=%s
child_pid=
finish() {
  rc=$?
  trap - EXIT HUP INT TERM
  if [ -n "$child_pid" ]; then
    kill -TERM "$child_pid" 2>/dev/null || :
    wait "$child_pid" 2>/dev/null || :
  fi
  rm -f "$input_fifo"
  printf '%%s 1\n' "$rc" > "$exit_file"
  exit "$rc"
}
trap finish EXIT
trap 'exit 129' HUP
trap 'exit 130' INT
trap 'exit 143' TERM
printf '%%s\n' "$$" > %s
: > %s
payload=$(/bin/cat "$input_fifo")
rm -f "$input_fifo"
eval "$payload"
unset payload
exit 126
`, provider.ShellQuote(fifoPath), provider.ShellQuote(exitPath), provider.ShellQuote(childPIDPath), provider.ShellQuote(pidPath), provider.ShellQuote(readyPath))
}

func terminalPayload(cwd, outputPath, childPIDPath string, environment, argv []string) string {
	var payload strings.Builder
	payload.WriteString("for name in $(/usr/bin/env | /usr/bin/sed 's/=.*//'); do unset \"$name\"; done\n")
	for _, item := range environment {
		payload.WriteString("export ")
		payload.WriteString(provider.ShellQuote(item))
		payload.WriteByte('\n')
	}
	parts := []string{"/usr/bin/script", "-q", "-F", outputPath}
	parts = append(parts, argv...)
	quoted := make([]string, 0, len(parts))
	for _, part := range parts {
		quoted = append(quoted, provider.ShellQuote(part))
	}
	fmt.Fprintf(&payload, "cd %s || exit 125\n%s &\nchild_pid=$!\nprintf '%%s\\n' \"$child_pid\" > %s\nwait \"$child_pid\"\nrc=$?\nchild_pid=\nexit \"$rc\"\n",
		provider.ShellQuote(cwd), strings.Join(quoted, " "), provider.ShellQuote(childPIDPath))
	return payload.String()
}

func terminalLaunchCommand(ctx context.Context, driver, wrapperPath string) *exec.Cmd {
	switch driver {
	case "iterm2":
		return exec.CommandContext(ctx, "osascript",
			"-e", "on run argv",
			"-e", `tell application "iTerm2"`,
			"-e", "activate",
			"-e", "create window with default profile command (item 1 of argv)",
			"-e", "end tell",
			"-e", "end run",
			"--", "/bin/sh "+provider.ShellQuote(wrapperPath),
		)
	default:
		return exec.CommandContext(ctx, "open", "-na", "Ghostty.app", "--args", "-e", "/bin/sh", wrapperPath)
	}
}

func waitForFile(ctx context.Context, path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timeout")
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-time.After(50 * time.Millisecond):
		}
	}
}

func writeTerminalFIFO(ctx context.Context, path, payload string, timeout time.Duration) error {
	done := make(chan error, 1)
	go func() {
		file, err := os.OpenFile(path, os.O_WRONLY, 0o600)
		if err != nil {
			done <- fmt.Errorf("open terminal environment pipe: %w", err)
			return
		}
		remaining := []byte(payload)
		total := len(remaining)
		for len(remaining) > 0 {
			written, writeErr := file.Write(remaining)
			if writeErr != nil {
				done <- errors.Join(fmt.Errorf("write terminal environment pipe after %d/%d bytes: %w", total-len(remaining), total, writeErr), file.Close())
				return
			}
			if written == 0 {
				done <- errors.Join(fmt.Errorf("terminal environment pipe made no write progress"), file.Close())
				return
			}
			remaining = remaining[written:]
		}
		done <- file.Close()
	}()
	select {
	case err := <-done:
		return err
	case <-ctx.Done():
		return ctx.Err()
	case <-time.After(timeout):
		return fmt.Errorf("write terminal environment pipe: timeout")
	}
}

func readPIDFile(path string) (int, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, fmt.Errorf("read terminal pid: %w", err)
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return 0, fmt.Errorf("invalid terminal pid")
	}
	return pid, nil
}

func childPID(status map[string]any) int {
	path, _ := status["child_pid_file"].(string)
	if path == "" {
		return 0
	}
	pid, _ := readPIDFile(path)
	return pid
}

func carrierID(status map[string]any) string {
	value, _ := status["carrier_id"].(string)
	if value == "" {
		value, _ = status["tmux_session"].(string)
	}
	return value
}

func intStatus(status map[string]any, key string) int {
	switch value := status[key].(type) {
	case int:
		return value
	case float64:
		return int(value)
	case string:
		parsed, _ := strconv.Atoi(value)
		return parsed
	default:
		return 0
	}
}
