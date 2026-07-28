package agent

import (
	"context"
	"encoding/json"
	"errors"
	"sync"
	"testing"

	"github.com/yy003x/runtime/contract"
)

type kernelModel struct {
	mu      sync.Mutex
	results []contract.ModelResult
}

func (model *kernelModel) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return model.GenerateStream(ctx, request, nil)
}

func (model *kernelModel) GenerateStream(
	_ context.Context,
	_ contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	model.mu.Lock()
	defer model.mu.Unlock()
	if len(model.results) == 0 {
		return contract.ModelResult{}, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseProvider,
			Message: "no scripted result",
		}
	}
	result := model.results[0]
	model.results = model.results[1:]
	if sink != nil {
		if err := sink(contract.Event{
			Sequence: 1, Type: contract.EventModelStarted,
		}); err != nil {
			return contract.ModelResult{}, consumerError(err)
		}
		if err := sink(contract.Event{
			Sequence: 2, Type: contract.EventModelCompleted,
			Model: &contract.ModelEvent{Result: &result},
		}); err != nil {
			return contract.ModelResult{}, consumerError(err)
		}
	}
	return result, nil
}

func TestKernelExecutesSequentialToolLoop(t *testing.T) {
	call := contract.ToolCall{
		ID: "call_1", Name: "echo",
		Arguments: json.RawMessage(`{"value":"ok"}`),
	}
	model := &kernelModel{results: []contract.ModelResult{
		{
			Message: contract.Message{
				Role: contract.RoleAssistant, ToolCalls: []contract.ToolCall{call},
			},
			FinishReason: contract.FinishToolCall,
		},
		{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "final"},
			FinishReason: contract.FinishStop,
		},
	}}
	registry, err := NewRegistry(RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Handler: func(_ context.Context, request ToolRequest) (ToolResult, error) {
			if request.CallID != "call_1" || request.IdempotencyKey == "" {
				t.Fatalf("request=%#v", request)
			}
			return ToolResult{Content: `{"value":"ok"}`}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	kernel := Kernel{Model: model, Tools: registry, Effects: NewMemoryEffects()}
	var events []contract.Event
	state, outcome, runtimeErr := kernel.Run(
		context.Background(),
		LoopState{
			RunID:        "run_00000000000000000000000000000001",
			ModelProfile: "api", Messages: []contract.Message{{
				Role: contract.RoleUser, Content: "start",
			}},
		},
		func(event contract.Event) error {
			events = append(events, event)
			return nil
		},
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if outcome.State != StateCompleted || outcome.Message.Content != "final" ||
		state.Round != 2 || state.ToolCallCount != 1 || len(state.Messages) != 4 {
		t.Fatalf("state=%#v outcome=%#v", state, outcome)
	}
	if state.Messages[1].ToolCalls[0].ID != "call_1" ||
		state.Messages[2].Role != contract.RoleTool ||
		state.Messages[2].ToolCallID != "call_1" {
		t.Fatalf("messages=%#v", state.Messages)
	}
	for index, event := range events {
		if event.Sequence != uint64(index+1) {
			t.Fatalf("events[%d].sequence=%d", index, event.Sequence)
		}
	}
	wantTypes := []contract.EventType{
		contract.EventModelStarted, contract.EventModelCompleted,
		contract.EventCheckpointCommitted, contract.EventToolStarted,
		contract.EventToolCompleted, contract.EventModelStarted,
		contract.EventModelCompleted, contract.EventAgentCompleted,
	}
	if len(events) != len(wantTypes) {
		t.Fatalf("events=%#v", events)
	}
	for index, want := range wantTypes {
		if events[index].Type != want {
			t.Fatalf("events[%d]=%s want=%s", index, events[index].Type, want)
		}
	}
}

func TestKernelPauseAndResume(t *testing.T) {
	call := contract.ToolCall{
		ID: "call_pause", Name: "approval",
		Arguments: json.RawMessage(`{"action":"write"}`),
	}
	model := &kernelModel{results: []contract.ModelResult{
		{
			Message: contract.Message{
				Role: contract.RoleAssistant, ToolCalls: []contract.ToolCall{call},
			},
			FinishReason: contract.FinishToolCall,
		},
		{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "approved"},
			FinishReason: contract.FinishStop,
		},
	}}
	registry, err := NewRegistry(RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "approval", InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Handler: func(context.Context, ToolRequest) (ToolResult, error) {
			return ToolResult{Pause: &Pause{
				ID: "pause_1", Kind: "approval",
				InputSchema: json.RawMessage(
					`{"type":"object","required":["approved"]}`,
				),
				Prompt: "approve?",
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	kernel := Kernel{Model: model, Tools: registry, Effects: NewMemoryEffects()}
	state, outcome, runtimeErr := kernel.Run(
		context.Background(),
		LoopState{
			RunID:        "run_00000000000000000000000000000002",
			ModelProfile: "api", Messages: []contract.Message{{
				Role: contract.RoleUser, Content: "start",
			}},
		},
		nil,
	)
	if runtimeErr != nil || outcome.State != StatePaused || state.Pause == nil {
		t.Fatalf("state=%#v outcome=%#v err=%v", state, outcome, runtimeErr)
	}
	if _, _, runtimeErr := kernel.Resume(
		context.Background(), state,
		ResumeInput{PauseID: "pause_1", Input: json.RawMessage(`{}`)}, nil,
	); runtimeErr == nil || runtimeErr.Code != contract.ErrorInvalidRequest {
		t.Fatalf("invalid resume err=%v", runtimeErr)
	}
	state, outcome, runtimeErr = kernel.Resume(
		context.Background(), state,
		ResumeInput{
			PauseID: "pause_1",
			Input:   json.RawMessage(`{"approved":true}`),
		},
		nil,
	)
	if runtimeErr != nil || outcome.State != StateCompleted ||
		state.Pause != nil || state.Messages[2].ToolCallID != "call_pause" {
		t.Fatalf("state=%#v outcome=%#v err=%v", state, outcome, runtimeErr)
	}
}

func TestKernelRejectsUnknownToolBeforeSideEffect(t *testing.T) {
	model := &kernelModel{results: []contract.ModelResult{{
		Message: contract.Message{
			Role: contract.RoleAssistant,
			ToolCalls: []contract.ToolCall{{
				ID: "call_unknown", Name: "missing",
				Arguments: json.RawMessage(`{}`),
			}},
		},
		FinishReason: contract.FinishToolCall,
	}}}
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	kernel := Kernel{Model: model, Tools: registry, Effects: NewMemoryEffects()}
	_, outcome, runtimeErr := kernel.Run(
		context.Background(),
		LoopState{
			RunID:        "run_00000000000000000000000000000003",
			ModelProfile: "api", Messages: []contract.Message{{
				Role: contract.RoleUser, Content: "start",
			}},
		}, nil,
	)
	if runtimeErr == nil ||
		runtimeErr.Code != contract.ErrorInvalidProviderResponse ||
		outcome.StopReason != "unknown_tool" {
		t.Fatalf("outcome=%#v err=%v", outcome, runtimeErr)
	}
}

func TestKernelEnforcesRoundBudget(t *testing.T) {
	model := &kernelModel{results: []contract.ModelResult{{
		Message: contract.Message{
			Role: contract.RoleAssistant,
			ToolCalls: []contract.ToolCall{{
				ID: "call_1", Name: "echo", Arguments: json.RawMessage(`{}`),
			}},
		},
		FinishReason: contract.FinishToolCall,
	}}}
	registry, err := NewRegistry(RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Handler: func(context.Context, ToolRequest) (ToolResult, error) {
			return ToolResult{Content: "{}"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	kernel := Kernel{
		Model: model, Tools: registry, Effects: NewMemoryEffects(),
		Budget: Budget{MaxRounds: 1, MaxToolCalls: 1},
	}
	_, outcome, runtimeErr := kernel.Run(
		context.Background(),
		LoopState{
			RunID:        "run_00000000000000000000000000000004",
			ModelProfile: "api", Messages: []contract.Message{{
				Role: contract.RoleUser, Content: "start",
			}},
		}, nil,
	)
	if runtimeErr == nil || outcome.StopReason != "round_budget" {
		t.Fatalf("outcome=%#v err=%v", outcome, runtimeErr)
	}
}

func consumerError(err error) *contract.RuntimeError {
	if err == nil {
		err = errors.New("consumer stopped")
	}
	return &contract.RuntimeError{
		Code: contract.ErrorCancelled, Phase: contract.PhaseConsumer,
		Message: err.Error(),
	}
}
