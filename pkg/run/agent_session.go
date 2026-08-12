package run

import (
	"bytes"
	"fmt"
	"reflect"

	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/profile"
	"github.com/yy003x/runtime/pkg/session"
)

func validateProjectedAgentSessionPause(
	turn session.AgentTurn,
	state agent.LoopState,
) *contract.RuntimeError {
	if turn.ProjectedPause == nil ||
		state.Pause == nil ||
		state.TerminalOutcome == nil ||
		state.TerminalOutcome.State != agent.StatePaused ||
		state.TerminalOutcome.Pause == nil ||
		!reflect.DeepEqual(turn.ProjectedPause, state.Pause) ||
		!reflect.DeepEqual(state.TerminalOutcome.Pause, state.Pause) {
		return agentRecoveryError(
			"Agent paused Session event does not match the durable checkpoint pause",
		)
	}
	safeEnd, openAssistant, completedCalls, err :=
		agentProviderSafePrefix(state.Messages, state.BaseMessageCount)
	if err != nil || openAssistant < 0 ||
		completedCalls >=
			len(state.Messages[openAssistant].ToolCalls) ||
		state.Messages[openAssistant].ToolCalls[completedCalls].ID !=
			state.Pause.ToolCallID {
		return agentRecoveryError(
			"Agent paused checkpoint has an invalid provider message boundary",
		)
	}
	if safeEnd != len(turn.Messages) ||
		!reflect.DeepEqual(turn.Messages, state.Messages[:safeEnd]) {
		return agentRecoveryError(
			"Agent paused Session messages are not the checkpoint provider-safe prefix",
		)
	}
	return nil
}

func validateAgentSessionSafeProjection(
	turn session.AgentTurn,
	state agent.LoopState,
) *contract.RuntimeError {
	safeEnd, _, _, err := agentProviderSafePrefix(
		state.Messages, state.BaseMessageCount,
	)
	if err != nil ||
		safeEnd != len(turn.Messages) ||
		!reflect.DeepEqual(turn.Messages, state.Messages[:safeEnd]) {
		return agentRecoveryError(
			"Agent Session messages are not the checkpoint provider-safe prefix",
		)
	}
	return nil
}

func validateAgentSessionPrefixProjection(
	turn session.AgentTurn,
	state agent.LoopState,
) *contract.RuntimeError {
	safeEnd, _, _, err := agentProviderSafePrefix(
		state.Messages, state.BaseMessageCount,
	)
	projectedSafeEnd, _, _, projectedErr := agentProviderSafePrefix(
		turn.Messages, turn.BaseMessageCount,
	)
	if err != nil || projectedErr != nil ||
		projectedSafeEnd != len(turn.Messages) ||
		len(turn.Messages) > safeEnd ||
		!reflect.DeepEqual(
			turn.Messages, state.Messages[:len(turn.Messages)],
		) {
		return agentRecoveryError(
			"Agent Session messages are not a prefix of the checkpoint provider-safe projection",
		)
	}
	return nil
}

