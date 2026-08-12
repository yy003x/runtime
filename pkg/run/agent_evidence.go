package run

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
)

func equalAgentToolDefinitions(
	left, right []contract.ToolSpec,
) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !reflect.DeepEqual(left[index], right[index]) {
			return false
		}
	}
	return true
}

type agentLifecycleEvidence struct {
	checkpoint map[string][]uint64
	started    map[string][]uint64
	completed  map[string][]uint64
	failed     map[string][]uint64
	paused     map[string][]uint64
}

type agentToolCallPosition struct {
	call       contract.ToolCall
	round      int
	callIndex  int
	roundCalls []contract.ToolCall
}

func (executor *AgentExecutor) validateToolEffectJournal(
	ctx context.Context,
	record Record,
	state agent.LoopState,
	modelCalls []ModelCall,
) *contract.RuntimeError {
	view, runtimeErr := partitionModelCallJournal(modelCalls, state)
	if runtimeErr != nil {
		return runtimeErr
	}
	callByID := make(map[string]agentToolCallPosition)
	for roundIndex, modelCall := range view.completed {
		result, runtimeErr := decodeDurableModelResult(modelCall)
		if runtimeErr != nil {
			return runtimeErr
		}
		for callIndex, call := range result.Message.ToolCalls {
			if _, exists := callByID[call.ID]; exists {
				isCurrentDuplicate :=
					state.TerminalOutcome != nil &&
						state.TerminalOutcome.State == agent.StateFailed &&
						state.TerminalOutcome.StopReason ==
							"duplicate_tool_call" &&
						roundIndex == len(view.completed)-1 &&
						len(state.PendingToolCalls) > 0 &&
						callIndex == state.PendingToolCursor &&
						state.PendingToolCalls[state.PendingToolCursor].ID ==
							call.ID
				if isCurrentDuplicate {
					continue
				}
				return agentRecoveryError(
					"Agent model results contain an unsupported duplicate tool call",
				)
			}
			callByID[call.ID] = agentToolCallPosition{
				call: call, round: roundIndex + 1, callIndex: callIndex,
				roundCalls: result.Message.ToolCalls,
			}
		}
	}
	effects, err := executor.Store.ToolEffects(ctx, record.ID)
	if err != nil {
		return agentRecoveryError(
			"load Agent tool effect journal: " + err.Error(),
		)
	}
	if len(effects) != len(state.SeenToolCallIDs) {
		return agentRecoveryError(
			"Agent tool effects do not match seen tool calls",
		)
	}
	effectByID := make(map[string]agent.EffectRecord, len(effects))
	checkpointByCallID := make(map[string]Checkpoint, len(effects))
	for _, indexed := range effects {
		position, seen := callByID[indexed.CallID]
		if !seen {
			return agentRecoveryError(
				"Agent tool effect is absent from model results",
			)
		}
		effect, exists, err := (&durableEffects{
			store: executor.Store,
		}).Lookup(ctx, record.ID, indexed.CallID)
		if err != nil {
			return agentRecoveryError(
				"decode Agent tool effect journal: " + err.Error(),
			)
		}
		if !exists {
			return agentRecoveryError(
				"Agent tool effect disappeared from its journal",
			)
		}
		call := position.call
		if effect.Request.RunID != record.ID ||
			effect.Request.CallID != call.ID ||
			effect.Request.Name != call.Name ||
			!bytes.Equal(effect.Request.Arguments, call.Arguments) ||
			effect.Request.IdempotencyKey !=
				agentToolIdempotencyKey(record.ID, call) {
			return agentRecoveryError(
				"Agent tool effect request does not match model result",
			)
		}
		if err := identity.Validate(
			effect.Request.CheckpointID, "checkpoint",
		); err != nil {
			return agentRecoveryError(
				"Agent tool effect has no valid preparation checkpoint",
			)
		}
		checkpoint, _, runtimeErr := executor.validateToolPreparationCheckpoint(
			ctx, record, state, position, effect.Request,
		)
		if runtimeErr != nil {
			return runtimeErr
		}
		effectByID[indexed.CallID] = effect
		checkpointByCallID[indexed.CallID] = checkpoint
	}
	for _, callID := range state.SeenToolCallIDs {
		if _, exists := effectByID[callID]; !exists {
			return agentRecoveryError(
				"seen Agent tool call has no durable effect",
			)
		}
	}
	evidence, runtimeErr := executor.loadAgentLifecycleEvidence(
		ctx, record.ID,
	)
	if runtimeErr != nil {
		return runtimeErr
	}
	checkpointOwner := make(map[string]string, len(effectByID))
	for callID, effect := range effectByID {
		checkpointID := effect.Request.CheckpointID
		if previous, exists := checkpointOwner[checkpointID]; exists &&
			previous != callID {
			return agentRecoveryError(
				"Agent tool effects reuse a preparation checkpoint",
			)
		}
		checkpointOwner[checkpointID] = callID
	}
	for checkpointID := range evidence.checkpoint {
		if _, exists := checkpointOwner[checkpointID]; !exists {
			return agentRecoveryError(
				"checkpoint.committed event has no matching Agent tool effect",
			)
		}
	}
	currentCallID := ""
	hasPendingEffect := false
	if state.PendingToolCursor < len(state.PendingToolCalls) {
		currentCallID =
			state.PendingToolCalls[state.PendingToolCursor].ID
		hasPendingEffect = state.PendingEffectCheckpointID != ""
	} else if state.TerminalOutcome != nil &&
		state.TerminalOutcome.State == agent.StatePaused &&
		state.Pause != nil {
		currentCallID = state.Pause.ToolCallID
	}
	for _, callID := range state.SeenToolCallIDs {
		effect := effectByID[callID]
		if callID == currentCallID &&
			hasPendingEffect {
			continue
		}
		if effect.State != "completed" ||
			effect.Result == nil || effect.Error != nil {
			return agentRecoveryError(
				"historical Agent tool call is not durably completed",
			)
		}
		checkpoints := evidence.checkpoint[effect.Request.CheckpointID]
		started := evidence.started[callID]
		checkpoint := checkpointByCallID[callID]
		if len(checkpoints) != 1 || len(started) != 1 ||
			checkpoints[0] != checkpoint.Sequence+1 ||
			checkpoints[0] >= started[0] {
			return agentRecoveryError(
				"historical Agent tool checkpoint/start evidence is inconsistent",
			)
		}
		if effect.Result.Pause == nil {
			completed := evidence.completed[callID]
			if len(completed) != 1 ||
				started[0] >= completed[0] ||
				len(evidence.failed[callID]) != 0 {
				return agentRecoveryError(
					"historical Agent tool completion evidence is inconsistent",
				)
			}
			continue
		}
		pause := effect.Result.Pause
		if pause.ToolCallID != callID {
			return agentRecoveryError(
				"historical Agent pause is not bound to its tool call",
			)
		}
		paused := evidence.paused[pause.ID]
		if len(paused) != 1 ||
			started[0] >= paused[0] ||
			len(evidence.completed[callID]) != 0 ||
			len(evidence.failed[callID]) != 0 {
			return agentRecoveryError(
				"historical Agent tool pause evidence is inconsistent",
			)
		}
	}
	if runtimeErr := validatePauseEventClosure(
		evidence, effectByID, currentCallID,
		hasPendingEffect,
	); runtimeErr != nil {
		return runtimeErr
	}
	resumes, err := executor.Store.Resumes(ctx, record.ID)
	if err != nil {
		return agentRecoveryError(
			"load Agent resume journal: " + err.Error(),
		)
	}
	if runtimeErr := validateAgentMessageJournal(
		record, state, view.completed, effectByID, resumes,
	); runtimeErr != nil {
		return runtimeErr
	}
	return nil
}

