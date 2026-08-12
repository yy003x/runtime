package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yy003x/runtime/internal/infrastructure/layout"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
	runtimeprofile "github.com/yy003x/runtime/pkg/profile"
	runtimetmux "github.com/yy003x/runtime/pkg/tmux"
)

func TestParseTmuxOpenOptionsRequiresProfileBeforeTypedOptions(t *testing.T) {
	options, err := parseTmuxOpenOptions([]string{
		"cx", "--model", "gpt-5.6-sol", "--effort=high",
		"--prompt", "prompt.txt", "--cwd", "work", "reply ok",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.profileID != "cx" || options.model == nil ||
		*options.model != "gpt-5.6-sol" || options.effort == nil ||
		*options.effort != runtimecommand.EffortHigh ||
		options.prompt == nil || *options.prompt != "prompt.txt" ||
		options.cwd == nil || *options.cwd != "work" ||
		options.input == nil || *options.input != "reply ok" {
		t.Fatalf("options = %#v", options)
	}
	for _, args := range [][]string{
		{"--exec", "cx"},
		{"cx", "--exec"},
		{"--model", "late", "cx"},
		{"cx", "--model", "one", "--model", "two"},
		{"cx", "--prompt", "--model", "one"},
		{"cx", "--prompt", "--exec"},
		{"--cwd="},
		{"cx", "one", "two"},
		{"cx", "one", "--model", "late"},
	} {
		if _, err := parseTmuxOpenOptions(args); err == nil {
			t.Fatalf("accepted invalid args: %#v", args)
		}
	}
}

func TestTmuxOpenRejectsInvalidOptionsBeforeProfileLoad(t *testing.T) {
	paths, err := layout.FromHome(filepath.Join(t.TempDir(), "missing-home"))
	if err != nil {
		t.Fatal(err)
	}
	output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
	err = runTmuxNamespaceVNext(
		paths, []string{"open", "--exec", "cx"}, output,
	)
	var runtimeErr *contract.RuntimeError
	if !errors.As(err, &runtimeErr) ||
		runtimeErr.Code != contract.ErrorInvalidRequest ||
		runtimeErr.Phase != contract.PhaseRequest {
		t.Fatalf("error = %#v", err)
	}
}

func TestTmuxOpenRejectsReservedProfileID(t *testing.T) {
	paths := prepareVNextHome(t)
	writeVNextCommand(t, paths.ConfigDir, "profile")
	output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
	err := runTmuxNamespaceVNext(
		paths, []string{"open", "profile"}, output,
	)
	if err == nil || !strings.Contains(err.Error(), "reserved profile ID") {
		t.Fatalf("error = %v", err)
	}
}

func TestResolveTmuxOpenInvocationUsesInteractiveAdapterAndPromptOrder(t *testing.T) {
	root := t.TempDir()
	t.Setenv("SN_CLI_HOME", root)
	commandPath := filepath.Join(root, "codex")
	if err := os.WriteFile(commandPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "base.txt"), []byte("base"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "typed.txt"), []byte("typed"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	configDir := filepath.Join(root, "configs")
	if err := os.Mkdir(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	config := map[string]any{
		"type": "cli", "command": commandPath,
		"model": "old", "effort": "low", "prompt": "base.txt",
	}
	data, err := json.Marshal(config)
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
	options, err := parseTmuxOpenOptions([]string{
		"cx", "--model", "new", "--effort", "high",
		"--prompt", "typed.txt", "--cwd", "work",
		"positional",
	})
	if err != nil {
		t.Fatal(err)
	}
	invocation, err := resolveTmuxOpenInvocation(
		catalog, options, "stdin", root,
		[]string{"PATH=" + root, "KEEP=value"}, filepath.Join(root, "logs"),
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
	if invocation.ProfileID != "cx" || invocation.Path != commandPath ||
		invocation.CWD != work || len(invocation.ConfigDigest) != 64 {
		t.Fatalf("invocation = %#v", invocation)
	}
}

func TestRunTmuxNamespaceMachineListEnvelope(t *testing.T) {
	manager := &fakeTmuxManager{
		windows: []runtimetmux.Window{{
			SchemaVersion: 1, TmuxID: "id", State: runtimetmux.StateRunning,
			WindowID: "@1", PaneID: "%1",
		}},
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	output := newCLIOutput(true, &stdout, &stderr)
	if err := runTmuxNamespaceWith(
		context.Background(), manager, nil, []string{"list"}, output,
		nil, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	var payload map[string]any
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["schema_version"] != float64(1) ||
		payload["contract_version"] != float64(5) {
		t.Fatalf("envelope = %#v", payload)
	}
	values, ok := payload["tmux_windows"].([]any)
	if !ok || len(values) != 1 {
		t.Fatalf("windows = %#v", payload["tmux_windows"])
	}
}

func TestRunTmuxNamespaceSendMergesPipeBeforePositional(t *testing.T) {
	manager := &fakeTmuxManager{}
	stdin := tmuxInputFile(t, "from pipe")
	var stdout bytes.Buffer
	output := newCLIOutput(false, &stdout, &bytes.Buffer{})
	if err := runTmuxNamespaceWith(
		context.Background(), manager, nil,
		[]string{"send", "--tmux-id", "id", "positional"},
		output, stdin, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	if manager.sentID != "id" ||
		manager.sentInput != "from pipe\npositional" {
		t.Fatalf(
			"send id=%q input=%q", manager.sentID, manager.sentInput,
		)
	}
}

func TestRunTmuxNamespaceAttachIsHumanOnly(t *testing.T) {
	output := newCLIOutput(true, &bytes.Buffer{}, &bytes.Buffer{})
	err := runTmuxNamespaceWith(
		context.Background(), &fakeTmuxManager{}, nil,
		[]string{"attach", "--tmux-id", "id"}, output,
		nil, nil, nil,
	)
	if err == nil || !strings.Contains(err.Error(), "human-only") {
		t.Fatalf("error = %v", err)
	}
}

func TestRunTmuxNamespaceStopAllStopsOnlyUnboundSnapshot(t *testing.T) {
	manager := &fakeTmuxManager{windows: []runtimetmux.Window{
		{TmuxID: "first"}, {TmuxID: "second"},
	}}
	var stdout bytes.Buffer
	if err := runTmuxNamespaceWith(
		context.Background(), manager, nil, []string{"stop-all"},
		newCLIOutput(true, &stdout, &bytes.Buffer{}), nil, nil, nil,
	); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(manager.stoppedIDs, []string{"first", "second"}) {
		t.Fatalf("stopped=%v", manager.stoppedIDs)
	}
	var payload struct {
		Action       string   `json:"action"`
		Accepted     bool     `json:"accepted"`
		StoppedCount int      `json:"stopped_count"`
		TmuxIDs      []string `json:"tmux_ids"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Action != "stop-all" || !payload.Accepted ||
		payload.StoppedCount != 2 ||
		!reflect.DeepEqual(payload.TmuxIDs, []string{"first", "second"}) {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestRunTmuxNamespaceStopAllRejectsSessionBindingsBeforeMutation(t *testing.T) {
	manager := &fakeTmuxManager{windows: []runtimetmux.Window{
		{TmuxID: "raw"},
		{
			TmuxID: "bound",
			Binding: &runtimetmux.Binding{
				Kind: "session", ID: "session_11111111111111111111111111111111",
			},
		},
	}}
	err := runTmuxNamespaceWith(
		context.Background(), manager, nil, []string{"stop-all"},
		newCLIOutput(true, &bytes.Buffer{}, &bytes.Buffer{}), nil, nil, nil,
	)
	var runtimeErr *contract.RuntimeError
	if !errors.As(err, &runtimeErr) || runtimeErr.Code != contract.ErrorConflict ||
		!strings.Contains(runtimeErr.Message, "session close-all") {
		t.Fatalf("error=%#v", err)
	}
	if len(manager.stoppedIDs) != 0 {
		t.Fatalf("stop-all mutated before binding rejection: %v", manager.stoppedIDs)
	}
}

type fakeTmuxManager struct {
	windows    []runtimetmux.Window
	sentID     string
	sentInput  string
	stoppedIDs []string
}

func (manager *fakeTmuxManager) Start(
	context.Context,
	runtimetmux.StartRequest,
) (runtimetmux.StartResult, error) {
	return runtimetmux.StartResult{}, nil
}

func (manager *fakeTmuxManager) List(
	context.Context,
) ([]runtimetmux.Window, error) {
	return manager.windows, nil
}

func (manager *fakeTmuxManager) Show(
	context.Context,
	string,
) (runtimetmux.Window, error) {
	return runtimetmux.Window{}, nil
}

func (manager *fakeTmuxManager) Send(
	_ context.Context,
	tmuxID string,
	input string,
) (runtimetmux.ActionResult, error) {
	manager.sentID = tmuxID
	manager.sentInput = input
	return runtimetmux.ActionResult{
		TmuxID: tmuxID, Action: "send", Accepted: true,
	}, nil
}

func (manager *fakeTmuxManager) SendFramed(
	ctx context.Context,
	tmuxID string,
	input string,
) (runtimetmux.ActionResult, error) {
	return manager.Send(ctx, tmuxID, input)
}

func (manager *fakeTmuxManager) Attach(
	context.Context,
	string,
	runtimetmux.TTYFiles,
) error {
	return nil
}

func (manager *fakeTmuxManager) Interrupt(
	_ context.Context,
	tmuxID string,
) (runtimetmux.ActionResult, error) {
	return runtimetmux.ActionResult{
		TmuxID: tmuxID, Action: "interrupt", Accepted: true,
	}, nil
}

func (manager *fakeTmuxManager) Stop(
	_ context.Context,
	tmuxID string,
) (runtimetmux.ActionResult, error) {
	manager.stoppedIDs = append(manager.stoppedIDs, tmuxID)
	return runtimetmux.ActionResult{
		TmuxID: tmuxID, Action: "stop", Accepted: true,
	}, nil
}

func tmuxInputFile(t *testing.T, value string) *os.File {
	t.Helper()
	path := filepath.Join(t.TempDir(), "stdin")
	if err := os.WriteFile(path, []byte(value), 0o600); err != nil {
		t.Fatal(err)
	}
	file, err := os.Open(path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = file.Close() })
	return file
}
