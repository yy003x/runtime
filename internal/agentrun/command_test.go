package agentrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-runtime/internal/daemon"
)

func TestCommandLifecycleWithTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := `{"command":"sh"}`
	if err := os.WriteFile(filepath.Join(root, "configs", "tmux-test.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	startAgentRunTestDaemon(t, service)
	started, err := service.StartCommand(context.Background(), CommandOptions{Profile: "tmux-test", CWD: root, Argv: []string{"printf", "command-ok"}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = service.CommandStop(context.Background(), started.RunID) })
	var status SessionSummary
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		status, err = service.CommandStatus(context.Background(), started.RunID)
		if err != nil || status.State == StateDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || status.State != StateDone {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	logs, err := service.CommandLogs(context.Background(), started.RunID, 30)
	if err != nil || !strings.Contains(logs.Content, "command-ok") {
		t.Fatalf("logs=%#v err=%v", logs, err)
	}
	stopped, err := service.CommandStop(context.Background(), started.RunID)
	if err != nil || stopped.Alive || stopped.State != StateDone {
		t.Fatalf("stopped=%#v err=%v", stopped, err)
	}
}

func TestCommandDeadlineStopsAndCleansTmux(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := `{"command":"sh"}`
	if err := os.WriteFile(filepath.Join(root, "configs", "tmux-test.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	startAgentRunTestDaemon(t, service)
	started, err := service.StartCommand(context.Background(), CommandOptions{
		Profile: "tmux-test", CWD: root, DeadlineSeconds: 1, Argv: []string{"sh", "-c", "printf start; sleep 10"},
	})
	if err != nil {
		t.Fatal(err)
	}
	var status SessionSummary
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		status, err = service.CommandStatus(context.Background(), started.RunID)
		if err != nil || terminalStateValue(status.State) {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || status.State != StateFailed || status.Alive || status.Status["returncode"] != float64(124) && status.Status["returncode"] != 124 {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	if reason, _ := status.Status["deadline_exceeded"].(bool); !reason {
		t.Fatalf("deadline flag missing: %#v", status.Status)
	}
	logs, err := service.CommandLogs(context.Background(), started.RunID, 20)
	if err != nil || !strings.Contains(logs.Content, "start") {
		t.Fatalf("logs=%q err=%v", logs.Content, err)
	}
}

func TestSessionStatusMarksMissingSessionOrphaned(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	service := New(t.TempDir())
	startAgentRunTestDaemon(t, service)
	runID := "session-20260716-160006-orphaned"
	paths, err := RunPaths(service.RunsDir, RunSession, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	request := Request{RunID: runID, RunType: RunSession, ProjectID: "test"}
	if err := service.store.WriteRequest(paths, request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.WriteStatus(paths, request, StateRunning, "", "running", map[string]any{"tmux_session": "missing-session"}); err != nil {
		t.Fatal(err)
	}
	status, err := service.SessionStatus(context.Background(), runID)
	if err != nil || status.State != StateFailed || status.Alive {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestSessionWrapsCommandProfileAndSubmitsInitialPrompt(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "session.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf 'argv:%s\\n' \"$*\"\nwhile IFS= read -r line; do printf 'reply:%s\\n' \"$line\"; done\n")
	profile := `{"command":"` + script + `","args":["base"]}`
	if err := os.WriteFile(filepath.Join(root, "configs", "shell.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	startAgentRunTestDaemon(t, service)
	started, err := service.StartSessionWithOptions(context.Background(), SessionOptions{
		Profile: "shell", CWD: root, Prompt: "hello", RawCLIArgs: []string{"raw"},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _, _ = service.SessionStop(context.Background(), started.RunID) })
	if submitted, _ := started.Status["prompt_submitted"].(bool); !submitted {
		t.Fatalf("status=%#v", started.Status)
	}
	var logs Logs
	for deadline := time.Now().Add(3 * time.Second); time.Now().Before(deadline); {
		logs, err = service.SessionLogs(context.Background(), started.RunID, 50)
		if err == nil && strings.Contains(logs.Content, "reply:hello") {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || !strings.Contains(logs.Content, "argv:base raw") || !strings.Contains(logs.Content, "reply:hello") || strings.Contains(logs.Content, "managed") {
		live, captureErr := service.DaemonClient().CaptureTmux(context.Background(), started.RunID, started.Session, 100)
		raw, _ := os.ReadFile(filepath.Join(started.RunDir, "output.log"))
		t.Fatalf("logs=%q raw=%q live=%q logs_err=%v capture_err=%v", logs.Content, raw, live, err, captureErr)
	}
	events, err := service.store.ReadEvents(mustSessionPaths(t, service, started.RunID))
	if err != nil {
		t.Fatal(err)
	}
	foundSubmitted := false
	for _, event := range events {
		if event.Type == "prompt.submitted" {
			foundSubmitted = true
		}
	}
	if !foundSubmitted {
		t.Fatalf("events=%#v", events)
	}
	listed, err := service.SessionList(context.Background())
	if err != nil || len(listed) != 1 || listed[0].RunID != started.RunID {
		t.Fatalf("sessions=%#v err=%v", listed, err)
	}
	if _, err := service.SessionStop(context.Background(), started.RunID); err != nil {
		t.Fatal(err)
	}
	result, err := service.ReadResult(RunSession, started.RunID)
	if err != nil || result.ResultKind != "execution_summary" || result.CaptureQuality != CaptureTranscriptOnly {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	view, err := NewSessionManager(service).Store().View(started.SessionID)
	if err != nil || len(view.Executions) != 1 || view.Executions[0].ResultRef == nil || view.Executions[0].ResultRef.RunID != started.RunID {
		t.Fatalf("view=%#v err=%v", view, err)
	}
	persisted, err := service.SessionLogs(context.Background(), started.RunID, 50)
	if err != nil || !strings.Contains(persisted.Content, "reply:hello") {
		t.Fatalf("persisted logs=%q err=%v", persisted.Content, err)
	}
}

func mustSessionPaths(t *testing.T, service *Service, runID string) Paths {
	t.Helper()
	paths, err := RunPaths(service.RunsDir, RunSession, runID)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}

func startAgentRunTestDaemon(t *testing.T, service *Service) {
	t.Helper()
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan error, 1)
	server := daemon.NewServer(service.DaemonConfig())
	go func() { done <- server.Run(ctx) }()
	client := service.DaemonClient()
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
			})
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	cancel()
	t.Fatal("test daemon did not start")
}

func TestPruneOnlyRemovesTerminalRegistryEntries(t *testing.T) {
	service := New(t.TempDir())
	request := Request{RunID: "task-20260713-000000-done", RunType: RunTask, ProjectID: "demo"}
	paths, _ := RunPaths(service.RunsDir, request.RunType, request.RunID)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.WriteStatus(paths, request, StateDone, "", "done", nil); err != nil {
		t.Fatal(err)
	}
	service.register(paths, request, StatePending)
	dry, err := service.Prune(true)
	if err != nil || len(dry["removed"].([]string)) != 1 {
		t.Fatalf("dry=%#v err=%v", dry, err)
	}
	applied, err := service.Prune(false)
	if err != nil || len(applied["removed"].([]string)) != 1 {
		t.Fatalf("applied=%#v err=%v", applied, err)
	}
	after, err := service.Prune(true)
	if err != nil || len(after["removed"].([]string)) != 0 {
		t.Fatalf("after=%#v err=%v", after, err)
	}
}