func (executor *AgentExecutor) validateToolPreparationCheckpoint(
	ctx context.Context,
	record Record,
	recoveredState agent.LoopState,
	position agentToolCallPosition,
	request agent.ToolRequest,
) (Checkpoint, agent.LoopState, *contract.RuntimeError) {
	checkpoint, exists, err := executor.Store.Checkpoint(
		ctx, request.CheckpointID,
	)
	if err != nil {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"load Agent tool preparation checkpoint: " + err.Error(),
		)
	}
	if !exists || checkpoint.ID != request.CheckpointID ||
		checkpoint.RunID != record.ID {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"Agent tool effect preparation checkpoint is missing or mismatched",
		)
	}
	var prepared agent.LoopState
	if err := strictjson.Decode(
		bytes.NewReader(checkpoint.State), 4<<20, &prepared,
	); err != nil {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"decode Agent tool preparation checkpoint: " + err.Error(),
		)
	}
	if prepared.SchemaVersion != agent.LoopStateSchemaVersion {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"Agent tool preparation checkpoint uses an unsupported schema",
		)
	}
	if runtimeErr := validateRecoveredLoopState(prepared); runtimeErr != nil {
		return Checkpoint{}, agent.LoopState{}, runtimeErr
	}
	if checkpoint.Sequence != prepared.NextEventSequence ||
		prepared.RunID != record.ID ||
		prepared.ModelProfile != record.Request.ProfileID ||
		prepared.BaseMessageCount != recoveredState.BaseMessageCount ||
		prepared.Round != position.round ||
		prepared.PendingToolCursor != position.callIndex ||
		prepared.PendingEffectCheckpointID != checkpoint.ID ||
		!reflect.DeepEqual(
			prepared.PendingToolCalls, position.roundCalls,
		) ||
		!reflect.DeepEqual(
			prepared.PendingToolCalls[position.callIndex],
			position.call,
		) {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"Agent tool preparation checkpoint does not match its model call",
		)
	}
	if len(prepared.SeenToolCallIDs) == 0 ||
		prepared.SeenToolCallIDs[len(prepared.SeenToolCallIDs)-1] !=
			position.call.ID ||
		len(prepared.SeenToolCallIDs) > len(recoveredState.SeenToolCallIDs) ||
		!equalStringSlice(
			prepared.SeenToolCallIDs,
			recoveredState.SeenToolCallIDs[:len(prepared.SeenToolCallIDs)],
		) {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"Agent tool preparation checkpoint seen calls are inconsistent",
		)
	}
	if len(prepared.Messages) > len(recoveredState.Messages) ||
		!reflect.DeepEqual(
			prepared.Messages,
			recoveredState.Messages[:len(prepared.Messages)],
		) {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"Agent tool preparation checkpoint is not a message prefix",
		)
	}
	assistantIndex := -1
	for index := len(prepared.Messages) - 1; index >= 0; index-- {
		if prepared.Messages[index].Role == contract.RoleAssistant {
			assistantIndex = index
			break
		}
	}
	if assistantIndex < prepared.BaseMessageCount ||
		!reflect.DeepEqual(
			prepared.Messages[assistantIndex].ToolCalls,
			position.roundCalls,
		) {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"Agent tool preparation checkpoint has no matching assistant call",
		)
	}
	return checkpoint, prepared, nil
}

func validatePauseEventClosure(
	evidence agentLifecycleEvidence,
	effects map[string]agent.EffectRecord,
	currentCallID string,
	hasPendingCall bool,
) *contract.RuntimeError {
	expected := make(map[string]string)
	for callID, effect := range effects {
		if effect.State != "completed" || effect.Result == nil ||
			effect.Result.Pause == nil {
			continue
		}
		pause := effect.Result.Pause
		if pause.ToolCallID != callID {
			return agentRecoveryError(
				"durable Agent pause is not bound to its tool call",
			)
		}
		if previous, exists := expected[pause.ID]; exists &&
			previous != callID {
			return agentRecoveryError(
				"durable Agent pauses reuse a pause_id",
			)
		}
		expected[pause.ID] = callID
	}
	for pauseID, sequences := range evidence.paused {
		if _, exists := expected[pauseID]; !exists ||
			len(sequences) != 1 {
			return agentRecoveryError(
				"agent.paused event has no unique durable pause",
			)
		}
	}
	for pauseID, callID := range expected {
		count := len(evidence.paused[pauseID])
		if callID == currentCallID && hasPendingCall {
			if count > 1 {
				return agentRecoveryError(
					"pending Agent pause has duplicate terminal events",
				)
			}
			continue
		}
		if count != 1 {
			return agentRecoveryError(
				"durable Agent pause has no terminal event",
			)
		}
	}
	return nil
}

