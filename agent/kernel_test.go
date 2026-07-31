package agent

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"reflect"
	"sync"
	"testing"
	"time"

	"github.com/yy003x/runtime/contract"
)

type kernelModel struct {
	mu      sync.Mutex
	results []contract.ModelResult
}

type kernelTerminalModel struct {
	mu         sync.Mutex
	result     contract.ModelResult
	runtimeErr *contract.RuntimeError
	requests   int
}

type countingEffects struct {
	prepared         int
	started          int
	failed           int
	effect           *EffectRecord
	lookupContextErr error
}

func (effects *countingEffects) Lookup(
	ctx context.Context,
	_ string,
	_ string,
) (EffectRecord, bool, error) {
	effects.lookupContextErr = ctx.Err()
	if effects.effect != nil {
		return *effects.effect, true, nil
	}
	return EffectRecord{}, false, nil
}

func (effects *countingEffects) Prepared(
	_ context.Context,
	request *ToolRequest,
	state *LoopState,
) (string, error) {
	effects.prepared++
	request.CheckpointID = "checkpoint_fixture"
	state.PendingEffectCheckpointID = request.CheckpointID
	state.PendingCheckpointID = request.CheckpointID
	return request.CheckpointID, nil
}

func (effects *countingEffects) Started(context.Context, ToolRequest) error {
	effects.started++
	return nil
}

func (*countingEffects) Completed(
	context.Context,
	ToolRequest,
	ToolResult,
) error {
	return nil
}

func (effects *countingEffects) Failed(
	context.Context,
	ToolRequest,
	*contract.RuntimeError,
) error {
	effects.failed++
	return nil
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

func (model *kernelTerminalModel) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return model.GenerateStream(ctx, request, nil)
}

func (model *kernelTerminalModel) GenerateStream(
	_ context.Context,
	_ contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.requests++
	if sink != nil {
		if err := sink(contract.Event{
			Type: contract.EventModelStarted,
		}); err != nil {
			return contract.ModelResult{}, consumerError(err)
		}
		if model.runtimeErr == nil {
			if err := sink(contract.Event{
				Type: contract.EventModelCompleted,
				Model: &contract.ModelEvent{
					Result: &model.result,
				},
			}); err != nil {
				return contract.ModelResult{}, consumerError(err)
			}
		}
	}
	return model.result, model.runtimeErr
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
			return ToolResult{Content: `{"value":"ok"}`, IsError: true}, nil
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
		state.Messages[2].ToolCallID != "call_1" ||
		!state.Messages[2].IsError {
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

func TestKernelFreezesKnownModelTerminalOutcomes(t *testing.T) {
	one := int64(1)
	two := int64(2)
	testCases := []struct {
		name           string
		model          *kernelTerminalModel
		budget         Budget
		totalTokens    int64
		wantState      State
		wantStopReason string
		wantError      *contract.RuntimeError
	}{
		{
			name: "model_failed",
			model: &kernelTerminalModel{
				runtimeErr: &contract.RuntimeError{
					Code:  contract.ErrorProviderUnavailable,
					Phase: contract.PhaseProvider, Message: "provider failed",
				},
			},
			wantState: StateFailed, wantStopReason: "model_failed",
			wantError: &contract.RuntimeError{
				Code:  contract.ErrorProviderUnavailable,
				Phase: contract.PhaseProvider, Message: "provider failed",
			},
		},
		{
			name: "model_timeout",
			model: &kernelTerminalModel{
				runtimeErr: &contract.RuntimeError{
					Code:  contract.ErrorTimeout,
					Phase: contract.PhaseProvider, Message: "provider timed out",
				},
			},
			wantState: StateCancelled, wantStopReason: "cancelled",
			wantError: &contract.RuntimeError{
				Code:  contract.ErrorTimeout,
				Phase: contract.PhaseProvider, Message: "provider timed out",
			},
		},
		{
			name: "finish_cancelled",
			model: &kernelTerminalModel{result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "partial",
				},
				FinishReason: contract.FinishCancelled,
			}},
			wantState: StateCancelled, wantStopReason: "cancelled",
			wantError: &contract.RuntimeError{
				Code: contract.ErrorCancelled, Phase: contract.PhaseRun,
				Message: "model generation was cancelled",
			},
		},
		{
			name: "token_budget",
			model: &kernelTerminalModel{result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "too many tokens",
				},
				FinishReason: contract.FinishStop,
				Usage:        contract.Usage{TotalTokens: &two},
			}},
			budget: Budget{
				MaxRounds: 1, MaxToolCalls: 1,
				MaxTotalTokens: 1, MaxWallTime: time.Minute,
			},
			wantState: StateFailed, wantStopReason: "token_budget",
			wantError: &contract.RuntimeError{
				Code: contract.ErrorInvalidRequest, Phase: contract.PhaseRun,
				Message: "token budget exhausted",
			},
		},
		{
			name: "invalid_usage_overflow",
			model: &kernelTerminalModel{result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "overflow",
				},
				FinishReason: contract.FinishStop,
				Usage:        contract.Usage{TotalTokens: &one},
			}},
			totalTokens: math.MaxInt64,
			wantState:   StateFailed, wantStopReason: "invalid_model_usage",
			wantError: &contract.RuntimeError{
				Code:    contract.ErrorInvalidProviderResponse,
				Phase:   contract.PhaseRun,
				Message: "model usage token total overflows int64",
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			registry, err := NewRegistry()
			if err != nil {
				t.Fatal(err)
			}
			kernel := &Kernel{
				Model: testCase.model, Tools: registry,
				Effects: NewMemoryEffects(), Budget: testCase.budget,
			}
			var events []contract.Event
			state, outcome, runtimeErr := kernel.Run(
				context.Background(),
				LoopState{
					RunID:        "run_11111111111111111111111111111111",
					ModelProfile: "api",
					Messages: []contract.Message{{
						Role: contract.RoleUser, Content: "start",
					}},
					TotalTokens: testCase.totalTokens,
				},
				func(event contract.Event) error {
					events = append(events, event)
					return nil
				},
			)
			if runtimeErr == nil ||
				outcome.State != testCase.wantState ||
				outcome.StopReason != testCase.wantStopReason ||
				!reflect.DeepEqual(runtimeErr, testCase.wantError) ||
				!reflect.DeepEqual(outcome.Error, testCase.wantError) ||
				state.TerminalOutcome == nil ||
				!reflect.DeepEqual(*state.TerminalOutcome, outcome) {
				t.Fatalf(
					"state=%#v outcome=%#v error=%#v",
					state, outcome, runtimeErr,
				)
			}
			for _, event := range events {
				if event.Type == contract.EventAgentCompleted {
					t.Fatalf("terminal failure emitted agent.completed: %#v", events)
				}
			}
			recovered, repeated, repeatedErr := kernel.Run(
				context.Background(), state, nil,
			)
			if testCase.model.requests != 1 ||
				!reflect.DeepEqual(recovered, state) ||
				!reflect.DeepEqual(repeated, outcome) ||
				!reflect.DeepEqual(repeatedErr, runtimeErr) {
				t.Fatalf(
					"recovered=%#v repeated=%#v error=%#v requests=%d",
					recovered, repeated, repeatedErr,
					testCase.model.requests,
				)
			}
		})
	}
}

