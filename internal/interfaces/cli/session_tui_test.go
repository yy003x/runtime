package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strconv"
	"strings"
	"syscall"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/application/nativeconsole"
	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	runtimetmux "github.com/yy003x/runtime/internal/infrastructure/tmux"
	"github.com/yy003x/runtime/internal/testkit/reporoot"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	runtimeprofile "github.com/yy003x/runtime/pkg/profile"
	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
)

func TestSessionNativeOpenOptionsStayAtOneActionLevel(t *testing.T) {
	sessionID := "session_11111111111111111111111111111111"
	value, err := parseSessionNativeOpenOptions([]string{
		"cx", "--session-id=" + sessionID, "--retention", "pinned",
		"--model", "fixture", "--effort=high", "--prompt", "prompt.txt",
		"--cwd", "work",
		"--detach", "continue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.profileID != "cx" || value.sessionID != sessionID ||
		value.retention != session.RetentionPinned || !value.retentionSet ||
		value.model != "fixture" || value.effort != "high" ||
		value.prompt != "prompt.txt" || value.cwd != "work" ||
		value.input != "continue" ||
		!value.detach || value.attach {
		t.Fatalf("options = %#v", value)
	}
	for _, args := range [][]string{
		{}, {"--session-id", sessionID}, {"cx", "--unknown", "value"},
		{"cx", "--effort", "extreme"}, {"cx", "first", "second"},
		{"cx", "--attach", "--detach"},
		{"cx", "--prompt", "--model", "fixture"},
		{"cx", "--attach=true"},
	} {
		if _, err := parseSessionNativeOpenOptions(args); err == nil {
			t.Fatalf("accepted invalid args %#v", args)
		}
	}
}

func TestResolveNativeTUIInvocationPreservesPromptOrder(t *testing.T) {
	root := t.TempDir()
	commandPath := filepath.Join(root, "codex")
	if err := os.WriteFile(commandPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	for name, content := range map[string]string{
		"base.txt": "base", "typed.txt": "typed",
	} {
		if err := os.WriteFile(
			filepath.Join(root, name), []byte(content), 0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "configs")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data, err := json.Marshal(map[string]any{
		"type": "cli", "command": commandPath,
		"model": "old", "effort": "low", "prompt": "base.txt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(configDir, "cx.json"), data, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	catalog, err := runtimeprofile.Load(configDir, fixedNamespaces...)
	if err != nil {
		t.Fatal(err)
	}
	model, typedPrompt, cwd, positional := "new", "typed.txt", "work", "positional"
	effort := runtimecommand.EffortHigh
	invocation, requestInput, err := resolveNativeTUIInvocation(
		catalog, nativeTUIInvocationOptions{
			profileID: "cx", model: &model, effort: &effort,
			prompt: &typedPrompt, cwd: &cwd, input: &positional,
		}, "stdin", root, []string{"PATH=" + root, "KEEP=value"},
		filepath.Join(root, "logs"),
	)
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := []string{
		commandPath, "--model", "new",
		"-c", "model_reasoning_effort=high",
		"--", "base\ntyped\nstdin\npositional",
	}
	if !reflect.DeepEqual(invocation.Argv, wantArgv) {
		t.Fatalf("argv = %#v, want %#v", invocation.Argv, wantArgv)
	}
	if requestInput != "typed\nstdin\npositional" {
		t.Fatalf("request input = %q", requestInput)
	}
	if invocation.ProfileID != "cx" || invocation.Path != commandPath ||
		invocation.CWD != work || len(invocation.ConfigDigest) != 64 {
		t.Fatalf("invocation = %#v", invocation)
	}
}

func TestFindSessionNativeTUIUsesOpaqueTmuxBinding(t *testing.T) {
	sessionID := "session_11111111111111111111111111111111"
	manager := &fakeTmuxManager{windows: []runtimetmux.Window{
		{TmuxID: "other"},
		{
			TmuxID:  "bound",
			Binding: &runtimetmux.Binding{Kind: "session", ID: sessionID},
		},
	}}
	value, err := findSessionNativeTUI(
		context.Background(), manager, sessionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if value.TmuxID != "bound" {
		t.Fatalf("window = %#v", value)
	}
}

func TestCloseAllSessionNativeTUIsClosesOnlySessionBindings(t *testing.T) {
	first := "session_11111111111111111111111111111111"
	second := "session_22222222222222222222222222222222"
	manager := &fakeTmuxManager{windows: []runtimetmux.Window{
		{
			TmuxID: "first", State: runtimetmux.StateExited,
			Binding: &runtimetmux.Binding{Kind: "session", ID: first},
		},
		{TmuxID: "raw", State: runtimetmux.StateExited},
		{
			TmuxID: "second", State: runtimetmux.StateExited,
			Binding: &runtimetmux.Binding{Kind: "session", ID: second},
		},
	}}
	result, err := closeAllSessionNativeTUIs(context.Background(), manager)
	if err != nil {
		t.Fatal(err)
	}
	if !result.Accepted || result.ClosedCount != 2 ||
		len(result.Closed) != 2 ||
		!reflect.DeepEqual(manager.stoppedIDs, []string{"first", "second"}) {
		t.Fatalf("result=%#v stopped=%v", result, manager.stoppedIDs)
	}
}

func TestOpenSessionNativeTUILaunchesInteractiveProfile(t *testing.T) {
	home := t.TempDir()
	paths, err := layout.FromHome(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(home, "codex")
	if err := os.WriteFile(command, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	profileID := "native-tui-test"
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
	sessionID := "session_33333333333333333333333333333333"
	manager := &fakeTmuxManager{startResult: runtimetmux.StartResult{
		Window: runtimetmux.Window{
			SchemaVersion: 1, TmuxID: "tmux-native",
			State: runtimetmux.StateRunning, WindowID: "@1", PaneID: "%1",
		},
		LaunchAccepted: true,
	}}
	result, err := openSessionNativeTUI(
		context.Background(), paths, manager,
		sessionNativeOpenOptions{
			profileID: profileID, sessionID: sessionID,
			retention: session.RetentionPinned,
			cwd:       home, input: "initial prompt",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if result.Session.ID != sessionID ||
		result.Session.Interface != session.InterfaceNativeTUI ||
		result.Session.Retention != session.RetentionPinned ||
		!result.LaunchAccepted || !result.InitialInputSupplied {
		t.Fatalf("result = %#v", result)
	}
	if manager.startRequest == nil {
		t.Fatal("tmux Start was not called")
	}
	invocation := manager.startRequest.Invocation
	if invocation.Binding == nil || invocation.Binding.Kind != "session" ||
		invocation.Binding.ID != sessionID || !invocation.CooperativeReady ||
		invocation.CWD != home ||
		len(invocation.Argv) != 6 ||
		invocation.Argv[1] != nativeconsole.SupervisorCommand ||
		invocation.Argv[2] != "--manifest" ||
		invocation.Argv[4] != "--digest" {
		t.Fatalf(
			"supervisor invocation path=%q argv=%q cwd=%q binding=%#v cooperative=%t",
			invocation.Path, invocation.Argv, invocation.CWD,
			invocation.Binding, invocation.CooperativeReady,
		)
	}
	if result.Run.Request.Kind != runtime.KindNativeTUI ||
		result.Run.Request.SessionID != sessionID ||
		result.Run.Request.ExecutionID != result.Execution.ID ||
		result.Execution.State != runtime.NativeTUIExecutionRunning ||
		result.Execution.CaptureQuality != runtime.NativeTUICaptureOpaque {
		t.Fatalf("lifecycle result=%#v", result)
	}
	for _, argument := range invocation.Argv {
		if argument == "exec" || argument == "--json" {
			t.Fatalf("native TUI invocation used managed argv token %q", argument)
		}
	}
	maintenance, err := runtimebootstrap.LoadSessionMaintenanceServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := maintenance.Sessions.Messages(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	events, err := maintenance.Sessions.Events(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 || len(events) != 0 {
		t.Fatalf("native TUI created canonical facts: messages=%#v events=%#v", messages, events)
	}
	services, err := runtimebootstrap.LoadSessionServices(paths, fixedNamespaces...)
	if err != nil {
		t.Fatal(err)
	}
	_, runtimeErr := services.Sessions.Run(context.Background(), session.RunRequest{
		SessionID: sessionID, ProfileID: profileID, Input: "must not execute",
		InvocationBase: home, Retention: session.RetentionPinned,
	})
	if runtimeErr == nil || !strings.Contains(runtimeErr.Message, "native_tui") {
		t.Fatalf("managed execution error = %#v", runtimeErr)
	}
}

func TestSessionNativeTUIMachineInputUsesTmuxPTYWithLifecycleRun(t *testing.T) {
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
	config, err := os.ReadFile(filepath.Join(reporoot.Root(t), "release", "tmux.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.TmuxConfigFile, config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.RuntimeConfigFile,
		[]byte(`{"tmux":{"server_mode":"dedicated"}}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	factFile := filepath.Join(home, "native-tui.fact")
	command := buildNativeTUITestTarget(t, home)
	profileID := "native-tmux-test"
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
	t.Setenv("SN_NATIVE_TUI_FACT", factFile)
	manager, err := runtimebootstrap.LoadTmuxService(paths)
	if err != nil {
		t.Fatal(err)
	}
	initialInput := "initial native prompt"
	result, err := openSessionNativeTUI(
		context.Background(), paths, manager,
		sessionNativeOpenOptions{
			profileID: profileID, retention: session.RetentionStandard,
			cwd: home, input: initialInput,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = manager.Stop(context.Background(), result.Window.TmuxID)
	})
	if !result.LaunchAccepted || result.Session.Interface != session.InterfaceNativeTUI ||
		result.Window.Binding == nil || result.Window.Binding.ID != result.Session.ID {
		t.Fatalf("open result = %#v", result)
	}
	waitForNativeTUIFact(t, factFile, "tty:true", "<initial native prompt>")

	emptyStdin := tmuxInputFile(t, "")
	previousStdin := os.Stdin
	os.Stdin = emptyStdin
	t.Cleanup(func() { os.Stdin = previousStdin })
	secondInput := "raw input through tmux"
	var sendOutput bytes.Buffer
	if err := runSessionNativeAction(
		paths, "send",
		[]string{"--session-id", result.Session.ID, secondInput},
		newCLIOutput(false, &sendOutput, &bytes.Buffer{}),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(sendOutput.String(), "send: accepted=true") {
		t.Fatalf("send output = %q", sendOutput.String())
	}
	waitForNativeTUIFact(t, factFile, "input:"+secondInput)

	maintenance, err := runtimebootstrap.LoadSessionMaintenanceServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := maintenance.Sessions.Messages(result.Session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	events, err := maintenance.Sessions.Events(result.Session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 || len(events) != 0 {
		t.Fatalf("native TUI persisted canonical Turn facts: messages=%#v events=%#v", messages, events)
	}
	queries, err := runtimebootstrap.LoadRunQueryServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer queries.Runs.Close()
	runs, err := queries.Runs.List(context.Background(), runtime.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(runs) != 1 || runs[0].Request.Kind != runtime.KindNativeTUI ||
		runs[0].ID != result.Run.ID ||
		runs[0].Request.ExecutionID != result.Execution.ID ||
		runs[0].State != runtime.StateRunning {
		t.Fatalf("native TUI lifecycle Runs: %#v", runs)
	}
	var showOutput bytes.Buffer
	if err := runSessionNamespace(
		paths, []string{"show", "--session-id", result.Session.ID},
		newCLIOutput(true, &showOutput, &bytes.Buffer{}),
	); err != nil {
		t.Fatal(err)
	}
	var shown struct {
		Session   session.Session            `json:"session"`
		Run       runtime.Record             `json:"run"`
		Execution runtime.NativeTUIExecution `json:"execution"`
		Window    *runtimetmux.Window        `json:"tmux_window"`
	}
	if err := json.Unmarshal(showOutput.Bytes(), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Session.ID != result.Session.ID || shown.Run.ID != result.Run.ID ||
		shown.Execution.ID != result.Execution.ID ||
		shown.Execution.State != runtime.NativeTUIExecutionRunning ||
		shown.Window == nil || shown.Window.TmuxID != result.Window.TmuxID {
		t.Fatalf("native TUI show projection = %#v", shown)
	}
	var listOutput bytes.Buffer
	if err := runSessionNamespace(
		paths, []string{"list", "--interface", "native_tui"},
		newCLIOutput(true, &listOutput, &bytes.Buffer{}),
	); err != nil {
		t.Fatal(err)
	}
	var listed struct {
		Sessions []session.Session `json:"sessions"`
	}
	if err := json.Unmarshal(listOutput.Bytes(), &listed); err != nil {
		t.Fatal(err)
	}
	if len(listed.Sessions) != 1 || listed.Sessions[0].ID != result.Session.ID {
		t.Fatalf("native TUI Session list = %#v", listed.Sessions)
	}

	var closeOutput bytes.Buffer
	if err := runSessionNativeAction(
		paths, "close", []string{"--session-id", result.Session.ID},
		newCLIOutput(false, &closeOutput, &bytes.Buffer{}),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(closeOutput.String(), "close: accepted=true") {
		t.Fatalf("close output = %q", closeOutput.String())
	}
	windows, err := manager.List(context.Background())
	if err != nil {
		t.Fatal(err)
	}
	if len(windows) != 0 {
		t.Fatalf("windows after close = %#v", windows)
	}
	settled, err := queries.Runs.Get(context.Background(), result.Run.ID)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != runtime.StateCancelled ||
		settled.SettledSequence == 0 {
		t.Fatalf("lifecycle Run after close = %#v", settled)
	}
	retained, err := maintenance.Sessions.Get(result.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Interface != session.InterfaceNativeTUI ||
		retained.State != session.SessionIdle || retained.ActiveTurnID != "" {
		t.Fatalf("retained Session = %#v", retained)
	}
	showOutput.Reset()
	if err := runSessionNamespace(
		paths, []string{"show", "--session-id", result.Session.ID},
		newCLIOutput(true, &showOutput, &bytes.Buffer{}),
	); err != nil {
		t.Fatal(err)
	}
	shown = struct {
		Session   session.Session            `json:"session"`
		Run       runtime.Record             `json:"run"`
		Execution runtime.NativeTUIExecution `json:"execution"`
		Window    *runtimetmux.Window        `json:"tmux_window"`
	}{}
	if err := json.Unmarshal(showOutput.Bytes(), &shown); err != nil {
		t.Fatal(err)
	}
	if shown.Run.State != runtime.StateCancelled ||
		shown.Execution.State != runtime.NativeTUIExecutionSettled ||
		shown.Execution.Outcome != runtime.NativeTUIOutcomeCancelled ||
		shown.Window != nil {
		t.Fatalf("settled native TUI show projection = %#v", shown)
	}
}

func TestSessionNativeTUIProcessExitSettlesRunAndClosesWindow(t *testing.T) {
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
	config, err := os.ReadFile(filepath.Join(reporoot.Root(t), "release", "tmux.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.TmuxConfigFile, config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.RuntimeConfigFile,
		[]byte(`{"tmux":{"server_mode":"dedicated"}}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	command := filepath.Join(home, "codex")
	if err := os.WriteFile(
		command, []byte("#!/bin/sh\nsleep 0.2\nexit 0\n"), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	profileID := "native-exit-test"
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
	opened, err := openSessionNativeTUI(
		context.Background(), paths, manager,
		sessionNativeOpenOptions{
			profileID: profileID, retention: session.RetentionStandard,
			cwd: home,
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	queries, err := runtimebootstrap.LoadRunQueryServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	defer queries.Runs.Close()
	var settled runtime.Record
	deadline := time.Now().Add(10 * time.Second)
	for {
		settled, err = queries.Runs.Get(context.Background(), opened.Run.ID)
		if err == nil && settled.State.Terminal() {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("native_tui Run did not settle: record=%#v error=%v", settled, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
	if settled.State != runtime.StateCompleted || settled.SettledSequence == 0 {
		t.Fatalf("native_tui terminal Run = %#v", settled)
	}
	execution, err := runtime.NativeTUIExecutionFromRecord(settled)
	if err != nil {
		t.Fatal(err)
	}
	if execution.ID != opened.Execution.ID ||
		execution.SessionID != opened.Session.ID ||
		execution.State != runtime.NativeTUIExecutionSettled ||
		execution.Outcome != runtime.NativeTUIOutcomeCompleted ||
		execution.ExitCode == nil || *execution.ExitCode != 0 ||
		execution.CompletionReason != "process_exited" ||
		execution.CaptureQuality != runtime.NativeTUICaptureOpaque {
		t.Fatalf("native_tui execution = %#v", execution)
	}
	for {
		windows, listErr := manager.List(context.Background())
		if listErr == nil && len(windows) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("native_tui window was not auto-closed: windows=%#v error=%v", windows, listErr)
		}
		time.Sleep(25 * time.Millisecond)
	}
	maintenance, err := runtimebootstrap.LoadSessionMaintenanceServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	messages, err := maintenance.Sessions.Messages(opened.Session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	events, err := maintenance.Sessions.Events(opened.Session.ID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 0 || len(events) != 0 {
		t.Fatalf("native TUI exit created canonical Turn facts: messages=%#v events=%#v", messages, events)
	}
}

func TestSessionNativeTUICloseForcesIgnoringProviderAndWaitsForSupervisorExit(
	t *testing.T,
) {
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
	config, err := os.ReadFile(filepath.Join(reporoot.Root(t), "release", "tmux.conf"))
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.TmuxConfigFile, config, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.RuntimeConfigFile,
		[]byte(`{"tmux":{"server_mode":"dedicated"}}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	factFile := filepath.Join(home, "native-tui-force-close.fact")
	command := buildNativeTUITestTarget(t, home)
	profileID := "native-force-close-test"
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
	t.Setenv("SN_NATIVE_TUI_FACT", factFile)
	t.Setenv("SN_NATIVE_TUI_IGNORE_TERM", "1")
	manager, err := runtimebootstrap.LoadTmuxService(paths)
	if err != nil {
		t.Fatal(err)
	}
	opened, err := openSessionNativeTUI(
		context.Background(), paths, manager,
		sessionNativeOpenOptions{
			profileID: profileID, retention: session.RetentionStandard,
			cwd: home,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_, _ = manager.Stop(context.Background(), opened.Window.TmuxID)
	})
	waitForNativeTUIFact(t, factFile, "ignore_termination:true")
	providerPID := nativeTUIFactPID(t, factFile, "pid")
	supervisorPID := nativeTUIFactPID(t, factFile, "parent_pid")

	if _, err := closeSessionNativeTUILifecycle(
		context.Background(), paths, opened.Session.ID,
	); err != nil {
		t.Fatal(err)
	}
	assertProcessExited(t, providerPID)
	assertProcessExited(t, supervisorPID)
}

func waitForNativeTUIFact(t *testing.T, path string, fragments ...string) {
	t.Helper()
	deadline := time.Now().Add(10 * time.Second)
	for {
		data, err := os.ReadFile(path)
		if err == nil {
			value := string(data)
			matched := true
			for _, fragment := range fragments {
				if !strings.Contains(value, fragment) {
					matched = false
					break
				}
			}
			if matched {
				return
			}
		}
		if time.Now().After(deadline) {
			data, _ := os.ReadFile(path)
			t.Fatalf("native TUI fact did not contain %q: value=%q error=%v", fragments, data, err)
		}
		time.Sleep(25 * time.Millisecond)
	}
}

func nativeTUIFactPID(t *testing.T, path string, key string) int {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	prefix := key + ":"
	for _, line := range strings.Split(string(data), "\n") {
		if !strings.HasPrefix(line, prefix) {
			continue
		}
		pid, parseErr := strconv.Atoi(strings.TrimPrefix(line, prefix))
		if parseErr != nil || pid <= 0 {
			t.Fatalf("invalid %s in native TUI fact: %q", key, line)
		}
		return pid
	}
	t.Fatalf("native TUI fact lacks %s: %q", key, data)
	return 0
}

func assertProcessExited(t *testing.T, pid int) {
	t.Helper()
	err := syscall.Kill(pid, 0)
	if !errors.Is(err, syscall.ESRCH) {
		t.Fatalf("process %d remains after session close: %v", pid, err)
	}
}

func buildNativeTUITestTarget(t *testing.T, home string) string {
	t.Helper()
	target := filepath.Join(home, "codex")
	command := exec.Command(
		"go", "build", "-o", target, "./internal/testkit/nativetuitarget",
	)
	command.Dir = reporoot.Root(t)
	if output, err := command.CombinedOutput(); err != nil {
		t.Fatalf("build native TUI target: %v\n%s", err, output)
	}
	return target
}
