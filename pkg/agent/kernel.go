package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"math"
	"reflect"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
)

// LoopStateSchemaVersion identifies the durable Agent checkpoint contract.
const LoopStateSchemaVersion = 2

func (kernel *Kernel) Run(
	ctx context.Context,
	state LoopState,
	sink contract.EventSink,
) (LoopState, Outcome, *contract.RuntimeError) {
	if runtimeErr := kernel.validate(&state); runtimeErr != nil {
		return state, failureOutcome(StateFailed, "invalid_state", runtimeErr), runtimeErr
	}
	if state.TerminalOutcome != nil {
		outcome := cloneOutcome(*state.TerminalOutcome)
		return state, outcome, outcome.Error
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
		if state.PendingToolCursor < len(state.PendingToolCalls) &&
			state.RecoveredFromCheckpoint {
			terminal, outcome, runtimeErr := kernel.processPendingTool(
				runContext, &state, seen, knownTools, budget, &emitter,
			)
			if terminal {
				return state, outcome, runtimeErr
			}
			continue
		}
		if err := runContext.Err(); err != nil {
			runtimeErr := cancellationError(err)
			outcome := freezeTerminalFailure(
				&state, StateCancelled, "cancelled", runtimeErr,
			)
			return state, outcome, runtimeErr
		}
		if state.PendingToolCursor < len(state.PendingToolCalls) {
			terminal, outcome, runtimeErr := kernel.processPendingTool(
				runContext, &state, seen, knownTools, budget, &emitter,
			)
			if terminal {
				return state, outcome, runtimeErr
			}
			continue
		}
		clearPendingTools(&state)
		if state.Round >= budget.MaxRounds {
			runtimeErr := budgetError("round budget exhausted")
			outcome := freezeTerminalFailure(
				&state, StateFailed, "round_budget", runtimeErr,
			)
			return state, outcome, runtimeErr
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
		modelContext := model.WithAttemptOrigin(runContext, model.AttemptOrigin{
			Namespace: model.AttemptNamespaceAgent,
			Source:    "agent " + state.RunID,
		})
		result, runtimeErr := kernel.Model.GenerateStream(
			modelContext, request, emitter.rebaseModelEvent,
		)
		if runtimeErr != nil {
			if executionSnapshotChanged(runtimeErr) {
				outcome := freezeTerminalFailure(
					&state, StateFailed,
					"execution_snapshot_changed", runtimeErr,
				)
				return state, outcome, runtimeErr
			}
			if runtimeErr.Code == contract.ErrorConflict &&
				runtimeErr.Phase == contract.PhaseRun {
				return state, failureOutcome(
					StateNeedsReconciliation,
					"model_call_unknown", runtimeErr,
				), runtimeErr
			}
			outcomeState := StateFailed
			stopReason := "model_failed"
			if runtimeErr.Code == contract.ErrorCancelled ||
				runtimeErr.Code == contract.ErrorTimeout {
				outcomeState = StateCancelled
				stopReason = "cancelled"
			}
			outcome := freezeTerminalFailure(
				&state, outcomeState, stopReason, runtimeErr,
			)
			return state, outcome, runtimeErr
		}
		state.Round++
		state.Messages = append(state.Messages, cloneMessage(result.Message))
		roundTokens, err := usageTotal(result.Usage)
		if err != nil ||
			roundTokens > math.MaxInt64-state.TotalTokens {
			message := "model usage token total overflows int64"
			if err != nil {
				message = err.Error()
			}
			runtimeErr := agentError(
				contract.ErrorInvalidProviderResponse, message,
			)
			outcome := freezeTerminalFailure(
				&state, StateFailed, "invalid_model_usage", runtimeErr,
			)
			return state, outcome, runtimeErr
		}
		state.TotalTokens += roundTokens
		if budget.MaxTotalTokens > 0 && state.TotalTokens > budget.MaxTotalTokens {
			runtimeErr := budgetError("token budget exhausted")
			outcome := freezeTerminalFailure(
				&state, StateFailed, "token_budget", runtimeErr,
			)
			return state, outcome, runtimeErr
		}
		if result.FinishReason == contract.FinishCancelled {
			runtimeErr := agentError(
				contract.ErrorCancelled, "model generation was cancelled",
			)
			outcome := freezeTerminalFailure(
				&state, StateCancelled,
				string(contract.FinishCancelled), runtimeErr,
			)
			return state, outcome, runtimeErr
		}
		if result.FinishReason != contract.FinishToolCall {
			message := cloneMessage(result.Message)
			outcome := Outcome{
				State: StateCompleted, StopReason: string(result.FinishReason),
				Message: &message,
			}
			state.TerminalOutcome = &outcome
			if runtimeErr := emitter.emit(contract.Event{
				Type: contract.EventAgentCompleted,
				Agent: &contract.AgentEvent{
					RunID: state.RunID, State: string(StateCompleted),
					StopReason: string(result.FinishReason),
				},
			}); runtimeErr != nil {
				return state, failureOutcome(
					StateNeedsReconciliation,
					"model_completion_unknown", runtimeErr,
				), runtimeErr
			}
			return state, outcome, nil
		}
		state.PendingToolCalls = cloneToolCalls(result.Message.ToolCalls)
		state.PendingToolCursor = 0
	}
}

func (kernel *Kernel) processPendingTool(
	ctx context.Context,
	state *LoopState,
	seen map[string]struct{},
	knownTools map[string]struct{},
	budget Budget,
	emitter *eventEmitter,
) (bool, Outcome, *contract.RuntimeError) {
	call := state.PendingToolCalls[state.PendingToolCursor]
	lookupContext := ctx
	if state.RecoveredFromCheckpoint {
		lookupContext = context.WithoutCancel(ctx)
	}
	effect, recovered, err := kernel.Effects.Lookup(
		lookupContext, state.RunID, call.ID,
	)
	if err != nil {
		runtimeErr := agentError(
			contract.ErrorInternal, "load prepared tool effect: "+err.Error(),
		)
		return true, failureOutcome(
			StateNeedsReconciliation, "effect_recovery_failed", runtimeErr,
		), runtimeErr
	}
	var request ToolRequest
	if !recovered && state.RecoveredFromCheckpoint {
		runtimeErr := agentError(
			contract.ErrorInternal,
			"recovered pending tool call has no durable effect",
		)
		return true, failureOutcome(
			StateNeedsReconciliation, "effect_recovery_failed", runtimeErr,
		), runtimeErr
	}
	if recovered {
		request = cloneToolRequest(effect.Request)
		if request.RunID != state.RunID || request.CallID != call.ID ||
			request.Name != call.Name ||
			!bytes.Equal(request.Arguments, call.Arguments) ||
			request.IdempotencyKey != toolIdempotencyKey(state.RunID, call) ||
			request.CheckpointID == "" ||
			request.CheckpointID != state.PendingEffectCheckpointID {
			runtimeErr := agentError(
				contract.ErrorInternal,
				"prepared tool effect does not match pending call",
			)
			return true, failureOutcome(
				StateNeedsReconciliation, "effect_recovery_failed", runtimeErr,
			), runtimeErr
		}
		if state.PendingCheckpointID == "" ||
			state.PendingCheckpointID != request.CheckpointID {
			runtimeErr := agentError(
				contract.ErrorInternal,
				"prepared tool effect has no matching durable checkpoint",
			)
			return true, failureOutcome(
				StateNeedsReconciliation, "effect_recovery_failed", runtimeErr,
			), runtimeErr
		}
		if ctx.Err() != nil &&
			(effect.State == "prepared" || effect.State == "started") {
			runtimeErr := agentError(
				contract.ErrorConflict,
				"recovered tool effect cannot be advanced after cancellation",
			)
			return true, failureOutcome(
				StateNeedsReconciliation,
				"tool_effect_unknown", runtimeErr,
			), runtimeErr
		}
	} else {
		if _, exists := seen[call.ID]; exists {
			runtimeErr := agentError(
				contract.ErrorInvalidProviderResponse,
				fmt.Sprintf("duplicate tool call ID %q", call.ID),
			)
			outcome := freezeTerminalFailure(
				state, StateFailed, "duplicate_tool_call", runtimeErr,
			)
			return true, outcome, runtimeErr
		}
		if _, exists := knownTools[call.Name]; !exists {
			runtimeErr := agentError(
				contract.ErrorInvalidProviderResponse,
				fmt.Sprintf("model requested unregistered tool %q", call.Name),
			)
			outcome := freezeTerminalFailure(
				state, StateFailed, "unknown_tool", runtimeErr,
			)
			return true, outcome, runtimeErr
		}
		if state.ToolCallCount >= budget.MaxToolCalls {
			runtimeErr := budgetError("tool-call budget exhausted")
			outcome := freezeTerminalFailure(
				state, StateFailed, "tool_budget", runtimeErr,
			)
			return true, outcome, runtimeErr
		}
		request = ToolRequest{
			RunID: state.RunID, CallID: call.ID, Name: call.Name,
			Arguments:      append([]byte(nil), call.Arguments...),
			IdempotencyKey: toolIdempotencyKey(state.RunID, call),
		}
	}
	if _, exists := knownTools[request.Name]; !exists {
		runtimeErr := agentError(
			contract.ErrorInvalidProviderResponse,
			fmt.Sprintf("model requested unregistered tool %q", request.Name),
		)
		outcome := freezeTerminalFailure(
			state, StateFailed, "unknown_tool", runtimeErr,
		)
		return true, outcome, runtimeErr
	}
	if err := kernel.Tools.Validate(request); err != nil {
		runtimeErr := agentError(
			contract.ErrorInvalidProviderResponse, err.Error(),
		)
		outcome := freezeTerminalFailure(
			state, StateFailed, "invalid_tool_arguments", runtimeErr,
		)
		return true, outcome, runtimeErr
	}
	if !recovered {
		if runtimeErr := kernel.checkBeforeEffect(ctx); runtimeErr != nil {
			outcome := freezeTerminalFailure(
				state, StateFailed,
				"execution_snapshot_changed", runtimeErr,
			)
			return true, outcome, runtimeErr
		}
		seen[call.ID] = struct{}{}
		state.SeenToolCallIDs = append(state.SeenToolCallIDs, call.ID)
		state.ToolCallCount++
		checkpointID, err := kernel.Effects.Prepared(ctx, &request, state)
		if err != nil {
			runtimeErr := agentError(
				contract.ErrorInternal, "prepare tool checkpoint: "+err.Error(),
			)
			return true, failureOutcome(
				StateNeedsReconciliation, "checkpoint_unknown", runtimeErr,
			), runtimeErr
		}
		if checkpointID == "" ||
			request.CheckpointID != checkpointID ||
			state.PendingEffectCheckpointID != checkpointID {
			runtimeErr := agentError(
				contract.ErrorInternal,
				"prepared tool effect checkpoint association is invalid",
			)
			return true, failureOutcome(
				StateNeedsReconciliation, "checkpoint_unknown", runtimeErr,
			), runtimeErr
		}
		state.PendingCheckpointID = checkpointID
		effect = EffectRecord{State: "prepared", Request: request}
	}
	if !state.PendingCheckpointCommitted {
		if runtimeErr := emitter.emit(contract.Event{
			Type: contract.EventCheckpointCommitted,
			Checkpoint: &contract.CheckpointEvent{
				RunID: state.RunID, CheckpointID: state.PendingCheckpointID,
			},
		}); runtimeErr != nil {
			return true, failureOutcome(
				StateNeedsReconciliation,
				"checkpoint_event_unknown", runtimeErr,
			), runtimeErr
		}
		state.PendingCheckpointCommitted = true
	}
	switch effect.State {
	case "prepared":
		return kernel.executePreparedTool(ctx, state, request, emitter)
	case "started":
		runtimeErr := agentError(
			contract.ErrorConflict,
			"tool effect outcome is unknown after process restart",
		)
		return true, failureOutcome(
			StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
		), runtimeErr
	case "completed":
		if effect.Result == nil {
			runtimeErr := agentError(
				contract.ErrorInternal,
				"completed tool effect has no durable result",
			)
			return true, failureOutcome(
				StateNeedsReconciliation, "effect_recovery_failed", runtimeErr,
			), runtimeErr
		}
		return kernel.finishCompletedTool(
			state, request, *effect.Result, emitter,
		)
	case "failed":
		if effect.Error == nil {
			effect.Error = agentError(
				contract.ErrorToolFailed,
				"tool effect failed without a durable error",
			)
		}
		if !state.PendingToolTerminal {
			if runtimeErr := emitter.emit(toolFailedEvent(request, effect.Error)); runtimeErr != nil {
				return true, failureOutcome(
					StateNeedsReconciliation,
					"tool_failure_event_unknown", runtimeErr,
				), runtimeErr
			}
			state.PendingToolTerminal = true
		}
		stopReason := "tool_failed"
		if !state.PendingToolStarted &&
			executionSnapshotChanged(effect.Error) {
			stopReason = "execution_snapshot_changed"
		}
		outcome := freezeTerminalFailure(
			state, StateFailed, stopReason, effect.Error,
		)
		return true, outcome, effect.Error
	default:
		runtimeErr := agentError(
			contract.ErrorInternal,
			fmt.Sprintf("unsupported durable tool effect state %q", effect.State),
		)
		return true, failureOutcome(
			StateNeedsReconciliation, "effect_recovery_failed", runtimeErr,
		), runtimeErr
	}
}

func (kernel *Kernel) executePreparedTool(
	ctx context.Context,
	state *LoopState,
	request ToolRequest,
	emitter *eventEmitter,
) (bool, Outcome, *contract.RuntimeError) {
	if runtimeErr := kernel.checkBeforeEffect(ctx); runtimeErr != nil {
		if err := kernel.Effects.Failed(
			context.WithoutCancel(ctx), request, runtimeErr,
		); err != nil {
			runtimeErr.Message += "; record pre-effect failure: " + err.Error()
			return true, failureOutcome(
				StateNeedsReconciliation,
				"tool_effect_unknown", runtimeErr,
			), runtimeErr
		}
		if !state.PendingToolTerminal {
			if eventErr := emitter.emit(
				toolFailedEvent(request, runtimeErr),
			); eventErr != nil {
				return true, failureOutcome(
					StateNeedsReconciliation,
					"tool_failure_event_unknown", eventErr,
				), eventErr
			}
			state.PendingToolTerminal = true
		}
		outcome := freezeTerminalFailure(
			state, StateFailed,
			"execution_snapshot_changed", runtimeErr,
		)
		return true, outcome, runtimeErr
	}
	if err := kernel.Effects.Started(ctx, request); err != nil {
		runtimeErr := agentError(
			contract.ErrorInternal, "record tool start: "+err.Error(),
		)
		return true, failureOutcome(
			StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
		), runtimeErr
	}
	if !state.PendingToolStarted {
		if runtimeErr := emitter.emit(contract.Event{
			Type: contract.EventToolStarted,
			Tool: &contract.ToolEvent{
				CallID: request.CallID, Name: request.Name,
				IdempotencyKey: request.IdempotencyKey,
			},
		}); runtimeErr != nil {
			return true, failureOutcome(
				StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
			), runtimeErr
		}
		state.PendingToolStarted = true
	}
	result, err := kernel.Tools.Execute(ctx, request)
	if err != nil {
		if ctx.Err() != nil ||
			errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) {
			runtimeErr := agentError(contract.ErrorToolFailed, err.Error())
			return true, failureOutcome(
				StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
			), runtimeErr
		}
		var knownFailure *KnownFailure
		if !errors.As(err, &knownFailure) ||
			knownFailure == nil ||
			knownFailure.RuntimeError == nil ||
			knownFailure.RuntimeError.Validate() != nil {
			runtimeErr := agentError(contract.ErrorToolFailed, err.Error())
			return true, failureOutcome(
				StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
			), runtimeErr
		}
		runtimeErr := cloneRuntimeError(knownFailure.RuntimeError)
		if recorderErr := kernel.Effects.Failed(
			context.WithoutCancel(ctx), request, runtimeErr,
		); recorderErr != nil {
			runtimeErr.Message += "; record failure: " + recorderErr.Error()
			return true, failureOutcome(
				StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
			), runtimeErr
		}
		if !state.PendingToolTerminal {
			if eventErr := emitter.emit(
				toolFailedEvent(request, runtimeErr),
			); eventErr != nil {
				return true, failureOutcome(
					StateNeedsReconciliation,
					"tool_failure_event_unknown", eventErr,
				), eventErr
			}
			state.PendingToolTerminal = true
		}
		outcome := freezeTerminalFailure(
			state, StateFailed, "tool_failed", runtimeErr,
		)
		return true, outcome, runtimeErr
	}
	if err := validateToolResult(result, request.CallID); err != nil {
		runtimeErr := agentError(contract.ErrorToolFailed, err.Error())
		return true, failureOutcome(
			StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
		), runtimeErr
	}
	if result.Pause != nil {
		pause := clonePause(*result.Pause)
		pause.ToolCallID = request.CallID
		result.Pause = &pause
	}
	// user_confirmation pause 发生在真正副作用之前：副作用尚未发生，不能闭合
	// durable effect。保持 effect 处于 started，由 Resume 携带 approval 重跑
	// handler 时再 Completed；期间崩溃则按 started→needs_reconciliation 收口。
	if result.Pause == nil ||
		result.Pause.Kind != PauseKindUserConfirmation {
		if err := kernel.Effects.Completed(
			context.WithoutCancel(ctx), request, result,
		); err != nil {
			runtimeErr := agentError(
				contract.ErrorInternal, "record tool completion: "+err.Error(),
			)
			return true, failureOutcome(
				StateNeedsReconciliation, "tool_completion_unknown", runtimeErr,
			), runtimeErr
		}
	}
	return kernel.finishCompletedTool(state, request, result, emitter)
}

