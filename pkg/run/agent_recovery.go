package run

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
)

func (executor *AgentExecutor) hasRecoveryEvidence(
	ctx context.Context,
	runID string,
) (bool, *contract.RuntimeError) {
	_, checkpointExists, err := executor.Store.LatestCheckpoint(ctx, runID)
	if err != nil {
		return false, agentRecoveryError(
			"probe durable Agent checkpoint: " + err.Error(),
		)
	}
	_, modelCallExists, err := executor.Store.LatestModelCall(ctx, runID)
	if err != nil {
		return false, agentRecoveryError(
			"probe durable Agent model call: " + err.Error(),
		)
	}
	return checkpointExists || modelCallExists, nil
}

func (executor *AgentExecutor) loadState(
	ctx context.Context,
	record Record,
	initialMessages []contract.Message,
	initialBaseMessageCount int,
	toolDefinitions []contract.ToolSpec,
) (agent.LoopState, *contract.RuntimeError) {
	modelCalls, err := executor.Store.ModelCalls(ctx, record.ID)
	if err != nil {
		return agent.LoopState{}, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseRun,
			Message: "load agent model call journal: " + err.Error(),
		}
	}
	modelCallExists := len(modelCalls) > 0
	var modelCall ModelCall
	if modelCallExists {
		modelCall = modelCalls[len(modelCalls)-1]
	}
	checkpoint, exists, err := executor.Store.LatestCheckpoint(ctx, record.ID)
	if err != nil {
		return agent.LoopState{}, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseRun,
			Message: "load agent checkpoint: " + err.Error(),
		}
	}
	if exists {
		var state agent.LoopState
		if err := strictjson.Decode(
			bytes.NewReader(checkpoint.State), 4<<20, &state,
		); err != nil {
			return agent.LoopState{}, &contract.RuntimeError{
				Code: contract.ErrorInternal, Phase: contract.PhaseRun,
				Message: "decode agent checkpoint: " + err.Error(),
			}
		}
		if checkpoint.RunID != record.ID ||
			state.RunID != record.ID ||
			state.ModelProfile != record.Request.ProfileID {
			return agent.LoopState{}, agentRecoveryError(
				"agent checkpoint correlation does not match the durable Run",
			)
		}
		if state.SchemaVersion != agent.LoopStateSchemaVersion {
			return agent.LoopState{}, agentRecoveryError(
				"agent checkpoint has an unsupported loop state schema",
			)
		}
		if state.BaseMessageCount != initialBaseMessageCount {
			return agent.LoopState{}, agentRecoveryError(
				"agent checkpoint base message boundary does not match its execution",
			)
		}
		if checkpoint.Sequence != state.NextEventSequence {
			return agent.LoopState{}, agentRecoveryError(
				"agent checkpoint sequence does not match loop state",
			)
		}
		state, runtimeErr := executor.recoverDurableModelTerminal(
			record, state, modelCalls, true,
			initialMessages, initialBaseMessageCount, toolDefinitions,
		)
		if runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if runtimeErr := validateRecoveredLoopState(state); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if runtimeErr := validateModelCallJournal(
			modelCalls, state,
		); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if runtimeErr := validateModelRequestJournal(
			record, state, modelCalls,
		); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if runtimeErr := validateRecoveredBudget(
			record.Request.AgentBudget, state, modelCalls,
		); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if modelCallExists {
			if modelCall.RunID != record.ID {
				return agent.LoopState{}, agentRecoveryError(
					"latest agent model call does not belong to the durable Run",
				)
			}
			if runtimeErr := validateModelResultEvidence(
				modelCall, state,
			); runtimeErr != nil {
				return agent.LoopState{}, runtimeErr
			}
		}
		latestSequence, err := executor.Store.LatestEventSequence(
			ctx, record.ID,
		)
		if err != nil {
			return agent.LoopState{}, &contract.RuntimeError{
				Code: contract.ErrorInternal, Phase: contract.PhaseRun,
				Message: "load durable agent event sequence: " + err.Error(),
			}
		}
		if latestSequence < checkpoint.Sequence {
			return agent.LoopState{}, agentRecoveryError(
				"agent checkpoint is ahead of durable events",
			)
		}
		state.NextEventSequence = latestSequence
		if runtimeErr := executor.validateDurableAgentEventJournal(
			ctx, record, state, modelCalls,
		); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if runtimeErr := executor.validateToolEffectJournal(
			ctx, record, state, modelCalls,
		); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if state.PendingToolCursor < len(state.PendingToolCalls) &&
			state.PendingEffectCheckpointID != "" {
			if runtimeErr := executor.recoverPendingToolEvidence(
				ctx, record, &state,
			); runtimeErr != nil {
				return agent.LoopState{}, runtimeErr
			}
		}
		return state, nil
	}
	messages := initialMessages
	if len(messages) == 0 {
		messages = []contract.Message{{
			Role: contract.RoleUser, Content: record.Request.Input,
		}}
	}
	if initialBaseMessageCount <= 0 ||
		initialBaseMessageCount > len(messages) {
		return agent.LoopState{}, agentRecoveryError(
			"initial Agent base message boundary is invalid",
		)
	}
	state := agent.LoopState{
		SchemaVersion: agent.LoopStateSchemaVersion,
		RunID:         record.ID, ModelProfile: record.Request.ProfileID,
		Messages: messages, BaseMessageCount: initialBaseMessageCount,
	}
	if !modelCallExists {
		return state, nil
	}
	state, runtimeErr := executor.recoverDurableModelTerminal(
		record, state, modelCalls, false,
		initialMessages, initialBaseMessageCount, toolDefinitions,
	)
	if runtimeErr != nil {
		return agent.LoopState{}, runtimeErr
	}
	if runtimeErr := validateRecoveredLoopState(state); runtimeErr != nil {
		return agent.LoopState{}, runtimeErr
	}
	for _, validator := range []func() *contract.RuntimeError{
		func() *contract.RuntimeError {
			return validateModelCallJournal(modelCalls, state)
		},
		func() *contract.RuntimeError {
			return validateModelRequestJournal(record, state, modelCalls)
		},
		func() *contract.RuntimeError {
			return validateRecoveredBudget(
				record.Request.AgentBudget, state, modelCalls,
			)
		},
	} {
		if runtimeErr := validator(); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
	}
	if runtimeErr := validateModelResultEvidence(
		modelCall, state,
	); runtimeErr != nil {
		return agent.LoopState{}, runtimeErr
	}
	latestSequence, err := executor.Store.LatestEventSequence(ctx, record.ID)
	if err != nil {
		return agent.LoopState{}, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseRun,
			Message: "load durable agent event sequence: " + err.Error(),
		}
	}
	state.NextEventSequence = latestSequence
	if runtimeErr := executor.validateDurableAgentEventJournal(
		ctx, record, state, modelCalls,
	); runtimeErr != nil {
		return agent.LoopState{}, runtimeErr
	}
	if runtimeErr := executor.validateToolEffectJournal(
		ctx, record, state, modelCalls,
	); runtimeErr != nil {
		return agent.LoopState{}, runtimeErr
	}
	return state, nil
}

