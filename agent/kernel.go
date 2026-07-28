package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
)

const agentSchemaVersion = 1

func (kernel *Kernel) Run(
	ctx context.Context,
	state LoopState,
	sink contract.EventSink,
) (LoopState, Outcome, *contract.RuntimeError) {
	if runtimeErr := kernel.validate(&state); runtimeErr != nil {
		return state, failureOutcome(StateFailed, "invalid_state", runtimeErr), runtimeErr
	}
	budget := kernel.effectiveBudget()
	runContext := ctx
	cancel := func() {}
	if budget.MaxWallTime > 0 {
		runContext, cancel = context.WithTimeout(ctx, budget.MaxWallTime)
	}
	defer cancel()
	emitter := eventEmitter{state: &state, sink: sink, now: kernel.now}
	seen := make(map[string]struct{}, len(state.SeenToolCallIDs))
	for _, callID := range state.SeenToolCallIDs {
		seen[callID] = struct{}{}
	}
	definitions := kernel.Tools.Definitions()
	knownTools := make(map[string]struct{}, len(definitions))
	for _, definition := range definitions {
		knownTools[definition.Name] = struct{}{}
	}
	for {
		if err := runContext.Err(); err != nil {
			runtimeErr := cancellationError(err)
			outcome := failureOutcome(StateCancelled, "cancelled", runtimeErr)
			return state, outcome, runtimeErr
		}
		if state.Round >= budget.MaxRounds {
			runtimeErr := budgetError("round budget exhausted")
			return state, failureOutcome(StateFailed, "round_budget", runtimeErr), runtimeErr
		}
		request := contract.GenerateRequest{
			ModelProfile: state.ModelProfile,
			Input: contract.ModelRequest{
				Messages: append([]contract.Message(nil), state.Messages...),
				Tools:    definitions,
				Trace: contract.TraceContext{Labels: map[string]string{
					"run_id": state.RunID,
				}},
			},
		}
		result, runtimeErr := kernel.Model.GenerateStream(
			runContext, request, emitter.rebaseModelEvent,
		)
		if runtimeErr != nil {
			outcomeState := StateFailed
			stopReason := "model_failed"
			if runtimeErr.Code == contract.ErrorCancelled ||
				runtimeErr.Code == contract.ErrorTimeout {
				outcomeState = StateCancelled
				stopReason = "cancelled"
			}
			return state, failureOutcome(outcomeState, stopReason, runtimeErr), runtimeErr
		}
		state.Round++
		state.Messages = append(state.Messages, cloneMessage(result.Message))
		state.TotalTokens += usageTotal(result.Usage)
		if budget.MaxTotalTokens > 0 && state.TotalTokens > budget.MaxTotalTokens {
			runtimeErr := budgetError("token budget exhausted")
			return state, failureOutcome(StateFailed, "token_budget", runtimeErr), runtimeErr
		}
		if result.FinishReason != contract.FinishToolCall {
			message := cloneMessage(result.Message)
			if runtimeErr := emitter.emit(contract.Event{
				Type: contract.EventAgentCompleted,
				Agent: &contract.AgentEvent{
					RunID: state.RunID, State: string(StateCompleted),
					StopReason: string(result.FinishReason),
				},
			}); runtimeErr != nil {
				return state, failureOutcome(StateFailed, "event_failed", runtimeErr), runtimeErr
			}
			return state, Outcome{
				State: StateCompleted, StopReason: string(result.FinishReason),
				Message: &message,
			}, nil
		}
		for _, call := range result.Message.ToolCalls {
			if _, exists := seen[call.ID]; exists {
				runtimeErr := agentError(
					contract.ErrorInvalidProviderResponse,
					fmt.Sprintf("duplicate tool call ID %q", call.ID),
				)
				return state, failureOutcome(StateFailed, "invalid_tool_call", runtimeErr), runtimeErr
			}
			if _, exists := knownTools[call.Name]; !exists {
				runtimeErr := agentError(
					contract.ErrorInvalidProviderResponse,
					fmt.Sprintf("model requested unregistered tool %q", call.Name),
				)
				return state, failureOutcome(StateFailed, "unknown_tool", runtimeErr), runtimeErr
			}
			if state.ToolCallCount >= budget.MaxToolCalls {
				runtimeErr := budgetError("tool-call budget exhausted")
				return state, failureOutcome(StateFailed, "tool_budget", runtimeErr), runtimeErr
			}
			seen[call.ID] = struct{}{}
			state.SeenToolCallIDs = append(state.SeenToolCallIDs, call.ID)
			state.ToolCallCount++
			toolRequest := ToolRequest{
				RunID: state.RunID, CallID: call.ID, Name: call.Name,
				Arguments:      append([]byte(nil), call.Arguments...),
				IdempotencyKey: toolIdempotencyKey(state.RunID, call),
			}
			checkpointID, err := kernel.Effects.Prepared(runContext, toolRequest, state)
			if err != nil {
				runtimeErr := agentError(
					contract.ErrorInternal, "prepare tool checkpoint: "+err.Error(),
				)
				return state, failureOutcome(StateFailed, "checkpoint_failed", runtimeErr), runtimeErr
			}
			if runtimeErr := emitter.emit(contract.Event{
				Type: contract.EventCheckpointCommitted,
				Checkpoint: &contract.CheckpointEvent{
					RunID: state.RunID, CheckpointID: checkpointID,
				},
			}); runtimeErr != nil {
				return state, failureOutcome(StateFailed, "event_failed", runtimeErr), runtimeErr
			}
			if err := kernel.Effects.Started(runContext, toolRequest); err != nil {
				runtimeErr := agentError(
					contract.ErrorInternal, "record tool start: "+err.Error(),
				)
				return state, failureOutcome(StateFailed, "tool_start_failed", runtimeErr), runtimeErr
			}
			if runtimeErr := emitter.emit(contract.Event{
				Type: contract.EventToolStarted,
				Tool: &contract.ToolEvent{
					CallID: call.ID, Name: call.Name,
					IdempotencyKey: toolRequest.IdempotencyKey,
				},
			}); runtimeErr != nil {
				return state, failureOutcome(
					StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
				), runtimeErr
			}
			toolResult, err := kernel.Tools.Execute(runContext, toolRequest)
			if err != nil {
				runtimeErr := agentError(contract.ErrorToolFailed, err.Error())
				if recorderErr := kernel.Effects.Failed(
					context.WithoutCancel(runContext), toolRequest, runtimeErr,
				); recorderErr != nil {
					runtimeErr.Message += "; record failure: " + recorderErr.Error()
				}
				_ = emitter.emit(contract.Event{
					Type: contract.EventToolFailed,
					Tool: &contract.ToolEvent{
						CallID: call.ID, Name: call.Name,
						IdempotencyKey: toolRequest.IdempotencyKey,
					},
					Error: runtimeErr,
				})
				if runContext.Err() != nil {
					return state, failureOutcome(
						StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
					), runtimeErr
				}
				return state, failureOutcome(StateFailed, "tool_failed", runtimeErr), runtimeErr
			}
			if err := validateToolResult(toolResult, call.ID); err != nil {
				runtimeErr := agentError(contract.ErrorToolFailed, err.Error())
				_ = kernel.Effects.Failed(
					context.WithoutCancel(runContext), toolRequest, runtimeErr,
				)
				return state, failureOutcome(StateFailed, "invalid_tool_result", runtimeErr), runtimeErr
			}
			if err := kernel.Effects.Completed(
				context.WithoutCancel(runContext), toolRequest, toolResult,
			); err != nil {
				runtimeErr := agentError(
					contract.ErrorInternal, "record tool completion: "+err.Error(),
				)
				return state, failureOutcome(
					StateNeedsReconciliation, "tool_completion_unknown", runtimeErr,
				), runtimeErr
			}
			if toolResult.Pause != nil {
				pause := clonePause(*toolResult.Pause)
				pause.ToolCallID = call.ID
				state.Pause = &pause
				if runtimeErr := emitter.emit(contract.Event{
					Type: contract.EventAgentPaused,
					Agent: &contract.AgentEvent{
						RunID: state.RunID, State: string(StatePaused),
						PauseID: pause.ID,
					},
				}); runtimeErr != nil {
					return state, failureOutcome(StateFailed, "event_failed", runtimeErr), runtimeErr
				}
				return state, Outcome{
					State: StatePaused, StopReason: "input_required", Pause: &pause,
				}, nil
			}
			state.Messages = append(state.Messages, contract.Message{
				Role: contract.RoleTool, ToolCallID: call.ID,
				Content: toolResult.Content,
			})
			if runtimeErr := emitter.emit(contract.Event{
				Type: contract.EventToolCompleted,
				Tool: &contract.ToolEvent{
					CallID: call.ID, Name: call.Name,
					IdempotencyKey: toolRequest.IdempotencyKey,
					Content:        toolResult.Content, IsError: toolResult.IsError,
				},
			}); runtimeErr != nil {
				return state, failureOutcome(StateFailed, "event_failed", runtimeErr), runtimeErr
			}
		}
	}
}