func TestKernelRecoveredToolFactsWinPreCancelledContext(t *testing.T) {
	call := contract.ToolCall{
		ID: "call_recovered_cancel", Name: "echo",
		Arguments: json.RawMessage(`{"value":"durable"}`),
	}
	request := ToolRequest{
		RunID:  "run_22222222222222222222222222222222",
		CallID: call.ID, Name: call.Name,
		Arguments: append([]byte(nil), call.Arguments...),
		IdempotencyKey: toolIdempotencyKey(
			"run_22222222222222222222222222222222", call,
		),
		CheckpointID: "checkpoint_recovered_cancel",
	}
	knownFailure := &contract.RuntimeError{
		Code: contract.ErrorToolFailed, Phase: contract.PhaseRun,
		Message: "durable tool failure",
	}
	testCases := []struct {
		effectState string
		result      *ToolResult
		runtimeErr  *contract.RuntimeError
		wantState   State
		wantStop    string
		wantMessage bool
		wantFrozen  bool
	}{
		{
			effectState: "prepared",
			wantState:   StateNeedsReconciliation,
			wantStop:    "tool_effect_unknown",
		},
		{
			effectState: "started",
			wantState:   StateNeedsReconciliation,
			wantStop:    "tool_effect_unknown",
		},
		{
			effectState: "completed",
			result:      &ToolResult{Content: `{"value":"durable"}`},
			wantState:   StateCancelled,
			wantStop:    "cancelled",
			wantMessage: true,
			wantFrozen:  true,
		},
		{
			effectState: "failed",
			runtimeErr:  knownFailure,
			wantState:   StateFailed,
			wantStop:    "tool_failed",
			wantFrozen:  true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.effectState, func(t *testing.T) {
			handlerCalls := 0
			registry, err := NewRegistry(RegisteredTool{
				Definition: contract.ToolSpec{
					Name: "echo",
					InputSchema: json.RawMessage(
						`{"type":"object","required":["value"]}`,
					),
				},
				Handler: func(
					context.Context,
					ToolRequest,
				) (ToolResult, error) {
					handlerCalls++
					return ToolResult{Content: `{"unexpected":true}`}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			effects := &countingEffects{effect: &EffectRecord{
				State: testCase.effectState, Request: request,
				Result: testCase.result, Error: testCase.runtimeErr,
			}}
			state := LoopState{
				SchemaVersion: LoopStateSchemaVersion,
				RunID:         request.RunID, ModelProfile: "api",
				Messages: []contract.Message{
					{Role: contract.RoleUser, Content: "start"},
					{
						Role:      contract.RoleAssistant,
						ToolCalls: []contract.ToolCall{call},
					},
				},
				BaseMessageCount:           1,
				Round:                      1,
				ToolCallCount:              1,
				SeenToolCallIDs:            []string{call.ID},
				PendingToolCalls:           []contract.ToolCall{call},
				PendingEffectCheckpointID:  request.CheckpointID,
				PendingCheckpointID:        request.CheckpointID,
				PendingCheckpointCommitted: true,
				PendingToolStarted:         true,
				PendingToolTerminal:        testCase.effectState == "failed",
				RecoveredFromCheckpoint:    true,
			}
			ctx, cancel := context.WithCancel(context.Background())
			cancel()
			recovered, outcome, runtimeErr := (&Kernel{
				Model: &kernelTerminalModel{}, Tools: registry,
				Effects: effects,
			}).Run(ctx, state, nil)
			if outcome.State != testCase.wantState ||
				outcome.StopReason != testCase.wantStop ||
				handlerCalls != 0 ||
				effects.lookupContextErr != nil ||
				(recovered.TerminalOutcome != nil) != testCase.wantFrozen {
				t.Fatalf(
					"state=%#v outcome=%#v error=%#v handlerCalls=%d lookupContextErr=%v",
					recovered, outcome, runtimeErr,
					handlerCalls, effects.lookupContextErr,
				)
			}
			if testCase.wantMessage {
				if len(recovered.Messages) != 3 ||
					recovered.Messages[2].Role != contract.RoleTool ||
					recovered.Messages[2].ToolCallID != call.ID {
					t.Fatalf("messages=%#v", recovered.Messages)
				}
			}
			if testCase.effectState == "failed" &&
				!reflect.DeepEqual(runtimeErr, knownFailure) {
				t.Fatalf("failed error=%#v", runtimeErr)
			}
		})
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
					`{
							"type":"object",
							"required":["approved"],
							"properties":{
								"approved":{"type":"boolean"},
								"metadata":{
									"type":"object",
									"properties":{"reason":{"type":"string"}},
									"additionalProperties":false
								}
							},
							"additionalProperties":false
						}`,
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
	for name, input := range map[string]string{
		"missing":           `{}`,
		"wrong_type":        `{"approved":"yes"}`,
		"additional":        `{"approved":true,"future":1}`,
		"nested_additional": `{"approved":true,"metadata":{"future":1}}`,
		"duplicate":         `{"approved":true,"approved":false}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, _, runtimeErr := kernel.Resume(
				context.Background(), state,
				ResumeInput{
					PauseID: "pause_1", Input: json.RawMessage(input),
				}, nil,
			); runtimeErr == nil ||
				runtimeErr.Code != contract.ErrorInvalidRequest {
				t.Fatalf("invalid resume err=%v", runtimeErr)
			}
		})
	}
	state, outcome, runtimeErr = kernel.Resume(
		context.Background(), state,
		ResumeInput{
			PauseID: "pause_1",
			Input: json.RawMessage(
				`{"approved":true,"metadata":{"reason":"ok"}}`,
			),
		},
		nil,
	)
	if runtimeErr != nil || outcome.State != StateCompleted ||
		state.Pause != nil || state.Messages[2].ToolCallID != "call_pause" {
		t.Fatalf("state=%#v outcome=%#v err=%v", state, outcome, runtimeErr)
	}
}

func TestValidatePauseRejectsZeroExpiry(t *testing.T) {
	zero := time.Time{}
	err := ValidatePause(Pause{
		ID: "pause_zero_expiry", Kind: "approval", Prompt: "approve?",
		InputSchema: json.RawMessage(`{"type":"boolean"}`),
		ExpiresAt:   &zero,
		ToolCallID:  "call_zero_expiry",
	})
	if err == nil || err.Error() != "pause expires_at must not be zero" {
		t.Fatalf("ValidatePause error=%v", err)
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

func TestKernelPersistsOnlyExplicitKnownToolFailure(t *testing.T) {
	call := contract.ToolCall{
		ID: "call_known_failure", Name: "known_failure",
		Arguments: json.RawMessage(`{}`),
	}
	model := &kernelModel{results: []contract.ModelResult{{
		Message: contract.Message{
			Role:      contract.RoleAssistant,
			ToolCalls: []contract.ToolCall{call},
		},
		FinishReason: contract.FinishToolCall,
	}}}
	registry, err := NewRegistry(RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "known_failure",
			InputSchema: json.RawMessage(
				`{"type":"object","additionalProperties":false}`,
			),
		},
		Handler: func(
			context.Context,
			ToolRequest,
		) (ToolResult, error) {
			return ToolResult{}, &KnownFailure{
				RuntimeError: &contract.RuntimeError{
					Code:    contract.ErrorToolFailed,
					Phase:   contract.PhaseRun,
					Message: "effect is proven not to have occurred",
				},
			}
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	effects := &countingEffects{}
	_, outcome, runtimeErr := (&Kernel{
		Model: model, Tools: registry, Effects: effects,
	}).Run(
		context.Background(),
		LoopState{
			RunID:        "run_88888888888888888888888888888888",
			ModelProfile: "api",
			Messages: []contract.Message{{
				Role: contract.RoleUser, Content: "start",
			}},
		},
		nil,
	)
	if runtimeErr == nil ||
		outcome.State != StateFailed ||
		outcome.StopReason != "tool_failed" ||
		effects.failed != 1 {
		t.Fatalf(
			"outcome=%#v error=%v effects=%#v",
			outcome, runtimeErr, effects,
		)
	}
}

func TestKernelRejectsArgumentsOutsideToolInputSchemaBeforeSideEffect(
	t *testing.T,
) {
	testCases := []struct {
		name      string
		arguments string
	}{
		{name: "missing", arguments: `{}`},
		{name: "wrong_type", arguments: `{"value":7}`},
		{name: "additional", arguments: `{"value":"ok","extra":true}`},
		{name: "duplicate", arguments: `{"value":"first","value":"second"}`},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			model := &kernelModel{results: []contract.ModelResult{{
				Message: contract.Message{
					Role: contract.RoleAssistant,
					ToolCalls: []contract.ToolCall{{
						ID: "call_invalid", Name: "echo",
						Arguments: json.RawMessage(testCase.arguments),
					}},
				},
				FinishReason: contract.FinishToolCall,
			}}}
			handlerCalls := 0
			registry, err := NewRegistry(RegisteredTool{
				Definition: contract.ToolSpec{
					Name: "echo",
					InputSchema: json.RawMessage(`{
						"type":"object",
						"required":["value"],
						"properties":{"value":{"type":"string"}},
						"additionalProperties":false
					}`),
				},
				Handler: func(
					context.Context,
					ToolRequest,
				) (ToolResult, error) {
					handlerCalls++
					return ToolResult{Content: "{}"}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			effects := &countingEffects{}
			var toolEvents int
			state, outcome, runtimeErr := (&Kernel{
				Model: model, Tools: registry, Effects: effects,
			}).Run(
				context.Background(),
				LoopState{
					RunID:        "run_55555555555555555555555555555555",
					ModelProfile: "api",
					Messages: []contract.Message{{
						Role: contract.RoleUser, Content: "start",
					}},
				},
				func(event contract.Event) error {
					switch event.Type {
					case contract.EventCheckpointCommitted,
						contract.EventToolStarted,
						contract.EventToolCompleted,
						contract.EventToolFailed:
						toolEvents++
					}
					return nil
				},
			)
			if runtimeErr == nil ||
				runtimeErr.Code != contract.ErrorInvalidProviderResponse ||
				outcome.StopReason != "invalid_tool_arguments" {
				t.Fatalf("outcome=%#v err=%v", outcome, runtimeErr)
			}
			if handlerCalls != 0 || effects.prepared != 0 ||
				effects.started != 0 || toolEvents != 0 ||
				state.ToolCallCount != 0 ||
				len(state.SeenToolCallIDs) != 0 {
				t.Fatalf(
					"handler=%d effects=%#v toolEvents=%d state=%#v",
					handlerCalls, effects, toolEvents, state,
				)
			}
		})
	}
}

func TestKernelExecutionSnapshotGatePrecedesToolEffects(t *testing.T) {
	call := contract.ToolCall{
		ID: "call_snapshot_gate", Name: "echo",
		Arguments: json.RawMessage(`{"value":"ok"}`),
	}
	changed := &contract.RuntimeError{
		Code: contract.ErrorConflict, Phase: contract.PhaseProfile,
		Message: "Agent execution snapshot changed",
	}
	newRegistry := func(handlerCalls *int) *Registry {
		t.Helper()
		registry, err := NewRegistry(RegisteredTool{
			Definition: contract.ToolSpec{
				Name: "echo",
				InputSchema: json.RawMessage(`{
					"type":"object",
					"required":["value"],
					"properties":{"value":{"type":"string"}},
					"additionalProperties":false
				}`),
			},
			Handler: func(
				context.Context,
				ToolRequest,
			) (ToolResult, error) {
				*handlerCalls++
				return ToolResult{Content: "must not run"}, nil
			},
		})
		if err != nil {
			t.Fatal(err)
		}
		return registry
	}

	t.Run("new_call_before_prepare", func(t *testing.T) {
		handlerCalls := 0
		effects := &countingEffects{}
		var toolEvents []contract.EventType
		state, outcome, runtimeErr := (&Kernel{
			Model: &kernelModel{results: []contract.ModelResult{{
				Message: contract.Message{
					Role:      contract.RoleAssistant,
					ToolCalls: []contract.ToolCall{call},
				},
				FinishReason: contract.FinishToolCall,
			}}},
			Tools: newRegistry(&handlerCalls), Effects: effects,
			BeforeEffect: func(context.Context) *contract.RuntimeError {
				return changed
			},
		}).Run(
			context.Background(),
			LoopState{
				RunID:        "run_12121212121212121212121212121212",
				ModelProfile: "api",
				Messages: []contract.Message{{
					Role: contract.RoleUser, Content: "start",
				}},
			},
			func(event contract.Event) error {
				switch event.Type {
				case contract.EventCheckpointCommitted,
					contract.EventToolStarted,
					contract.EventToolCompleted,
					contract.EventToolFailed:
					toolEvents = append(toolEvents, event.Type)
				}
				return nil
			},
		)
		if runtimeErr != changed ||
			outcome.State != StateFailed ||
			outcome.StopReason != "execution_snapshot_changed" ||
			state.ToolCallCount != 0 ||
			len(state.SeenToolCallIDs) != 0 {
			t.Fatalf(
				"state=%#v outcome=%#v error=%v",
				state, outcome, runtimeErr,
			)
		}
		if handlerCalls != 0 || effects.prepared != 0 ||
			effects.started != 0 || effects.failed != 0 ||
			len(toolEvents) != 0 {
			t.Fatalf(
				"handler=%d effects=%#v events=%#v",
				handlerCalls, effects, toolEvents,
			)
		}
	})

	t.Run("recovered_prepare_before_start", func(t *testing.T) {
		handlerCalls := 0
		runID := "run_34343434343434343434343434343434"
		checkpointID :=
			"checkpoint_34343434343434343434343434343434"
		request := ToolRequest{
			RunID: runID, CallID: call.ID, Name: call.Name,
			Arguments:      append(json.RawMessage(nil), call.Arguments...),
			IdempotencyKey: toolIdempotencyKey(runID, call),
			CheckpointID:   checkpointID,
		}
		effects := &countingEffects{effect: &EffectRecord{
			State: "prepared", Request: request,
		}}
		var toolEvents []contract.EventType
		state, outcome, runtimeErr := (&Kernel{
			Model: &kernelModel{}, Tools: newRegistry(&handlerCalls),
			Effects: effects,
			BeforeEffect: func(context.Context) *contract.RuntimeError {
				return changed
			},
		}).Run(
			context.Background(),
			LoopState{
				RunID: runID, ModelProfile: "api",
				Messages: []contract.Message{
					{Role: contract.RoleUser, Content: "start"},
					{
						Role:      contract.RoleAssistant,
						ToolCalls: []contract.ToolCall{call},
					},
				},
				Round: 1, ToolCallCount: 1,
				SeenToolCallIDs:           []string{call.ID},
				PendingToolCalls:          []contract.ToolCall{call},
				PendingEffectCheckpointID: checkpointID,
				PendingCheckpointID:       checkpointID,
				RecoveredFromCheckpoint:   true,
			},
			func(event contract.Event) error {
				switch event.Type {
				case contract.EventCheckpointCommitted,
					contract.EventToolStarted,
					contract.EventToolCompleted,
					contract.EventToolFailed:
					toolEvents = append(toolEvents, event.Type)
				}
				return nil
			},
		)
		if runtimeErr != changed ||
			outcome.State != StateFailed ||
			outcome.StopReason != "execution_snapshot_changed" ||
			state.TerminalOutcome == nil ||
			state.TerminalOutcome.StopReason !=
				"execution_snapshot_changed" {
			t.Fatalf(
				"state=%#v outcome=%#v error=%v",
				state, outcome, runtimeErr,
			)
		}
		if handlerCalls != 0 || effects.prepared != 0 ||
			effects.started != 0 || effects.failed != 1 ||
			!reflect.DeepEqual(toolEvents, []contract.EventType{
				contract.EventCheckpointCommitted,
				contract.EventToolFailed,
			}) {
			t.Fatalf(
				"handler=%d effects=%#v events=%#v",
				handlerCalls, effects, toolEvents,
			)
		}
	})

	t.Run("recover_failed_snapshot_effect", func(t *testing.T) {
		handlerCalls := 0
		runID := "run_56565656565656565656565656565656"
		checkpointID :=
			"checkpoint_56565656565656565656565656565656"
		request := ToolRequest{
			RunID: runID, CallID: call.ID, Name: call.Name,
			Arguments:      append(json.RawMessage(nil), call.Arguments...),
			IdempotencyKey: toolIdempotencyKey(runID, call),
			CheckpointID:   checkpointID,
		}
		effects := &countingEffects{effect: &EffectRecord{
			State: "failed", Request: request, Error: changed,
		}}
		state, outcome, runtimeErr := (&Kernel{
			Model: &kernelModel{}, Tools: newRegistry(&handlerCalls),
			Effects: effects,
		}).Run(
			context.Background(),
			LoopState{
				RunID: runID, ModelProfile: "api",
				Messages: []contract.Message{
					{Role: contract.RoleUser, Content: "start"},
					{
						Role:      contract.RoleAssistant,
						ToolCalls: []contract.ToolCall{call},
					},
				},
				Round: 1, ToolCallCount: 1,
				SeenToolCallIDs:            []string{call.ID},
				PendingToolCalls:           []contract.ToolCall{call},
				PendingEffectCheckpointID:  checkpointID,
				PendingCheckpointID:        checkpointID,
				PendingCheckpointCommitted: true,
				RecoveredFromCheckpoint:    true,
			},
			nil,
		)
		if runtimeErr != changed ||
			outcome.State != StateFailed ||
			outcome.StopReason != "execution_snapshot_changed" ||
			state.TerminalOutcome == nil ||
			state.TerminalOutcome.StopReason !=
				"execution_snapshot_changed" ||
			handlerCalls != 0 || effects.started != 0 {
			t.Fatalf(
				"state=%#v outcome=%#v error=%v effects=%#v",
				state, outcome, runtimeErr, effects,
			)
		}
	})
}

func TestKernelRejectsPreparedEffectThatDiffersFromCheckpoint(t *testing.T) {
	call := contract.ToolCall{
		ID: "call_recovered", Name: "echo",
		Arguments: json.RawMessage(`{"value":"checkpoint"}`),
	}
	validRequest := ToolRequest{
		RunID:  "run_99999999999999999999999999999999",
		CallID: call.ID, Name: call.Name,
		Arguments:    append([]byte(nil), call.Arguments...),
		CheckpointID: "checkpoint_99999999999999999999999999999999",
		IdempotencyKey: toolIdempotencyKey(
			"run_99999999999999999999999999999999", call,
		),
	}
	testCases := []struct {
		name   string
		mutate func(*ToolRequest)
	}{
		{
			name: "name",
			mutate: func(request *ToolRequest) {
				request.Name = "other"
			},
		},
		{
			name: "arguments",
			mutate: func(request *ToolRequest) {
				request.Arguments = json.RawMessage(`{"value":"tampered"}`)
			},
		},
		{
			name: "idempotency_key",
			mutate: func(request *ToolRequest) {
				request.IdempotencyKey = "sha256:tampered"
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			recovered := cloneToolRequest(validRequest)
			testCase.mutate(&recovered)
			effects := &countingEffects{effect: &EffectRecord{
				State: "prepared", Request: recovered,
			}}
			handlerCalls := 0
			registry, err := NewRegistry(RegisteredTool{
				Definition: contract.ToolSpec{
					Name: "echo",
					InputSchema: json.RawMessage(`{
						"type":"object",
						"required":["value"],
						"properties":{"value":{"type":"string"}},
						"additionalProperties":false
					}`),
				},
				Handler: func(
					context.Context,
					ToolRequest,
				) (ToolResult, error) {
					handlerCalls++
					return ToolResult{Content: "{}"}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var toolEvents int
			state, outcome, runtimeErr := (&Kernel{
				Model: &kernelModel{}, Tools: registry, Effects: effects,
			}).Run(
				context.Background(),
				LoopState{
					RunID: validRequest.RunID, ModelProfile: "api",
					Messages: []contract.Message{
						{Role: contract.RoleUser, Content: "start"},
						{
							Role:      contract.RoleAssistant,
							ToolCalls: []contract.ToolCall{call},
						},
					},
					ToolCallCount: 1, SeenToolCallIDs: []string{call.ID},
					PendingToolCalls:          []contract.ToolCall{call},
					PendingEffectCheckpointID: validRequest.CheckpointID,
					PendingCheckpointID:       validRequest.CheckpointID,
				},
				func(event contract.Event) error {
					switch event.Type {
					case contract.EventCheckpointCommitted,
						contract.EventToolStarted,
						contract.EventToolCompleted,
						contract.EventToolFailed:
						toolEvents++
					}
					return nil
				},
			)
			if runtimeErr == nil ||
				runtimeErr.Code != contract.ErrorInternal ||
				outcome.State != StateNeedsReconciliation ||
				outcome.StopReason != "effect_recovery_failed" {
				t.Fatalf("state=%#v outcome=%#v err=%v", state, outcome, runtimeErr)
			}
			if handlerCalls != 0 || effects.started != 0 ||
				toolEvents != 0 {
				t.Fatalf(
					"handler=%d started=%d events=%d",
					handlerCalls, effects.started, toolEvents,
				)
			}
		})
	}
}

func TestKernelRecoveryDoesNotDuplicateDurableToolMessage(t *testing.T) {
	runID := "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	checkpointID := "checkpoint_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	call := contract.ToolCall{
		ID: "call_completed", Name: "echo",
		Arguments: json.RawMessage(`{"value":"durable"}`),
	}
	request := ToolRequest{
		RunID: runID, CallID: call.ID, Name: call.Name,
		Arguments:      append([]byte(nil), call.Arguments...),
		IdempotencyKey: toolIdempotencyKey(runID, call),
		CheckpointID:   checkpointID,
	}
	effects := &countingEffects{effect: &EffectRecord{
		State: "completed", Request: request,
		Result: &ToolResult{Content: "durable result"},
	}}
	registry, err := NewRegistry(RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "echo",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"required":["value"],
				"properties":{"value":{"type":"string"}},
				"additionalProperties":false
			}`),
		},
		Handler: func(
			context.Context,
			ToolRequest,
		) (ToolResult, error) {
			t.Fatal("completed durable effect was replayed")
			return ToolResult{}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &kernelModel{results: []contract.ModelResult{{
		Message: contract.Message{
			Role: contract.RoleAssistant, Content: "done",
		},
		FinishReason: contract.FinishStop,
	}}}
	state, outcome, runtimeErr := (&Kernel{
		Model: model, Tools: registry, Effects: effects,
	}).Run(
		context.Background(),
		LoopState{
			RunID: runID, ModelProfile: "api",
			Messages: []contract.Message{
				{Role: contract.RoleUser, Content: "start"},
				{
					Role:      contract.RoleAssistant,
					ToolCalls: []contract.ToolCall{call},
				},
				{
					Role:       contract.RoleTool,
					ToolCallID: call.ID,
					Content:    "durable result",
				},
			},
			Round: 1, ToolCallCount: 1,
			SeenToolCallIDs:           []string{call.ID},
			PendingToolCalls:          []contract.ToolCall{call},
			PendingEffectCheckpointID: checkpointID,
			PendingCheckpointID:       checkpointID,
			RecoveredFromCheckpoint:   true,
		},
		nil,
	)
	if runtimeErr != nil || outcome.State != StateCompleted {
		t.Fatalf("state=%#v outcome=%#v error=%v", state, outcome, runtimeErr)
	}
	toolMessages := 0
	for _, message := range state.Messages {
		if message.Role == contract.RoleTool &&
			message.ToolCallID == call.ID {
			toolMessages++
		}
	}
	if toolMessages != 1 || effects.started != 0 {
		t.Fatalf(
			"toolMessages=%d effects=%#v state=%#v",
			toolMessages, effects, state,
		)
	}
}

func TestRegistryRejectsInvalidInputSchemaAtRegistration(t *testing.T) {
	for name, schema := range map[string]string{
		"invalid_keyword": `{"type":7}`,
		"duplicate_field": `{"type":"object","type":"array"}`,
	} {
		t.Run(name, func(t *testing.T) {
			_, err := NewRegistry(RegisteredTool{
				Definition: contract.ToolSpec{
					Name: "echo", InputSchema: json.RawMessage(schema),
				},
				Handler: func(
					context.Context,
					ToolRequest,
				) (ToolResult, error) {
					return ToolResult{Content: "{}"}, nil
				},
			})
			if err == nil {
				t.Fatal("invalid input_schema was accepted")
			}
		})
	}
}

func TestValidateResumeInputAllowsAnySchemaApprovedJSONShape(
	t *testing.T,
) {
	testCases := []struct {
		name   string
		schema string
		input  string
	}{
		{
			name:   "string",
			schema: `{"type":"string","minLength":1}`,
			input:  `"approved"`,
		},
		{
			name: "array",
			schema: `{
				"type":"array",
				"items":{"type":"integer"},
				"minItems":1
			}`,
			input: `[1,2,3]`,
		},
		{
			name:   "number",
			schema: `{"type":"number","minimum":1}`,
			input:  `1.5`,
		},
		{
			name:   "null",
			schema: `{"type":"null"}`,
			input:  `null`,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			if err := ValidateResumeInput(
				json.RawMessage(testCase.schema),
				json.RawMessage(testCase.input),
			); err != nil {
				t.Fatalf("resume input was rejected: %v", err)
			}
		})
	}
	if err := ValidateResumeInput(
		json.RawMessage(`{"type":"object"}`),
		json.RawMessage(`null`),
	); err == nil {
		t.Fatal("schema-invalid null resume input was accepted")
	}
}

func TestKernelDurableTerminalEventFailureNeedsReconciliation(
	t *testing.T,
) {
	testCases := []struct {
		name       string
		result     contract.ModelResult
		tool       *RegisteredTool
		rejectType contract.EventType
		stopReason string
	}{
		{
			name: "agent_completed",
			result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "done",
				},
				FinishReason: contract.FinishStop,
			},
			rejectType: contract.EventAgentCompleted,
			stopReason: "model_completion_unknown",
		},
		{
			name: "tool_completed",
			result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant,
					ToolCalls: []contract.ToolCall{{
						ID: "call_completed_event", Name: "echo",
						Arguments: json.RawMessage(`{}`),
					}},
				},
				FinishReason: contract.FinishToolCall,
			},
			tool: &RegisteredTool{
				Definition: contract.ToolSpec{
					Name:        "echo",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				},
				Handler: func(
					context.Context,
					ToolRequest,
				) (ToolResult, error) {
					return ToolResult{Content: "durable"}, nil
				},
			},
			rejectType: contract.EventToolCompleted,
			stopReason: "tool_completion_unknown",
		},
		{
			name: "agent_paused",
			result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant,
					ToolCalls: []contract.ToolCall{{
						ID: "call_pause_event", Name: "approval",
						Arguments: json.RawMessage(`{}`),
					}},
				},
				FinishReason: contract.FinishToolCall,
			},
			tool: &RegisteredTool{
				Definition: contract.ToolSpec{
					Name:        "approval",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				},
				Handler: func(
					context.Context,
					ToolRequest,
				) (ToolResult, error) {
					return ToolResult{Pause: &Pause{
						ID: "pause_event", Kind: "approval",
						Prompt:      "approve?",
						InputSchema: json.RawMessage(`{"type":"boolean"}`),
					}}, nil
				},
			},
			rejectType: contract.EventAgentPaused,
			stopReason: "pause_event_unknown",
		},
		{
			name: "tool_failed",
			result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant,
					ToolCalls: []contract.ToolCall{{
						ID: "call_failed_event", Name: "known_failure",
						Arguments: json.RawMessage(`{}`),
					}},
				},
				FinishReason: contract.FinishToolCall,
			},
			tool: &RegisteredTool{
				Definition: contract.ToolSpec{
					Name:        "known_failure",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				},
				Handler: func(
					context.Context,
					ToolRequest,
				) (ToolResult, error) {
					return ToolResult{}, &KnownFailure{
						RuntimeError: &contract.RuntimeError{
							Code:    contract.ErrorToolFailed,
							Phase:   contract.PhaseRun,
							Message: "known failure",
						},
					}
				},
			},
			rejectType: contract.EventToolFailed,
			stopReason: "tool_failure_event_unknown",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			var tools []RegisteredTool
			if testCase.tool != nil {
				tools = append(tools, *testCase.tool)
			}
			registry, err := NewRegistry(tools...)
			if err != nil {
				t.Fatal(err)
			}
			effects := &countingEffects{}
			state, outcome, runtimeErr := (&Kernel{
				Model: &kernelModel{
					results: []contract.ModelResult{testCase.result},
				},
				Tools: registry, Effects: effects,
			}).Run(
				context.Background(),
				LoopState{
					RunID:        "run_33333333333333333333333333333333",
					ModelProfile: "api",
					Messages: []contract.Message{{
						Role: contract.RoleUser, Content: "start",
					}},
				},
				func(event contract.Event) error {
					if event.Type == testCase.rejectType {
						return errors.New("durable event sink failed")
					}
					return nil
				},
			)
			if runtimeErr == nil ||
				outcome.State != StateNeedsReconciliation ||
				outcome.StopReason != testCase.stopReason {
				t.Fatalf(
					"state=%#v outcome=%#v err=%v",
					state, outcome, runtimeErr,
				)
			}
			if testCase.tool != nil && effects.started != 1 {
				t.Fatalf("effects=%#v", effects)
			}
		})
	}
}

