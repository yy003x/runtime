package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"time"

	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/profile"
)

type AgentTurn struct {
	SessionID        string             `json:"session_id"`
	TurnID           string             `json:"turn_id"`
	RunID            string             `json:"run_id"`
	ExecutionID      string             `json:"execution_id"`
	ProfileID        string             `json:"profile_id"`
	ProfileKind      profile.Kind       `json:"profile_kind"`
	RequestDigest    string             `json:"request_digest"`
	ConfigDigest     string             `json:"config_digest"`
	BaseMessageCount int                `json:"base_message_count"`
	Messages         []contract.Message `json:"messages"`
	ExistingResult   *RunResult         `json:"existing_result,omitempty"`
	ProjectedPause   *agent.Pause       `json:"projected_pause,omitempty"`
}

func (service *Service) PrepareAgent(
	request RunRequest,
) (AgentTurn, *contract.RuntimeError) {
	entry, exists := service.profiles.Resolve(request.ProfileID)
	if !exists || entry.Kind != profile.KindModel {
		return AgentTurn{}, sessionRuntimeError(
			contract.ErrorInvalidRequest,
			"agent Session requires an API model profile",
		)
	}
	if request.SessionID == "" || request.RunID == "" {
		return AgentTurn{}, sessionRuntimeError(
			contract.ErrorInvalidRequest,
			"agent Session requires session_id and run_id",
		)
	}
	prepared, runtimeErr := service.prepareRunRequest(request, entry)
	if runtimeErr != nil {
		return AgentTurn{}, runtimeErr
	}
	request = prepared
	existing, found, runtimeErr := service.findAgentTurn(
		request.SessionID, request.RunID, false,
	)
	if runtimeErr != nil {
		return existing, runtimeErr
	}
	if found {
		if existing.ExistingResult == nil {
			if err := service.markAgentTurnStarted(existing); err != nil {
				return AgentTurn{}, sessionRuntimeError(
					contract.ErrorInternal, err.Error(),
				)
			}
		}
		return existing, runtimeErr
	}
	ids, err := service.newExecutionIDs(request.SessionID, request.RunID)
	if err != nil {
		return AgentTurn{}, sessionRuntimeError(contract.ErrorInvalidRequest, err.Error())
	}
	started, runtimeErr := service.begin(ids, request, entry)
	if runtimeErr != nil {
		return AgentTurn{}, runtimeErr
	}
	if err := service.markExecutionRunning(ids); err != nil {
		runtimeErr := sessionRuntimeError(contract.ErrorInternal, err.Error())
		service.finishFailure(ids, runtimeErr)
		return AgentTurn{}, runtimeErr
	}
	result := AgentTurn{
		SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
		ExecutionID: ids.execution, ProfileID: entry.ID,
		ProfileKind:      entry.Kind,
		RequestDigest:    requestDigest(request),
		ConfigDigest:     requestConfigDigest(request, entry),
		BaseMessageCount: len(started.projection.modelMessages),
		Messages:         cloneMessages(started.projection.modelMessages),
	}
	if err := service.markAgentTurnStarted(result); err != nil {
		runtimeErr := sessionRuntimeError(contract.ErrorInternal, err.Error())
		service.finishFailure(ids, runtimeErr)
		return AgentTurn{}, runtimeErr
	}
	return result, nil
}

// RecoverAgent resolves an already registered Agent-owned Turn without ever
// creating a replacement. Durable Run recovery must use this path so missing
// Session evidence cannot be silently reconstructed from a checkpoint.
func (service *Service) RecoverAgent(
	request RunRequest,
) (AgentTurn, *contract.RuntimeError) {
	existing, found, runtimeErr := service.LookupAgent(request)
	if runtimeErr != nil {
		return AgentTurn{}, runtimeErr
	}
	if !found {
		return AgentTurn{}, sessionRuntimeError(
			contract.ErrorConflict,
			"durable Agent Run has no matching Session Turn",
		)
	}
	return existing, nil
}