func (executor *AgentExecutor) validateExistingAgentSessionCancellation(
	record Record,
	turn session.AgentTurn,
) (agent.Outcome, error) {
	result := turn.ExistingResult
	if result == nil ||
		result.SessionID != record.Request.SessionID ||
		result.RunID != record.ID ||
		result.TurnID != turn.TurnID ||
		result.ExecutionID != turn.ExecutionID ||
		result.State != session.TurnCancelled ||
		result.CaptureQuality != session.CaptureStructured ||
		result.Message != nil ||
		result.Error == nil ||
		(result.Error.Code != contract.ErrorCancelled &&
			result.Error.Code != contract.ErrorTimeout) {
		return agent.Outcome{}, agentRecoveryError(
			"terminal Agent Session cancellation does not match the durable Run",
		)
	}
	if err := result.Error.Validate(); err != nil {
		return agent.Outcome{}, agentRecoveryError(
			"terminal Agent Session cancellation error is invalid: " +
				err.Error(),
		)
	}
	execution, err := executor.Sessions.Execution(
		record.Request.SessionID, turn.ExecutionID,
	)
	if err != nil {
		return agent.Outcome{}, agentRecoveryError(
			"load terminal Agent Session cancellation: " + err.Error(),
		)
	}
	if execution.State != session.ExecutionSettled ||
		execution.Outcome != session.OutcomeCancelled ||
		execution.SessionID != record.Request.SessionID ||
		execution.RunID != record.ID ||
		execution.TurnID != turn.TurnID ||
		execution.ID != turn.ExecutionID ||
		execution.ProfileID != record.Request.ProfileID ||
		execution.ProfileKind != profile.KindModel ||
		execution.RequestDigest != turn.RequestDigest ||
		execution.ConfigDigest != turn.ConfigDigest ||
		execution.Error == nil ||
		!reflect.DeepEqual(execution.Error, result.Error) {
		return agent.Outcome{}, agentRecoveryError(
			"terminal Agent Session cancellation evidence is inconsistent",
		)
	}
	sessionValue, err := executor.Sessions.Get(record.Request.SessionID)
	if err != nil {
		return agent.Outcome{}, agentRecoveryError(
			"load cancelled Agent Session: " + err.Error(),
		)
	}
	if sessionValue.State != session.SessionIdle ||
		sessionValue.ActiveTurnID != "" {
		return agent.Outcome{}, agentRecoveryError(
			"cancelled Agent Session remains active or blocked",
		)
	}
	events, err := executor.Sessions.Events(
		record.Request.SessionID, 0,
	)
	if err != nil {
		return agent.Outcome{}, agentRecoveryError(
			"load cancelled Agent Session events: " + err.Error(),
		)
	}
	var terminal *session.EventRecord
	for index := range events {
		event := events[index]
		if event.TurnID == turn.TurnID &&
			event.RunID == record.ID &&
			event.ExecutionID == turn.ExecutionID {
			current := event
			terminal = &current
		}
	}
	if terminal == nil ||
		terminal.Type != "agent.cancelled" ||
		terminal.State != string(session.TurnCancelled) ||
		!reflect.DeepEqual(terminal.Error, result.Error) {
		return agent.Outcome{}, agentRecoveryError(
			"terminal Agent Session cancellation event is missing or inconsistent",
		)
	}
	var outcome agent.Outcome
	if err := strictjson.DecodeObject(
		bytes.NewReader(terminal.Detail), 1<<20, &outcome,
	); err != nil {
		return agent.Outcome{}, agentRecoveryError(
			"decode terminal Agent Session cancellation outcome: " +
				err.Error(),
		)
	}
	if outcome.State != agent.StateCancelled ||
		outcome.StopReason != "cancelled" ||
		outcome.Message != nil ||
		outcome.Pause != nil ||
		!reflect.DeepEqual(outcome.Error, result.Error) {
		return agent.Outcome{}, agentRecoveryError(
			"terminal Agent Session cancellation outcome is inconsistent",
		)
	}
	return outcome, nil
}

func agentProviderSafePrefix(
	messages []contract.Message,
	baseMessageCount int,
) (safeEnd, openAssistant, completedCalls int, err error) {
	if baseMessageCount < 0 || baseMessageCount > len(messages) {
		return 0, -1, 0, fmt.Errorf(
			"agent base message boundary is invalid",
		)
	}
	safeEnd = baseMessageCount
	openAssistant = -1
	for index := baseMessageCount; index < len(messages); {
		assistant := messages[index]
		if assistant.Role != contract.RoleAssistant {
			return 0, -1, 0, fmt.Errorf(
				"agent message suffix contains a non-assistant round boundary",
			)
		}
		if len(assistant.ToolCalls) == 0 {
			safeEnd = index + 1
			index++
			if index != len(messages) {
				return 0, -1, 0, fmt.Errorf(
					"agent message suffix continues after a terminal assistant message",
				)
			}
			continue
		}
		cursor := index + 1
		for callIndex, call := range assistant.ToolCalls {
			if cursor >= len(messages) {
				return safeEnd, index, callIndex, nil
			}
			toolMessage := messages[cursor]
			if toolMessage.Role != contract.RoleTool ||
				toolMessage.ToolCallID != call.ID {
				return safeEnd, index, callIndex, nil
			}
			cursor++
		}
		safeEnd = cursor
		index = cursor
	}
	return safeEnd, openAssistant, completedCalls, nil
}