// recoverDurableModelTerminal closes only the one model crash window whose
// outcome is independently proven by the append-only model-call row. A
// running call or a completed call ahead of the checkpoint remains ambiguous.
func (executor *AgentExecutor) recoverDurableModelTerminal(
	record Record,
	state agent.LoopState,
	modelCalls []ModelCall,
	hasCheckpoint bool,
	initialMessages []contract.Message,
	initialBaseMessageCount int,
	toolDefinitions []contract.ToolSpec,
) (agent.LoopState, *contract.RuntimeError) {
	if len(modelCalls) == 0 {
		return state, nil
	}
	last := modelCalls[len(modelCalls)-1]
	if last.State != "failed" && last.State != "cancelled" {
		if !hasCheckpoint || len(modelCalls) > state.Round {
			return agent.LoopState{}, agentRecoveryError(
				fmt.Sprintf(
					"agent model call sequence=%d state=%s is ahead of a durable terminal checkpoint",
					last.Sequence, last.State,
				),
			)
		}
		return state, nil
	}
	if last.Sequence != state.Round+1 ||
		len(modelCalls) != last.Sequence ||
		last.Error == nil ||
		len(last.Result) != 0 ||
		last.ResultDigest != "" {
		return agent.LoopState{}, agentRecoveryError(
			"durable terminal model call is not the exact next checkpoint round",
		)
	}
	if err := last.Error.Validate(); err != nil {
		return agent.LoopState{}, agentRecoveryError(
			"durable terminal model call error is invalid: " + err.Error(),
		)
	}
	outcome := agent.Outcome{
		State: agent.StateFailed, StopReason: "model_failed",
		Error: cloneRunRuntimeError(last.Error),
	}
	if last.State == "cancelled" {
		if last.Error.Code != contract.ErrorCancelled &&
			last.Error.Code != contract.ErrorTimeout {
			return agent.LoopState{}, agentRecoveryError(
				"cancelled model call has a non-cancellation error",
			)
		}
		outcome.State = agent.StateCancelled
		outcome.StopReason = "cancelled"
	} else if last.Error.Code == contract.ErrorCancelled ||
		last.Error.Code == contract.ErrorTimeout {
		return agent.LoopState{}, agentRecoveryError(
			"failed model call contains a cancellation error",
		)
	}
	var frozen contract.GenerateRequest
	if err := strictjson.DecodeObject(
		bytes.NewReader(last.Request), 4<<20, &frozen,
	); err != nil {
		return agent.LoopState{}, agentRecoveryError(
			"decode terminal frozen model request: " + err.Error(),
		)
	}
	if err := frozen.Validate(); err != nil {
		return agent.LoopState{}, agentRecoveryError(
			"terminal frozen model request is invalid: " + err.Error(),
		)
	}
	requestJSON, err := json.Marshal(frozen)
	if err != nil {
		return agent.LoopState{}, agentRecoveryError(
			"encode terminal frozen model request: " + err.Error(),
		)
	}
	sum := sha256.Sum256(requestJSON)
	if last.RequestDigest !=
		"sha256:"+hex.EncodeToString(sum[:]) {
		return agent.LoopState{}, agentRecoveryError(
			"terminal frozen model request digest does not match",
		)
	}
	if frozen.ModelProfile != record.Request.ProfileID ||
		frozen.Input.System != "" ||
		!reflect.DeepEqual(
			frozen.Input.Options, contract.GenerateOptions{},
		) ||
		!reflect.DeepEqual(
			frozen.Input.Trace,
			contract.TraceContext{Labels: map[string]string{
				"run_id": record.ID,
			}},
		) ||
		!equalAgentToolDefinitions(
			frozen.Input.Tools, toolDefinitions,
		) {
		return agent.LoopState{}, agentRecoveryError(
			"terminal frozen model request does not match the Agent execution adapter",
		)
	}
	baseMessages := initialMessages
	if len(baseMessages) == 0 {
		baseMessages = []contract.Message{{
			Role: contract.RoleUser, Content: record.Request.Input,
		}}
	}
	if initialBaseMessageCount <= 0 ||
		initialBaseMessageCount > len(baseMessages) ||
		initialBaseMessageCount > len(frozen.Input.Messages) ||
		state.BaseMessageCount != initialBaseMessageCount {
		return agent.LoopState{}, agentRecoveryError(
			"terminal model request has an invalid base message boundary",
		)
	}
	for index := 0; index < initialBaseMessageCount; index++ {
		if !reflect.DeepEqual(
			baseMessages[index], frozen.Input.Messages[index],
		) {
			return agent.LoopState{}, agentRecoveryError(
				"terminal model request changed the Agent base messages",
			)
		}
	}
	if state.TerminalOutcome != nil &&
		state.TerminalOutcome.State != agent.StatePaused {
		if !reflect.DeepEqual(state.TerminalOutcome, &outcome) ||
			!reflect.DeepEqual(state.Messages, frozen.Input.Messages) {
			return agent.LoopState{}, agentRecoveryError(
				"terminal model call conflicts with the durable Agent checkpoint",
			)
		}
		return state, nil
	}
	state.Messages = append(
		[]contract.Message(nil), frozen.Input.Messages...,
	)
	state.Pause = nil
	state.PendingToolCalls = nil
	state.PendingToolCursor = 0
	state.PendingEffectCheckpointID = ""
	state.PendingCheckpointID = ""
	state.PendingCheckpointCommitted = false
	state.PendingToolStarted = false
	state.PendingToolTerminal = false
	state.RecoveredFromCheckpoint = false
	state.TerminalOutcome = &outcome
	return state, nil
}

