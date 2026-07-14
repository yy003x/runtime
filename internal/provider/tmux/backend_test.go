package tmux

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-runtime/internal/daemon"
)

func TestBackendStartSendAndCapture(t *testing.T) {
	requireTmux(t)
	backend := New(Config{
		Daemon:       startBackendTestDaemon(t),
		SessionName:  "provider-tmux-test",
		PollInterval: 20 * time.Millisecond,
		ReadyTimeout: 2 * time.Second,
		ReadySettle:  50 * time.Millisecond,
	})
	script := writeScript(t, `#!/bin/sh
while IFS= read -r line; do
  printf 'reply:%s\n' "$line"
done
`)
	session, err := backend.StartShell(context.Background(), "task-start-send", t.TempDir(), "exec "+shellQuote(script))
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = backend.Kill(context.Background(), session) })
	if err := backend.Send(context.Background(), session, "hello", SendOptions{Submit: true}); err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(5 * time.Second)
	lastOutput := ""
	for time.Now().Before(deadline) {
		output, captureErr := backend.Capture(context.Background(), session, 50)
		lastOutput = output
		if captureErr == nil && strings.Contains(output, "reply:hello") {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	t.Fatalf("tmux session did not emit pasted input; pane=%q", lastOutput)
}

func TestExecuteTaskRequiresDoneAndReportsResult(t *testing.T) {
	requireTmux(t)
	dir := t.TempDir()
	resultFile := filepath.Join(dir, "result.json")
	doneFile := filepath.Join(dir, "done")
	script := writeScript(t, fmt.Sprintf(`#!/bin/sh
while IFS= read -r line; do
  printf '{"summary":"ok"}\n' > %s
  : > %s
  printf 'completed\n'
done
`, shellQuote(resultFile), shellQuote(doneFile)))
	backend := New(Config{
		Daemon:       startBackendTestDaemon(t),
		SessionName:  "provider-task-test",
		PollInterval: 20 * time.Millisecond,
		ReadyTimeout: 2 * time.Second,
		ReadySettle:  50 * time.Millisecond,
	})
	result, err := backend.ExecuteTask(context.Background(), TaskRequest{
		RunID:      "task-complete",
		CWD:        dir,
		Command:    "exec " + shellQuote(script),
		Prompt:     "finish this",
		ResultFile: resultFile,
		DoneFile:   doneFile,
	}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Done || !result.Result || result.ExitCode != 0 {
		t.Fatalf("result=%#v", result)
	}
	if info, statErr := os.Stat(doneFile); statErr != nil || info.Size() != 0 {
		t.Fatalf("done info=%#v err=%v", info, statErr)
	}
}

func TestExecuteTaskPropagatesObserverFailure(t *testing.T) {
	requireTmux(t)
	backend := New(Config{
		Daemon:       startBackendTestDaemon(t),
		SessionName:  "provider-observer-test",
		PollInterval: 20 * time.Millisecond,
		ReadyTimeout: 2 * time.Second,
		ReadySettle:  50 * time.Millisecond,
	})
	result, err := backend.ExecuteTask(context.Background(), TaskRequest{
		RunID: "observer-failure", CWD: t.TempDir(), Command: "exec sleep 30",
		Prompt: "unused", ResultFile: filepath.Join(t.TempDir(), "result.json"), DoneFile: filepath.Join(t.TempDir(), "done"),
	}, func(string, map[string]any) error { return fmt.Errorf("observer failed") })
	if err == nil || !strings.Contains(err.Error(), "observer failed") || result.ExitCode != 1 {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func startBackendTestDaemon(t *testing.T) *daemon.Client {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "provider-tmux-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	config := daemon.Config{Home: t.TempDir(), Dir: dir, Version: "provider-test", IdleTimeout: time.Minute}
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	server := daemon.NewServer(config)
	go func() { done <- server.Run(ctx) }()
	client := daemon.NewClient(config)
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		if _, err := client.Status(context.Background()); err == nil {
			t.Cleanup(func() {
				_ = client.Shutdown(context.Background(), true)
				select {
				case <-done:
				case <-time.After(3 * time.Second):
					cancel()
				}
				_ = os.RemoveAll(dir)
			})
			return client
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	t.Fatal("test daemon did not start")
	return nil
}

func TestSanitizeName(t *testing.T) {
	if got := sanitizeName("agent:run / 01"); got != "agent-run---01" {
		t.Fatalf("sanitizeName=%q", got)
	}
}

func requireTmux(t *testing.T) {
	t.Helper()
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
}

func writeScript(t *testing.T, content string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), "backend-test.sh")
	if err := os.WriteFile(path, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	return path
}

func shellQuote(value string) string {
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
