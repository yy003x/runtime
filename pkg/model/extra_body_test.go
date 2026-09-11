package model

import (
	"encoding/json"
	"strings"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
)

func TestExtraBodyProfileCloneAndExecutionDigest(t *testing.T) {
	profile := Profile{Driver: DriverOpenAIChatCompletions, BaseURL: "https://example.invalid/v1", Model: "fixture", Timeout: "1m", Parameters: Parameters{ExtraBody: contract.ExtraBody{"seed": json.RawMessage(`9007199254740993`), "metadata": json.RawMessage(`null`)}}}
	catalog, err := NewCatalog(map[string]Profile{"fixture": profile})
	if err != nil {
		t.Fatal(err)
	}
	profile.Parameters.ExtraBody["seed"][0] = '1'
	first, _ := catalog.Get("fixture")
	if string(first.Parameters.ExtraBody["seed"]) != "9007199254740993" {
		t.Fatal("catalog retained input alias")
	}
	identity := DriverExecutionIdentity{Driver: DriverOpenAIChatCompletions, Implementation: "runtime.provider.fixture", ImplementationVersion: 1}
	snapshot, err := newExecutionSnapshot("fixture", first, identity)
	if err != nil {
		t.Fatal(err)
	}
	before, err := snapshot.Digest()
	if err != nil {
		t.Fatal(err)
	}
	first.Parameters.ExtraBody["seed"][0] = '2'
	if _, err := snapshot.Digest(); err != nil {
		t.Fatalf("snapshot retained a RawMessage alias: %v", err)
	}
	second, _ := catalog.Get("fixture")
	if string(second.Parameters.ExtraBody["seed"]) != "9007199254740993" {
		t.Fatal("catalog Get exposed a RawMessage alias")
	}
	second.Parameters.ExtraBody["metadata"] = json.RawMessage(`{"changed":true}`)
	changed, err := newExecutionSnapshot("fixture", second, identity)
	if err != nil {
		t.Fatal(err)
	}
	after, err := changed.Digest()
	if err != nil || before == after {
		t.Fatalf("extra_body missing from durable execution identity: %v", err)
	}
}

func TestExtraBodyManagedThinkingBoundary(t *testing.T) {
	profile := Profile{Driver: DriverAnthropicMessages, Parameters: Parameters{ExtraBody: contract.ExtraBody{"thinking": json.RawMessage(`{"type":"adaptive"}`)}}}
	if err := profile.ValidateManagedExecution(); err == nil || !strings.Contains(err.Error(), "thinking-block history") {
		t.Fatalf("unexpected managed result: %v", err)
	}
	for _, value := range []string{`null`, `0`, `{"type":"disabled"}`} {
		if err := profile.ValidateManagedOptions(contract.GenerateOptions{ExtraBody: contract.ExtraBody{"thinking": json.RawMessage(value)}}); err != nil {
			t.Fatal(err)
		}
	}
	request := contract.ModelRequest{Messages: []contract.Message{{Role: contract.RoleUser, Content: "hello"}}}
	if err := profile.ValidateSingleCallInput(request); err != nil {
		t.Fatal(err)
	}
	request.Messages = append(request.Messages, contract.Message{Role: contract.RoleAssistant, Content: "prior"})
	if err := profile.ValidateSingleCallInput(request); err == nil {
		t.Fatal("thinking history silently discarded")
	}
	request.Options.ExtraBody = contract.ExtraBody{"thinking": json.RawMessage(`{"type":"disabled"}`)}
	if err := profile.ValidateSingleCallInput(request); err != nil {
		t.Fatal(err)
	}
}

func TestContentPartsRejectedForResponsesDrivers(t *testing.T) {
	request := contract.ModelRequest{
		Messages: []contract.Message{{
			Role: contract.RoleUser,
			ContentParts: []contract.ContentPart{{
				Type: contract.ContentPartText, Text: "hello",
			}},
		}},
	}
	for _, driver := range []DriverName{DriverOpenAIResponses, DriverOpenAICodexResponses} {
		profile := Profile{Driver: driver}
		if err := profile.ValidateSingleCallInput(request); err == nil || !strings.Contains(err.Error(), "content_parts") {
			t.Fatalf("driver=%s err=%v", driver, err)
		}
	}
	if err := (Profile{Driver: DriverOpenAIChatCompletions}).ValidateSingleCallInput(request); err != nil {
		t.Fatal(err)
	}
}

func TestCodexRejectsStreamFalseOverride(t *testing.T) {
	falseValue := false
	profile := Profile{Driver: DriverOpenAICodexResponses}
	request := contract.ModelRequest{
		Messages: []contract.Message{{Role: contract.RoleUser, Content: "hello"}},
		Options:  contract.GenerateOptions{Stream: &falseValue},
	}
	if err := profile.ValidateSingleCallInput(request); err == nil ||
		!strings.Contains(err.Error(), "options.stream=true") {
		t.Fatalf("err=%v", err)
	}
}

func TestExtraBodyCodexStoreBoundary(t *testing.T) {
	for _, value := range []string{`null`, `0`, `true`, `"false"`} {
		if err := validateCodexExtraBody(contract.ExtraBody{"store": json.RawMessage(value)}); err == nil {
			t.Fatalf("Codex accepted store=%s", value)
		}
	}
	if err := validateCodexExtraBody(contract.ExtraBody{"store": json.RawMessage(` false `)}); err != nil {
		t.Fatal(err)
	}
	if err := validateCodexExtraBody(nil); err != nil {
		t.Fatal(err)
	}
}
