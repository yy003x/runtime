package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
)

func (service *Service) List(filter ListFilter) ([]Session, error) {
	if err := ValidateListFilter(filter); err != nil {
		return nil, err
	}
	return service.store.list(filter)
}

// ValidateListFilter defines the Session state filter shared by CLI, HTTP,
// Service, and Store callers.
func ValidateListFilter(filter ListFilter) error {
	switch filter.State {
	case "", SessionIdle, SessionActive, SessionBlocked, SessionArchived:
		return nil
	default:
		return fmt.Errorf(
			"state must be idle, active, blocked, or archived",
		)
	}
}

func (service *Service) Create(retention Retention) (Session, error) {
	switch retention {
	case "", RetentionEphemeral, RetentionStandard, RetentionPinned:
	default:
		return Session{}, fmt.Errorf("unsupported retention %q", retention)
	}
	sessionID, err := NewID()
	if err != nil {
		return Session{}, err
	}
	var value Session
	err = service.store.withLock(sessionID, func() error {
		value, err = service.loadOrCreateSession(
			sessionID, retention, service.now().UTC(),
		)
		return err
	})
	if err == nil {
		if indexErr := service.store.rebuildIndex(); indexErr != nil {
			err = fmt.Errorf(
				"Session %s was created but index rebuild failed: %w",
				value.ID, indexErr,
			)
		}
	}
	return value, err
}

func (service *Service) Get(sessionID string) (Session, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return Session{}, err
	}
	var value Session
	err := service.store.withLock(sessionID, func() error {
		var err error
		value, err = service.store.loadSession(sessionID)
		return err
	})
	return value, err
}

func (service *Service) Messages(
	sessionID string,
	afterSequence uint64,
) ([]MessageRecord, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return nil, err
	}
	var values []MessageRecord
	err := service.store.withLock(sessionID, func() error {
		if _, err := service.store.loadSession(sessionID); err != nil {
			return err
		}
		var err error
		values, err = service.store.messages(sessionID)
		return err
	})
	if err != nil || afterSequence == 0 {
		return values, err
	}
	filtered := values[:0]
	for _, value := range values {
		if value.Sequence > afterSequence {
			filtered = append(filtered, value)
		}
	}
	return filtered, nil
}

func (service *Service) Events(
	sessionID string,
	afterSequence uint64,
) ([]EventRecord, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return nil, err
	}
	var values []EventRecord
	err := service.store.withLock(sessionID, func() error {
		if _, err := service.store.loadSession(sessionID); err != nil {
			return err
		}
		var err error
		values, err = service.store.events(sessionID)
		return err
	})
	if err != nil || afterSequence == 0 {
		return values, err
	}
	filtered := values[:0]
	for _, value := range values {
		if value.Sequence > afterSequence {
			filtered = append(filtered, value)
		}
	}
	return filtered, nil
}

func (service *Service) Executions(
	sessionID string,
) ([]Execution, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return nil, err
	}
	var values []Execution
	err := service.store.withLock(sessionID, func() error {
		if _, err := service.store.loadSession(sessionID); err != nil {
			return err
		}
		root, err := service.store.openSessionRoot(sessionID)
		if err != nil {
			return err
		}
		defer root.close()
		directory, err := root.openDirectory("executions", false)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		defer directory.close()
		entries, err := directory.entries()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.isRegular() ||
				filepath.Ext(entry.name) != ".json" {
				continue
			}
			current, err := service.store.loadExecution(
				sessionID, strings.TrimSuffix(entry.name, ".json"),
			)
			if err != nil {
				return err
			}
			values = append(values, current)
		}
		return nil
	})
	sort.Slice(values, func(left, right int) bool {
		if values[left].UpdatedAt.Equal(values[right].UpdatedAt) {
			return values[left].ID < values[right].ID
		}
		return values[left].UpdatedAt.After(values[right].UpdatedAt)
	})
	return values, err
}

func (service *Service) Execution(
	sessionID, executionID string,
) (Execution, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return Execution{}, err
	}
	if err := identity.Validate(executionID, "execution"); err != nil {
		return Execution{}, err
	}
	var value Execution
	err := service.store.withLock(sessionID, func() error {
		var err error
		value, err = service.store.loadExecution(sessionID, executionID)
		return err
	})
	return value, err
}

