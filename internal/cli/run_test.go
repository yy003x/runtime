package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
	runtime "github.com/yy003x/runtime/run"
	sqlitestore "github.com/yy003x/runtime/store/sqlite"
)

func TestRenderRunResultPrintsStoredAgentAnswer(t *testing.T) {
	result, err := json.Marshal(map[string]any{
		"outcome": map[string]any{
			"message": contract.Message{
				Role: contract.RoleAssistant, Content: "stored answer",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	output := newCLIOutput(false, &stdout, &bytes.Buffer{})
	if err := renderRunResult(output, runtime.Record{
		ID: "run_2", State: runtime.StateCompleted,
		Result: result, SettledSequence: 9,
	}); err != nil {
		t.Fatal(err)
	}
	if value := stdout.String(); !strings.Contains(value, "stored answer\n") ||
		!strings.Contains(value, "settled_sequence=9") {
		t.Fatalf("output=%q", value)
	}
}

func TestRunWatchSuccessEndsWithUniqueFinal(t *testing.T) {
	fixture := executeAgentStreamFixture(t, 200, `{
	  "id":"watch-success",
	  "model":"fixture",
	  "choices":[{
	    "message":{"role":"assistant","content":"watch me"},
	    "finish_reason":"stop"
	  }]
	}`)
	if fixture.Err != nil {
		t.Fatal(fixture.Err)
	}
	agentInspection := inspectRunStream(t, fixture.Stdout.String())
	if agentInspection.FinalCount != 1 || agentInspection.Run.ID == "" {
		t.Fatalf("agent stream=%#v", agentInspection)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	output := newCLIOutput(false, &stdout, &stderr)
	if err := runRunNamespaceVNext(
		fixture.Paths,
		[]string{
			"watch", "--run-id", agentInspection.Run.ID,
		},
		output,
	); err != nil {
		t.Fatal(err)
	}
	inspection := inspectRunStream(t, stdout.String())
	if inspection.EventCount == 0 || inspection.FinalCount != 1 ||
		inspection.FinalIndex != inspection.LineCount-1 ||
		inspection.Run.ID != agentInspection.Run.ID {
		t.Fatalf("inspection=%#v stdout=%q", inspection, stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunWatchFailureAfterEventHasNoFinal(t *testing.T) {
	fixture := executeAgentStreamFixture(t, 200, `{
	  "id":"watch-failure",
	  "model":"fixture",
	  "choices":[{
	    "message":{"role":"assistant","content":"watch failure fixture"},
	    "finish_reason":"stop"
	  }]
	}`)
	if fixture.Err != nil {
		t.Fatal(fixture.Err)
	}
	agentInspection := inspectRunStream(t, fixture.Stdout.String())
	if agentInspection.FinalCount != 1 || agentInspection.Run.ID == "" {
		t.Fatalf("agent stream=%#v", agentInspection)
	}

	stdout := &failAfterWrites{remaining: 1}
	var stderr bytes.Buffer
	output := newCLIOutput(false, stdout, &stderr)
	err := runRunNamespaceVNext(
		fixture.Paths,
		[]string{
			"watch", "--run-id", agentInspection.Run.ID,
		},
		output,
	)
	if err == nil {
		t.Fatal("expected event consumer failure")
	}
	if !output.streamMode || !output.streamStarted {
		t.Fatalf(
			"streamMode=%t streamStarted=%t error=%v",
			output.streamMode, output.streamStarted, err,
		)
	}
	if exitCode := output.fail(err); exitCode != 1 {
		t.Fatalf("exit=%d", exitCode)
	}
	inspection := inspectRunStream(t, stdout.buffer.String())
	if inspection.EventCount != 1 || inspection.FinalCount != 0 {
		t.Fatalf(
			"inspection=%#v stdout=%q",
			inspection, stdout.buffer.String(),
		)
	}
	assertSingleV4StreamError(
		t, stdout.buffer.String(), stderr.String(),
	)
}

func TestRunSubmitIsRemovedBeforeStatefulBootstrap(t *testing.T) {
	paths := prepareVNextHome(t)
	err := runRunNamespaceVNext(
		paths,
		[]string{"submit", "--profile", "api", "hello"},
		newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{}),
	)
	if err == nil || !strings.Contains(err.Error(), `unknown run action "submit"`) {
		t.Fatalf("error=%v", err)
	}
	if _, statErr := os.Stat(paths.RunDBFile); !os.IsNotExist(statErr) {
		t.Fatalf("removed run submit created Run database: %v", statErr)
	}
}

func TestRunManagementOptionsRejectUnknownDuplicateAndTrailingArgs(
	t *testing.T,
) {
	for _, args := range [][]string{
		{"--run-id", "run_1", "--unknown"},
		{"--run-id", "run_1", "--run-id", "run_2"},
		{"--run-id", "run_1", "trailing"},
		{"--run-id", "--apply"},
	} {
		if err := validateManagementArgs(
			args, []string{"--run-id"}, []string{"--apply"},
		); err == nil {
			t.Fatalf("accepted args=%#v", args)
		}
	}
	if err := validateManagementArgs(
		[]string{
			"--run-id", "run_1", "--after-seq", "2", "--apply",
		},
		[]string{"--run-id", "--after-seq"},
		[]string{"--apply"},
	); err != nil {
		t.Fatal(err)
	}
}

func TestRunManagementPreflightRejectsBeforeStatefulBootstrap(t *testing.T) {
	paths := prepareVNextHome(t)
	for _, args := range [][]string{
		{"unknown"},
		{
			"submit", "--profile", "cx", "hello",
			"--kind", "session",
		},
		{"list", "--state", "unknown"},
		{"list", "--state", ""},
		{"list", "--state", "queued", "--state", "running"},
		{"list", "--kind", "unknown"},
		{"list", "--kind", ""},
		{"list", "--kind", "agent", "--kind", "session"},
		{"list", "--limit", "-1"},
		{"list", "--limit", "1001"},
		{"list", "--limit", ""},
		{"list", "--limit", "1", "--limit", "2"},
		{"gc", "--older-than", ""},
		{"gc", "--limit", "0"},
		{"get", "--run-id", "run_1", "trailing"},
		{"reconcile", "--run-id", ""},
		{
			"resume", "--run-id", "run_1",
			"--input-json", "", "--input-file", "resume.json",
		},
	} {
		output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
		if err := runRunNamespaceVNext(paths, args, output); err == nil {
			t.Fatalf("accepted args=%#v", args)
		}
		if _, err := os.Stat(paths.SessionsDir); !os.IsNotExist(err) {
			t.Fatalf(
				"invalid args=%#v bootstrapped Session state: %v",
				args, err,
			)
		}
		if _, err := os.Stat(paths.RunDBFile); !os.IsNotExist(err) {
			t.Fatalf(
				"invalid args=%#v opened Run database: %v",
				args, err,
			)
		}
	}
}

func TestRunManagementPreflightAcceptsBoundedFilters(t *testing.T) {
	defaultFilter, err := parseRunListFilter(nil)
	if err != nil {
		t.Fatal(err)
	}
	if defaultFilter != (runtime.ListFilter{
		Limit: runtime.DefaultListLimit,
	}) {
		t.Fatalf("default filter=%#v", defaultFilter)
	}
	if err := validateRunManagementInvocation([]string{
		"list", "--state", "needs_reconciliation",
		"--kind", "session", "--limit", "1000",
	}); err != nil {
		t.Fatal(err)
	}
	output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
	if err := runRunNamespaceVNext(
		prepareVNextHome(t),
		[]string{"watch", "--run-id", "--unknown"},
		output,
	); err == nil {
		t.Fatal("accepted invalid watch")
	}
	if !output.streamMode || output.streamStarted {
		t.Fatal("invalid watch did not select machine stream errors")
	}
}

func TestResumeInputClassifiesFileShapeBeforeRuntimeBootstrap(t *testing.T) {
	rawInput := " \n [1,{\"answer\":true}] \t"
	preserved, err := readResumeInput([]string{"--input-json", rawInput})
	if err != nil {
		t.Fatal(err)
	}
	if string(preserved) != rawInput {
		t.Fatalf("resume JSON changed: got=%q want=%q", preserved, rawInput)
	}

	root := t.TempDir()
	regular := filepath.Join(root, "regular.json")
	if err := os.WriteFile(regular, []byte(`{"answer":true}`), 0o600); err != nil {
		t.Fatal(err)
	}
	directory := filepath.Join(root, "directory")
	if err := os.Mkdir(directory, 0o700); err != nil {
		t.Fatal(err)
	}
	symlink := filepath.Join(root, "symlink.json")
	if err := os.Symlink(regular, symlink); err != nil {
		t.Fatal(err)
	}
	oversized := filepath.Join(root, "oversized.json")
	if err := os.WriteFile(
		oversized, []byte(`"`+strings.Repeat("x", (1<<20)+1)+`"`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	duplicate := filepath.Join(root, "duplicate.json")
	if err := os.WriteFile(
		duplicate, []byte(`{"answer":true,"answer":false}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	trailing := filepath.Join(root, "trailing.json")
	if err := os.WriteFile(
		trailing, []byte(`{"answer":true} {}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	for _, path := range []string{
		directory, symlink, oversized, duplicate, trailing,
	} {
		_, err := readResumeInput([]string{"--input-file", path})
		var validationErr *cliValidationError
		if err == nil || !errors.As(err, &validationErr) {
			t.Fatalf("path=%s error=%v, want CLI validation", path, err)
		}
		assertMachineErrorCode(t, err, contract.ErrorInvalidRequest)
	}

	missing := filepath.Join(root, "missing.json")
	_, err = readResumeInput([]string{"--input-file", missing})
	var validationErr *cliValidationError
	if err == nil || errors.As(err, &validationErr) ||
		!errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing error=%v, want internal file I/O", err)
	}
	assertMachineErrorCode(t, err, contract.ErrorInternal)

	paths := prepareVNextHome(t)
	err = runRunNamespaceVNext(
		paths,
		[]string{
			"resume",
			"--run-id", "run_00000000000000000000000000000000",
			"--input-file", symlink,
		},
		newCLIOutput(true, &bytes.Buffer{}, &bytes.Buffer{}),
	)
	if err == nil {
		t.Fatal("invalid resume file was accepted")
	}
	if _, statErr := os.Stat(paths.SessionsDir); !os.IsNotExist(statErr) {
		t.Fatalf("invalid resume bootstrapped Session state: %v", statErr)
	}
	if _, statErr := os.Stat(paths.RunDBFile); !os.IsNotExist(statErr) {
		t.Fatalf("invalid resume opened Run database: %v", statErr)
	}
}

func TestRunManagementUsesCanonicalIDAndNotFoundErrors(t *testing.T) {
	const missingRunID = "run_00000000000000000000000000000000"
	if err := validateRunManagementInvocation([]string{
		"get", "--run-id", missingRunID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateRunManagementInvocation([]string{
		"get", "--run-id", "run_missing",
	}); err == nil {
		t.Fatal("malformed run ID was accepted")
	}

	output := newCLIOutput(true, &bytes.Buffer{}, &bytes.Buffer{})
	err := runRunNamespaceVNext(
		prepareVNextHome(t),
		[]string{"get", "--run-id", missingRunID},
		output,
	)
	var runtimeErr *contract.RuntimeError
	if !errors.As(err, &runtimeErr) ||
		runtimeErr.Code != contract.ErrorNotFound {
		t.Fatalf("error=%v", err)
	}
}

func TestRunCompositionIgnoresUnrelatedExecutionInputs(t *testing.T) {
	ctx := context.Background()
	paths := prepareVNextHome(t)
	writeVNextModel(
		t, paths.ConfigDir, "api",
		"https://example.invalid/v1/chat/completions",
	)
	if err := os.WriteFile(
		paths.RuntimeConfigFile,
		[]byte(`{"agent":{"tools":["read_file","list_directory"]}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	setup, err := runtimebootstrap.LoadServices(paths, paths.Home)
	if err != nil {
		t.Fatal(err)
	}
	submitted, runtimeErr := setup.Runs.Submit(ctx, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "cancel",
		AgentBudget: agent.DefaultBudget(),
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	cancelID := submitted.ID
	if err := setup.Runs.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(paths.ConfigDir, "api.json"),
		[]byte(`{"type":"api","driver":`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.RuntimeConfigFile, []byte(`{"run":`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		paths.RunDBFile,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	queryID := "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if _, err := store.Create(ctx, queryID, runtime.Request{
		Kind: runtime.KindSession, ProfileID: "missing", Input: "stored",
		SessionID: "session_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(ctx, queryID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Settle(
		ctx, queryID, runtime.StateCompleted,
		json.RawMessage(`{"stored":true}`), nil,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	for _, args := range [][]string{
		{"get", "--run-id", queryID},
		{"list"},
		{"result", "--run-id", queryID},
		{"events", "--run-id", queryID},
		{"watch", "--run-id", queryID},
		{"gc", "--older-than", "1h", "--limit", "10"},
	} {
		output := newCLIOutput(
			false, &bytes.Buffer{}, &bytes.Buffer{},
		)
		if err := runRunNamespaceVNext(paths, args, output); err != nil {
			t.Fatalf("args=%#v error=%v", args, err)
		}
	}
	if err := runRunNamespaceVNext(
		paths, []string{"cancel", "--run-id", cancelID},
		newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{}),
	); err != nil {
		t.Fatalf("cancel error=%v", err)
	}
	if err := runRunNamespaceVNext(
		paths, []string{"reconcile", "--run-id", queryID},
		newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{}),
	); err != nil {
		t.Fatalf("reconcile error=%v", err)
	}

	if err := runRunNamespaceVNext(
		paths, []string{"gc"},
		newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{}),
	); err == nil || !strings.Contains(err.Error(), "load runtime config") {
		t.Fatalf("default GC ignored invalid runtime config: %v", err)
	}
	if err := os.WriteFile(
		paths.RuntimeConfigFile, []byte(`{}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := runRunNamespaceVNext(
		paths, []string{"gc"},
		newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{}),
	); err != nil {
		t.Fatalf("default GC loaded broken Profile: %v", err)
	}
}

type failAfterWrites struct {
	buffer    bytes.Buffer
	remaining int
}

func (writer *failAfterWrites) Write(value []byte) (int, error) {
	if writer.remaining == 0 {
		return 0, errors.New("fixture output failure")
	}
	writer.remaining--
	return writer.buffer.Write(value)
}