// LookupAgent resolves Agent-owned Session facts without creating, activating,
// or otherwise mutating a Turn.
func (service *Service) LookupAgent(
	request RunRequest,
) (AgentTurn, bool, *contract.RuntimeError) {
	if request.SessionID == "" || request.RunID == "" {
		return AgentTurn{}, false, sessionRuntimeError(
			contract.ErrorConflict,
			"agent Session recovery requires session_id and run_id",
		)
	}
	existing, found, runtimeErr := service.findAgentTurn(
		request.SessionID, request.RunID, true,
	)
	if runtimeErr != nil {
		return AgentTurn{}, found, runtimeErr
	}
	if !found {
		return AgentTurn{}, false, nil
	}
	if existing.SessionID != request.SessionID ||
		existing.RunID != request.RunID ||
		existing.ProfileID != request.ProfileID ||
		existing.TurnID == "" ||
		existing.ExecutionID == "" {
		return AgentTurn{}, true, sessionRuntimeError(
			contract.ErrorConflict,
			"Agent Session recovery correlation does not match",
		)
	}
	return existing, true, nil
}

func (service *Service) findAgentTurn(
	sessionID, runID string,
	recovering bool,
) (AgentTurn, bool, *contract.RuntimeError) {
	var result AgentTurn
	found := false
	var runtimeErr *contract.RuntimeError
	err := service.store.withLock(sessionID, func() error {
		entries, err := service.store.sessionDirectoryEntries(
			sessionID, "turns",
		)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, directory := range entries {
			if !directory.isDirectory() {
				continue
			}
			turn, err := service.store.loadTurn(sessionID, directory.name)
			if err != nil {
				return err
			}
			if turn.RunID != runID {
				continue
			}
			if found {
				runtimeErr = sessionRuntimeError(
					contract.ErrorConflict,
					"multiple Session Turns reuse the same durable run_id",
				)
				return nil
			}
			found = true
			if turn.ProfileKind != profile.KindModel {
				runtimeErr = sessionRuntimeError(
					contract.ErrorConflict,
					"run_id belongs to a non-model Session turn",
				)
				return nil
			}
			sessionValue, err := service.store.loadSession(sessionID)
			if err != nil {
				return err
			}
			execution, err := service.store.loadExecution(
				sessionID, turn.ExecutionID,
			)
			if err != nil {
				return err
			}
			if !agentExecutionMatchesTurn(execution, turn) {
				runtimeErr = sessionRuntimeError(
					contract.ErrorConflict,
					"Agent Session Turn and Execution facts do not match",
				)
				return nil
			}
			records, err := service.store.messages(sessionID)
			if err != nil {
				return err
			}
			messages := make([]contract.Message, 0, len(records))
			var lastAssistant *contract.Message
			currentTurnMessages := 0
			for _, record := range records {
				message := cloneContractMessage(record.Message)
				messages = append(messages, message)
				if record.TurnID == turn.ID {
					currentTurnMessages++
				}
				if record.TurnID == turn.ID &&
					message.Role == contract.RoleAssistant {
					current := cloneContractMessage(message)
					lastAssistant = &current
				}
			}
			if offset, err := compactionOffset(service, sessionID, turn.ID, records); err != nil {
				return err
			} else if offset > 0 {
				// The turn was prepared from a compacted projection; reconstruct
				// the same compacted message set so recovery matches the frozen
				// checkpoint. currentTurnMessages is unaffected because the
				// current turn always lives in the kept tail.
				messages = messages[offset:]
			}
			result = AgentTurn{
				SessionID: sessionID, TurnID: turn.ID, RunID: runID,
				ExecutionID: turn.ExecutionID, ProfileID: turn.ProfileID,
				ProfileKind:      turn.ProfileKind,
				RequestDigest:    turn.RequestDigest,
				ConfigDigest:     turn.ConfigDigest,
				BaseMessageCount: len(messages) - max(currentTurnMessages-1, 0),
				Messages:         messages,
			}
			switch turn.State {
			case TurnCompleted, TurnFailed, TurnCancelled:
				existing := service.resultFromTurn(turn, lastAssistant)
				result.ExistingResult = &existing
			case TurnRequiresAction:
				if !recovering {
					runtimeErr = sessionRuntimeError(
						contract.ErrorConflict,
						"agent turn is paused and requires resume input",
					)
					return nil
				}
				pause, err := service.validateProjectedAgentPause(
					sessionValue, turn, false,
				)
				if err != nil {
					runtimeErr = sessionRuntimeError(
						contract.ErrorConflict, err.Error(),
					)
					return nil
				}
				result.ProjectedPause = &pause
				return nil
			case TurnRunning:
			default:
				runtimeErr = sessionRuntimeError(
					contract.ErrorConflict,
					fmt.Sprintf("agent turn has unsupported state %q", turn.State),
				)
			}
			return nil
		}
		return nil
	})
	if err != nil {
		return AgentTurn{}, false, sessionRuntimeError(
			contract.ErrorInternal, err.Error(),
		)
	}
	return result, found, runtimeErr
}

