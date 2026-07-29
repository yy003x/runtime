package contract

import (
	"encoding/json"
	"math"
	"strings"
	"testing"
)

func TestGenerateRequestValidation(t *testing.T) {
	maxTokens := int64(1024)
	temperature := 0.2
	request := GenerateRequest{
		ModelProfile: "fixture",
		Input: ModelRequest{
			Messages: []Message{{Role: RoleUser, Content: "hello"}},
			Tools: []ToolSpec{{
				Name: "weather", InputSchema: json.RawMessage(`{"type":"object"}`),
			}},
			Options: GenerateOptions{MaxOutputTokens: &maxTokens, Temperature: &temperature},
			Trace:   TraceContext{Labels: map[string]string{"task": "fixture"}},
		},
	}
	if err := request.Validate(); err != nil {
		t.Fatal(err)
	}
	request.Input.Tools[0].InputSchema = json.RawMessage(`[]`)
	if err := request.Validate(); err == nil {
		t.Fatal("array input schema was accepted")
	}
}

func TestModelRequestValidationRejectsNonFiniteTemperature(t *testing.T) {
	for _, temperature := range []float64{
		math.NaN(), math.Inf(1), math.Inf(-1),
	} {
		request := ModelRequest{
			Messages: []Message{{Role: RoleUser, Content: "hello"}},
			Options:  GenerateOptions{Temperature: &temperature},
		}
		if err := request.Validate(); err == nil {
			t.Fatalf("temperature %v was accepted", temperature)
		}
	}
}

func TestEventAndErrorValidation(t *testing.T) {
	result := ModelResult{
		Message:      Message{Role: RoleAssistant, Content: "ok"},
		FinishReason: FinishStop,
	}
	event := Event{
		Sequence: 1,
		Type:     EventModelCompleted,
		Model:    &ModelEvent{Result: &result},
	}
	if err := event.Validate(); err != nil {
		t.Fatal(err)
	}
	event.Model = &ModelEvent{}
	if err := event.Validate(); err == nil {
		t.Fatal("model.completed without result was accepted")
	}

	runtimeError := RuntimeError{
		Code: ErrorRateLimited, Phase: PhaseProvider,
		Message: "provider returned 429", Retryable: true, HTTPStatus: 429,
	}
	if err := runtimeError.Validate(); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(runtimeError.Error(), "rate_limited") {
		t.Fatalf("Error()=%q", runtimeError.Error())
	}

	argumentsDelta := Event{
		Sequence: 2,
		Type:     EventToolCallArgumentsDelta,
		Model: &ModelEvent{
			Text: `{"city":`, ToolCallID: "call-1",
		},
	}
	if err := argumentsDelta.Validate(); err != nil {
		t.Fatal(err)
	}
	argumentsDelta.Model.ToolCallID = ""
	if err := argumentsDelta.Validate(); err == nil {
		t.Fatal("tool arguments delta without tool_call_id was accepted")
	}

	settled := Event{
		Sequence: 3, Type: EventRunSettled,
		Run: &RunEvent{RunID: "run_fixture", State: "completed"},
	}
	if err := settled.Validate(); err != nil {
		t.Fatal(err)
	}

	withToolCall := ModelResult{
		Message: Message{
			Role: RoleAssistant,
			ToolCalls: []ToolCall{{
				ID: "call-1", Name: "weather", Arguments: json.RawMessage(`{}`),
			}},
		},
		FinishReason: FinishStop,
	}
	if err := withToolCall.Validate(); err == nil {
		t.Fatal("tool_calls with non-tool finish_reason were accepted")
	}
}
