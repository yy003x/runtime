package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"agent-runtime/internal/daemon"
)

type Config struct {
	Daemon        *daemon.Client
	SessionName   string
	PollInterval  time.Duration
	ReadyTimeout  time.Duration
	ReadySettle   time.Duration
	StableTimeout time.Duration
	Depends       []daemon.Dependency
	Execution     daemon.ExecutionEnvironment
}

type Backend struct {
	config Config
}

type SendOptions struct {
	Submit    bool
	Bracketed bool
	Stabilize bool
}

type StartOptions struct {
	LogFile             string
	ExitFile            string
	RestartMaxAttempts  int
	RestartDelaySeconds float64
}

func New(config Config) *Backend {
	if config.PollInterval <= 0 {
		config.PollInterval = 100 * time.Millisecond
	}
	if config.ReadyTimeout <= 0 {
		config.ReadyTimeout = 10 * time.Second
	}
	if config.ReadySettle <= 0 {
		config.ReadySettle = 300 * time.Millisecond
	}
	return &Backend{config: config}
}

func (b *Backend) StartShell(ctx context.Context, runID, cwd, command string) (string, error) {
	return b.StartShellWithOptions(ctx, runID, cwd, command, StartOptions{})
}

func (b *Backend) StartShellWithOptions(ctx context.Context, runID, cwd, command string, options StartOptions) (string, error) {
	if b.config.Daemon == nil {
		return "", fmt.Errorf("daemon client is required")
	}
	base := b.config.SessionName
	if regexp.MustCompile(`^[0-9]+$`).MatchString(base) {
		return "", fmt.Errorf("numeric tmux session_name is not allowed: %s", base)
	}
	suffix := runID
	if len(suffix) > 12 {
		suffix = suffix[len(suffix)-12:]
	}
	session := sanitizeName(base + "-" + suffix)
	started, err := b.config.Daemon.StartTmux(ctx, daemon.TmuxStartRequest{
		ProcessID: runID, Session: session, CWD: cwd, Command: command,
		LogFile: options.LogFile, ExitFile: options.ExitFile,
		RestartMaxAttempts: options.RestartMaxAttempts, RestartDelaySeconds: options.RestartDelaySeconds,
		Depends: b.config.Depends, Execution: b.config.Execution,
	})
	if err != nil {
		return "", err
	}
	if err := b.waitReady(ctx, started); err != nil {
		_ = b.Kill(context.Background(), started)
		return "", err
	}
	return started, nil
}

func (b *Backend) Send(ctx context.Context, session, text string, options SendOptions) error {
	if options.Submit {
		if options.Stabilize && b.config.StableTimeout > 0 {
			_ = b.waitStable(ctx, session, b.config.StableTimeout, b.config.ReadySettle)
		}
	}
	return b.config.Daemon.SendTmux(ctx, "", session, text, options.Submit, options.Bracketed)
}

func (b *Backend) waitReady(ctx context.Context, session string) error {
	if b.waitStable(ctx, session, b.config.ReadyTimeout, b.config.ReadySettle) {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("tmux session did not become ready: %s", session)
}

func (b *Backend) waitStable(ctx context.Context, session string, timeout, settle time.Duration) bool {
	deadline := time.Now().Add(timeout)
	lastOutput := ""
	stableSince := time.Now()
	for time.Now().Before(deadline) {
		alive, err := b.HasSession(ctx, session)
		if ctx.Err() != nil || err != nil || !alive {
			return false
		}
		output, err := b.Capture(ctx, session, 200)
		if err == nil && output != lastOutput {
			lastOutput = output
			stableSince = time.Now()
		}
		if time.Since(stableSince) >= settle {
			return true
		}
		timer := time.NewTimer(b.config.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return false
		case <-timer.C:
		}
	}
	return false
}

func (b *Backend) Capture(ctx context.Context, session string, tail int) (string, error) {
	return b.config.Daemon.CaptureTmux(ctx, "", session, tail)
}

func (b *Backend) HasSession(ctx context.Context, session string) (bool, error) {
	if session == "" {
		return false, nil
	}
	return b.config.Daemon.HasTmux(ctx, "", session)
}

func (b *Backend) Kill(ctx context.Context, session string) error {
	if session == "" {
		return nil
	}
	return b.config.Daemon.KillTmux(ctx, "", session)
}

func (b *Backend) Interrupt(ctx context.Context, session string) error {
	return b.config.Daemon.InterruptTmux(ctx, "", session)
}

func Attach(ctx context.Context, session string) error {
	command := commandContext(ctx, "attach-session", "-t", session)
	command.Stdin, command.Stdout, command.Stderr = os.Stdin, os.Stdout, os.Stderr
	return command.Run()
}

func commandContext(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "tmux", args...)
}

func sanitizeName(value string) string {
	value = strings.Map(func(char rune) rune {
		if (char >= 'a' && char <= 'z') || (char >= 'A' && char <= 'Z') || (char >= '0' && char <= '9') || char == '-' || char == '_' {
			return char
		}
		return '-'
	}, value)
	return strings.Trim(value, "-")
}