func TestKernelResumeValidatesBeforeAppendingMessage(t *testing.T) {
	registry, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	pause := &Pause{
		ID: "pause_invalid_state", Kind: "approval", Prompt: "approve?",
		ToolCallID:  "call_pause",
		InputSchema: json.RawMessage(`{"type":"boolean"}`),
	}
	state := LoopState{
		SchemaVersion: 2,
		RunID:         "run_44444444444444444444444444444444",
		ModelProfile:  "api",
		Messages: []contract.Message{{
			Role: contract.RoleUser, Content: "start",
		}},
		BaseMessageCount: 1,
		ToolCallCount:    2,
		SeenToolCallIDs:  []string{"call_duplicate", "call_duplicate"},
		Pause:            pause,
		TerminalOutcome: &Outcome{
			State: StatePaused, StopReason: "input_required", Pause: pause,
		},
	}
	recovered, outcome, runtimeErr := (&Kernel{
		Model: &kernelModel{}, Tools: registry, Effects: NewMemoryEffects(),
	}).Resume(
		context.Background(), state,
		ResumeInput{
			PauseID: pause.ID, Input: json.RawMessage(`true`),
		},
		nil,
	)
	if runtimeErr == nil || outcome.StopReason != "invalid_state" ||
		len(recovered.Messages) != len(state.Messages) ||
		!reflect.DeepEqual(recovered.Messages, state.Messages) {
		t.Fatalf(
			"recovered=%#v outcome=%#v err=%v",
			recovered, outcome, runtimeErr,
		)
	}
}