func validateAgentMessageJournal(
	record Record,
	state agent.LoopState,
	modelCalls []ModelCall,
	effects map[string]agent.EffectRecord,
	resumes []ResumeRecord,
) *contract.RuntimeError {
	if state.BaseMessageCount <= 0 ||
		state.BaseMessageCount > len(state.Messages) {
		return agentRecoveryError(
			"Agent base message boundary is invalid",
		)
	}
	if record.Request.SessionID == "" {
		base := contract.Message{
			Role: contract.RoleUser, Content: record.Request.Input,
		}
		if state.BaseMessageCount != 1 ||
			!reflect.DeepEqual(state.Messages[0], base) {
			return agentRecoveryError(
				"Agent base messages do not match the Run input",
			)
		}
	}
	expected := append(
		[]contract.Message(nil),
		state.Messages[:state.BaseMessageCount]...,
	)
	currentCallID := ""
	if state.PendingToolCursor < len(state.PendingToolCalls) {
		currentCallID =
			state.PendingToolCalls[state.PendingToolCursor].ID
	} else if state.TerminalOutcome != nil &&
		state.TerminalOutcome.State == agent.StatePaused &&
		state.Pause != nil {
		currentCallID = state.Pause.ToolCallID
	}
	resumeIndex := 0
	pauseIDs := make(map[string]struct{})
	seenIndex := 0
	for _, modelCall := range modelCalls {
		result, runtimeErr := decodeDurableModelResult(modelCall)
		if runtimeErr != nil {
			return runtimeErr
		}
		expected = append(expected, result.Message)
		for _, call := range result.Message.ToolCalls {
			if seenIndex >= len(state.SeenToolCallIDs) ||
				state.SeenToolCallIDs[seenIndex] != call.ID {
				continue
			}
			effect, exists := effects[call.ID]
			if !exists {
				return agentRecoveryError(
					"Agent message journal tool call has no durable effect",
				)
			}
			isPending := len(state.PendingToolCalls) > 0 &&
				state.PendingEffectCheckpointID != "" &&
				call.ID == currentCallID
			isActivePause :=
				state.TerminalOutcome != nil &&
					state.TerminalOutcome.State == agent.StatePaused &&
					call.ID == currentCallID
			if effect.State == "completed" && effect.Result != nil {
				if effect.Result.Pause != nil {
					pause := effect.Result.Pause
					if pause.ToolCallID != call.ID {
						return agentRecoveryError(
							"Agent pause message is not bound to its tool call",
						)
					}
					if _, exists := pauseIDs[pause.ID]; exists {
						return agentRecoveryError(
							"durable Agent pauses reuse a pause_id",
						)
					}
					pauseIDs[pause.ID] = struct{}{}
					var resumeInput *agent.ResumeInput
					var resumeRecord *ResumeRecord
					if resumeIndex < len(resumes) {
						candidate, err := decodeAgentResumeInput(
							resumes[resumeIndex].Input,
						)
						if err != nil {
							return agentRecoveryError(
								"decode durable Agent resume journal: " + err.Error(),
							)
						}
						if candidate.PauseID == pause.ID {
							if err := agent.ValidateResumeInput(
								pause.InputSchema, candidate.Input,
							); err != nil {
								return agentRecoveryError(
									"Agent resume journal input is invalid: " + err.Error(),
								)
							}
							if pause.ExpiresAt != nil &&
								resumes[resumeIndex].AcceptedAt.After(
									*pause.ExpiresAt,
								) {
								return agentRecoveryError(
									"Agent resume journal was accepted after pause expiry",
								)
							}
							resumeInput = &candidate
							resumeRecord = &resumes[resumeIndex]
							resumeIndex++
						}
					}
					if !isPending && !isActivePause {
						if resumeInput == nil {
							return agentRecoveryError(
								"resumed Agent pause has no durable resume journal entry",
							)
						}
						if len(expected) >= len(state.Messages) {
							return agentRecoveryError(
								"resumed Agent pause has no tool message",
							)
						}
						message := state.Messages[len(expected)]
						if message.Role != contract.RoleTool ||
							message.ToolCallID != call.ID ||
							message.IsError ||
							len(message.ToolCalls) != 0 ||
							agent.ValidateResumeInput(
								pause.InputSchema,
								json.RawMessage(message.Content),
							) != nil {
							return agentRecoveryError(
								"resumed Agent pause tool message is invalid",
							)
						}
						if !bytes.Equal(
							resumeInput.Input,
							[]byte(message.Content),
						) {
							return agentRecoveryError(
								"Agent resume message does not match its journal input",
							)
						}
						expected = append(expected, message)
					} else if isActivePause && resumeRecord == nil {
						// The current pause has not been accepted yet.
					}
				} else {
					message := contract.Message{
						Role: contract.RoleTool, ToolCallID: call.ID,
						Content: effect.Result.Content,
						IsError: effect.Result.IsError,
					}
					if isPending {
						if len(expected) < len(state.Messages) &&
							reflect.DeepEqual(
								state.Messages[len(expected)], message,
							) {
							expected = append(expected, message)
						}
					} else {
						expected = append(expected, message)
					}
				}
			}
			seenIndex++
		}
	}
	if seenIndex != len(state.SeenToolCallIDs) ||
		!reflect.DeepEqual(expected, state.Messages) {
		return agentRecoveryError(
			"Agent checkpoint messages do not match the durable execution journal",
		)
	}
	if resumeIndex != len(resumes) {
		return agentRecoveryError(
			"Agent resume journal has no one-to-one durable pause mapping",
		)
	}
	if len(resumes) == 0 {
		if len(record.Request.Resume) != 0 ||
			record.ResumeAcceptedAt != nil {
			return agentRecoveryError(
				"Agent Run latest resume fields have no journal entry",
			)
		}
	} else {
		latest := resumes[len(resumes)-1]
		if !bytes.Equal(record.Request.Resume, latest.Input) ||
			record.ResumeAcceptedAt == nil ||
			!record.ResumeAcceptedAt.Equal(latest.AcceptedAt) {
			return agentRecoveryError(
				"Agent Run latest resume fields do not match the append-only journal",
			)
		}
	}
	return nil
}

func (executor *AgentExecutor) loadAgentLifecycleEvidence(
	ctx context.Context,
	runID string,
) (agentLifecycleEvidence, *contract.RuntimeError) {
	result := agentLifecycleEvidence{
		checkpoint: make(map[string][]uint64),
		started:    make(map[string][]uint64),
		completed:  make(map[string][]uint64),
		failed:     make(map[string][]uint64),
		paused:     make(map[string][]uint64),
	}
	var afterSequence uint64
	for {
		events, err := executor.Store.Events(
			ctx, runID, afterSequence, 1000,
		)
		if err != nil {
			return agentLifecycleEvidence{}, agentRecoveryError(
				"load Agent lifecycle evidence: " + err.Error(),
			)
		}
		for _, event := range events {
			switch event.Type {
			case contract.EventCheckpointCommitted:
				result.checkpoint[event.Checkpoint.CheckpointID] =
					append(
						result.checkpoint[event.Checkpoint.CheckpointID],
						event.Sequence,
					)
			case contract.EventToolStarted:
				result.started[event.Tool.CallID] = append(
					result.started[event.Tool.CallID], event.Sequence,
				)
			case contract.EventToolCompleted:
				result.completed[event.Tool.CallID] = append(
					result.completed[event.Tool.CallID], event.Sequence,
				)
			case contract.EventToolFailed:
				result.failed[event.Tool.CallID] = append(
					result.failed[event.Tool.CallID], event.Sequence,
				)
			case contract.EventAgentPaused:
				result.paused[event.Agent.PauseID] = append(
					result.paused[event.Agent.PauseID], event.Sequence,
				)
			}
		}
		if len(events) < 1000 {
			break
		}
		afterSequence = events[len(events)-1].Sequence
	}
	return result, nil
}