func (executor *AgentExecutor) recoverPendingToolEvidence(
	ctx context.Context,
	record Record,
	state *agent.LoopState,
) *contract.RuntimeError {
	checkpointID := state.PendingEffectCheckpointID
	call := state.PendingToolCalls[state.PendingToolCursor]
	effect, effectExists, err := (&durableEffects{
		store: executor.Store,
	}).Lookup(ctx, record.ID, call.ID)
	if err != nil {
		return agentRecoveryError(
			"load durable Agent tool effect: " + err.Error(),
		)
	}
	if !effectExists {
		return agentRecoveryError(
			"recovered pending tool call has no durable effect",
		)
	}
	request := effect.Request
	if request.RunID != record.ID ||
		request.CallID != call.ID ||
		request.Name != call.Name ||
		!bytes.Equal(request.Arguments, call.Arguments) ||
		request.CheckpointID != checkpointID ||
		request.IdempotencyKey != agentToolIdempotencyKey(record.ID, call) {
		return agentRecoveryError(
			"durable Agent tool effect does not match its preparation checkpoint",
		)
	}
	position := agentToolCallPosition{
		call: call, round: state.Round, callIndex: state.PendingToolCursor,
		roundCalls: state.PendingToolCalls,
	}
	original, preparedState, runtimeErr :=
		executor.validateToolPreparationCheckpoint(
			ctx, record, *state, position, request,
		)
	if runtimeErr != nil {
		return runtimeErr
	}
	var checkpointEvents, startedEvents, completedEvents int
	var failedEvents, pausedEvents int
	var checkpointSequence, startedSequence, terminalSequence uint64
	var afterSequence uint64
	for {
		events, err := executor.Store.Events(
			ctx, record.ID, afterSequence, 1000,
		)
		if err != nil {
			return agentRecoveryError(
				"load durable agent recovery events: " + err.Error(),
			)
		}
		for _, event := range events {
			if event.Tool != nil {
				if runtimeErr := executor.validateDurableToolEvent(
					ctx, record.ID, event,
				); runtimeErr != nil {
					return runtimeErr
				}
			}
			switch event.Type {
			case contract.EventCheckpointCommitted:
				if event.Checkpoint != nil &&
					event.Checkpoint.CheckpointID == checkpointID {
					checkpointEvents++
					checkpointSequence = event.Sequence
				}
			case contract.EventToolStarted:
				if event.Tool != nil && event.Tool.CallID == call.ID {
					startedEvents++
					startedSequence = event.Sequence
				}
			case contract.EventToolCompleted:
				if event.Tool != nil && event.Tool.CallID == call.ID {
					completedEvents++
					terminalSequence = event.Sequence
				}
			case contract.EventToolFailed:
				if event.Tool != nil && event.Tool.CallID == call.ID {
					failedEvents++
					terminalSequence = event.Sequence
				}
			case contract.EventAgentPaused:
				if event.Agent != nil &&
					event.Agent.RunID == record.ID &&
					event.Sequence > original.Sequence &&
					effect.Result != nil &&
					effect.Result.Pause != nil &&
					event.Agent.PauseID == effect.Result.Pause.ID &&
					effect.Result.Pause.ToolCallID == call.ID {
					pausedEvents++
					terminalSequence = event.Sequence
				} else if event.Agent != nil &&
					event.Agent.RunID == record.ID &&
					event.Sequence > original.Sequence {
					return agentRecoveryError(
						"agent.paused event does not match durable tool pause",
					)
				}
			}
		}
		if len(events) < 1000 {
			break
		}
		afterSequence = events[len(events)-1].Sequence
	}
	preStartSnapshotFailure :=
		effect.State == "failed" &&
			isExecutionSnapshotChangedError(effect.Error) &&
			startedEvents == 0
	if checkpointEvents > 1 || startedEvents > 1 ||
		completedEvents > 1 || failedEvents > 1 || pausedEvents > 1 ||
		completedEvents+failedEvents+pausedEvents > 1 ||
		checkpointEvents == 1 &&
			checkpointSequence != original.Sequence+1 ||
		startedEvents > 0 &&
			(checkpointEvents != 1 ||
				checkpointSequence >= startedSequence) ||
		terminalSequence > 0 &&
			(preStartSnapshotFailure &&
				(startedEvents != 0 ||
					checkpointSequence >= terminalSequence) ||
				!preStartSnapshotFailure &&
					(startedEvents != 1 ||
						startedSequence >= terminalSequence)) {
		return agentRecoveryError(
			"durable Agent tool lifecycle events are inconsistent",
		)
	}
	switch effect.State {
	case "prepared":
		if effect.Result != nil || effect.Error != nil ||
			startedEvents != 0 || completedEvents != 0 ||
			failedEvents != 0 {
			return agentRecoveryError(
				"prepared Agent tool effect has terminal evidence",
			)
		}
	case "started":
		if effect.Result != nil || effect.Error != nil ||
			checkpointEvents != 1 ||
			completedEvents != 0 || failedEvents != 0 ||
			pausedEvents != 0 {
			return agentRecoveryError(
				"started Agent tool effect has inconsistent terminal evidence",
			)
		}
	case "completed":
		if effect.Result == nil || effect.Error != nil ||
			checkpointEvents != 1 || startedEvents != 1 ||
			failedEvents != 0 {
			return agentRecoveryError(
				"completed Agent tool effect has inconsistent evidence",
			)
		}
		if effect.Result.Pause == nil && pausedEvents != 0 ||
			effect.Result.Pause != nil && completedEvents != 0 {
			return agentRecoveryError(
				"completed Agent tool effect terminal event type is inconsistent",
			)
		}
	case "failed":
		if effect.Result != nil || effect.Error == nil ||
			checkpointEvents != 1 ||
			completedEvents != 0 || pausedEvents != 0 ||
			failedEvents > 1 ||
			state.TerminalOutcome != nil && failedEvents != 1 ||
			preStartSnapshotFailure && startedEvents != 0 ||
			!preStartSnapshotFailure && startedEvents != 1 {
			return agentRecoveryError(
				"failed Agent tool effect has inconsistent evidence",
			)
		}
		if state.TerminalOutcome != nil {
			wantStopReason := "tool_failed"
			if preStartSnapshotFailure {
				wantStopReason = "execution_snapshot_changed"
			}
			if state.TerminalOutcome.State != agent.StateFailed ||
				state.TerminalOutcome.StopReason != wantStopReason ||
				!reflect.DeepEqual(
					state.TerminalOutcome.Error, effect.Error,
				) ||
				failedEvents != 1 {
				return agentRecoveryError(
					"known terminal tool failure is not backed by one exact tool.failed event",
				)
			}
		}
	default:
		return agentRecoveryError(
			"durable Agent tool effect has an unsupported state",
		)
	}
	if runtimeErr := executor.validateRecoveredPendingMessages(
		ctx, record, preparedState, *state, effect,
	); runtimeErr != nil {
		return runtimeErr
	}
	state.PendingCheckpointID = checkpointID
	state.PendingCheckpointCommitted = checkpointEvents == 1
	state.PendingToolStarted = startedEvents == 1
	state.PendingToolTerminal =
		completedEvents == 1 || failedEvents == 1 ||
			pausedEvents == 1
	state.RecoveredFromCheckpoint = true
	return nil
}

func agentRecoveryError(message string) *contract.RuntimeError {
	return &contract.RuntimeError{
		Code: contract.ErrorConflict, Phase: contract.PhaseRun,
		Message: message,
	}
}

func cloneRunRuntimeError(
	value *contract.RuntimeError,
) *contract.RuntimeError {
	if value == nil {
		return nil
	}
	current := *value
	return &current
}