func (service *Service) validateProjectedAgentPause(
	sessionValue Session,
	turn Turn,
	activated bool,
) (agent.Pause, error) {
	stateMatches := turn.State == TurnRequiresAction &&
		sessionValue.State == SessionBlocked &&
		sessionValue.ActiveTurnID == ""
	if activated {
		stateMatches = turn.State == TurnRunning &&
			(sessionValue.State == SessionActive ||
				sessionValue.State == SessionBlocked) &&
			sessionValue.ActiveTurnID == turn.ID
	}
	if !turn.AgentOwned ||
		turn.ProfileKind != profile.KindModel ||
		turn.CaptureQuality != CaptureStructured ||
		turn.Error != nil ||
		!stateMatches {
		return agent.Pause{}, fmt.Errorf(
			"Agent paused Session facts are incomplete or inconsistent",
		)
	}
	execution, err := service.store.loadExecution(
		turn.SessionID, turn.ExecutionID,
	)
	if err != nil {
		return agent.Pause{}, err
	}
	if execution.ID != turn.ExecutionID ||
		execution.SessionID != turn.SessionID ||
		execution.TurnID != turn.ID ||
		execution.RunID != turn.RunID ||
		execution.ProfileID != turn.ProfileID ||
		execution.ProfileKind != profile.KindModel ||
		execution.State != ExecutionSettled ||
		execution.Outcome != OutcomeCompleted ||
		execution.Error != nil {
		return agent.Pause{}, fmt.Errorf(
			"Agent paused execution facts do not match the Session Turn",
		)
	}
	events, err := service.store.events(turn.SessionID)
	if err != nil {
		return agent.Pause{}, err
	}
	var latest *EventRecord
	var pauses []agent.Pause
	for index := range events {
		event := events[index]
		if event.TurnID != turn.ID ||
			event.RunID != turn.RunID ||
			event.ExecutionID != turn.ExecutionID {
			continue
		}
		current := event
		latest = &current
		if event.Type != "agent.paused" {
			continue
		}
		if event.State != string(TurnRequiresAction) ||
			event.Error != nil {
			return agent.Pause{}, fmt.Errorf(
				"Agent paused event envelope is inconsistent",
			)
		}
		var outcome agent.Outcome
		if err := decodeStrict(event.Detail, &outcome); err != nil {
			return agent.Pause{}, fmt.Errorf(
				"decode Agent paused event detail: %w", err,
			)
		}
		if outcome.State != agent.StatePaused ||
			outcome.StopReason != "input_required" ||
			outcome.Pause == nil ||
			outcome.Error != nil ||
			outcome.Message != nil {
			return agent.Pause{}, fmt.Errorf(
				"Agent paused event detail is inconsistent",
			)
		}
		if err := agent.ValidatePause(*outcome.Pause); err != nil {
			return agent.Pause{}, fmt.Errorf(
				"Agent paused event detail: %w", err,
			)
		}
		pauses = append(pauses, *outcome.Pause)
	}
	if latest == nil || latest.Type != "agent.paused" ||
		len(pauses) == 0 {
		return agent.Pause{}, fmt.Errorf(
			"Agent paused Session has no final paused event",
		)
	}
	current := pauses[len(pauses)-1]
	matches := 0
	for _, pause := range pauses {
		if reflect.DeepEqual(pause, current) {
			matches++
		}
	}
	if matches != 1 {
		return agent.Pause{}, fmt.Errorf(
			"Agent paused Session has duplicate matching pause events",
		)
	}
	return current, nil
}

