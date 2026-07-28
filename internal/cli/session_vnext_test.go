package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"

	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/layout"
	runtimemodel "github.com/yy003x/runtime/model"
	runtimeprofile "github.com/yy003x/runtime/profile"
	"github.com/yy003x/runtime/session"
)

func TestCarrierSendInputDoesNotTreatOptionValueAsInput(t *testing.T) {
	if _, err := carrierSendInput([]string{"--session-id", "session_deadbeef"}); err == nil {
		t.Fatal("accepted missing send input")
	}
	value, err := carrierSendInput([]string{
		"--session-id", "session_deadbeef", "继续",
	})
	if err != nil || value != "继续" {
		t.Fatalf("value=%q error=%v", value, err)
	}
	if _, err := carrierSendInput([]string{
		"--session-id", "session_deadbeef", "one", "two",
	}); err == nil {
		t.Fatal("accepted more than one positional input")
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

func TestSessionAttachRejectsJSONBeforeInteractiveLookup(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	output := newCLIOutput(true, &stdout, &stderr)
	err := runSessionNamespaceVNext(
		layout.Paths{},
		[]string{"attach", "--session-id", "session_1"},
		output,
	)
	if err == nil {
		t.Fatal("JSON attach was accepted")
	}
	if exitCode := output.fail(err); exitCode != 1 || stdout.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q", exitCode, stdout.String())
	}
	var payload struct {
		ContractVersion int `json:"contract_version"`
		Error           struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.ContractVersion != 2 ||
		payload.Error.Message != "session attach does not support --json" {
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
