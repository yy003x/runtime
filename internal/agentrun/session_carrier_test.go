package agentrun

import (
	"context"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestTerminalCarrierCreatesIndependentRecordedExecution(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	settings := "default_project: _default\ndefault_profile: shell\nsession:\n  default_carrier: terminal\n  terminal:\n    driver: ghostty\n"
	if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), []byte(settings), 0o644); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(root, "terminal-command.sh")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nprintf 'terminal-ok\\n'\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	profile := `{"type":"cli","cli":{"driver":"generic","executor":"command","command":{"binary":"` + command + `","args":[],"model":"","env":{"TERMINAL_TEST_SECRET":"${TERMINAL_TEST_SECRET}"}},"runtime":{"prompt_delivery":"stdin"}}}`
	if err := os.WriteFile(filepath.Join(root, "configs", "shell.json"), []byte(profile), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TERMINAL_TEST_SECRET", "must-not-persist")

	previousLauncher := terminalLaunchCommandFn
	terminalLaunchCommandFn = func(ctx context.Context, _ string, wrapperPath string) *exec.Cmd {
		return exec.CommandContext(ctx, "/bin/sh", "-c", `nohup /bin/sh "$1" >"$2" 2>&1 &`, "terminal-test", wrapperPath, wrapperPath+".stderr")
	}
	t.Cleanup(func() { terminalLaunchCommandFn = previousLauncher })

	service := New(root)
	started, err := service.StartSessionWithOptions(context.Background(), SessionOptions{Profile: "shell", Carrier: "terminal", CWD: root})
	if err != nil {
		files, _ := filepath.Glob(filepath.Join(root, "runs", "session", "*", "*", "*"))
		var diagnostics []string
		for _, path := range files {
			data, _ := os.ReadFile(path)
			diagnostics = append(diagnostics, filepath.Base(path)+":"+string(data))
		}
		t.Fatalf("%v\n%s", err, strings.Join(diagnostics, "\n"))
	}
	if started.SessionID == "" || started.RunID == "" || started.ExecutionID == "" || started.SessionID == started.RunID || started.Carrier != "terminal" {
		t.Fatalf("summary=%#v", started)
	}

	var status SessionSummary
	for deadline := time.Now().Add(5 * time.Second); time.Now().Before(deadline); {
		status, err = service.SessionStatus(context.Background(), started.RunID)
		if err == nil && status.State == StateDone {
			break
		}
		time.Sleep(50 * time.Millisecond)
	}
	if err != nil || status.State != StateDone || status.Alive {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	logs, err := service.SessionLogs(context.Background(), started.RunID, 20)
	if err != nil || !strings.Contains(logs.Content, "terminal-ok") {
		t.Fatalf("logs=%q err=%v", logs.Content, err)
	}
	view, err := NewSessionManager(service).Store().View(started.SessionID)
	if err != nil || len(view.Executions) != 1 {
		t.Fatalf("view=%#v err=%v", view, err)
	}
	execution := view.Executions[0]
	if execution.Kind != ExecutionTerminal || execution.Carrier != "terminal" || execution.CarrierID == "" || execution.RunIDs[0] != started.RunID || execution.TranscriptRef == "" {
		t.Fatalf("execution=%#v", execution)
	}
	result, err := service.ReadResult(RunSession, started.RunID)
	if err != nil || result.CaptureQuality != CaptureTranscriptOnly || result.ResultKind != "execution_summary" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	matches, _ := filepath.Glob(filepath.Join(started.RunDir, "*"))
	if len(matches) == 0 {
		t.Fatal("terminal run artifacts are missing")
	}
	for _, path := range matches {
		data, readErr := os.ReadFile(path)
		if readErr == nil && strings.Contains(string(data), "must-not-persist") {
			t.Fatalf("secret persisted in %s", path)
		}
	}
}

func TestTerminalLaunchCommandUsesExplicitDriver(t *testing.T) {
	ghostty := terminalLaunchCommand(context.Background(), "ghostty", "/tmp/wrapper.sh")
	if got := strings.Join(ghostty.Args, " "); !strings.Contains(got, "Ghostty.app") || !strings.Contains(got, "/tmp/wrapper.sh") {
		t.Fatalf("ghostty argv=%q", got)
	}
	iterm := terminalLaunchCommand(context.Background(), "iterm2", "/tmp/wrapper.sh")
	if got := strings.Join(iterm.Args, " "); !strings.Contains(got, "iTerm2") || !strings.Contains(got, "/tmp/wrapper.sh") {
		t.Fatalf("iterm argv=%q", got)
	}
}

func TestResolveSessionCarrierRejectsUnsupportedTerminalDriver(t *testing.T) {
	service := &Service{TerminalDriver: "unknown", DefaultCarrier: "terminal"}
	if _, err := service.resolveSessionCarrier("terminal"); err == nil || !strings.Contains(err.Error(), "does not support driver") {
		t.Fatalf("resolveSessionCarrier=%v", err)
	}
}