// ActivateAgentResume is the only mutation that releases a provider-safe
// paused projection for resumed execution. Callers must first validate the
// durable resume envelope against the Agent checkpoint.
func (service *Service) ActivateAgentResume(
	turn AgentTurn,
) *contract.RuntimeError {
	err := service.store.withLock(turn.SessionID, func() error {
		sessionValue, err := service.store.loadSession(turn.SessionID)
		if err != nil {
			return err
		}
		turnValue, err := service.store.loadTurn(
			turn.SessionID, turn.TurnID,
		)
		if err != nil {
			return err
		}
		if !agentTurnFactsMatch(turnValue, turn) ||
			!turnValue.AgentOwned {
			return fmt.Errorf(
				"%w: Agent resume correlation does not match",
				ErrConflict,
			)
		}
		activated := turnValue.State == TurnRunning &&
			sessionValue.State == SessionActive &&
			sessionValue.ActiveTurnID == turn.TurnID
		if !activated &&
			(turnValue.State != TurnRequiresAction ||
				sessionValue.State != SessionBlocked ||
				sessionValue.ActiveTurnID != "") {
			return fmt.Errorf(
				"%w: Agent Session is not awaiting resume activation",
				ErrConflict,
			)
		}
		pause, err := service.validateProjectedAgentPause(
			sessionValue, turnValue, activated,
		)
		if err != nil {
			return err
		}
		if turn.ProjectedPause == nil ||
			!reflect.DeepEqual(turn.ProjectedPause, &pause) {
			return fmt.Errorf(
				"%w: Agent resume pause evidence changed before activation",
				ErrConflict,
			)
		}
		if activated {
			return nil
		}
		now := service.now().UTC()
		turnValue.State = TurnRunning
		turnValue.UpdatedAt = now
		sessionValue.State = SessionActive
		sessionValue.ActiveTurnID = turn.TurnID
		sessionValue.UpdatedAt = now
		if err := service.store.writeTurn(turnValue); err != nil {
			return err
		}
		return service.store.writeSession(sessionValue)
	})
	if err != nil {
		code := contract.ErrorInternal
		if errors.Is(err, ErrConflict) {
			code = contract.ErrorConflict
		}
		return sessionRuntimeError(code, err.Error())
	}
	return nil
}

// RestoreAgentPause rolls back only the Session projection of an accepted
// Agent resume when execution is still durably paused. It is idempotent for
// an already restored projection and also accepts the crash-reconciled state
// where the Turn remains running while the Session has been re-blocked.
//
// The original agent.paused event and settled Execution remain authoritative;
// this method does not append a duplicate event or rewrite the Execution.
func (service *Service) RestoreAgentPause(
	turn AgentTurn,
	expected agent.Pause,
) *contract.RuntimeError {
	err := service.store.withLock(turn.SessionID, func() error {
		sessionValue, err := service.store.loadSession(turn.SessionID)
		if err != nil {
			return err
		}
		turnValue, err := service.store.loadTurn(
			turn.SessionID, turn.TurnID,
		)
		if err != nil {
			return err
		}
		if !agentTurnFactsMatch(turnValue, turn) ||
			!turnValue.AgentOwned {
			return fmt.Errorf(
				"%w: Agent pause restoration correlation does not match",
				ErrConflict,
			)
		}
		restored := turnValue.State == TurnRequiresAction &&
			sessionValue.State == SessionBlocked &&
			sessionValue.ActiveTurnID == ""
		activated := turnValue.State == TurnRunning &&
			(sessionValue.State == SessionActive ||
				sessionValue.State == SessionBlocked) &&
			sessionValue.ActiveTurnID == turn.TurnID
		if !restored && !activated {
			return fmt.Errorf(
				"%w: Agent Session cannot restore its paused projection",
				ErrConflict,
			)
		}
		pause, err := service.validateProjectedAgentPause(
			sessionValue, turnValue, activated,
		)
		if err != nil {
			return err
		}
		if !reflect.DeepEqual(&expected, &pause) {
			return fmt.Errorf(
				"%w: Agent pause evidence changed before restoration",
				ErrConflict,
			)
		}
		if restored {
			return nil
		}
		now := service.now().UTC()
		turnValue.State = TurnRequiresAction
		turnValue.UpdatedAt = now
		sessionValue.State = SessionBlocked
		sessionValue.ActiveTurnID = ""
		sessionValue.UpdatedAt = now
		if err := service.store.writeTurn(turnValue); err != nil {
			return err
		}
		return service.store.writeSession(sessionValue)
	})
	if err != nil {
		code := contract.ErrorInternal
		if errors.Is(err, ErrConflict) {
			code = contract.ErrorConflict
		}
		return sessionRuntimeError(code, err.Error())
	}
	return nil
}

