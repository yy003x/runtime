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
	profile := `{"type":"cli","cli":{"driver":"generic","executor":"tmux","command":{"binary":"sh","args":[],"model":""},"tmux":{"session_name":"agentrun-test","session_ready_settle_seconds":0.1},"runtime":{"prompt_delivery":"paste","result_contract":"optional"}}}`
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

func TestTmuxTaskRequiresDoneFileAndResult(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "reply.sh")
	writeExecutable(t, script, `#!/bin/sh
while IFS= read -r line; do
  printf '{"schema_version":1,"run_id":"%s","outcome":"succeeded","summary":"tmux ok","artifacts":[],"errors":[],"validation":{"commands":[],"passed":true}}\n' "$AGENTRUN_RUN_ID" > "$AGENTRUN_RESULT_FILE"
  : > "$AGENTRUN_DONE_FILE"
  printf 'reply:%s\n' "$line"
done
`)
	profile := `{"type":"cli","timeout_seconds":5,"cli":{"driver":"generic","executor":"tmux","command":{"binary":"` + script + `","args":[],"model":""},"tmux":{"session_name":"agentrun-task-test","session_ready_settle_seconds":0.1,"poll_interval_seconds":0.05,"silence_threshold_seconds":0.1},"runtime":{"prompt_delivery":"paste","result_contract":"optional"}}}`
	if err := os.WriteFile(filepath.Join(root, "configs", "tmux-task.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	startAgentRunTestDaemon(t, service)
	run, err := service.Run(context.Background(), RunOptions{Profile: "tmux-task", CWD: root, Prompt: "hello", ExecutionMode: ModeManaged})
	if err != nil || run.State != StateDone {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	result, err := service.ReadResult(RunTask, run.RunID)
	if err != nil || result.Summary != "tmux ok" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	paths, _ := RunPaths(service.RunsDir, RunTask, run.RunID)
	if info, err := os.Stat(paths.DoneFile); err != nil || info.Size() != 0 {
		t.Fatalf("done file info=%#v err=%v", info, err)
	}
}

func TestTmuxDoneWithoutResultFails(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "reply.sh")
	writeExecutable(t, script, "#!/bin/sh\nwhile IFS= read -r line; do : > \"$AGENTRUN_DONE_FILE\"; done\n")
	profile := `{"type":"cli","timeout_seconds":5,"cli":{"driver":"generic","executor":"tmux","command":{"binary":"` + script + `","args":[],"model":""},"tmux":{"session_name":"agentrun-task-missing","session_ready_settle_seconds":0.1,"poll_interval_seconds":0.05},"runtime":{"prompt_delivery":"paste","result_contract":"optional"}}}`
	if err := os.WriteFile(filepath.Join(root, "configs", "tmux-task.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	startAgentRunTestDaemon(t, service)
	run, err := service.Run(context.Background(), RunOptions{Profile: "tmux-task", CWD: root, Prompt: "hello", ExecutionMode: ModeCapture})
	if err == nil || run.State != StateFailed || run.FailureReason != "result_missing" {
		t.Fatalf("run=%#v err=%v", run, err)
	}
}

func TestCancelTmuxRunCleansDaemonRegistry(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux not installed")
	}
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(root, "wait.sh")
	writeExecutable(t, script, "#!/bin/sh\nwhile IFS= read -r line; do sleep 30; done\n")
	profile := `{"type":"cli","timeout_seconds":30,"cli":{"driver":"generic","executor":"tmux","command":{"binary":"` + script + `","args":[],"model":""},"tmux":{"session_name":"agentrun-cancel","session_ready_settle_seconds":0.05,"poll_interval_seconds":0.05},"runtime":{"prompt_delivery":"paste","result_contract":"optional"}}}`
	if err := os.WriteFile(filepath.Join(root, "configs", "tmux-cancel.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	startAgentRunTestDaemon(t, service)
	runID := "task-20260714-000000-cancel"
	done := make(chan error, 1)
	go func() {
		_, err := service.Run(context.Background(), RunOptions{RunID: runID, Profile: "tmux-cancel", CWD: root, Prompt: "wait", ExecutionMode: ModeCapture})
		done <- err
	}()
	deadline := time.Now().Add(3 * time.Second)
	found := false
	for time.Now().Before(deadline) {
		status, err := service.DaemonClient().Status(context.Background())
		if err == nil && len(status.Processes) == 1 {
			found = true
			break
		}
		time.Sleep(20 * time.Millisecond)
	}
	if !found {
		t.Fatal("tmux process was not registered")
	}
	if _, err := service.Cancel(RunTask, runID); err != nil {
		t.Fatal(err)
	}
	select {
	case <-done:
	case <-time.After(3 * time.Second):
		t.Fatal("cancelled run did not return")
	}
	status, err := service.DaemonClient().Status(context.Background())
	if err != nil || len(status.Processes) != 0 {
		t.Fatalf("daemon status=%#v err=%v", status, err)
	}
}

func startAgentRunTestDaemon(t *testing.T, service *Service) {
	t.Helper()
	dir, err := os.MkdirTemp("/tmp", "agentrun-daemon-")
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("AGENT_RUNTIME_DAEMON_DIR", dir)
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
				_ = os.RemoveAll(dir)
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
