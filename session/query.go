package session

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
)

func (service *Service) List(filter ListFilter) ([]Session, error) {
	return service.store.list(filter)
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
		_ = service.store.rebuildIndex()
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

func (service *Service) LatestExecution(
	sessionID string,
) (Execution, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return Execution{}, err
	}
	var latest Execution
	err := service.store.withLock(sessionID, func() error {
		directory := filepath.Join(service.store.sessionDir(sessionID), "executions")
		entries, err := os.ReadDir(directory)
		if err != nil {
			return err
		}
		for _, entry := range entries {
			if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 ||
				filepath.Ext(entry.Name()) != ".json" {
				continue
			}
			var current Execution
			if err := readStrictJSON(filepath.Join(directory, entry.Name()), &current); err != nil {
				return err
			}
			if current.UpdatedAt.After(latest.UpdatedAt) {
				latest = current
			}
		}
		if latest.ID == "" {
			return fmt.Errorf("session %s has no execution", sessionID)
		}
		return nil
	})
	return latest, err
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
	var receipt ToolResultReceipt
	var runtimeErr *contract.RuntimeError
	err := service.store.withLock(sessionID, func() error {
		sessionValue, err := service.store.loadSession(sessionID)
		if err != nil {
			return err
		}
		turn, err := service.store.loadTurn(sessionID, turnID)
		if err != nil {
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
				Content: input.Content,
			},
			IsError: input.IsError,
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
		return ToolResultReceipt{}, sessionRuntimeError(contract.ErrorInternal, err.Error())
	}
	if runtimeErr != nil {
		return ToolResultReceipt{}, runtimeErr
	}
	_ = service.store.rebuildIndex()
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
		_ = service.store.rebuildIndex()
	}
	return value, err
}

func (service *Service) Delete(sessionID string) (string, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return "", err
	}
	var target string
	err := service.store.withLock(sessionID, func() error {
		value, err := service.store.loadSession(sessionID)
		if err != nil {
			return err
		}
		if value.State == SessionActive || value.State == SessionBlocked {
			return fmt.Errorf("cannot delete %s session", value.State)
		}
		stamp := service.now().UTC().Format("20060102T150405.000000000Z")
		target = filepath.Join(service.store.historyDir, "trash", stamp, sessionID)
		if err := ensureDirectory(filepath.Dir(target)); err != nil {
			return err
		}
		return os.Rename(service.store.sessionDir(sessionID), target)
	})
	if err == nil {
		_ = service.store.rebuildIndex()
	}
	return target, err
}

func (service *Service) GC(options GCOptions) (GCResult, error) {
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
	result := GCResult{DryRun: !options.Apply, Candidates: candidates}
	if !options.Apply {
		return result, nil
	}
	for _, sessionID := range candidates {
		if _, err := service.Delete(sessionID); err != nil {
			return result, err
		}
		result.Moved = append(result.Moved, sessionID)
	}
	return result, nil
}