// compactionOffset returns the number of leading canonical messages a turn's
// context compaction dropped. 0 means no compaction: the prepared prefix is a
// verbatim canonical prefix and callers use the legacy comparison. When the
// turn manifest carries a CheckpointRef it loads the grounding summary and
// re-verifies the dropped-prefix digest, fail-closing on any drift.
func compactionOffset(
	service *Service,
	sessionID, turnID string,
	records []MessageRecord,
) (int, error) {
	manifest, err := service.store.loadManifest(sessionID, turnID)
	if err != nil {
		return 0, err
	}
	if manifest.CheckpointRef == "" {
		return 0, nil
	}
	summary, err := service.store.summaryByID(sessionID, manifest.CheckpointRef)
	if err != nil {
		return 0, err
	}
	offset, err := verifyCompaction(records, *summary)
	if err != nil {
		return 0, fmt.Errorf("%w: %v", ErrConflict, err)
	}
	return offset, nil
}

func (service *Service) SettleAgent(
	turn AgentTurn,
	messages []contract.Message,
	outcome agent.Outcome,
) (RunResult, *contract.RuntimeError) {
	result := RunResult{
		SessionID: turn.SessionID, TurnID: turn.TurnID, RunID: turn.RunID,
		ExecutionID: turn.ExecutionID, CaptureQuality: CaptureStructured,
	}
	if turn.BaseMessageCount < 0 ||
		turn.BaseMessageCount > len(turn.Messages) ||
		turn.BaseMessageCount > len(messages) {
		runtimeErr := sessionRuntimeError(
			contract.ErrorConflict,
			"agent Session base message boundary is invalid",
		)
		result.State = TurnFailed
		result.Error = runtimeErr
		return result, runtimeErr
	}
	for index := 0; index < turn.BaseMessageCount; index++ {
		if !messagesEqual(turn.Messages[index], messages[index]) {
			runtimeErr := sessionRuntimeError(
				contract.ErrorConflict,
				"agent Session messages do not match the prepared prefix",
			)
			result.State = TurnFailed
			result.Error = runtimeErr
			return result, runtimeErr
		}
	}
	safeEnd, openAssistant, completedCalls, projectionErr :=
		agentProviderSafePrefix(messages, turn.BaseMessageCount)
	if projectionErr != nil {
		runtimeErr := sessionRuntimeError(
			contract.ErrorConflict, projectionErr.Error(),
		)
		result.State = TurnFailed
		result.Error = runtimeErr
		return result, runtimeErr
	}
	projectedMessages := messages[:safeEnd]
	if outcome.State == agent.StatePaused {
		if outcome.Pause == nil || openAssistant < 0 ||
			completedCalls >=
				len(messages[openAssistant].ToolCalls) ||
			messages[openAssistant].ToolCalls[completedCalls].ID !=
				outcome.Pause.ToolCallID {
			runtimeErr := sessionRuntimeError(
				contract.ErrorConflict,
				"paused Agent outcome does not match the open provider tool-call boundary",
			)
			result.State = TurnFailed
			result.Error = runtimeErr
			return result, runtimeErr
		}
	} else {
		if outcome.State == agent.StateCompleted && safeEnd != len(messages) {
			runtimeErr := sessionRuntimeError(
				contract.ErrorConflict,
				"completed Agent outcome leaves an incomplete provider tool-call round",
			)
			result.State = TurnFailed
			result.Error = runtimeErr
			return result, runtimeErr
		}
	}
	outcomeErr := outcome.Error
	if outcome.State == agent.StateNeedsReconciliation && outcomeErr == nil {
		outcomeErr = sessionRuntimeError(
			contract.ErrorConflict,
			"Agent tool effect outcome is unknown",
		)
	}
	err := service.store.withLock(turn.SessionID, func() error {
		sessionValue, err := service.store.loadSession(turn.SessionID)
		if err != nil {
			return err
		}
		turnValue, err := service.store.loadTurn(turn.SessionID, turn.TurnID)
		if err != nil {
			return err
		}
		if !agentTurnFactsMatch(turnValue, turn) {
			return fmt.Errorf(
				"%w: agent Session correlation does not match",
				ErrConflict,
			)
		}
		records, err := service.store.messages(turn.SessionID)
		if err != nil {
			return err
		}
		canonicalMessages := make([]contract.Message, 0, len(records))
		existingCurrent := make([]contract.Message, 0)
		for _, record := range records {
			message := cloneContractMessage(record.Message)
			canonicalMessages = append(canonicalMessages, message)
			if record.TurnID == turn.TurnID {
				existingCurrent = append(
					existingCurrent, cloneContractMessage(message),
				)
			}
		}
		offset, compactionErr := compactionOffset(service, turn.SessionID, turn.TurnID, records)
		if compactionErr != nil {
			return compactionErr
		}
		if turn.BaseMessageCount+offset > len(canonicalMessages) {
			return fmt.Errorf(
				"%w: agent Session canonical prefix is shorter than its prepared boundary",
				ErrConflict,
			)
		}
		for index := 0; index < turn.BaseMessageCount; index++ {
			if !messagesEqual(
				canonicalMessages[offset+index], turn.Messages[index],
			) || !messagesEqual(canonicalMessages[offset+index], messages[index]) {
				return fmt.Errorf(
					"%w: agent Session canonical prefix does not match",
					ErrConflict,
				)
			}
		}
		baseCurrent := 1
		expectedSuffix := projectedMessages[turn.BaseMessageCount:]
		alreadyAppended := len(existingCurrent) - baseCurrent
		if alreadyAppended < 0 || alreadyAppended > len(expectedSuffix) {
			return fmt.Errorf(
				"%w: agent Session message projection is inconsistent",
				ErrConflict,
			)
		}
		for index := 0; index < alreadyAppended; index++ {
			if !messagesEqual(existingCurrent[baseCurrent+index], expectedSuffix[index]) {
				return fmt.Errorf(
					"%w: agent Session replay conflicts with persisted message",
					ErrConflict,
				)
			}
		}
		now := service.now().UTC()
		for _, message := range expectedSuffix[alreadyAppended:] {
			if err := service.store.appendMessage(&sessionValue, MessageRecord{
				Time: now, TurnID: turn.TurnID, RunID: turn.RunID,
				ExecutionID: turn.ExecutionID, ProfileID: turn.ProfileID,
				Message: cloneContractMessage(message),
			}); err != nil {
				return err
			}
		}
		turnValue.CaptureQuality = CaptureStructured
		turnValue.UpdatedAt = now
		sessionValue.UpdatedAt = now
		eventType := "agent.completed"
		executionOutcome := OutcomeCompleted
		switch outcome.State {
		case agent.StateCompleted:
			turnValue.State = TurnCompleted
			sessionValue.State = SessionIdle
			sessionValue.ActiveTurnID = ""
			result.State = TurnCompleted
		case agent.StatePaused:
			turnValue.State = TurnRequiresAction
			sessionValue.State = SessionBlocked
			sessionValue.ActiveTurnID = ""
			result.State = TurnRequiresAction
			eventType = "agent.paused"
		case agent.StateCancelled:
			turnValue.State = TurnCancelled
			sessionValue.State = SessionIdle
			sessionValue.ActiveTurnID = ""
			result.State = TurnCancelled
			eventType = "agent.cancelled"
			executionOutcome = OutcomeCancelled
		case agent.StateNeedsReconciliation:
			// Unknown tool effects are not pending tool input. Keep the Turn
			// active and blocked so explicit Run reconciliation is the only
			// path that can acknowledge and close the unknown outcome.
			turnValue.State = TurnRunning
			sessionValue.State = SessionBlocked
			sessionValue.ActiveTurnID = turnValue.ID
			result.State = TurnRunning
			eventType = "agent.needs_reconciliation"
			executionOutcome = OutcomeUnknown
		default:
			turnValue.State = TurnFailed
			sessionValue.State = SessionIdle
			sessionValue.ActiveTurnID = ""
			result.State = TurnFailed
			eventType = "agent.failed"
			executionOutcome = OutcomeFailed
		}
		turnValue.Error = outcomeErr
		if outcome.State == agent.StateCompleted {
			for index := safeEnd - 1; index >= turn.BaseMessageCount; index-- {
				if projectedMessages[index].Role == contract.RoleAssistant {
					current := cloneContractMessage(projectedMessages[index])
					result.Message = &current
					break
				}
			}
		}
		detail, _ := json.Marshal(outcome)
		if err := service.store.appendEvent(&sessionValue, EventRecord{
			Time: now, Type: eventType, TurnID: turn.TurnID,
			RunID: turn.RunID, ExecutionID: turn.ExecutionID,
			State: string(turnValue.State), Error: outcomeErr, Detail: detail,
		}); err != nil {
			return err
		}
		execution, err := service.store.loadExecution(
			turn.SessionID, turn.ExecutionID,
		)
		if err != nil {
			return err
		}
		if !agentExecutionMatchesTurn(execution, turnValue) {
			return fmt.Errorf(
				"%w: agent Session execution facts do not match",
				ErrConflict,
			)
		}
		execution.State = ExecutionSettled
		execution.Outcome = executionOutcome
		execution.Error = outcomeErr
		execution.UpdatedAt = now
		if err := service.store.writeExecution(execution); err != nil {
			return err
		}
		if err := service.store.writeTurn(turnValue); err != nil {
			return err
		}
		return service.store.writeSession(sessionValue)
	})
	if err != nil {
		code := contract.ErrorInternal
		if errors.Is(err, ErrConflict) {
			code = contract.ErrorConflict
		}
		runtimeErr := sessionRuntimeError(code, err.Error())
		result.State = TurnFailed
		result.Error = runtimeErr
		return result, runtimeErr
	}
	result.Error = outcomeErr
	return result, nil
}