func (kernel *Kernel) Resume(
	ctx context.Context,
	state LoopState,
	input ResumeInput,
	sink contract.EventSink,
) (LoopState, Outcome, *contract.RuntimeError) {
	if state.Pause == nil || input.PauseID == "" ||
		state.Pause.ID != input.PauseID {
		runtimeErr := agentError(contract.ErrorConflict, "pause_id does not match active pause")
		return state, failureOutcome(StateFailed, "invalid_resume", runtimeErr), runtimeErr
	}
	if state.Pause.ExpiresAt != nil && kernel.now().After(*state.Pause.ExpiresAt) {
		runtimeErr := agentError(contract.ErrorConflict, "pause has expired")
		return state, failureOutcome(StateFailed, "pause_expired", runtimeErr), runtimeErr
	}
	if err := validateResumeInput(state.Pause.InputSchema, input.Input); err != nil {
		runtimeErr := agentError(contract.ErrorInvalidRequest, err.Error())
		return state, failureOutcome(StateFailed, "invalid_resume", runtimeErr), runtimeErr
	}
	state.Messages = append(state.Messages, contract.Message{
		Role: contract.RoleTool, ToolCallID: state.Pause.ToolCallID,
		Content: string(input.Input),
	})
	state.Pause = nil
	return kernel.Run(ctx, state, sink)
}