func (service *Service) ResultForRun(
	sessionID, runID string,
) (RunResult, bool, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return RunResult{}, false, err
	}
	if err := identity.Validate(runID, "run"); err != nil {
		return RunResult{}, false, err
	}
	var result RunResult
	found := false
	err := service.store.withLock(sessionID, func() error {
		root, err := service.store.openSessionRoot(sessionID)
		if err != nil {
			return err
		}
		defer root.close()
		turns, err := root.openDirectory("turns", false)
		if err != nil {
			return err
		}
		defer turns.close()
		entries, err := turns.entries()
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if !entry.isDirectory() {
				continue
			}
			turn, err := service.store.loadTurn(sessionID, entry.name)
			if err != nil {
				return err
			}
			if turn.RunID != runID {
				continue
			}
			if found {
				return fmt.Errorf(
					"%w: multiple Session Turns reuse run_id %s",
					ErrConflict, runID,
				)
			}
			messages, err := service.store.messages(sessionID)
			if err != nil {
				return err
			}
			var assistant *contract.Message
			for _, message := range messages {
				if message.TurnID == turn.ID &&
					message.Message.Role == contract.RoleAssistant {
					current := message.Message
					assistant = &current
				}
			}
			result = service.resultFromTurn(turn, assistant)
			execution, err := service.store.loadExecution(
				sessionID, turn.ExecutionID,
			)
			if err != nil {
				return err
			}
			if turn.AgentOwned &&
				!agentExecutionMatchesTurn(execution, turn) {
				return fmt.Errorf(
					"%w: Agent Session Turn and Execution facts do not match",
					ErrConflict,
				)
			}
			result.ExitCode = execution.ExitCode
			found = true
		}
		return nil
	})
	return result, found, err
}