func (executor *AgentExecutor) validateDurableAgentEventJournal(
	ctx context.Context,
	record Record,
	state agent.LoopState,
	modelCalls []ModelCall,
) *contract.RuntimeError {
	view, runtimeErr := partitionModelCallJournal(modelCalls, state)
	if runtimeErr != nil {
		return runtimeErr
	}
	durableResults := make(
		[]contract.ModelResult, len(view.completed),
	)
	for index, call := range view.completed {
		result, runtimeErr := decodeDurableModelResult(call)
		if runtimeErr != nil {
			return runtimeErr
		}
		durableResults[index] = result
	}
	var modelStarted, modelCompleted int
	var agentCompleted int
	agentPaused := make(map[string]int)
	eventModelResults := make(
		[]contract.ModelResult, 0, len(view.completed),
	)
	modelActive := false
	var afterSequence uint64
	for {
		events, err := executor.Store.Events(
			ctx, record.ID, afterSequence, 1000,
		)
		if err != nil {
			return agentRecoveryError(
				"validate durable Agent event journal: " + err.Error(),
			)
		}
		for _, event := range events {
			switch event.Type {
			case contract.EventModelStarted:
				if modelActive || modelStarted != modelCompleted ||
					modelStarted >= len(modelCalls) {
					return agentRecoveryError(
						"model.started event is out of order",
					)
				}
				modelActive = true
				modelStarted++
			case contract.EventContentDelta,
				contract.EventReasoningDelta,
				contract.EventToolCallStarted,
				contract.EventToolCallArgumentsDelta:
				if !modelActive {
					return agentRecoveryError(
						"model streaming event is outside an active model round",
					)
				}
			case contract.EventModelCompleted:
				if !modelActive ||
					modelCompleted >= len(durableResults) {
					return agentRecoveryError(
						"model.completed event is out of order",
					)
				}
				modelCompleted++
				if event.Model != nil && event.Model.Result != nil {
					eventModelResults = append(
						eventModelResults, *event.Model.Result,
					)
				}
				modelActive = false
			case contract.EventCheckpointCommitted:
				if modelActive {
					return agentRecoveryError(
						"checkpoint event occurred during a model round",
					)
				}
				checkpoint, exists, err := executor.Store.Checkpoint(
					ctx, event.Checkpoint.CheckpointID,
				)
				if err != nil {
					return agentRecoveryError(
						"load event checkpoint evidence: " + err.Error(),
					)
				}
				if !exists || checkpoint.RunID != record.ID {
					return agentRecoveryError(
						"checkpoint.committed event has no matching checkpoint",
					)
				}
			case contract.EventToolStarted,
				contract.EventToolCompleted,
				contract.EventToolFailed:
				if modelActive {
					return agentRecoveryError(
						"tool event occurred during a model round",
					)
				}
				if runtimeErr := executor.validateDurableToolEvent(
					ctx, record.ID, event,
				); runtimeErr != nil {
					return runtimeErr
				}
			case contract.EventAgentCompleted:
				if modelActive {
					return agentRecoveryError(
						"agent.completed occurred during a model round",
					)
				}
				agentCompleted++
				if state.TerminalOutcome == nil ||
					state.TerminalOutcome.State != agent.StateCompleted ||
					event.Agent.StopReason !=
						state.TerminalOutcome.StopReason {
					return agentRecoveryError(
						"agent.completed event does not match checkpoint outcome",
					)
				}
			case contract.EventAgentPaused:
				if modelActive {
					return agentRecoveryError(
						"agent.paused occurred during a model round",
					)
				}
				agentPaused[event.Agent.PauseID]++
			default:
				return agentRecoveryError(
					"durable Agent journal contains an unsupported event type",
				)
			}
		}
		if len(events) < 1000 {
			break
		}
		afterSequence = events[len(events)-1].Sequence
	}
	modelLifecycleValid :=
		modelCompleted == len(view.completed) &&
			len(eventModelResults) == len(view.completed)
	if view.terminal == nil {
		modelLifecycleValid = modelLifecycleValid &&
			!modelActive && modelStarted == modelCompleted
	} else {
		modelLifecycleValid = modelLifecycleValid &&
			(modelStarted == modelCompleted && !modelActive ||
				modelStarted == modelCompleted+1 && modelActive)
	}
	if !modelLifecycleValid || agentCompleted > 1 {
		return agentRecoveryError(
			"durable Agent event journal lifecycle counts are inconsistent",
		)
	}
	for index := range durableResults {
		if !reflect.DeepEqual(
			eventModelResults[index], durableResults[index],
		) {
			return agentRecoveryError(
				"model.completed event does not match durable model result",
			)
		}
	}
	if state.TerminalOutcome != nil {
		switch state.TerminalOutcome.State {
		case agent.StateCompleted:
			if agentCompleted != 1 {
				return agentRecoveryError(
					"completed Agent checkpoint has no terminal event",
				)
			}
		case agent.StatePaused:
			if state.Pause == nil ||
				agentPaused[state.Pause.ID] != 1 {
				return agentRecoveryError(
					"paused Agent checkpoint has no terminal event",
				)
			}
		case agent.StateFailed, agent.StateCancelled:
			if agentCompleted != 0 {
				return agentRecoveryError(
					"failed or cancelled Agent checkpoint has a completed terminal event",
				)
			}
		}
	}
	return nil
}

type modelCallJournalView struct {
	completed []ModelCall
	terminal  *ModelCall
}

func partitionModelCallJournal(
	modelCalls []ModelCall,
	state agent.LoopState,
) (modelCallJournalView, *contract.RuntimeError) {
	view := modelCallJournalView{
		completed: make([]ModelCall, 0, len(modelCalls)),
	}
	for index := range modelCalls {
		call := modelCalls[index]
		if call.Sequence != index+1 {
			return modelCallJournalView{}, agentRecoveryError(
				"agent model call journal is not contiguous",
			)
		}
		switch call.State {
		case "completed":
			if view.terminal != nil {
				return modelCallJournalView{}, agentRecoveryError(
					"completed Agent model call follows a terminal model error",
				)
			}
			view.completed = append(view.completed, call)
		case "failed", "cancelled":
			if index != len(modelCalls)-1 || view.terminal != nil ||
				call.Error == nil || len(call.Result) != 0 ||
				call.ResultDigest != "" {
				return modelCallJournalView{}, agentRecoveryError(
					"agent model call terminal error evidence is inconsistent",
				)
			}
			if err := call.Error.Validate(); err != nil {
				return modelCallJournalView{}, agentRecoveryError(
					"agent model call terminal error is invalid: " + err.Error(),
				)
			}
			current := call
			view.terminal = &current
		default:
			return modelCallJournalView{}, agentRecoveryError(
				fmt.Sprintf(
					"agent model call sequence=%d state=%s cannot be replayed safely",
					call.Sequence, call.State,
				),
			)
		}
	}
	if len(view.completed) != state.Round {
		return modelCallJournalView{}, agentRecoveryError(
			"agent checkpoint round is not covered by completed model calls",
		)
	}
	if view.terminal == nil {
		if len(modelCalls) != state.Round {
			return modelCallJournalView{}, agentRecoveryError(
				"agent model call journal is ahead of its checkpoint",
			)
		}
		return view, nil
	}
	if len(modelCalls) != state.Round+1 ||
		state.TerminalOutcome == nil ||
		state.TerminalOutcome.Error == nil ||
		!reflect.DeepEqual(
			state.TerminalOutcome.Error, view.terminal.Error,
		) {
		return modelCallJournalView{}, agentRecoveryError(
			"terminal Agent model call does not match checkpoint outcome",
		)
	}
	switch view.terminal.State {
	case "failed":
		if state.TerminalOutcome.State != agent.StateFailed ||
			state.TerminalOutcome.StopReason != "model_failed" ||
			view.terminal.Error.Code == contract.ErrorCancelled ||
			view.terminal.Error.Code == contract.ErrorTimeout {
			return modelCallJournalView{}, agentRecoveryError(
				"failed Agent model call does not match failed checkpoint evidence",
			)
		}
	case "cancelled":
		if state.TerminalOutcome.State != agent.StateCancelled ||
			state.TerminalOutcome.StopReason != "cancelled" ||
			(view.terminal.Error.Code != contract.ErrorCancelled &&
				view.terminal.Error.Code != contract.ErrorTimeout) {
			return modelCallJournalView{}, agentRecoveryError(
				"cancelled Agent model call does not match cancelled checkpoint evidence",
			)
		}
	}
	return view, nil
}

