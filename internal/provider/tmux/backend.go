package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/daemon"
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
	WaitForOutput       bool
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
	ready, err := os.CreateTemp("", "sn-runtime-tmux-ready-*")
	if err != nil {
		return "", fmt.Errorf("create tmux ready marker: %w", err)
	}
	readyFile := ready.Name()
	if closeErr := ready.Close(); closeErr != nil {
		_ = os.Remove(readyFile)
		return "", fmt.Errorf("close tmux ready marker: %w", closeErr)
	}
	_ = os.Remove(readyFile)
	defer os.Remove(readyFile)
	started, err := b.config.Daemon.StartTmux(ctx, daemon.TmuxStartRequest{
		ProcessID: runID, Session: session, CWD: cwd, Command: command,
		LogFile: options.LogFile, ExitFile: options.ExitFile, ReadyFile: readyFile,
		RestartMaxAttempts: options.RestartMaxAttempts, RestartDelaySeconds: options.RestartDelaySeconds,
		Depends: b.config.Depends, Execution: b.config.Execution,
	})
	if err != nil {
		return "", err
	}
	if err := b.waitReadyFile(ctx, readyFile); err != nil {
		if options.ExitFile != "" {
			if _, exitErr := os.Stat(options.ExitFile); exitErr == nil {
				return started, nil
			}
		}
		_ = b.Kill(context.Background(), started)
		return "", err
	}
	if err := b.waitReady(ctx, started, options.WaitForOutput); err != nil {
		if options.ExitFile != "" {
			if _, exitErr := os.Stat(options.ExitFile); exitErr == nil {
				return started, nil
			}
		}
		_ = b.Kill(context.Background(), started)
		return "", err
	}
	return started, nil
}

func (b *Backend) waitReadyFile(ctx context.Context, path string) error {
	deadline := time.Now().Add(b.config.ReadyTimeout)
	for time.Now().Before(deadline) {
		if _, err := os.Stat(path); err == nil {
			return nil
		}
		timer := time.NewTimer(b.config.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
	return fmt.Errorf("tmux process did not signal readiness: %s", path)
}

func (b *Backend) Send(ctx context.Context, session, text string, options SendOptions) error {
	if options.Submit {
		if options.Stabilize && b.config.StableTimeout > 0 {
			_ = b.waitStable(ctx, session, b.config.StableTimeout, b.config.ReadySettle, false)
		}
	}
	return b.config.Daemon.SendTmux(ctx, "", session, text, options.Submit, options.Bracketed)
}

func (b *Backend) waitReady(ctx context.Context, session string, waitForOutput bool) error {
	if b.waitStable(ctx, session, b.config.ReadyTimeout, b.config.ReadySettle, waitForOutput) {
		return nil
	}
	if ctx.Err() != nil {
		return ctx.Err()
	}
	return fmt.Errorf("tmux session did not become ready: %s", session)
}

func (b *Backend) waitStable(ctx context.Context, session string, timeout, settle time.Duration, waitForOutput bool) bool {
	deadline := time.Now().Add(timeout)
	startedAt := time.Now()
	lastOutput := ""
	stableSince := time.Now()
	seenOutput := false
	noOutputDelay := 500 * time.Millisecond
	if waitForOutput {
		noOutputDelay = timeout / 2
		if noOutputDelay > 8*time.Second {
			noOutputDelay = 8 * time.Second
		}
		if noOutputDelay < 500*time.Millisecond {
			noOutputDelay = 500 * time.Millisecond
		}
	}
	if settle > noOutputDelay {
		noOutputDelay = settle
	}
	for time.Now().Before(deadline) {
		alive, err := b.HasSession(ctx, session)
		if ctx.Err() != nil || err != nil || !alive {
			return false
		}
		output, err := b.Capture(ctx, session, 200)
		if err == nil {
			if strings.TrimSpace(output) != "" {
				seenOutput = true
			}
			if output != lastOutput {
				lastOutput = output
				stableSince = time.Now()
			}
		}
		if time.Since(stableSince) >= settle && (seenOutput || time.Since(startedAt) >= noOutputDelay) {
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