func (kernel *Kernel) validate(state *LoopState) *contract.RuntimeError {
	if kernel == nil || kernel.Model == nil || kernel.Tools == nil ||
		kernel.Effects == nil {
		return agentError(contract.ErrorInternal, "agent kernel ports are required")
	}
	if state.SchemaVersion == 0 {
		state.SchemaVersion = agentSchemaVersion
	}
	if state.SchemaVersion != agentSchemaVersion {
		return agentError(contract.ErrorInvalidRequest, "unsupported loop state schema")
	}
	if err := identity.Validate(state.RunID, "run"); err != nil {
		return agentError(contract.ErrorInvalidRequest, err.Error())
	}
	if strings.TrimSpace(state.ModelProfile) == "" {
		return agentError(contract.ErrorInvalidRequest, "model_profile is required")
	}
	if len(state.Messages) == 0 {
		return agentError(contract.ErrorInvalidRequest, "messages are required")
	}
	for index, message := range state.Messages {
		if err := message.Validate(); err != nil {
			return agentError(
				contract.ErrorInvalidRequest,
				fmt.Sprintf("messages[%d]: %v", index, err),
			)
		}
	}
	for _, definition := range kernel.Tools.Definitions() {
		if err := definition.Validate(); err != nil {
			return agentError(contract.ErrorInvalidRequest, err.Error())
		}
	}
	return nil
}

func (kernel *Kernel) effectiveBudget() Budget {
	value := kernel.Budget
	defaults := DefaultBudget()
	if value.MaxRounds <= 0 {
		value.MaxRounds = defaults.MaxRounds
	}
	if value.MaxToolCalls <= 0 {
		value.MaxToolCalls = defaults.MaxToolCalls
	}
	if value.MaxWallTime <= 0 {
		value.MaxWallTime = defaults.MaxWallTime
	}
	return value
}

func (kernel *Kernel) now() time.Time {
	if kernel.Now != nil {
		return kernel.Now().UTC()
	}
	return time.Now().UTC()
}

type eventEmitter struct {
	state *LoopState
	sink  contract.EventSink
	now   func() time.Time
}

func (emitter *eventEmitter) rebaseModelEvent(event contract.Event) error {
	event.Sequence = 0
	if runtimeErr := emitter.emit(event); runtimeErr != nil {
		return runtimeErr
	}
	return nil
}

func (emitter *eventEmitter) emit(event contract.Event) *contract.RuntimeError {
	emitter.state.NextEventSequence++
	event.Sequence = emitter.state.NextEventSequence
	now := time.Now().UTC()
	if emitter.now != nil {
		now = emitter.now().UTC()
	}
	event.Time = &now
	if err := event.Validate(); err != nil {
		return agentError(contract.ErrorInternal, "invalid agent event: "+err.Error())
	}
	if emitter.sink != nil {
		if err := emitter.sink(event); err != nil {
			return agentError(contract.ErrorCancelled, "event sink stopped")
		}
	}
	return nil
}