func validateModelCallJournal(
	modelCalls []ModelCall,
	state agent.LoopState,
) *contract.RuntimeError {
	view, runtimeErr := partitionModelCallJournal(modelCalls, state)
	if runtimeErr != nil {
		return runtimeErr
	}
	assistantMessages := make(
		[]contract.Message, 0, len(state.Messages),
	)
	for _, message := range state.Messages {
		if message.Role == contract.RoleAssistant {
			assistantMessages = append(assistantMessages, message)
		}
	}
	if len(assistantMessages) < len(view.completed) {
		return agentRecoveryError(
			"agent checkpoint has fewer assistant messages than model calls",
		)
	}
	assistantMessages = assistantMessages[len(assistantMessages)-len(view.completed):]
	seenCallIDs := make(map[string]struct{})
	expectedSeen := make([]string, 0, state.ToolCallCount)
	for index, call := range view.completed {
		result, runtimeErr := decodeDurableModelResult(call)
		if runtimeErr != nil {
			return runtimeErr
		}
		if !reflect.DeepEqual(
			assistantMessages[index], result.Message,
		) {
			return agentRecoveryError(
				"agent model call result does not match checkpoint messages",
			)
		}
		if index < len(view.completed)-1 &&
			result.FinishReason != contract.FinishToolCall {
			return agentRecoveryError(
				"non-final Agent model call did not request a tool",
			)
		}
		seenLimit := len(result.Message.ToolCalls)
		if index == len(view.completed)-1 {
			switch {
			case len(state.PendingToolCalls) > 0:
				seenLimit = state.PendingToolCursor
				if state.PendingEffectCheckpointID != "" {
					seenLimit++
				}
			case state.TerminalOutcome != nil &&
				state.TerminalOutcome.State == agent.StatePaused &&
				state.Pause != nil:
				seenLimit = 0
				for callIndex, toolCall := range result.Message.ToolCalls {
					if toolCall.ID == state.Pause.ToolCallID {
						seenLimit = callIndex + 1
						break
					}
				}
				if seenLimit == 0 {
					return agentRecoveryError(
						"paused Agent call is absent from the model result",
					)
				}
			}
		}
		if seenLimit > len(result.Message.ToolCalls) {
			return agentRecoveryError(
				"agent checkpoint pending cursor exceeds model tool calls",
			)
		}
		for callIndex, toolCall := range result.Message.ToolCalls {
			if _, exists := seenCallIDs[toolCall.ID]; exists {
				isCurrentDuplicate :=
					state.TerminalOutcome != nil &&
						state.TerminalOutcome.State == agent.StateFailed &&
						state.TerminalOutcome.StopReason ==
							"duplicate_tool_call" &&
						index == len(view.completed)-1 &&
						len(state.PendingToolCalls) > 0 &&
						callIndex == state.PendingToolCursor &&
						state.PendingToolCalls[state.PendingToolCursor].ID ==
							toolCall.ID
				if isCurrentDuplicate {
					continue
				}
				return agentRecoveryError(
					"agent model call journal contains duplicate tool call IDs",
				)
			}
			seenCallIDs[toolCall.ID] = struct{}{}
			if callIndex < seenLimit {
				expectedSeen = append(expectedSeen, toolCall.ID)
			}
		}
	}
	if !equalStringSlice(expectedSeen, state.SeenToolCallIDs) ||
		len(expectedSeen) != state.ToolCallCount {
		return agentRecoveryError(
			"agent checkpoint seen tool calls do not match model results",
		)
	}
	return nil
}

func validateModelRequestJournal(
	record Record,
	state agent.LoopState,
	modelCalls []ModelCall,
) *contract.RuntimeError {
	view, runtimeErr := partitionModelCallJournal(modelCalls, state)
	if runtimeErr != nil {
		return runtimeErr
	}
	assistantPositions := make([]int, 0, len(view.completed))
	for index := state.BaseMessageCount; index < len(state.Messages); index++ {
		if state.Messages[index].Role == contract.RoleAssistant {
			assistantPositions = append(assistantPositions, index)
		}
	}
	if len(assistantPositions) != len(view.completed) {
		return agentRecoveryError(
			"Agent message journal does not have one exact assistant boundary per model call",
		)
	}
	var frozenTools []contract.ToolSpec
	for index, call := range modelCalls {
		if !validSHA256Digest(call.RequestDigest) {
			return agentRecoveryError(
				"agent model call request_digest is not a canonical sha256 digest",
			)
		}
		var frozen contract.GenerateRequest
		if err := strictjson.DecodeObject(
			bytes.NewReader(call.Request), 4<<20, &frozen,
		); err != nil {
			return agentRecoveryError(
				"decode frozen agent model request: " + err.Error(),
			)
		}
		if err := frozen.Validate(); err != nil {
			return agentRecoveryError(
				"frozen agent model request is invalid: " + err.Error(),
			)
		}
		var expectedMessages []contract.Message
		if index < len(view.completed) {
			result, runtimeErr := decodeDurableModelResult(call)
			if runtimeErr != nil {
				return runtimeErr
			}
			assistantIndex := assistantPositions[index]
			if !reflect.DeepEqual(
				state.Messages[assistantIndex], result.Message,
			) {
				return agentRecoveryError(
					"agent model call result does not match its exact message boundary",
				)
			}
			expectedMessages = state.Messages[:assistantIndex]
		} else {
			if view.terminal == nil {
				return agentRecoveryError(
					"Agent model request has no matching result or terminal error",
				)
			}
			expectedMessages = state.Messages
		}
		if index == 0 {
			frozenTools = append(
				[]contract.ToolSpec(nil), frozen.Input.Tools...,
			)
		} else if !reflect.DeepEqual(frozenTools, frozen.Input.Tools) {
			return agentRecoveryError(
				"Agent model request tool definitions changed within one run",
			)
		}
		expected := contract.GenerateRequest{
			ModelProfile: state.ModelProfile,
			Input: contract.ModelRequest{
				Messages: append(
					[]contract.Message(nil),
					expectedMessages...,
				),
				Tools: append(
					[]contract.ToolSpec(nil), frozenTools...,
				),
				Trace: contract.TraceContext{Labels: map[string]string{
					"run_id": record.ID,
				}},
			},
		}
		if !reflect.DeepEqual(frozen, expected) {
			return agentRecoveryError(
				fmt.Sprintf(
					"frozen agent model request sequence=%d does not match its exact profile/message/trace boundary",
					call.Sequence,
				),
			)
		}
		requestJSON, err := json.Marshal(frozen)
		if err != nil {
			return agentRecoveryError(
				"encode reconstructed agent model request: " + err.Error(),
			)
		}
		sum := sha256.Sum256(requestJSON)
		expectedDigest := "sha256:" + hex.EncodeToString(sum[:])
		if call.RequestDigest != expectedDigest {
			return agentRecoveryError(
				fmt.Sprintf(
					"agent model call sequence=%d request digest does not match reconstructed request",
					call.Sequence,
				),
			)
		}
	}
	return nil
}

