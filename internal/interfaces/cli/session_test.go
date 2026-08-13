package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"testing"

	runtimecommand "github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
	runtimemodel "github.com/yy003x/runtime/pkg/model"
	runtimeprofile "github.com/yy003x/runtime/pkg/profile"
	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
)

func TestSessionInvocationUsesTypedCLIOverridesAndRejectsUnknownOptions(t *testing.T) {
	value, err := parseSessionInvocation([]string{
		"cx", "--model", "gpt-5.6-sol", "--effort", "high",
		"--cwd", "work", "--queue", "继续",
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.model != "gpt-5.6-sol" || value.effort != "high" ||
		value.cwd != "work" || value.profileID != "cx" || !value.queue ||
		value.input != "继续" {
		t.Fatalf("value=%#v", value)
	}
	for _, option := range []string{"--unknown", "--unsupported"} {
		if _, err := parseSessionInvocation(
			[]string{"cx", option, "value", "input"},
		); err == nil {
			t.Fatalf("accepted unknown option %s", option)
		}
	}
	if _, err := parseSessionInvocation([]string{
		"cx", "--effort", "high", "--effort", "low", "input",
	}); err == nil || !strings.Contains(err.Error(), "only be used once") {
		t.Fatalf("duplicate option error=%v", err)
	}
	if _, err := parseSessionInvocation([]string{
		"cx", "--model", "--effort", "high", "input",
	}); err == nil || !strings.Contains(err.Error(), "--model requires value") {
		t.Fatalf("missing value error=%v", err)
	}
	for _, args := range [][]string{
		{"cx", "--retention", "forever", "input"},
		{"cx", "--effort", "extreme", "input"},
		{"cx", "--model", "", "input"},
		{"api-cx", "--temperature", "NaN", "input"},
		{"--queue", "cx", "input"},
		{"cx", "--queue", "--queue", "input"},
	} {
		if _, err := parseSessionInvocation(args); err == nil {
			t.Fatalf("accepted invalid invocation args=%#v", args)
		}
	}
}

func TestSessionInvocationAndProfileMismatchAreCLIValidation(t *testing.T) {
	_, err := parseSessionInvocation([]string{"--unknown"})
	var validationErr *cliValidationError
	if err == nil || !errors.As(err, &validationErr) {
		t.Fatalf("argument error=%v, want CLI validation", err)
	}
	assertMachineErrorCode(t, err, contract.ErrorInvalidRequest)

	profiles := sessionTestProfiles(t, runtimemodel.Profile{
		Driver:   runtimemodel.DriverOpenAI,
		Endpoint: "https://example.invalid/v1/chat/completions",
		Model:    "fixture",
		Headers:  map[string]string{"Authorization": "${MODEL_API_KEY}"},
		Timeout:  "1m",
	})
	for _, test := range []struct {
		action     string
		invocation sessionInvocation
	}{
		{action: "exec", invocation: sessionInvocation{profileID: "missing"}},
		{action: "req", invocation: sessionInvocation{profileID: "api-cx", model: "override"}},
		{action: "exec", invocation: sessionInvocation{profileID: "api-cx"}},
		{action: "req", invocation: sessionInvocation{profileID: "cx"}},
	} {
		err := validateSessionProfileOptions(
			test.action, test.invocation, profiles,
		)
		validationErr = nil
		if err == nil || !errors.As(err, &validationErr) {
			t.Fatalf("test=%#v error=%v, want CLI validation", test, err)
		}
	}
}

func TestSessionInvocationDoesNotClassifyStdinIOAsValidation(t *testing.T) {
	directory, err := os.Open(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	defer directory.Close()
	originalStdin := os.Stdin
	os.Stdin = directory
	defer func() { os.Stdin = originalStdin }()

	_, err = parseSessionInvocation([]string{"api-cx", "input"})
	var validationErr *cliValidationError
	if err == nil || errors.As(err, &validationErr) {
		t.Fatalf("stdin error=%v, want unclassified I/O", err)
	}
	assertMachineErrorCode(t, err, contract.ErrorInternal)
}

func TestCanonicalSessionDeleteConflict(t *testing.T) {
	err := canonicalSessionResourceError(
		fmt.Errorf("delete: %w", session.ErrConflict),
		"session", "session_00000000000000000000000000000000",
	)
	var runtimeErr *contract.RuntimeError
	if !errors.As(err, &runtimeErr) ||
		runtimeErr.Code != contract.ErrorConflict ||
		runtimeErr.Phase != contract.PhaseRun {
		t.Fatalf("error=%v", err)
	}
}

func TestSessionUnknownActionDoesNotCreateState(t *testing.T) {
	paths := prepareRuntimeHome(t)
	err := runSessionNamespace(
		paths,
		[]string{
			"unknown-action",
			"--session-id", "session_00000000000000000000000000000000",
			"--turn-id", "turn_00000000000000000000000000000000",
			"--tool-call-id", "call_missing",
			"--idempotency-key", "missing-turn",
			"--content", "result",
		},
		newCLIOutput(true, &bytes.Buffer{}, &bytes.Buffer{}),
	)
	if err == nil || !strings.Contains(err.Error(), "unknown session action") {
		t.Fatalf("error=%v", err)
	}
	if _, statErr := os.Stat(paths.SessionsDir); !os.IsNotExist(statErr) {
		t.Fatalf("unknown action bootstrapped Session state: %v", statErr)
	}
}

func TestSessionReqQueueSubmitsDurableSessionRun(t *testing.T) {
	paths := prepareRuntimeHome(t)
	writeRuntimeModel(
		t, paths.ConfigDir, "api-cx",
		"https://example.invalid/v1/chat/completions",
	)
	t.Setenv("MODEL_API_KEY", "secret")
	var stdout bytes.Buffer
	err := runSessionNamespace(
		paths,
		[]string{"req", "api-cx", "--queue", "queued turn"},
		newCLIOutput(true, &stdout, &bytes.Buffer{}),
	)
	if err != nil {
		t.Fatal(err)
	}
	var payload struct {
		Run       runtime.Record `json:"run"`
		SessionID string         `json:"session_id"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatalf("decode output=%q: %v", stdout.String(), err)
	}
	if payload.Run.State != runtime.StateQueued ||
		payload.Run.Request.Kind != runtime.KindSession ||
		payload.Run.Request.ProfileID != "api-cx" ||
		payload.Run.Request.SessionID != payload.SessionID ||
		payload.SessionID == "" {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestSessionManagementDoesNotLoadProfileOrRuntimeConfig(t *testing.T) {
	paths := prepareRuntimeHome(t)
	if err := os.WriteFile(
		paths.RuntimeConfigFile, []byte(`{"agent":{"max_rounds":0}}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.ConfigDir+"/broken.json", []byte(`{"type":"unknown"}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	if err := runSessionNamespace(
		paths, []string{"list"}, newCLIOutput(false, &stdout, os.Stderr),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "Sessions (0)") {
		t.Fatalf("output=%q", stdout.String())
	}
}

func TestRenderSessionRunResultIsActionAware(t *testing.T) {
	var stdout bytes.Buffer
	output := newCLIOutput(false, &stdout, &bytes.Buffer{})
	err := renderSessionRunResult(output, session.RunResult{
		SessionID: "session_1",
		TurnID:    "turn_1",
		State:     session.TurnCompleted,
		Message: &contract.Message{
			Role: contract.RoleAssistant, Content: "answer",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if value := stdout.String(); value !=
		"answer\nSession session_1, turn turn_1: completed\n" {
		t.Fatalf("output=%q", value)
	}
}

func TestSessionExportJSONReturnsBusinessResult(t *testing.T) {
	var stdout bytes.Buffer
	output := newCLIOutput(true, &stdout, &bytes.Buffer{})
	if err := renderSessionExportResult(
		output, "session_1", "/tmp/session.json",
	); err != nil {
		t.Fatal(err)
	}
	var payload struct {
		SessionID string `json:"session_id"`
		Output    string `json:"output"`
		Exported  bool   `json:"exported"`
	}
	if err := json.Unmarshal(stdout.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SessionID != "session_1" ||
		payload.Output != "/tmp/session.json" || !payload.Exported {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestRenderSessionGCResultIncludesSkippedCount(t *testing.T) {
	value := session.GCResult{
		Candidates: []string{"session_1", "session_2"},
		Moved:      []string{"session_1"},
		Skipped:    []string{"session_2"},
	}
	var stdout bytes.Buffer
	if err := renderSessionGCResult(
		newCLIOutput(false, &stdout, &bytes.Buffer{}), value,
	); err != nil {
		t.Fatal(err)
	}
	if got, want := stdout.String(),
		"Session GC: candidates=2 moved=1 skipped=1 apply=true\n"; got != want {
		t.Fatalf("output=%q want=%q", got, want)
	}
}

func TestSessionTokenLimitFlagIsProviderNeutral(t *testing.T) {
	openAIInvocation, err := parseSessionInvocation([]string{
		"api-cx", "--max-tokens", "128", "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	openAIProfiles := sessionTestProfiles(t, runtimemodel.Profile{
		Driver:   runtimemodel.DriverOpenAI,
		Endpoint: "https://example.invalid/v1/chat/completions", Model: "fixture",
		Headers: map[string]string{"Authorization": "${MODEL_API_KEY}"},
		Timeout: "1m",
	})
	if err := validateSessionProfileOptions(
		"req", openAIInvocation, openAIProfiles,
	); err != nil {
		t.Fatal(err)
	}
}

func TestSessionReconcileOptionsAreStrictAndMutuallyExclusive(t *testing.T) {
	sessionID, options, err := parseSessionReconcileOptions([]string{
		"--session-id", "session_11111111111111111111111111111111",
		"--terminate",
	})
	if err != nil {
		t.Fatal(err)
	}
	if sessionID == "" || !options.Terminate ||
		options.AcknowledgeUnknown {
		t.Fatalf("sessionID=%q options=%#v", sessionID, options)
	}
	for _, args := range [][]string{
		{"--session-id", sessionID, "--unknown"},
		{"--session-id", sessionID, "--terminate", "--terminate"},
		{
			"--session-id", sessionID,
			"--terminate", "--acknowledge-unknown",
		},
	} {
		if _, _, err := parseSessionReconcileOptions(args); err == nil {
			t.Fatalf("accepted args=%#v", args)
		}
	}
}

func TestSessionManagementOptionsRejectUnknownDuplicateAndTrailingArgs(
	t *testing.T,
) {
	for _, args := range [][]string{
		{"--session-id", "session_1", "--unknown"},
		{"--session-id", "session_1", "--session-id", "session_2"},
		{"--session-id", "session_1", "trailing"},
		{"--session-id", "--apply"},
	} {
		if err := validateManagementArgs(
			args, []string{"--session-id"}, []string{"--apply"},
		); err == nil {
			t.Fatalf("accepted args=%#v", args)
		}
	}
	if err := validateManagementArgs(
		[]string{
			"--session-id", "session_1", "--after-seq", "2", "--apply",
		},
		[]string{"--session-id", "--after-seq"},
		[]string{"--apply"},
	); err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"0", "-1", "not-a-number"} {
		if _, err := intOptionValue(
			[]string{"--tail", value}, "--tail", 120,
		); err == nil {
			t.Fatalf("accepted non-positive tail %q", value)
		}
	}
}

func TestSessionManagementPreflightRejectsBeforeStatefulBootstrap(
	t *testing.T,
) {
	paths := prepareRuntimeHome(t)
	for _, args := range [][]string{
		{"unknown"},
		{"run", "cx", "input"},
		{"submit", "api-cx", "input"},
		{"exec", "--effort", "high", "cx", "input"},
		{"list", "--state", "unknown"},
		{"list", "--state", ""},
		{"list", "--state", "idle", "--state", "archived"},
		{"gc", "--limit", "1001"},
		{"gc", "--limit", ""},
		{"gc", "--older-than-hours", "2562048"},
		{"gc", "--older-than-hours", ""},
		{"show", "--session-id", "session_1", "trailing"},
	} {
		output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
		if err := runSessionNamespace(paths, args, output); err == nil {
			t.Fatalf("accepted args=%#v", args)
		}
		if _, err := os.Stat(paths.SessionsDir); !os.IsNotExist(err) {
			t.Fatalf(
				"invalid args=%#v bootstrapped Session state: %v",
				args, err,
			)
		}
	}
}

func TestSessionManagementPreflightAcceptsBoundedFilters(t *testing.T) {
	if filter, err := parseSessionListFilter(nil); err != nil ||
		filter != (session.ListFilter{}) {
		t.Fatalf("default filter=%#v error=%v", filter, err)
	}
	for _, args := range [][]string{
		{"list", "--state", "idle"},
		{"list", "--interface", "native_tui"},
		{"list", "--state", "idle", "--interface", "managed"},
		{
			"gc", "--older-than-hours", "2562047",
			"--limit", "1000", "--apply",
		},
	} {
		if err := validateSessionManagementInvocation(args); err != nil {
			t.Fatalf("args=%#v error=%v", args, err)
		}
	}
	if _, err := parseSessionListFilter([]string{
		"--interface", "future",
	}); err == nil {
		t.Fatal("accepted invalid Session interface filter")
	}
}

func TestSessionManagementUsesCanonicalIDAndNotFoundErrors(t *testing.T) {
	const missingSessionID = "session_00000000000000000000000000000000"
	if err := validateSessionManagementInvocation([]string{
		"show", "--session-id", missingSessionID,
	}); err != nil {
		t.Fatal(err)
	}
	if err := validateSessionManagementInvocation([]string{
		"show", "--session-id", "session_missing",
	}); err == nil {
		t.Fatal("malformed session ID was accepted")
	}

	output := newCLIOutput(true, &bytes.Buffer{}, &bytes.Buffer{})
	err := runSessionNamespace(
		prepareRuntimeHome(t),
		[]string{"show", "--session-id", missingSessionID},
		output,
	)
	var runtimeErr *contract.RuntimeError
	if !errors.As(err, &runtimeErr) ||
		runtimeErr.Code != contract.ErrorNotFound {
		t.Fatalf("error=%v", err)
	}
}

func sessionTestProfiles(
	t *testing.T,
	apiProfile runtimemodel.Profile,
) *runtimeprofile.Catalog {
	t.Helper()
	commands, err := runtimecommand.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	models, err := runtimemodel.NewCatalog(
		map[string]runtimemodel.Profile{"api-cx": apiProfile},
	)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := runtimeprofile.NewCatalog(commands, models)
	if err != nil {
		t.Fatal(err)
	}
	return profiles
}
