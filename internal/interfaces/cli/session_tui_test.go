package cli

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/internal/testkit/reporoot"
	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
	runtimetmux "github.com/yy003x/runtime/pkg/tmux"
)

func TestSessionNativeOpenOptionsStayAtOneActionLevel(t *testing.T) {
	sessionID := "session_11111111111111111111111111111111"
	value, err := parseSessionNativeOpenOptions([]string{
		"cx", "--session-id", sessionID, "--retention", "pinned",
		"--model", "fixture", "--effort", "high", "--cwd", "work",
		"--detach", "continue",
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.profileID != "cx" || value.sessionID != sessionID ||
		value.retention != session.RetentionPinned || !value.retentionSet ||
		value.model != "fixture" || value.effort != "high" ||
		value.cwd != "work" || value.input != "continue" ||
		!value.detach || value.attach {
		t.Fatalf("options = %#v", value)
	}
	for _, args := range [][]string{
		{}, {"--session-id", sessionID}, {"cx", "--unknown", "value"},
		{"cx", "--effort", "extreme"}, {"cx", "first", "second"},
		{"cx", "--attach", "--detach"},
	} {
		if _, err := parseSessionNativeOpenOptions(args); err == nil {
			t.Fatalf("accepted invalid args %#v", args)
		}
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
		invocation.Binding.ID != sessionID || invocation.Path != command ||
		invocation.CWD != home {
		t.Fatalf("invocation = %#v", invocation)
	}
	if !reflect.DeepEqual(invocation.Argv, []string{
		command, "--model", "fixture", "--", "initial prompt",
	}) {
		t.Fatalf("argv = %#v", invocation.Argv)
	}
	if strings.Contains(strings.Join(invocation.Argv, "\x00"), "exec") ||
		strings.Contains(strings.Join(invocation.Argv, "\x00"), "--json") {
		t.Fatalf("native TUI invocation used exec argv: %#v", invocation.Argv)
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

func TestSessionNativeTUIMachineInputUsesTmuxPTYWithoutCanonicalRun(t *testing.T) {
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
	if len(runs) != 0 {
		t.Fatalf("native TUI created durable Runs: %#v", runs)
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
	retained, err := maintenance.Sessions.Get(result.Session.ID)
	if err != nil {
		t.Fatal(err)
	}
	if retained.Interface != session.InterfaceNativeTUI ||
		retained.State != session.SessionIdle || retained.ActiveTurnID != "" {
		t.Fatalf("retained Session = %#v", retained)
	}
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