// agentProviderSafePrefix returns the longest prefix in which every projected
// assistant tool-call message is immediately closed by one tool message per
// call, in declared order. The one open assistant boundary is returned
// separately so a paused Session may expose it while remaining blocked.
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

func cloneMessages(values []contract.Message) []contract.Message {
	result := make([]contract.Message, len(values))
	for index, value := range values {
		result[index] = cloneContractMessage(value)
	}
	return result
}

func messagesEqual(left, right contract.Message) bool {
	leftJSON, _ := json.Marshal(left)
	rightJSON, _ := json.Marshal(right)
	return string(leftJSON) == string(rightJSON)
}

func agentTurnFactsMatch(value Turn, expected AgentTurn) bool {
	return value.ID == expected.TurnID &&
		value.SessionID == expected.SessionID &&
		value.RunID == expected.RunID &&
		value.ExecutionID == expected.ExecutionID &&
		value.ProfileID == expected.ProfileID &&
		value.ProfileKind == expected.ProfileKind &&
		value.ProfileKind == profile.KindModel &&
		value.RequestDigest != "" &&
		value.RequestDigest == expected.RequestDigest &&
		value.ConfigDigest != "" &&
		value.ConfigDigest == expected.ConfigDigest
}

func agentExecutionMatchesTurn(value Execution, turn Turn) bool {
	return value.ID == turn.ExecutionID &&
		value.SessionID == turn.SessionID &&
		value.TurnID == turn.ID &&
		value.RunID == turn.RunID &&
		value.ProfileID == turn.ProfileID &&
		value.ProfileKind == turn.ProfileKind &&
		value.RequestDigest == turn.RequestDigest &&
		value.ConfigDigest == turn.ConfigDigest
}

