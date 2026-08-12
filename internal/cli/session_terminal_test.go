package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
	runtime "github.com/yy003x/runtime/run"
	"github.com/yy003x/runtime/session"
	runtimetmux "github.com/yy003x/runtime/tmux"
)

func TestSessionTerminalOpenOptionsStayAtOneActionLevel(t *testing.T) {
	sessionID := "session_11111111111111111111111111111111"
	value, err := parseSessionTerminalOpenOptions([]string{
		"cx", "--session-id", sessionID, "--retention", "pinned",
		"--model", "fixture", "--effort", "high", "--cwd", "work",
		"continue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.profileID != "cx" || value.sessionID != sessionID ||
		value.retention != session.RetentionPinned || !value.retentionSet ||
		value.model != "fixture" || value.effort != "high" ||
		value.cwd != "work" || value.input != "continue" {
		t.Fatalf("options = %#v", value)
	}
	for _, args := range [][]string{
		{}, {"--session-id", sessionID}, {"cx", "--unknown", "value"},
		{"cx", "--effort", "extreme"}, {"cx", "first", "second"},
	} {
		if _, err := parseSessionTerminalOpenOptions(args); err == nil {
			t.Fatalf("accepted invalid args %#v", args)
		}
	}
}

func TestSessionTerminalFramePreservesStructuredMultilineInput(t *testing.T) {
	input := "first line\n第二行\n  final  "
	frame, err := encodeSessionTerminalInput(input)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(frame, "\n") || !strings.HasPrefix(frame, sessionTerminalInputPrefix) {
		t.Fatalf("frame = %q", frame)
	}
	decoded, err := decodeSessionTerminalInput(frame)
	if err != nil {
		t.Fatal(err)
	}
	if decoded != input {
		t.Fatalf("decoded = %q, want %q", decoded, input)
	}
	if _, err := decodeSessionTerminalInput(sessionTerminalInputPrefix + "%%%"); err == nil {
		t.Fatal("accepted invalid terminal frame")
	}
	prompt, closeRequested, err := decodeSessionTerminalLine(sessionTerminalCloseFrame)
	if err != nil || prompt != "" || !closeRequested {
		t.Fatalf(
			"close frame = prompt %q close %t error %v",
			prompt, closeRequested, err,
		)
	}
	if _, _, err := decodeSessionTerminalLine(
		sessionTerminalControlPrefix + "unknown",
	); err == nil {
		t.Fatal("accepted unknown terminal control frame")
	}
}

func TestFindSessionTerminalUsesOpaqueTmuxBinding(t *testing.T) {
	sessionID := "session_11111111111111111111111111111111"
	manager := &fakeTmuxManager{windows: []runtimetmux.Window{
		{TmuxID: "other"},
		{
			TmuxID:  "bound",
			Binding: &runtimetmux.Binding{Kind: "session", ID: sessionID},
		},
	}}
	value, err := findSessionTerminal(context.Background(), manager, sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if value.TmuxID != "bound" {
		t.Fatalf("window = %#v", value)
	}
}

func TestSessionTerminalHelperCreatesDurableRunAndCanonicalTurn(t *testing.T) {
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := layout.FromHome(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(home, "bin", "codex")
	if err := os.WriteFile(command, []byte(`#!/bin/sh
printf '%s\n' \
  '{"type":"item.completed","item":{"type":"agent_message","text":"terminal done"}}' \
  '{"type":"turn.completed"}'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	profileID := "terminal-test"
	profileJSON := fmt.Sprintf(
		`{"type":"cli","command":%q,"model":"fixture"}`,
		command,
	)
	if err := os.WriteFile(
		filepath.Join(paths.ConfigDir, profileID+".json"),
		[]byte(profileJSON), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	setup, err := runtimebootstrap.LoadSessionServices(paths, fixedNamespaces...)
	if err != nil {
		t.Fatal(err)
	}
	sessionID := "session_22222222222222222222222222222222"
	if _, err := setup.Sessions.CreateWithID(
		sessionID, session.RetentionStandard,
	); err != nil {
		t.Fatal(err)
	}
	prepared, runtimeErr := setup.Sessions.PrepareRunRequest(session.RunRequest{
		SessionID: sessionID, ProfileID: profileID, Input: "preflight",
		InvocationBase: home, Retention: session.RetentionStandard,
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	config := sessionTerminalHelperConfig{
		SessionID: sessionID, ProfileID: profileID,
		Retention: session.RetentionStandard, Model: prepared.Model,
		Effort: prepared.Effort, CWD: prepared.CWD, InvocationBase: home,
		ConfigDigest:     prepared.ConfigDigest(),
		BasePromptDigest: prepared.BasePromptDigest(),
	}
	frame, err := encodeSessionTerminalInput("do the work")
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	if err := serveSessionTerminal(
		context.Background(), paths, config,
		strings.NewReader(frame+"\n"+sessionTerminalCloseFrame+"\n"),
		&stdout, &stderr,
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "terminal done") ||
		!strings.Contains(stdout.String(), "canonical Session retained") ||
		stderr.Len() != 0 {
		t.Fatalf("stdout=%q stderr=%q", stdout.String(), stderr.String())
	}
	messages, err := setup.Sessions.Messages(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[0].Message.Content != "do the work" ||
		messages[1].Message.Content != "terminal done" {
		t.Fatalf("messages = %#v", messages)
	}
	queries, err := runtimebootstrap.LoadRunQueryServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer queries.Runs.Close()
	runs, err := queries.Runs.List(context.Background(), runtime.ListFilter{
		Kind: runtime.KindSession, State: runtime.StateCompleted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Request.SessionID != sessionID ||
		runs[0].Request.Labels["interface"] != "session_terminal" {
		t.Fatalf("runs = %#v", runs)
	}
}

func TestSessionTerminalOpenRunsThroughRealTmuxCarrier(t *testing.T) {
	if _, err := exec.LookPath("tmux"); err != nil {
		t.Skip("tmux is not installed")
	}
	home := t.TempDir()
	if err := os.Chmod(home, 0o700); err != nil {
		t.Fatal(err)
	}
	paths, err := layout.FromHome(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	config, err := os.ReadFile(filepath.Join("..", "..", "release", "tmux.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.TmuxConfigFile, config, 0o600); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(home, "bin", "codex")
	if err := os.WriteFile(command, []byte(`#!/bin/sh
printf '%s\n' \
  '{"type":"item.completed","item":{"type":"agent_message","text":"tmux terminal done"}}' \
  '{"type":"turn.completed"}'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	profileID := "tmux-terminal-test"
	profileJSON := fmt.Sprintf(
		`{"type":"cli","command":%q,"model":"fixture"}`,
		command,
	)
	if err := os.WriteFile(
		filepath.Join(paths.ConfigDir, profileID+".json"),
		[]byte(profileJSON), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv(layout.HomeEnv, home)
	manager, err := runtimebootstrap.LoadTmuxService(paths)
	if err != nil {
		t.Fatal(err)
	}
	result, err := openSessionTerminal(
		context.Background(), paths, manager,
		sessionTerminalOpenOptions{
			profileID: profileID, retention: session.RetentionStandard,
			cwd: home, input: "from real tmux",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = manager.Stop(context.Background(), result.Window.TmuxID)
	})
	if !result.LaunchAccepted || !result.InitialAccepted ||
		result.Window.Binding == nil ||
		result.Window.Binding.ID != result.Session.ID {
		t.Fatalf("open result = %#v", result)
	}
	maintenance, err := runtimebootstrap.LoadSessionMaintenanceServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	deadline := time.Now().Add(10 * time.Second)
	for {
		messages, readErr := maintenance.Sessions.Messages(result.Session.ID, 0)
		if readErr == nil && len(messages) == 2 &&
			messages[1].Message.Content == "tmux terminal done" {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("terminal Turn did not settle: messages=%#v error=%v", messages, readErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	var closeOutput bytes.Buffer
	if err := runSessionTerminalAction(
		paths, "close", []string{"--session-id", result.Session.ID},
		newCLIOutput(false, &closeOutput, &bytes.Buffer{}),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(closeOutput.String(), "terminal close: accepted=true") {
		t.Fatalf("close output = %q", closeOutput.String())
	}
	windows, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 0 {
		t.Fatalf("windows after close = %#v", windows)
	}
	retained, err := maintenance.Sessions.Get(result.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.State != session.SessionIdle || retained.ActiveTurnID != "" {
		t.Fatalf("retained Session = %#v", retained)
	}
}