func (kernel *Kernel) checkBeforeEffect(
	ctx context.Context,
) *contract.RuntimeError {
	if kernel == nil || kernel.BeforeEffect == nil {
		return nil
	}
	runtimeErr := kernel.BeforeEffect(ctx)
	if runtimeErr == nil {
		return nil
	}
	if executionSnapshotChanged(runtimeErr) {
		return runtimeErr
	}
	return &contract.RuntimeError{
		Code: contract.ErrorConflict, Phase: contract.PhaseProfile,
		Message: "Agent execution snapshot changed",
	}
}

func executionSnapshotChanged(
	runtimeErr *contract.RuntimeError,
) bool {
	return runtimeErr != nil &&
		runtimeErr.Code == contract.ErrorConflict &&
		runtimeErr.Phase == contract.PhaseProfile
}

func (kernel *Kernel) finishCompletedTool(
	state *LoopState,
	request ToolRequest,
	result ToolResult,
	emitter *eventEmitter,
) (bool, Outcome, *contract.RuntimeError) {
	if err := validateToolResult(result, request.CallID); err != nil {
		runtimeErr := agentError(contract.ErrorInternal, err.Error())
		return true, failureOutcome(
			StateNeedsReconciliation, "effect_recovery_failed", runtimeErr,
		), runtimeErr
	}
	if result.Pause != nil {
		pause := clonePause(*result.Pause)
		pause.ToolCallID = request.CallID
		state.Pause = &pause
		outcome := Outcome{
			State: StatePaused, StopReason: "input_required", Pause: &pause,
		}
		state.TerminalOutcome = &outcome
		if !state.PendingToolTerminal {
			if runtimeErr := emitter.emit(contract.Event{
				Type: contract.EventAgentPaused,
				Agent: &contract.AgentEvent{
					RunID: state.RunID, State: string(StatePaused),
					PauseID: pause.ID,
				},
			}); runtimeErr != nil {
				return true, failureOutcome(
					StateNeedsReconciliation,
					"pause_event_unknown", runtimeErr,
				), runtimeErr
			}
		}
		return true, outcome, nil
	}
	toolMessage := contract.Message{
		Role: contract.RoleTool, ToolCallID: request.CallID,
		Content: result.Content, IsError: result.IsError,
	}
	callMessageIndex := -1
	for index := len(state.Messages) - 1; index >= 0; index-- {
		for _, call := range state.Messages[index].ToolCalls {
			if call.ID == request.CallID {
				callMessageIndex = index
				break
			}
		}
		if callMessageIndex >= 0 {
			break
		}
	}
	if callMessageIndex < 0 {
		runtimeErr := agentError(
			contract.ErrorInternal,
			"durable tool result has no matching assistant tool call",
		)
		return true, failureOutcome(
			StateNeedsReconciliation, "effect_recovery_failed", runtimeErr,
		), runtimeErr
	}
	alreadyAppended := false
	for index := callMessageIndex + 1; index < len(state.Messages); index++ {
		message := state.Messages[index]
		if message.Role != contract.RoleTool ||
			message.ToolCallID != request.CallID {
			continue
		}
		if index != len(state.Messages)-1 ||
			!reflect.DeepEqual(message, toolMessage) ||
			alreadyAppended {
			runtimeErr := agentError(
				contract.ErrorInternal,
				"durable tool result conflicts with checkpoint messages",
			)
			return true, failureOutcome(
				StateNeedsReconciliation, "effect_recovery_failed", runtimeErr,
			), runtimeErr
		}
		alreadyAppended = true
	}
	if !alreadyAppended {
		state.Messages = append(state.Messages, toolMessage)
	}
	if !state.PendingToolTerminal {
		if runtimeErr := emitter.emit(contract.Event{
			Type: contract.EventToolCompleted,
			Tool: &contract.ToolEvent{
				CallID: request.CallID, Name: request.Name,
				IdempotencyKey: request.IdempotencyKey,
				Content:        result.Content, IsError: result.IsError,
			},
		}); runtimeErr != nil {
			return true, failureOutcome(
				StateNeedsReconciliation,
				"tool_completion_unknown", runtimeErr,
			), runtimeErr
		}
	}
	state.PendingToolCursor++
	state.RecoveredFromCheckpoint = false
	clearPendingToolEvidence(state)
	if state.PendingToolCursor >= len(state.PendingToolCalls) {
		clearPendingTools(state)
	}
	return false, Outcome{}, nil
}