func validateAgentSessionState(
	record Record,
	turn session.AgentTurn,
	messages []contract.Message,
) *contract.RuntimeError {
	if turn.SessionID != record.Request.SessionID ||
		turn.RunID != record.ID ||
		turn.ProfileID != record.Request.ProfileID ||
		turn.ProfileKind != profile.KindModel ||
		!validSHA256Digest(turn.RequestDigest) ||
		!validSHA256Digest(turn.ConfigDigest) ||
		turn.TurnID == "" ||
		turn.ExecutionID == "" {
		return agentRecoveryError(
			"Agent Session correlation does not match the durable Run",
		)
	}
	if turn.BaseMessageCount < 0 ||
		turn.BaseMessageCount > len(turn.Messages) ||
		turn.BaseMessageCount > len(messages) {
		return agentRecoveryError(
			"Agent Session base message boundary is invalid",
		)
	}
	for index := 0; index < turn.BaseMessageCount; index++ {
		if !reflect.DeepEqual(turn.Messages[index], messages[index]) {
			return agentRecoveryError(
				"Agent checkpoint messages do not match the Session canonical prefix",
			)
		}
	}
	return nil
}

func (executor *AgentExecutor) validateExistingAgentSessionResult(
	record Record,
	turn session.AgentTurn,
	state agent.LoopState,
) *contract.RuntimeError {
	result := turn.ExistingResult
	if result == nil {
		return nil
	}
	if result.SessionID != record.Request.SessionID ||
		result.RunID != record.ID ||
		result.TurnID != turn.TurnID ||
		result.ExecutionID != turn.ExecutionID ||
		result.CaptureQuality != session.CaptureStructured {
		return agentRecoveryError(
			"terminal Agent Session result does not match the durable Run",
		)
	}
	safeEnd, _, _, err := agentProviderSafePrefix(
		state.Messages, state.BaseMessageCount,
	)
	if err != nil ||
		safeEnd != len(turn.Messages) ||
		!reflect.DeepEqual(
			turn.Messages, state.Messages[:safeEnd],
		) {
		return agentRecoveryError(
			"terminal Agent Session messages do not match the checkpoint provider-safe prefix",
		)
	}
	execution, err := executor.Sessions.Execution(
		record.Request.SessionID, turn.ExecutionID,
	)
	if err != nil {
		return agentRecoveryError(
			"load terminal Agent Session execution: " + err.Error(),
		)
	}
	if execution.State != session.ExecutionSettled ||
		execution.SessionID != record.Request.SessionID ||
		execution.RunID != record.ID ||
		execution.TurnID != turn.TurnID ||
		execution.ID != turn.ExecutionID ||
		execution.ProfileID != record.Request.ProfileID ||
		execution.ProfileKind != profile.KindModel ||
		execution.RequestDigest != turn.RequestDigest ||
		execution.ConfigDigest != turn.ConfigDigest {
		return agentRecoveryError(
			"terminal Agent Session execution does not match the durable Run",
		)
	}
	if state.TerminalOutcome == nil {
		return agentRecoveryError(
			"terminal Agent Session outcome does not match Agent checkpoint evidence",
		)
	}
	switch state.TerminalOutcome.State {
	case agent.StateCompleted:
		if safeEnd != len(state.Messages) ||
			result.State != session.TurnCompleted ||
			execution.Outcome != session.OutcomeCompleted ||
			result.Error != nil ||
			execution.Error != nil ||
			state.TerminalOutcome.Error != nil ||
			state.TerminalOutcome.Message == nil ||
			!reflect.DeepEqual(
				result.Message, state.TerminalOutcome.Message,
			) {
			return agentRecoveryError(
				"completed Agent Session outcome does not match checkpoint evidence",
			)
		}
	case agent.StateFailed:
		if result.State != session.TurnFailed ||
			execution.Outcome != session.OutcomeFailed ||
			result.Message != nil ||
			state.TerminalOutcome.Error == nil ||
			!reflect.DeepEqual(
				result.Error, state.TerminalOutcome.Error,
			) ||
			!reflect.DeepEqual(
				execution.Error, state.TerminalOutcome.Error,
			) {
			return agentRecoveryError(
				"failed Agent Session outcome does not match checkpoint evidence",
			)
		}
	case agent.StateCancelled:
		if result.State != session.TurnCancelled ||
			execution.Outcome != session.OutcomeCancelled ||
			result.Message != nil ||
			state.TerminalOutcome.Error == nil ||
			!reflect.DeepEqual(
				result.Error, state.TerminalOutcome.Error,
			) ||
			!reflect.DeepEqual(
				execution.Error, state.TerminalOutcome.Error,
			) {
			return agentRecoveryError(
				"cancelled Agent Session outcome does not match checkpoint evidence",
			)
		}
	default:
		return agentRecoveryError(
			"terminal Agent Session is not backed by a completed, failed, or cancelled checkpoint",
		)
	}
	return nil
}
