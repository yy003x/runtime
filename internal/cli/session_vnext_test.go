package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"

	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	runtimemodel "github.com/yy003x/runtime/model"
	runtimeprofile "github.com/yy003x/runtime/profile"
	"github.com/yy003x/runtime/session"
)

func TestSessionInvocationUsesTypedCLIOverridesAndRejectsCarrierOptions(t *testing.T) {
	value, err := parseSessionInvocation([]string{
		"--model", "gpt-5.6-sol", "--effort", "high",
		"--cwd", "work", "cx", "继续",
	})
	if err != nil {
		t.Fatal(err)
	}
	if value.model != "gpt-5.6-sol" || value.effort != "high" ||
		value.cwd != "work" || value.profileID != "cx" ||
		value.input != "继续" {
		t.Fatalf("value=%#v", value)
	}
	for _, option := range []string{
		"--prompt-file", "--terminal-driver", "--command-arg",
	} {
		if _, err := parseSessionInvocation(
			[]string{option, "value", "cx", "input"},
		); err == nil {
			t.Fatalf("accepted removed option %s", option)
		}
	}
	if _, err := parseSessionInvocation([]string{
		"--effort", "high", "--effort", "low", "cx", "input",
	}); err == nil || !strings.Contains(err.Error(), "only be used once") {
		t.Fatalf("duplicate option error=%v", err)
	}
	if _, err := parseSessionInvocation([]string{
		"--model", "--effort", "high", "cx", "input",
	}); err == nil || !strings.Contains(err.Error(), "--model requires value") {
		t.Fatalf("missing value error=%v", err)
	}
	for _, args := range [][]string{
		{"--retention", "forever", "cx", "input"},
		{"--effort", "extreme", "cx", "input"},
		{"--model", "", "cx", "input"},
		{"--temperature", "NaN", "api-cx", "input"},
	} {
		if _, err := parseSessionInvocation(args); err == nil {
			t.Fatalf("accepted invalid invocation args=%#v", args)
		}
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

func TestSessionTokenLimitFlagMatchesModelDriver(t *testing.T) {
	openAIInvocation, err := parseSessionInvocation([]string{
		"--max-completion-tokens", "128", "api-cx", "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	openAIProfiles := sessionTestProfiles(t, runtimemodel.Profile{
		Driver:   runtimemodel.DriverOpenAICompatible,
		Endpoint: "https://example.invalid/v1/chat/completions", Model: "fixture",
		Auth: runtimemodel.Auth{
			Header: "Authorization", Scheme: "Bearer", FromEnv: "MODEL_API_KEY",
		},
		Timeout: "1m",
	})
	if err := validateSessionProfileOptions(
		openAIInvocation, openAIProfiles,
	); err != nil {
		t.Fatal(err)
	}

	invalidInvocation, err := parseSessionInvocation([]string{
		"--max-tokens", "128", "api-cx", "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := validateSessionProfileOptions(
		invalidInvocation, openAIProfiles,
	); err == nil || !strings.Contains(err.Error(), "--max-completion-tokens") {
		t.Fatalf("error=%v", err)
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
	paths := prepareVNextHome(t)
	for _, args := range [][]string{
		{"unknown"},
		{"run", "--unknown"},
		{"list", "--state", "unknown"},
		{"list", "--state", ""},
		{"gc", "--limit", "1001"},
		{"gc", "--limit", ""},
		{"gc", "--older-than-hours", "2562048"},
		{"gc", "--older-than-hours", ""},
		{"show", "--session-id", "session_1", "trailing"},
		{
			"tool-result",
			"--session-id", "session_1",
			"--turn-id", "turn_1",
			"--tool-call-id", "call_1",
			"--idempotency-key", "key_1",
			"--content", "",
			"--content-file", "result.txt",
		},
	} {
		output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
		if err := runSessionNamespaceVNext(paths, args, output); err == nil {
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
	for _, args := range [][]string{
		{"list", "--state", "idle"},
		{
			"gc", "--older-than-hours", "2562047",
			"--limit", "1000", "--apply",
		},
	} {
		if err := validateSessionManagementInvocation(args); err != nil {
			t.Fatalf("args=%#v error=%v", args, err)
		}
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