func (service *Service) SubmitToolResult(
	sessionID, turnID string,
	input ToolResultInput,
) (ToolResultReceipt, *contract.RuntimeError) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return ToolResultReceipt{}, sessionRuntimeError(contract.ErrorInvalidRequest, err.Error())
	}
	if err := identity.Validate(turnID, "turn"); err != nil {
		return ToolResultReceipt{}, sessionRuntimeError(contract.ErrorInvalidRequest, err.Error())
	}
	if strings.TrimSpace(input.ToolCallID) == "" ||
		strings.TrimSpace(input.IdempotencyKey) == "" {
		return ToolResultReceipt{}, sessionRuntimeError(
			contract.ErrorInvalidRequest,
			"tool_call_id and idempotency_key are required",
		)
	}
	if len(input.Content) > maxSessionInputBytes || len(input.IdempotencyKey) > 256 {
		return ToolResultReceipt{}, sessionRuntimeError(
			contract.ErrorInvalidRequest, "tool result exceeds size limits",
		)
	}
	if !utf8.ValidString(input.Content) ||
		strings.ContainsRune(input.Content, '\x00') ||
		!utf8.ValidString(input.IdempotencyKey) ||
		strings.ContainsRune(input.IdempotencyKey, '\x00') {
		return ToolResultReceipt{}, sessionRuntimeError(
			contract.ErrorInvalidRequest,
			"tool result content and idempotency_key must be UTF-8 without NUL",
		)
	}
	var receipt ToolResultReceipt
	var runtimeErr *contract.RuntimeError
	var missingTurnErr error
	err := service.store.withLock(sessionID, func() error {
		sessionValue, err := service.store.loadSession(sessionID)
		if err != nil {
			return err
		}
		turn, err := service.store.loadTurn(sessionID, turnID)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				missingTurnErr = err
			}
			return err
		}
		contentDigest := digest([]byte(input.Content))
		if existing, exists := turn.ToolResults[input.IdempotencyKey]; exists {
			if existing.ToolCallID == input.ToolCallID &&
				existing.ContentDigest == contentDigest &&
				existing.IsError == input.IsError {
				receipt = existing
				return nil
			}
			runtimeErr = sessionRuntimeError(
				contract.ErrorConflict,
				"idempotency key was already used with different tool result content",
			)
			return nil
		}
		if turn.State != TurnRequiresAction || sessionValue.State != SessionBlocked {
			runtimeErr = sessionRuntimeError(
				contract.ErrorConflict, "turn is not waiting for tool results",
			)
			return nil
		}
		found := false
		for _, call := range turn.PendingToolCalls {
			if call.ID == input.ToolCallID {
				found = true
				break
			}
		}
		if !found {
			runtimeErr = sessionRuntimeError(
				contract.ErrorInvalidRequest,
				fmt.Sprintf("unknown or settled tool call %q", input.ToolCallID),
			)
			return nil
		}
		for _, existing := range turn.ToolResults {
			if existing.ToolCallID == input.ToolCallID {
				runtimeErr = sessionRuntimeError(
					contract.ErrorConflict,
					fmt.Sprintf("tool call %q is already settled", input.ToolCallID),
				)
				return nil
			}
		}
		now := service.now().UTC()
		message := MessageRecord{
			Time: now, TurnID: turn.ID, RunID: turn.RunID,
			ExecutionID: turn.ExecutionID, ProfileID: turn.ProfileID,
			Message: contract.Message{
				Role: contract.RoleTool, ToolCallID: input.ToolCallID,
				Content: input.Content, IsError: input.IsError,
			},
		}
		if err := service.store.appendMessage(&sessionValue, message); err != nil {
			return err
		}
		receipt = ToolResultReceipt{
			ToolCallID: input.ToolCallID, IdempotencyKey: input.IdempotencyKey,
			ContentDigest: contentDigest, IsError: input.IsError,
			MessageSequence: sessionValue.MessageCount, AcceptedAt: now,
		}
		if turn.ToolResults == nil {
			turn.ToolResults = make(map[string]ToolResultReceipt)
		}
		turn.ToolResults[input.IdempotencyKey] = receipt
		if len(turn.ToolResults) == len(turn.PendingToolCalls) {
			turn.State = TurnCompleted
			sessionValue.State = SessionIdle
		}
		turn.UpdatedAt = now
		sessionValue.UpdatedAt = now
		if err := service.store.appendEvent(&sessionValue, EventRecord{
			Time: now, Type: "tool.result.accepted", TurnID: turn.ID,
			RunID: turn.RunID, ExecutionID: turn.ExecutionID,
			State: string(turn.State),
		}); err != nil {
			return err
		}
		if err := service.store.writeTurn(turn); err != nil {
			return err
		}
		return service.store.writeSession(sessionValue)
	})
	if err != nil {
		if missingTurnErr != nil && err == missingTurnErr {
			return ToolResultReceipt{}, sessionRuntimeError(
				contract.ErrorNotFound,
				fmt.Sprintf("turn %s was not found", turnID),
			)
		}
		return ToolResultReceipt{}, sessionRuntimeError(contract.ErrorInternal, err.Error())
	}
	if runtimeErr != nil {
		return ToolResultReceipt{}, runtimeErr
	}
	if err := service.store.rebuildIndex(); err != nil {
		return receipt, sessionRuntimeError(
			contract.ErrorInternal,
			"tool result was committed but Session index rebuild failed: "+
				err.Error(),
		)
	}
	return receipt, nil
}

func (service *Service) ConfigureRetention(
	sessionID string,
	retention Retention,
) (Session, error) {
	switch retention {
	case RetentionEphemeral, RetentionStandard, RetentionPinned:
	default:
		return Session{}, fmt.Errorf("unsupported retention %q", retention)
	}
	var value Session
	err := service.store.withLock(sessionID, func() error {
		var err error
		value, err = service.store.loadSession(sessionID)
		if err != nil {
			return err
		}
		value.Retention = retention
		value.UpdatedAt = service.now().UTC()
		return service.store.writeSession(value)
	})
	if err == nil {
		if indexErr := service.store.rebuildIndex(); indexErr != nil {
			err = fmt.Errorf(
				"Session retention was committed but index rebuild failed: %w",
				indexErr,
			)
		}
	}
	return value, err
}