func toolIdempotencyKey(runID string, call contract.ToolCall) string {
	hash := sha256.New()
	hash.Write([]byte(runID))
	hash.Write([]byte{0})
	hash.Write([]byte(call.ID))
	hash.Write([]byte{0})
	hash.Write([]byte(call.Name))
	hash.Write([]byte{0})
	hash.Write(call.Arguments)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func validateToolResult(result ToolResult, callID string) error {
	if result.Pause != nil {
		if result.Content != "" || result.IsError {
			return fmt.Errorf("paused tool result cannot also contain content")
		}
		if strings.TrimSpace(result.Pause.ID) == "" ||
			strings.TrimSpace(result.Pause.Kind) == "" ||
			strings.TrimSpace(result.Pause.Prompt) == "" {
			return fmt.Errorf("pause requires pause_id, kind, and prompt")
		}
		if err := validateJSONObject(result.Pause.InputSchema); err != nil {
			return fmt.Errorf("pause input_schema: %w", err)
		}
		if result.Pause.ToolCallID != "" && result.Pause.ToolCallID != callID {
			return fmt.Errorf("pause tool_call_id does not match")
		}
		return nil
	}
	if result.Content == "" {
		return fmt.Errorf("tool result content is required")
	}
	return nil
}

func validateResumeInput(schema, input json.RawMessage) error {
	if err := validateJSONObject(schema); err != nil {
		return fmt.Errorf("pause input_schema: %w", err)
	}
	if err := validateJSONObject(input); err != nil {
		return fmt.Errorf("resume input: %w", err)
	}
	var schemaValue struct {
		Required []string `json:"required"`
	}
	var inputValue map[string]json.RawMessage
	if err := json.Unmarshal(schema, &schemaValue); err != nil {
		return err
	}
	if err := json.Unmarshal(input, &inputValue); err != nil {
		return err
	}
	for _, name := range schemaValue.Required {
		if _, exists := inputValue[name]; !exists {
			return fmt.Errorf("resume input is missing required property %q", name)
		}
	}
	return nil
}

func validateJSONObject(value json.RawMessage) error {
	value = bytes.TrimSpace(value)
	if len(value) == 0 || value[0] != '{' || !json.Valid(value) {
		return fmt.Errorf("must be a JSON object")
	}
	return nil
}

func usageTotal(usage contract.Usage) int64 {
	if usage.TotalTokens != nil {
		return *usage.TotalTokens
	}
	var total int64
	if usage.InputTokens != nil {
		total += *usage.InputTokens
	}
	if usage.OutputTokens != nil {
		total += *usage.OutputTokens
	}
	return total
}

func cloneMessage(value contract.Message) contract.Message {
	result := value
	result.ToolCalls = append([]contract.ToolCall(nil), value.ToolCalls...)
	for index := range result.ToolCalls {
		result.ToolCalls[index].Arguments = append(
			[]byte(nil), result.ToolCalls[index].Arguments...,
		)
	}
	return result
}

func clonePause(value Pause) Pause {
	value.InputSchema = append([]byte(nil), value.InputSchema...)
	if value.ExpiresAt != nil {
		current := *value.ExpiresAt
		value.ExpiresAt = &current
	}
	return value
}

func failureOutcome(
	state State,
	stopReason string,
	runtimeErr *contract.RuntimeError,
) Outcome {
	return Outcome{
		State: state, StopReason: stopReason, Error: runtimeErr,
	}
}

func cancellationError(err error) *contract.RuntimeError {
	code := contract.ErrorCancelled
	if errors.Is(err, context.DeadlineExceeded) {
		code = contract.ErrorTimeout
	}
	return agentError(code, err.Error())
}

func budgetError(message string) *contract.RuntimeError {
	return agentError(contract.ErrorInvalidRequest, message)
}

func agentError(
	code contract.ErrorCode,
	message string,
) *contract.RuntimeError {
	return &contract.RuntimeError{
		Code: code, Phase: contract.PhaseRun, Message: message,
	}
}

type memoryEffects struct{}

func NewMemoryEffects() EffectRecorder {
	return memoryEffects{}
}

func (memoryEffects) Prepared(
	_ context.Context,
	request ToolRequest,
	_ LoopState,
) (string, error) {
	sum := sha256.Sum256([]byte(request.IdempotencyKey))
	return "checkpoint_" + hex.EncodeToString(sum[:16]), nil
}

func (memoryEffects) Started(context.Context, ToolRequest) error {
	return nil
}

func (memoryEffects) Completed(context.Context, ToolRequest, ToolResult) error {
	return nil
}

func (memoryEffects) Failed(
	context.Context,
	ToolRequest,
	*contract.RuntimeError,
) error {
	return nil
}