func toolFailedEvent(
	request ToolRequest,
	runtimeErr *contract.RuntimeError,
) contract.Event {
	return contract.Event{
		Type: contract.EventToolFailed,
		Tool: &contract.ToolEvent{
			CallID: request.CallID, Name: request.Name,
			IdempotencyKey: request.IdempotencyKey,
		},
		Error: runtimeErr,
	}
}

func clearPendingToolEvidence(state *LoopState) {
	state.PendingCheckpointID = ""
	state.PendingEffectCheckpointID = ""
	state.PendingCheckpointCommitted = false
	state.PendingToolStarted = false
	state.PendingToolTerminal = false
}

func clearPendingTools(state *LoopState) {
	state.PendingToolCalls = nil
	state.PendingToolCursor = 0
	state.RecoveredFromCheckpoint = false
	clearPendingToolEvidence(state)
}

func (kernel *Kernel) Resume(
	ctx context.Context,
	state LoopState,
	input ResumeInput,
	sink contract.EventSink,
) (LoopState, Outcome, *contract.RuntimeError) {
	if runtimeErr := kernel.validate(&state); runtimeErr != nil {
		return state, failureOutcome(
			StateFailed, "invalid_state", runtimeErr,
		), runtimeErr
	}
	if state.Pause == nil ||
		state.TerminalOutcome == nil ||
		state.TerminalOutcome.State != StatePaused ||
		input.PauseID == "" ||
		state.Pause.ID != input.PauseID {
		runtimeErr := agentError(contract.ErrorConflict, "pause_id does not match active pause")
		return state, failureOutcome(StateFailed, "invalid_resume", runtimeErr), runtimeErr
	}
	expiryClock := kernel.now()
	if input.AcceptedAt != nil {
		expiryClock = input.AcceptedAt.UTC()
	}
	if state.Pause.ExpiresAt != nil &&
		expiryClock.After(*state.Pause.ExpiresAt) {
		runtimeErr := agentError(contract.ErrorConflict, "pause has expired")
		return state, failureOutcome(StateFailed, "pause_expired", runtimeErr), runtimeErr
	}
	if err := ValidateResumeInput(state.Pause.InputSchema, input.Input); err != nil {
		runtimeErr := agentError(contract.ErrorInvalidRequest, err.Error())
		return state, failureOutcome(StateFailed, "invalid_resume", runtimeErr), runtimeErr
	}
	// user_confirmation 的 resume 输入是"是否批准"，不是 tool 结果：必须重跑
	// handler 让被批准的副作用真正发生，再用真实结果闭合 effect。
	if state.Pause.Kind == PauseKindUserConfirmation {
		return kernel.resumeUserConfirmation(ctx, state, input, sink)
	}
	state.Messages = append(state.Messages, contract.Message{
		Role: contract.RoleTool, ToolCallID: state.Pause.ToolCallID,
		Content: string(input.Input),
	})
	state.Pause = nil
	state.TerminalOutcome = nil
	state.PendingToolCursor++
	state.RecoveredFromCheckpoint = false
	clearPendingToolEvidence(&state)
	if state.PendingToolCursor >= len(state.PendingToolCalls) {
		clearPendingTools(&state)
	}
	return kernel.Run(ctx, state, sink)
}