func TestRegistryExecuteValidatesArgumentsBeforeHandler(t *testing.T) {
	handlerCalls := 0
	registry, err := NewRegistry(RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "echo",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"required":["value"],
				"properties":{"value":{"type":"string"}},
				"additionalProperties":false
			}`),
		},
		Handler: func(
			context.Context,
			ToolRequest,
		) (ToolResult, error) {
			handlerCalls++
			return ToolResult{Content: "{}"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := registry.Execute(context.Background(), ToolRequest{
		Name: "echo", Arguments: json.RawMessage(`{"value":7}`),
	}); err == nil {
		t.Fatal("Registry.Execute accepted schema-invalid arguments")
	}
	if handlerCalls != 0 {
		t.Fatalf("handlerCalls=%d", handlerCalls)
	}
}

func TestRegistryPreservesLargeJSONIntegerPrecision(t *testing.T) {
	registry, err := NewRegistry(RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "exact_integer",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"required":["value"],
				"properties":{
					"value":{"const":9007199254740993}
				},
				"additionalProperties":false
			}`),
		},
		Handler: func(
			context.Context,
			ToolRequest,
		) (ToolResult, error) {
			return ToolResult{Content: "{}"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Validate(ToolRequest{
		Name:      "exact_integer",
		Arguments: json.RawMessage(`{"value":9007199254740993}`),
	}); err != nil {
		t.Fatalf("exact integer was rounded or rejected: %v", err)
	}
	if err := registry.Validate(ToolRequest{
		Name:      "exact_integer",
		Arguments: json.RawMessage(`{"value":9007199254740992}`),
	}); err == nil {
		t.Fatal("adjacent large integer incorrectly matched const")
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