func validateRecoveredBudget(
	configured agent.Budget,
	state agent.LoopState,
	modelCalls []ModelCall,
) *contract.RuntimeError {
	view, runtimeErr := partitionModelCallJournal(modelCalls, state)
	if runtimeErr != nil {
		return runtimeErr
	}
	budget := configured.Effective()
	if err := budget.Validate(); err != nil {
		return agentRecoveryError(
			"durable Agent budget is invalid: " + err.Error(),
		)
	}
	var total int64
	invalidUsage := false
	for index, call := range view.completed {
		result, runtimeErr := decodeDurableModelResult(call)
		if runtimeErr != nil {
			return runtimeErr
		}
		current, err := modelUsageTotal(result.Usage)
		if err != nil {
			if index != len(view.completed)-1 ||
				state.TerminalOutcome == nil ||
				state.TerminalOutcome.State != agent.StateFailed ||
				state.TerminalOutcome.StopReason != "invalid_model_usage" {
				return agentRecoveryError(err.Error())
			}
			invalidUsage = true
			break
		}
		if current > math.MaxInt64-total {
			if index != len(view.completed)-1 ||
				state.TerminalOutcome == nil ||
				state.TerminalOutcome.State != agent.StateFailed ||
				state.TerminalOutcome.StopReason != "invalid_model_usage" {
				return agentRecoveryError(
					"durable Agent model usage overflows total_tokens",
				)
			}
			invalidUsage = true
			break
		}
		total += current
	}
	if state.TotalTokens != total {
		return agentRecoveryError(
			"agent checkpoint total_tokens does not match durable model usage",
		)
	}
	if invalidUsage {
		if state.TotalTokens != total {
			return agentRecoveryError(
				"invalid model usage checkpoint changed total_tokens",
			)
		}
	} else if budget.MaxTotalTokens > 0 && total > budget.MaxTotalTokens {
		if state.TerminalOutcome == nil ||
			state.TerminalOutcome.State != agent.StateFailed ||
			state.TerminalOutcome.StopReason != "token_budget" {
			return agentRecoveryError(
				"durable Agent token usage exceeds its configured budget",
			)
		}
	} else if state.TerminalOutcome != nil &&
		state.TerminalOutcome.StopReason == "token_budget" {
		return agentRecoveryError(
			"token-budget Agent failure is not backed by excess durable usage",
		)
	}
	if state.Round > budget.MaxRounds ||
		state.ToolCallCount > budget.MaxToolCalls {
		return agentRecoveryError(
			"durable Agent counters exceed their configured budget",
		)
	}
	return nil
}

func modelUsageTotal(usage contract.Usage) (int64, error) {
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
				"durable Agent model usage overflows total_tokens",
			)
		}
		total += *usage.OutputTokens
	}
	return total, nil
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 ||
		value[:len("sha256:")] != "sha256:" {
		return false
	}
	for _, current := range value[len("sha256:"):] {
		if current < '0' || current > '9' &&
			(current < 'a' || current > 'f') {
			return false
		}
	}
	return true
}

func equalStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func decodeDurableModelResult(
	modelCall ModelCall,
) (contract.ModelResult, *contract.RuntimeError) {
	if len(modelCall.Result) == 0 || modelCall.ResultDigest == "" {
		return contract.ModelResult{}, agentRecoveryError(
			"completed agent model call has no durable result evidence",
		)
	}
	var result contract.ModelResult
	if err := strictjson.DecodeObject(
		bytes.NewReader(modelCall.Result), 4<<20, &result,
	); err != nil {
		return contract.ModelResult{}, agentRecoveryError(
			"decode durable agent model result: " + err.Error(),
		)
	}
	if err := result.Validate(); err != nil {
		return contract.ModelResult{}, agentRecoveryError(
			"durable agent model result is invalid: " + err.Error(),
		)
	}
	if result.Provider.RequestID != modelCall.ProviderRequestID {
		return contract.ModelResult{}, agentRecoveryError(
			"durable agent model result provider request ID does not match",
		)
	}
	return result, nil
}