// resumeUserConfirmation 在 user_confirmation pause 被批准后重跑对应 tool
// handler。effect 此前已进入 started 且未闭合；这里把 approval 附进 ToolRequest
// 重新 Execute，成功后 Completed 并以真实结果继续 loop。handler 仍可再次返回
// user_confirmation pause（例如多阶段确认），此时保持 started 并重新冻结。
func (kernel *Kernel) resumeUserConfirmation(
	ctx context.Context,
	state LoopState,
	input ResumeInput,
	sink contract.EventSink,
) (LoopState, Outcome, *contract.RuntimeError) {
	emitter := eventEmitter{state: &state, sink: sink, now: kernel.now}
	call := state.PendingToolCalls[state.PendingToolCursor]
	request := ToolRequest{
		RunID: state.RunID, CallID: call.ID, Name: call.Name,
		Arguments:      append([]byte(nil), call.Arguments...),
		IdempotencyKey: toolIdempotencyKey(state.RunID, call),
		CheckpointID:   state.PendingEffectCheckpointID,
		Approval:       append([]byte(nil), input.Input...),
	}
	result, err := kernel.Tools.Execute(ctx, request)
	if err != nil {
		if ctx.Err() != nil ||
			errors.Is(err, context.Canceled) ||
			errors.Is(err, context.DeadlineExceeded) {
			runtimeErr := agentError(contract.ErrorToolFailed, err.Error())
			return state, failureOutcome(
				StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
			), runtimeErr
		}
		var knownFailure *KnownFailure
		if !errors.As(err, &knownFailure) ||
			knownFailure == nil ||
			knownFailure.RuntimeError == nil ||
			knownFailure.RuntimeError.Validate() != nil {
			runtimeErr := agentError(contract.ErrorToolFailed, err.Error())
			return state, failureOutcome(
				StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
			), runtimeErr
		}
		runtimeErr := cloneRuntimeError(knownFailure.RuntimeError)
		if recorderErr := kernel.Effects.Failed(
			context.WithoutCancel(ctx), request, runtimeErr,
		); recorderErr != nil {
			runtimeErr.Message += "; record failure: " + recorderErr.Error()
			return state, failureOutcome(
				StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
			), runtimeErr
		}
		if !state.PendingToolTerminal {
			if eventErr := emitter.emit(
				toolFailedEvent(request, runtimeErr),
			); eventErr != nil {
				return state, failureOutcome(
					StateNeedsReconciliation,
					"tool_failure_event_unknown", eventErr,
				), eventErr
			}
			state.PendingToolTerminal = true
		}
		outcome := freezeTerminalFailure(
			&state, StateFailed, "tool_failed", runtimeErr,
		)
		return state, outcome, runtimeErr
	}
	if vErr := validateToolResult(result, request.CallID); vErr != nil {
		runtimeErr := agentError(contract.ErrorToolFailed, vErr.Error())
		return state, failureOutcome(
			StateNeedsReconciliation, "tool_effect_unknown", runtimeErr,
		), runtimeErr
	}
	if result.Pause != nil {
		pause := clonePause(*result.Pause)
		pause.ToolCallID = request.CallID
		result.Pause = &pause
	}
	// 仅当 handler 这次返回非 user_confirmation 结果时才闭合 effect；若再次
	// user_confirmation 则保持 started，由 finishCompletedTool 重新冻结 pause。
	if result.Pause == nil ||
		result.Pause.Kind != PauseKindUserConfirmation {
		if err := kernel.Effects.Completed(
			context.WithoutCancel(ctx), request, result,
		); err != nil {
			runtimeErr := agentError(
				contract.ErrorInternal, "record tool completion: "+err.Error(),
			)
			return state, failureOutcome(
				StateNeedsReconciliation, "tool_completion_unknown", runtimeErr,
			), runtimeErr
		}
	}
	terminal, outcome, runtimeErr := kernel.finishCompletedTool(
		&state, request, result, &emitter,
	)
	if terminal {
		// handler 再次返回 pause（例如多阶段确认）：finishCompletedTool 已用新
		// pause 重新冻结，effect 仍处于 started，直接返回该 paused outcome。
		return state, outcome, runtimeErr
	}
	// 真实结果已闭合 effect、追加 tool message、推进游标并清证；清除旧 pause 后
	// 继续主循环。
	state.Pause = nil
	state.TerminalOutcome = nil
	return kernel.Run(ctx, state, sink)
}