func (service *Service) Delete(sessionID string) (string, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return "", err
	}
	if pending, err := service.store.readTrashMoveJournal(sessionID); err == nil {
		target := filepath.Join(
			service.store.sessionsDir, pending.TargetRelative,
		)
		err = service.store.withSessionFileLock(sessionID, func() error {
			return service.store.reconcileTrashMove(pending)
		})
		if err == nil {
			err = service.store.rebuildIndex()
		}
		if err == nil {
			err = service.store.completeTrashMove(sessionID)
		}
		return target, err
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	var target string
	err := service.store.withLock(sessionID, func() error {
		value, err := service.store.loadSession(sessionID)
		if err != nil {
			return err
		}
		if value.State == SessionActive || value.State == SessionBlocked {
			return fmt.Errorf(
				"%w: cannot delete %s session", ErrConflict, value.State,
			)
		}
		stamp := service.now().UTC().Format("20060102T150405.000000000Z")
		target = filepath.Join(service.store.historyDir, "trash", stamp, sessionID)
		target, err = service.store.durableMoveSession(sessionID, target)
		return err
	})
	if err == nil {
		err = service.store.rebuildIndex()
	}
	if err == nil {
		err = service.store.completeTrashMove(sessionID)
	}
	return target, err
}

func (service *Service) GC(options GCOptions) (GCResult, error) {
	if recovered, err := service.store.recoverTrashMoves(); err != nil {
		return GCResult{}, err
	} else if recovered {
		if err := service.store.rebuildIndex(); err != nil {
			return GCResult{}, err
		}
		if err := service.store.completeAllTrashMoves(); err != nil {
			return GCResult{}, err
		}
	}
	if options.OlderThan <= 0 {
		options.OlderThan = 24 * time.Hour
	}
	if options.Limit <= 0 || options.Limit > 1000 {
		options.Limit = 100
	}
	values, err := service.List(ListFilter{})
	if err != nil {
		return GCResult{}, err
	}
	cutoff := service.now().UTC().Add(-options.OlderThan)
	var candidates []string
	for _, value := range values {
		if len(candidates) >= options.Limit {
			break
		}
		if value.Retention == RetentionEphemeral &&
			value.State == SessionIdle &&
			value.UpdatedAt.Before(cutoff) {
			candidates = append(candidates, value.ID)
		}
	}
	sort.Strings(candidates)
	result := GCResult{
		DryRun: !options.Apply, Candidates: candidates, Skipped: []string{},
	}
	if !options.Apply {
		return result, nil
	}
	result.Moved, result.Skipped, err = service.applyGCCandidates(
		candidates, cutoff,
	)
	if err != nil {
		return result, err
	}
	return result, nil
}

func (service *Service) applyGCCandidates(
	candidates []string,
	cutoff time.Time,
) ([]string, []string, error) {
	var moved []string
	skipped := make([]string, 0)
	for _, sessionID := range candidates {
		candidateMoved := false
		candidateSkipped := false
		err := service.store.withLock(sessionID, func() error {
			value, err := service.store.loadSession(sessionID)
			if errors.Is(err, os.ErrNotExist) {
				candidateSkipped = true
				return nil
			}
			if err != nil {
				return err
			}
			if value.Retention != RetentionEphemeral ||
				value.State != SessionIdle ||
				!value.UpdatedAt.Before(cutoff) {
				candidateSkipped = true
				return nil
			}
			stamp := service.now().UTC().Format("20060102T150405.000000000Z")
			target := filepath.Join(
				service.store.historyDir, "trash", stamp, sessionID,
			)
			if _, err := service.store.durableMoveSession(
				sessionID, target,
			); err != nil {
				return err
			}
			candidateMoved = true
			return nil
		})
		if err != nil {
			return moved, skipped, err
		}
		switch {
		case candidateMoved:
			moved = append(moved, sessionID)
		case candidateSkipped:
			skipped = append(skipped, sessionID)
		}
	}
	if len(moved) > 0 {
		if err := service.store.rebuildIndex(); err != nil {
			return moved, skipped, err
		}
		for _, sessionID := range moved {
			if err := service.store.completeTrashMove(sessionID); err != nil {
				return moved, skipped, err
			}
		}
	}
	return moved, skipped, nil
}