func validateRecoveredLoopState(
	state agent.LoopState,
) *contract.RuntimeError {
	if len(state.Messages) == 0 {
		return agentRecoveryError("agent checkpoint messages are required")
	}
	if state.BaseMessageCount <= 0 ||
		state.BaseMessageCount > len(state.Messages) {
		return agentRecoveryError(
			"agent checkpoint base message boundary is invalid",
		)
	}
	seen := make(map[string]struct{}, len(state.SeenToolCallIDs))
	for _, callID := range state.SeenToolCallIDs {
		if callID == "" {
			return agentRecoveryError(
				"agent checkpoint contains an empty seen tool call ID",
			)
		}
		if _, exists := seen[callID]; exists {
			return agentRecoveryError(
				"agent checkpoint contains duplicate seen tool call IDs",
			)
		}
		seen[callID] = struct{}{}
	}
	if state.ToolCallCount != len(state.SeenToolCallIDs) {
		return agentRecoveryError(
			"agent checkpoint tool call count does not match seen IDs",
		)
	}
	if state.TerminalOutcome != nil {
		outcome := state.TerminalOutcome
		if outcome.StopReason == "" {
			return agentRecoveryError(
				"agent checkpoint terminal outcome has no stop reason",
			)
		}
		switch outcome.State {
		case agent.StateCompleted:
			if outcome.Message == nil || outcome.Pause != nil ||
				outcome.Error != nil || state.Pause != nil {
				return agentRecoveryError(
					"completed Agent outcome requires only a message",
				)
			}
		case agent.StatePaused:
			if outcome.Pause == nil || outcome.Message != nil ||
				outcome.Error != nil || state.Pause == nil ||
				!reflect.DeepEqual(*state.Pause, *outcome.Pause) {
				return agentRecoveryError(
					"paused Agent outcome does not match its durable pause",
				)
			}
		case agent.StateFailed, agent.StateCancelled:
			if outcome.Message != nil || outcome.Pause != nil ||
				outcome.Error == nil || state.Pause != nil {
				return agentRecoveryError(
					"failed or cancelled Agent outcome requires only an error",
				)
			}
			if err := outcome.Error.Validate(); err != nil {
				return agentRecoveryError(
					"Agent terminal error is invalid: " + err.Error(),
				)
			}
			if outcome.State == agent.StateCancelled &&
				outcome.Error.Code != contract.ErrorCancelled &&
				outcome.Error.Code != contract.ErrorTimeout {
				return agentRecoveryError(
					"cancelled Agent outcome requires cancelled or timeout error",
				)
			}
			if outcome.State == agent.StateFailed &&
				outcome.Error.Code == contract.ErrorCancelled {
				return agentRecoveryError(
					"failed Agent outcome contains a cancelled error",
				)
			}
		default:
			return agentRecoveryError(
				"agent checkpoint contains an unsupported terminal outcome",
			)
		}
	}
	if len(state.PendingToolCalls) > 0 {
		if state.PendingToolCursor < 0 ||
			state.PendingToolCursor >= len(state.PendingToolCalls) {
			return agentRecoveryError(
				"agent checkpoint pending tool state is inconsistent",
			)
		}
		for _, call := range state.PendingToolCalls {
			if err := call.Validate(); err != nil {
				return agentRecoveryError(
					"agent checkpoint contains an invalid pending tool call: " +
						err.Error(),
				)
			}
		}
		current := state.PendingToolCalls[state.PendingToolCursor]
		_, currentSeen := seen[current.ID]
		if state.TerminalOutcome == nil {
			if !currentSeen {
				return agentRecoveryError(
					"recovered pending tool call is absent from seen tool call IDs",
				)
			}
			if err := identity.Validate(
				state.PendingEffectCheckpointID, "checkpoint",
			); err != nil {
				return agentRecoveryError(
					"recovered pending tool call has no valid preparation checkpoint",
				)
			}
			return nil
		}
		outcome := state.TerminalOutcome
		switch outcome.State {
		case agent.StatePaused:
			if !currentSeen ||
				state.Pause == nil ||
				outcome.Pause == nil ||
				current.ID != state.Pause.ToolCallID ||
				!reflect.DeepEqual(outcome.Pause, state.Pause) {
				return agentRecoveryError(
					"paused Agent checkpoint does not match its pending tool cursor",
				)
			}
			if err := identity.Validate(
				state.PendingEffectCheckpointID, "checkpoint",
			); err != nil {
				return agentRecoveryError(
					"paused Agent checkpoint has no valid preparation checkpoint",
				)
			}
		case agent.StateFailed:
			switch outcome.StopReason {
			case "duplicate_tool_call":
				if !currentSeen ||
					state.PendingEffectCheckpointID != "" {
					return agentRecoveryError(
						"duplicate tool-call failure has invalid pre-effect evidence",
					)
				}
			case "unknown_tool", "tool_budget", "invalid_tool_arguments",
				"execution_snapshot_changed":
				if currentSeen ||
					state.PendingEffectCheckpointID != "" {
					if outcome.StopReason !=
						"execution_snapshot_changed" ||
						!currentSeen {
						return agentRecoveryError(
							"pre-effect Agent failure has unexpected effect evidence",
						)
					}
					if err := identity.Validate(
						state.PendingEffectCheckpointID, "checkpoint",
					); err != nil {
						return agentRecoveryError(
							"execution snapshot failure has invalid preparation evidence",
						)
					}
				}
				if outcome.StopReason == "execution_snapshot_changed" &&
					!isExecutionSnapshotChangedError(outcome.Error) {
					return agentRecoveryError(
						"execution snapshot failure has an invalid error",
					)
				}
			case "tool_failed":
				if !currentSeen {
					return agentRecoveryError(
						"known tool failure is absent from seen tool calls",
					)
				}
				if err := identity.Validate(
					state.PendingEffectCheckpointID, "checkpoint",
				); err != nil {
					return agentRecoveryError(
						"known tool failure has no valid preparation checkpoint",
					)
				}
			default:
				return agentRecoveryError(
					"pending failed Agent checkpoint has an unsupported stop reason",
				)
			}
		case agent.StateCancelled:
			if currentSeen || state.PendingEffectCheckpointID != "" ||
				outcome.StopReason != "cancelled" {
				return agentRecoveryError(
					"pending cancelled Agent checkpoint has unexpected effect evidence",
				)
			}
		default:
			return agentRecoveryError(
				"pending Agent checkpoint has an unsupported terminal outcome",
			)
		}
		return nil
	}
	if state.PendingToolCursor != 0 ||
		state.PendingEffectCheckpointID != "" ||
		state.TerminalOutcome == nil {
		return agentRecoveryError(
			"agent checkpoint does not prove a pending or terminal model outcome",
		)
	}
	switch state.TerminalOutcome.State {
	case agent.StateCompleted:
		if state.TerminalOutcome.Message == nil ||
			len(state.Messages) == 0 ||
			state.Messages[len(state.Messages)-1].Role !=
				contract.RoleAssistant ||
			!reflect.DeepEqual(
				state.Messages[len(state.Messages)-1],
				*state.TerminalOutcome.Message,
			) {
			return agentRecoveryError(
				"completed Agent outcome does not match the last assistant message",
			)
		}
	case agent.StatePaused:
		return agentRecoveryError(
			"paused Agent outcome requires a pending tool call",
		)
	case agent.StateFailed, agent.StateCancelled:
	}
	return nil
}

func validateModelResultEvidence(
	modelCall ModelCall,
	state agent.LoopState,
) *contract.RuntimeError {
	if modelCall.State == "failed" || modelCall.State == "cancelled" {
		if len(modelCall.Result) != 0 ||
			modelCall.ResultDigest != "" ||
			modelCall.Error == nil ||
			state.TerminalOutcome == nil ||
			state.TerminalOutcome.Error == nil ||
			!reflect.DeepEqual(
				modelCall.Error, state.TerminalOutcome.Error,
			) {
			return agentRecoveryError(
				"terminal Agent model error does not match checkpoint evidence",
			)
		}
		if modelCall.State == "failed" &&
			(state.TerminalOutcome.State != agent.StateFailed ||
				state.TerminalOutcome.StopReason != "model_failed") {
			return agentRecoveryError(
				"failed Agent model call does not match checkpoint state",
			)
		}
		if modelCall.State == "cancelled" &&
			(state.TerminalOutcome.State != agent.StateCancelled ||
				state.TerminalOutcome.StopReason != "cancelled") {
			return agentRecoveryError(
				"cancelled Agent model call does not match checkpoint state",
			)
		}
		return nil
	}
	if modelCall.State != "completed" {
		return agentRecoveryError(
			"latest Agent model call has no proven terminal result",
		)
	}
	if len(modelCall.Result) == 0 || modelCall.ResultDigest == "" {
		return agentRecoveryError(
			"completed agent model call has no durable result evidence",
		)
	}
	var result contract.ModelResult
	if err := strictjson.DecodeObject(
		bytes.NewReader(modelCall.Result), 4<<20, &result,
	); err != nil {
		return agentRecoveryError(
			"decode durable agent model result: " + err.Error(),
		)
	}
	if err := result.Validate(); err != nil {
		return agentRecoveryError(
			"durable agent model result is invalid: " + err.Error(),
		)
	}
	if result.Provider.RequestID != modelCall.ProviderRequestID {
		return agentRecoveryError(
			"durable agent model result provider request ID does not match",
		)
	}
	assistantIndex := -1
	for index := len(state.Messages) - 1; index >= 0; index-- {
		if state.Messages[index].Role == contract.RoleAssistant {
			assistantIndex = index
			break
		}
	}
	if assistantIndex < 0 ||
		!reflect.DeepEqual(state.Messages[assistantIndex], result.Message) {
		return agentRecoveryError(
			"durable agent model result does not match checkpoint messages",
		)
	}
	if len(state.PendingToolCalls) > 0 {
		if result.FinishReason != contract.FinishToolCall ||
			!reflect.DeepEqual(
				state.PendingToolCalls, result.Message.ToolCalls,
			) {
			return agentRecoveryError(
				"pending tool checkpoint does not match durable model result",
			)
		}
		return nil
	}
	switch state.TerminalOutcome.State {
	case agent.StateCompleted:
		if result.FinishReason == contract.FinishToolCall ||
			state.TerminalOutcome.StopReason != string(result.FinishReason) ||
			!reflect.DeepEqual(
				state.TerminalOutcome.Message, &result.Message,
			) {
			return agentRecoveryError(
				"completed Agent outcome does not match durable model result",
			)
		}
	case agent.StatePaused:
		if result.FinishReason != contract.FinishToolCall ||
			state.Pause == nil {
			return agentRecoveryError(
				"paused Agent outcome is not backed by a tool-call model result",
			)
		}
		found := false
		for _, call := range result.Message.ToolCalls {
			if call.ID == state.Pause.ToolCallID {
				found = true
				break
			}
		}
		if !found {
			return agentRecoveryError(
				"paused Agent outcome does not match a durable tool call",
			)
		}
	case agent.StateCancelled:
		if result.FinishReason == contract.FinishCancelled {
			expected := &contract.RuntimeError{
				Code: contract.ErrorCancelled, Phase: contract.PhaseRun,
				Message: "model generation was cancelled",
			}
			if state.TerminalOutcome.StopReason !=
				string(contract.FinishCancelled) ||
				!reflect.DeepEqual(
					state.TerminalOutcome.Error, expected,
				) {
				return agentRecoveryError(
					"FinishCancelled model result does not match canonical Agent cancellation",
				)
			}
		}
	case agent.StateFailed:
	}
	return nil
}