func (kernel *Kernel) validate(state *LoopState) *contract.RuntimeError {
	if kernel == nil || kernel.Model == nil || kernel.Tools == nil ||
		kernel.Effects == nil {
		return agentError(contract.ErrorInternal, "agent kernel ports are required")
	}
	if err := kernel.effectiveBudget().Validate(); err != nil {
		return agentError(contract.ErrorInvalidRequest, "agent budget: "+err.Error())
	}
	if state.SchemaVersion != LoopStateSchemaVersion {
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
	if state.BaseMessageCount <= 0 ||
		state.BaseMessageCount > len(state.Messages) {
		return agentError(
			contract.ErrorInvalidRequest,
			"base_message_count must identify a non-empty message prefix",
		)
	}
	for index, message := range state.Messages {
		if err := message.Validate(); err != nil {
			return agentError(
				contract.ErrorInvalidRequest,
				fmt.Sprintf("messages[%d]: %v", index, err),
			)
		}
	}
	seen := make(map[string]struct{}, len(state.SeenToolCallIDs))
	for index, callID := range state.SeenToolCallIDs {
		if strings.TrimSpace(callID) == "" {
			return agentError(
				contract.ErrorInvalidRequest,
				fmt.Sprintf("seen_tool_call_ids[%d] is required", index),
			)
		}
		if _, exists := seen[callID]; exists {
			return agentError(
				contract.ErrorInvalidRequest,
				fmt.Sprintf("duplicate seen tool call ID %q", callID),
			)
		}
		seen[callID] = struct{}{}
	}
	if state.ToolCallCount != len(state.SeenToolCallIDs) {
		return agentError(
			contract.ErrorInvalidRequest,
			"tool_call_count does not match seen_tool_call_ids",
		)
	}
	if len(state.PendingToolCalls) == 0 {
		if state.PendingToolCursor != 0 {
			return agentError(
				contract.ErrorInvalidRequest,
				"pending_tool_cursor requires pending_tool_calls",
			)
		}
		if state.PendingEffectCheckpointID != "" {
			return agentError(
				contract.ErrorInvalidRequest,
				"pending_effect_checkpoint_id requires pending_tool_calls",
			)
		}
	} else {
		if state.PendingToolCursor < 0 ||
			state.PendingToolCursor >= len(state.PendingToolCalls) {
			return agentError(
				contract.ErrorInvalidRequest,
				"pending_tool_cursor is outside pending_tool_calls",
			)
		}
		for index, call := range state.PendingToolCalls {
			if err := call.Validate(); err != nil {
				return agentError(
					contract.ErrorInvalidRequest,
					fmt.Sprintf("pending_tool_calls[%d]: %v", index, err),
				)
			}
		}
	}
	if state.TerminalOutcome != nil {
		if len(state.PendingToolCalls) != 0 &&
			state.TerminalOutcome.State != StatePaused &&
			state.TerminalOutcome.State != StateFailed &&
			state.TerminalOutcome.State != StateCancelled {
			return agentError(
				contract.ErrorInvalidRequest,
				"terminal_outcome state cannot coexist with pending_tool_calls",
			)
		}
		switch state.TerminalOutcome.State {
		case StateCompleted:
			if state.TerminalOutcome.Message == nil ||
				state.TerminalOutcome.Pause != nil ||
				state.TerminalOutcome.Error != nil ||
				state.Pause != nil {
				return agentError(
					contract.ErrorInvalidRequest,
					"completed terminal_outcome requires only message",
				)
			}
			last := state.Messages[len(state.Messages)-1]
			if last.Role != contract.RoleAssistant ||
				!reflect.DeepEqual(last, *state.TerminalOutcome.Message) {
				return agentError(
					contract.ErrorInvalidRequest,
					"completed terminal_outcome does not match the last assistant message",
				)
			}
		case StatePaused:
			if state.TerminalOutcome.Pause == nil ||
				state.TerminalOutcome.Message != nil ||
				state.TerminalOutcome.Error != nil ||
				state.Pause == nil {
				return agentError(
					contract.ErrorInvalidRequest,
					"paused terminal_outcome requires only pause",
				)
			}
			if state.Pause.ToolCallID == "" ||
				!reflect.DeepEqual(
					state.TerminalOutcome.Pause, state.Pause,
				) {
				return agentError(
					contract.ErrorInvalidRequest,
					"paused terminal_outcome does not match pause",
				)
			}
			if err := validateToolResult(
				ToolResult{Pause: state.Pause},
				state.Pause.ToolCallID,
			); err != nil {
				return agentError(
					contract.ErrorInvalidRequest,
					"paused terminal_outcome: "+err.Error(),
				)
			}
			if len(state.PendingToolCalls) == 0 ||
				state.PendingToolCalls[state.PendingToolCursor].ID !=
					state.Pause.ToolCallID {
				return agentError(
					contract.ErrorInvalidRequest,
					"paused terminal_outcome does not match pending tool cursor",
				)
			}
		case StateFailed, StateCancelled:
			if state.TerminalOutcome.Message != nil ||
				state.TerminalOutcome.Pause != nil ||
				state.TerminalOutcome.Error == nil ||
				state.Pause != nil {
				return agentError(
					contract.ErrorInvalidRequest,
					"failed or cancelled terminal_outcome requires only error",
				)
			}
			if err := state.TerminalOutcome.Error.Validate(); err != nil {
				return agentError(
					contract.ErrorInvalidRequest,
					"terminal_outcome error: "+err.Error(),
				)
			}
			if state.TerminalOutcome.State == StateCancelled &&
				state.TerminalOutcome.Error.Code != contract.ErrorCancelled &&
				state.TerminalOutcome.Error.Code != contract.ErrorTimeout {
				return agentError(
					contract.ErrorInvalidRequest,
					"cancelled terminal_outcome requires cancelled or timeout error",
				)
			}
			if state.TerminalOutcome.State == StateFailed &&
				state.TerminalOutcome.Error.Code == contract.ErrorCancelled {
				return agentError(
					contract.ErrorInvalidRequest,
					"failed terminal_outcome cannot contain cancelled error",
				)
			}
		default:
			return agentError(
				contract.ErrorInvalidRequest,
				"unsupported terminal_outcome state",
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
	return kernel.Budget.Effective()
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
		if _, err := compileRuntimeSchema(result.Pause.InputSchema); err != nil {
			return fmt.Errorf("pause input_schema: %w", err)
		}
		if result.Pause.ToolCallID != "" && result.Pause.ToolCallID != callID {
			return fmt.Errorf("pause tool_call_id does not match")
		}
		if result.Pause.ExpiresAt != nil &&
			result.Pause.ExpiresAt.IsZero() {
			return fmt.Errorf("pause expires_at must not be zero")
		}
		return nil
	}
	if result.Content == "" {
		return fmt.Errorf("tool result content is required")
	}
	return nil
}

// ValidatePause validates the durable provider-neutral pause payload without
// evaluating any resume input.
func ValidatePause(pause Pause) error {
	return validateToolResult(
		ToolResult{Pause: &pause},
		pause.ToolCallID,
	)
}

// ValidateResumeInput validates one strict JSON value against a pause schema.
func ValidateResumeInput(schema, input json.RawMessage) error {
	compiled, err := compileRuntimeSchema(schema)
	if err != nil {
		return fmt.Errorf("pause input_schema: %w", err)
	}
	var raw json.RawMessage
	if err := strictjson.Decode(
		bytes.NewReader(input), maxToolJSONBytes, &raw,
	); err != nil {
		return fmt.Errorf("resume input: %w", err)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("resume input: %w", err)
	}
	if err := compiled.Validate(document); err != nil {
		return fmt.Errorf(
			"resume input does not match input_schema: %w", err,
		)
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

func compileRuntimeSchema(
	value json.RawMessage,
) (*jsonschema.Schema, error) {
	var raw json.RawMessage
	if err := strictjson.Decode(
		bytes.NewReader(value), maxToolJSONBytes, &raw,
	); err != nil {
		return nil, err
	}
	if err := validateJSONObject(raw); err != nil {
		return nil, err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, err
	}
	sum := sha256.Sum256(raw)
	resource := fmt.Sprintf("urn:sn-runtime:pause-schema:%x", sum[:])
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resource, document); err != nil {
		return nil, err
	}
	return compiler.Compile(resource)
}

func usageTotal(usage contract.Usage) (int64, error) {
	if usage.TotalTokens != nil {
		return *usage.TotalTokens, nil
	}
	var total int64
	if usage.InputTokens != nil {
		total += *usage.InputTokens
	}
	if usage.OutputTokens != nil {
		if *usage.OutputTokens > math.MaxInt64-total {
			return 0, fmt.Errorf(
				"model usage input_tokens + output_tokens overflows int64",
			)
		}
		total += *usage.OutputTokens
	}
	return total, nil
}

func cloneMessage(value contract.Message) contract.Message {
	result := value
	result.ToolCalls = cloneToolCalls(value.ToolCalls)
	return result
}

func cloneToolCalls(values []contract.ToolCall) []contract.ToolCall {
	result := append([]contract.ToolCall(nil), values...)
	for index := range result {
		result[index].Arguments = append([]byte(nil), result[index].Arguments...)
	}
	return result
}

func cloneToolRequest(value ToolRequest) ToolRequest {
	value.Arguments = append([]byte(nil), value.Arguments...)
	value.Approval = append([]byte(nil), value.Approval...)
	return value
}

func clonePause(value Pause) Pause {
	value.InputSchema = append([]byte(nil), value.InputSchema...)
	if value.ExpiresAt != nil {
		current := *value.ExpiresAt
		value.ExpiresAt = &current
	}
	return value
}

func cloneOutcome(value Outcome) Outcome {
	if value.Message != nil {
		message := cloneMessage(*value.Message)
		value.Message = &message
	}
	if value.Pause != nil {
		pause := clonePause(*value.Pause)
		value.Pause = &pause
	}
	if value.Error != nil {
		runtimeErr := *value.Error
		value.Error = &runtimeErr
	}
	return value
}

func cloneRuntimeError(
	value *contract.RuntimeError,
) *contract.RuntimeError {
	if value == nil {
		return nil
	}
	result := *value
	return &result
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

func freezeTerminalFailure(
	state *LoopState,
	outcomeState State,
	stopReason string,
	runtimeErr *contract.RuntimeError,
) Outcome {
	outcome := failureOutcome(outcomeState, stopReason, runtimeErr)
	frozen := cloneOutcome(outcome)
	state.TerminalOutcome = &frozen
	state.Pause = nil
	return outcome
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

func (memoryEffects) Lookup(
	context.Context,
	string,
	string,
) (EffectRecord, bool, error) {
	return EffectRecord{}, false, nil
}

func (memoryEffects) Prepared(
	_ context.Context,
	request *ToolRequest,
	state *LoopState,
) (string, error) {
	sum := sha256.Sum256([]byte(request.IdempotencyKey))
	checkpointID := "checkpoint_" + hex.EncodeToString(sum[:16])
	request.CheckpointID = checkpointID
	state.PendingEffectCheckpointID = checkpointID
	state.PendingCheckpointID = checkpointID
	return checkpointID, nil
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
