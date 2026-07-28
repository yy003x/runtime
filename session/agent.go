package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"time"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/profile"
)

type AgentTurn struct {
	SessionID        string             `json:"session_id"`
	TurnID           string             `json:"turn_id"`
	RunID            string             `json:"run_id"`
	ExecutionID      string             `json:"execution_id"`
	ProfileID        string             `json:"profile_id"`
	BaseMessageCount int                `json:"base_message_count"`
	Messages         []contract.Message `json:"messages"`
	ExistingResult   *RunResult         `json:"existing_result,omitempty"`
}

func (service *Service) PrepareAgent(
	request RunRequest,
	resuming bool,
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
	existing, found, runtimeErr := service.findAgentTurn(
		request.SessionID, request.RunID, resuming,
	)
	if runtimeErr != nil || found {
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
	return AgentTurn{
		SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
		ExecutionID: ids.execution, ProfileID: entry.ID,
		BaseMessageCount: len(started.projection.modelMessages),
		Messages:         cloneMessages(started.projection.modelMessages),
	}, nil
}

func (service *Service) findAgentTurn(
	sessionID, runID string,
	resuming bool,
) (AgentTurn, bool, *contract.RuntimeError) {
	var result AgentTurn
	found := false
	var runtimeErr *contract.RuntimeError
	err := service.store.withLock(sessionID, func() error {
		turnsDir := filepath.Join(service.store.sessionDir(sessionID), "turns")
		entries, err := os.ReadDir(turnsDir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, directory := range entries {
			if !directory.IsDir() || directory.Type()&os.ModeSymlink != 0 {
				continue
			}
			turn, err := service.store.loadTurn(sessionID, directory.Name())
			if err != nil {
				return err
			}
			if turn.RunID != runID {
				continue
			}
			found = true
			if turn.ProfileKind != profile.KindModel {
				runtimeErr = sessionRuntimeError(
					contract.ErrorConflict,
					"run_id belongs to a non-model Session turn",
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
				messages = append(messages, cloneContractMessage(record.Message))
				if record.TurnID == turn.ID {
					currentTurnMessages++
				}
				if record.TurnID == turn.ID &&
					record.Message.Role == contract.RoleAssistant {
					current := cloneContractMessage(record.Message)
					lastAssistant = &current
				}
			}
			result = AgentTurn{
				SessionID: sessionID, TurnID: turn.ID, RunID: runID,
				ExecutionID: turn.ExecutionID, ProfileID: turn.ProfileID,
				BaseMessageCount: len(messages) - max(currentTurnMessages-1, 0),
				Messages:         messages,
			}
			switch turn.State {
			case TurnCompleted, TurnFailed, TurnCancelled, TurnSubmitted:
				existing := service.resultFromTurn(turn, lastAssistant)
				result.ExistingResult = &existing
			case TurnRequiresAction:
				if !resuming {
					runtimeErr = sessionRuntimeError(
						contract.ErrorConflict,
						"agent turn is paused and requires resume input",
					)
					return nil
				}
				sessionValue, err := service.store.loadSession(sessionID)
				if err != nil {
					return err
				}
				now := service.now().UTC()
				turn.State = TurnRunning
				turn.UpdatedAt = now
				sessionValue.State = SessionActive
				sessionValue.ActiveTurnID = turn.ID
				sessionValue.UpdatedAt = now
				if err := service.store.writeTurn(turn); err != nil {
					return err
				}
				if err := service.store.writeSession(sessionValue); err != nil {
					return err
				}
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

func (service *Service) SettleAgent(
	turn AgentTurn,
	messages []contract.Message,
	outcome agent.Outcome,
) (RunResult, *contract.RuntimeError) {
	result := RunResult{
		SessionID: turn.SessionID, TurnID: turn.TurnID, RunID: turn.RunID,
		ExecutionID: turn.ExecutionID, CaptureQuality: CaptureStructured,
	}
	var runtimeErr *contract.RuntimeError
	err := service.store.withLock(turn.SessionID, func() error {
		sessionValue, err := service.store.loadSession(turn.SessionID)
		if err != nil {
			return err
		}
		turnValue, err := service.store.loadTurn(turn.SessionID, turn.TurnID)
		if err != nil {
			return err
		}
		if turnValue.RunID != turn.RunID || turnValue.ExecutionID != turn.ExecutionID {
			return fmt.Errorf("agent Session correlation does not match")
		}
		records, err := service.store.messages(turn.SessionID)
		if err != nil {
			return err
		}
		existingCurrent := make([]contract.Message, 0)
		for _, record := range records {
			if record.TurnID == turn.TurnID {
				existingCurrent = append(
					existingCurrent, cloneContractMessage(record.Message),
				)
			}
		}
		baseCurrent := 1
		expectedSuffix := messages[turn.BaseMessageCount:]
		alreadyAppended := len(existingCurrent) - baseCurrent
		if alreadyAppended < 0 || alreadyAppended > len(expectedSuffix) {
			return fmt.Errorf("agent Session message projection is inconsistent")
		}
		for index := 0; index < alreadyAppended; index++ {
			if !messagesEqual(existingCurrent[baseCurrent+index], expectedSuffix[index]) {
				return fmt.Errorf("agent Session replay conflicts with persisted message")
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
		sessionValue.ActiveTurnID = ""
		sessionValue.UpdatedAt = now
		eventType := "agent.completed"
		switch outcome.State {
		case agent.StateCompleted:
			turnValue.State = TurnCompleted
			sessionValue.State = SessionIdle
			result.State = TurnCompleted
		case agent.StatePaused:
			turnValue.State = TurnRequiresAction
			sessionValue.State = SessionBlocked
			result.State = TurnRequiresAction
			eventType = "agent.paused"
		case agent.StateCancelled:
			turnValue.State = TurnCancelled
			sessionValue.State = SessionIdle
			result.State = TurnCancelled
			eventType = "agent.cancelled"
			runtimeErr = outcome.Error
		case agent.StateNeedsReconciliation:
			turnValue.State = TurnRequiresAction
			sessionValue.State = SessionBlocked
			result.State = TurnRequiresAction
			eventType = "agent.needs_reconciliation"
			runtimeErr = outcome.Error
		default:
			turnValue.State = TurnFailed
			sessionValue.State = SessionIdle
			result.State = TurnFailed
			eventType = "agent.failed"
			runtimeErr = outcome.Error
		}
		turnValue.Error = runtimeErr
		for index := len(messages) - 1; index >= 0; index-- {
			if messages[index].Role == contract.RoleAssistant {
				current := cloneContractMessage(messages[index])
				result.Message = &current
				break
			}
		}
		detail, _ := json.Marshal(outcome)
		if err := service.store.appendEvent(&sessionValue, EventRecord{
			Time: now, Type: eventType, TurnID: turn.TurnID,
			RunID: turn.RunID, ExecutionID: turn.ExecutionID,
			State: string(turnValue.State), Error: runtimeErr, Detail: detail,
		}); err != nil {
			return err
		}
		execution := Execution{
			SchemaVersion: SchemaVersion, ID: turn.ExecutionID,
			SessionID: turn.SessionID, TurnID: turn.TurnID, RunID: turn.RunID,
			ProfileID: turn.ProfileID, ProfileKind: profile.KindModel,
			Transport: "http", State: turnValue.State,
			CaptureQuality: CaptureStructured,
			CreatedAt:      turnValue.CreatedAt, UpdatedAt: now,
		}
		if err := service.store.writeExecution(execution); err != nil {
			return err
		}
		if err := service.store.writeTurn(turnValue); err != nil {
			return err
		}
		return service.store.writeSession(sessionValue)
	})
	if err != nil {
		runtimeErr = sessionRuntimeError(contract.ErrorInternal, err.Error())
		result.State = TurnFailed
		result.Error = runtimeErr
		return result, runtimeErr
	}
	result.Error = runtimeErr
	_ = service.store.rebuildIndex()
	return result, runtimeErr
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