func (executor *AgentExecutor) validateDurableToolEvent(
	ctx context.Context,
	runID string,
	event contract.Event,
) *contract.RuntimeError {
	effect, exists, err := (&durableEffects{
		store: executor.Store,
	}).Lookup(ctx, runID, event.Tool.CallID)
	if err != nil {
		return agentRecoveryError(
			"load tool effect for durable event: " + err.Error(),
		)
	}
	if !exists {
		return agentRecoveryError(
			"durable tool event has no matching effect",
		)
	}
	request := effect.Request
	if event.Tool.Name != request.Name ||
		event.Tool.IdempotencyKey != request.IdempotencyKey {
		return agentRecoveryError(
			"durable tool event metadata does not match its effect",
		)
	}
	switch event.Type {
	case contract.EventToolStarted:
		if effect.State == "prepared" {
			return agentRecoveryError(
				"tool.started event is not backed by a started effect",
			)
		}
	case contract.EventToolCompleted:
		if effect.State != "completed" || effect.Result == nil ||
			effect.Result.Pause != nil ||
			event.Tool.Content != effect.Result.Content ||
			event.Tool.IsError != effect.Result.IsError ||
			effect.Error != nil {
			return agentRecoveryError(
				"tool.completed event does not match durable result",
			)
		}
	case contract.EventToolFailed:
		if effect.State != "failed" || effect.Error == nil ||
			!reflect.DeepEqual(event.Error, effect.Error) ||
			effect.Result != nil {
			return agentRecoveryError(
				"tool.failed event does not match durable error",
			)
		}
	default:
		return agentRecoveryError(
			"unexpected durable tool event type",
		)
	}
	return nil
}

func (executor *AgentExecutor) validateRecoveredPendingMessages(
	ctx context.Context,
	record Record,
	preparedState agent.LoopState,
	state agent.LoopState,
	currentEffect agent.EffectRecord,
) *contract.RuntimeError {
	resumes, err := executor.Store.Resumes(ctx, record.ID)
	if err != nil {
		return agentRecoveryError(
			"load Agent resume journal: " + err.Error(),
		)
	}
	assistantIndex := -1
	for index := len(preparedState.Messages) - 1; index >= 0; index-- {
		if preparedState.Messages[index].Role == contract.RoleAssistant {
			assistantIndex = index
			break
		}
	}
	if assistantIndex < 0 ||
		!reflect.DeepEqual(
			preparedState.Messages[assistantIndex].ToolCalls,
			preparedState.PendingToolCalls,
		) {
		return agentRecoveryError(
			"tool preparation checkpoint has no matching assistant tool calls",
		)
	}
	expectedPrepared := append(
		[]contract.Message(nil),
		preparedState.Messages[:assistantIndex+1]...,
	)
	for index := 0; index < preparedState.PendingToolCursor; index++ {
		call := preparedState.PendingToolCalls[index]
		effect, exists, err := (&durableEffects{
			store: executor.Store,
		}).Lookup(ctx, record.ID, call.ID)
		if err != nil {
			return agentRecoveryError(
				"load prior durable tool result: " + err.Error(),
			)
		}
		if !exists || effect.State != "completed" ||
			effect.Result == nil || effect.Error != nil ||
			effect.Request.RunID != record.ID ||
			effect.Request.CallID != call.ID ||
			effect.Request.Name != call.Name ||
			!bytes.Equal(effect.Request.Arguments, call.Arguments) ||
			effect.Request.IdempotencyKey !=
				agentToolIdempotencyKey(record.ID, call) {
			return agentRecoveryError(
				"prior tool message has no matching durable result",
			)
		}
		message := contract.Message{
			Role: contract.RoleTool, ToolCallID: call.ID,
			Content: effect.Result.Content,
			IsError: effect.Result.IsError,
		}
		if effect.Result.Pause != nil {
			resumeMessage, runtimeErr := recoveredPauseMessage(
				call, *effect.Result.Pause, resumes,
			)
			if runtimeErr != nil {
				return runtimeErr
			}
			message = resumeMessage
		} else if effect.Result.Content == "" {
			return agentRecoveryError(
				"prior tool message has an empty durable result",
			)
		}
		expectedPrepared = append(
			expectedPrepared,
			message,
		)
	}
	if !reflect.DeepEqual(expectedPrepared, preparedState.Messages) {
		return agentRecoveryError(
			"tool preparation checkpoint contains an unproven message suffix",
		)
	}
	call := state.PendingToolCalls[state.PendingToolCursor]
	expectedPause := (*agent.Pause)(nil)
	expectedLatest := expectedPrepared
	if currentEffect.State == "completed" &&
		currentEffect.Result != nil {
		if currentEffect.Result.Pause != nil {
			pause := *currentEffect.Result.Pause
			pause.ToolCallID = call.ID
			expectedPause = &pause
		} else {
			expectedLatest = append(
				append([]contract.Message(nil), expectedPrepared...),
				contract.Message{
					Role: contract.RoleTool, ToolCallID: call.ID,
					Content: currentEffect.Result.Content,
					IsError: currentEffect.Result.IsError,
				},
			)
		}
	}
	messagesMatch := reflect.DeepEqual(state.Messages, expectedPrepared)
	if !messagesMatch &&
		!reflect.DeepEqual(state.Messages, expectedLatest) {
		return agentRecoveryError(
			"recovered tool checkpoint contains an unproven message suffix",
		)
	}
	if expectedPause == nil {
		if state.Pause != nil {
			return agentRecoveryError(
				"recovered tool checkpoint contains an unproven pause",
			)
		}
	} else if state.Pause != nil &&
		!reflect.DeepEqual(state.Pause, expectedPause) {
		return agentRecoveryError(
			"recovered tool checkpoint pause does not match durable result",
		)
	}
	return nil
}

func agentToolIdempotencyKey(
	runID string,
	call contract.ToolCall,
) string {
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