func (service *Service) markAgentTurnStarted(turn AgentTurn) error {
	return service.store.withLock(turn.SessionID, func() error {
		turnValue, err := service.store.loadTurn(turn.SessionID, turn.TurnID)
		if err != nil {
			return err
		}
		if !agentTurnFactsMatch(turnValue, turn) {
			return fmt.Errorf("agent Session correlation does not match")
		}
		if turnValue.AgentOwned {
			return nil
		}
		turnValue.AgentOwned = true
		turnValue.UpdatedAt = service.now().UTC()
		return service.store.writeTurn(turnValue)
	})
}

func (service *Service) touchAgentTurn(
	turn AgentTurn,
	state TurnState,
	sessionState SessionState,
	now time.Time,
) error {
	return service.store.withLock(turn.SessionID, func() error {
		sessionValue, err := service.store.loadSession(turn.SessionID)
		if err != nil {
			return err
		}
		turnValue, err := service.store.loadTurn(turn.SessionID, turn.TurnID)
		if err != nil {
			return err
		}
		turnValue.State = state
		turnValue.UpdatedAt = now
		sessionValue.State = sessionState
		sessionValue.UpdatedAt = now
		if state == TurnRunning {
			sessionValue.ActiveTurnID = turn.TurnID
		} else {
			sessionValue.ActiveTurnID = ""
		}
		if err := service.store.writeTurn(turnValue); err != nil {
			return err
		}
		return service.store.writeSession(sessionValue)
	})
}
