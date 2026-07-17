package agentrun

import (
	"bufio"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type SessionStore struct {
	sessionsDir string
	historyDir  string
	stateDir    string
}

func NewSessionStore(sessionsDir, historyDir, stateDir string) *SessionStore {
	return &SessionStore{sessionsDir: sessionsDir, historyDir: historyDir, stateDir: stateDir}
}

func (s *SessionStore) ensure() error {
	for _, dir := range []string{s.sessionsDir, s.historyDir, filepath.Join(s.stateDir, "locks")} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return err
		}
	}
	return nil
}

func (s *SessionStore) sessionDir(sessionID string) (string, error) {
	if err := validateRunID(sessionID); err != nil {
		return "", fmt.Errorf("invalid session_id: %w", err)
	}
	if entry, ok, err := s.indexEntry(sessionID); err == nil && ok {
		trusted, trustErr := trustedHistoryPath(s.sessionsDir, entry.SessionDir, sessionID)
		if trustErr != nil {
			return "", fmt.Errorf("unsafe history index entry for %s: %w", sessionID, trustErr)
		}
		return trusted, nil
	}
	return filepath.Join(s.sessionsDir, dateFromRunID(sessionID), sessionID), nil
}

func trustedHistoryPath(root, candidate, sessionID string) (string, error) {
	rootPath, err := filepath.Abs(root)
	if err != nil {
		return "", err
	}
	candidatePath, err := filepath.Abs(candidate)
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(rootPath, candidatePath)
	if err != nil || relative == "." || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) || filepath.Base(candidatePath) != sessionID {
		return "", fmt.Errorf("session_dir must stay under sessions root")
	}
	resolvedRoot, rootErr := filepath.EvalSymlinks(rootPath)
	resolvedCandidate, candidateErr := filepath.EvalSymlinks(candidatePath)
	if rootErr == nil && candidateErr == nil {
		resolvedRelative, relErr := filepath.Rel(resolvedRoot, resolvedCandidate)
		if relErr != nil || resolvedRelative == "." || resolvedRelative == ".." || strings.HasPrefix(resolvedRelative, ".."+string(filepath.Separator)) {
			return "", fmt.Errorf("session_dir resolves outside sessions root")
		}
	}
	return candidatePath, nil
}

func (s *SessionStore) lockPath(sessionID string) string {
	return filepath.Join(s.stateDir, "locks", sessionID+".lock")
}

func (s *SessionStore) Create(record SessionRecord) (SessionRecord, error) {
	if err := validateSessionRecord(record); err != nil {
		return SessionRecord{}, err
	}
	if err := s.ensure(); err != nil {
		return SessionRecord{}, err
	}
	dir, err := s.sessionDir(record.SessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "turns"), 0o700); err != nil {
		return SessionRecord{}, err
	}
	if err := os.MkdirAll(filepath.Join(dir, "executions"), 0o700); err != nil {
		return SessionRecord{}, err
	}
	err = withAdvisoryFileLock(s.lockPath(record.SessionID), func() error {
		path := filepath.Join(dir, "session.json")
		var existing SessionRecord
		if readErr := readJSON(path, &existing); readErr == nil {
			record = existing
			return nil
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
		now := time.Now().UTC()
		record.SchemaVersion = SessionSchemaVersion
		if record.CreatedAt.IsZero() {
			record.CreatedAt = now
		}
		record.UpdatedAt = now
		record.LastSequence = 1
		if record.State == "" {
			record.State = StateRunning
		}
		if record.RecordMode == "" {
			record.RecordMode = RecordFull
		}
		if record.Retention == "" {
			record.Retention = RetentionStandard
		}
		if record.CaptureQuality == "" {
			record.CaptureQuality = CaptureStructured
		}
		if err := writeJSONAtomic(path, record); err != nil {
			return err
		}
		return appendJSONL(filepath.Join(dir, "events.jsonl"), SessionEvent{
			SchemaVersion: SessionSchemaVersion, EventID: newEntryID("event"), SessionID: record.SessionID,
			Sequence: 1, Timestamp: now, Type: "session.created", Data: map[string]any{
				"record_mode": record.RecordMode, "retention": record.Retention, "capture_quality": record.CaptureQuality,
			},
		})
	})
	if err != nil {
		return SessionRecord{}, err
	}
	if err := s.updateIndex(record, dir); err != nil {
		return SessionRecord{}, err
	}
	return record, nil
}

func validateSessionRecord(record SessionRecord) error {
	if record.RecordMode != "" && !oneOf(record.RecordMode, RecordFull, RecordMetadata) {
		return fmt.Errorf("session record_mode must be full|metadata")
	}
	if record.Retention != "" && !oneOf(record.Retention, RetentionEphemeral, RetentionStandard, RetentionPinned) {
		return fmt.Errorf("session retention must be ephemeral|standard|pinned")
	}
	if record.CaptureQuality != "" && !oneOf(record.CaptureQuality, CaptureStructured, CaptureParsed, CaptureTranscriptOnly, CaptureMetadataOnly) {
		return fmt.Errorf("invalid session capture_quality")
	}
	if record.State != "" && !oneOf(record.State, StatePending, StateRunning, StateResultPending, StateDone, StateFailed, StateBlocked, StateCancelled, TurnStateSubmitted) {
		return fmt.Errorf("invalid session state")
	}
	return ValidateSessionTags(record.Tags)
}

func (s *SessionStore) Get(sessionID string) (SessionRecord, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	var record SessionRecord
	if err := readJSON(filepath.Join(dir, "session.json"), &record); err != nil {
		if os.IsNotExist(err) {
			return SessionRecord{}, fmt.Errorf("session not found: %s", sessionID)
		}
		return SessionRecord{}, err
	}
	return record, nil
}

func (s *SessionStore) AddTurn(sessionID string, turn TurnRecord, input string, manifest ContextManifest) (TurnRecord, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return TurnRecord{}, err
	}
	err = withAdvisoryFileLock(s.lockPath(sessionID), func() error {
		record, readErr := s.readSession(dir)
		if readErr != nil {
			return readErr
		}
		turnDir := filepath.Join(dir, "turns", turn.TurnID)
		if err := os.MkdirAll(filepath.Join(turnDir, "attempts"), 0o700); err != nil {
			return err
		}
		turnPath := filepath.Join(turnDir, "turn.json")
		var existing TurnRecord
		if readErr := readJSON(turnPath, &existing); readErr == nil {
			turn = existing
			return nil
		} else if !os.IsNotExist(readErr) {
			return readErr
		}
		now := time.Now().UTC()
		turn.SchemaVersion = SessionSchemaVersion
		turn.SessionID = sessionID
		turn.Sequence = record.TurnCount + 1
		turn.State = StateRunning
		turn.CreatedAt, turn.UpdatedAt = now, now
		if turn.CaptureQuality == "" {
			turn.CaptureQuality = record.CaptureQuality
		}
		if record.RecordMode == RecordFull && strings.TrimSpace(input) != "" {
			record.LastSequence++
			message := SessionMessage{SchemaVersion: SessionSchemaVersion, MessageID: newEntryID("message"), SessionID: sessionID,
				TurnID: turn.TurnID, Sequence: record.LastSequence, Timestamp: now, Role: "user", Kind: "message", Content: input}
			turn.InputMessageID = message.MessageID
			if err := appendJSONL(filepath.Join(dir, "messages.jsonl"), message); err != nil {
				return err
			}
		}
		if manifest.TurnID != "" {
			manifestPath := filepath.Join(turnDir, "context-manifest.json")
			if err := writeJSONAtomic(manifestPath, manifest); err != nil {
				return err
			}
			turn.ContextManifest = manifestPath
		}
		record.LastSequence++
		if err := appendJSONL(filepath.Join(dir, "events.jsonl"), SessionEvent{SchemaVersion: SessionSchemaVersion,
			EventID: newEntryID("event"), SessionID: sessionID, TurnID: turn.TurnID, Sequence: record.LastSequence,
			Timestamp: now, Type: "turn.started", Data: map[string]any{"turn_sequence": turn.Sequence}}); err != nil {
			return err
		}
		record.TurnCount++
		record.LastTurnID = turn.TurnID
		record.State = StateRunning
		if strings.TrimSpace(record.Title) == "" && strings.TrimSpace(input) != "" {
			record.Title = truncateHistoryText(input, 120)
		}
		record.UpdatedAt = now
		if err := writeJSONAtomic(turnPath, turn); err != nil {
			return err
		}
		return s.writeSession(dir, record)
	})
	if err != nil {
		return TurnRecord{}, err
	}
	record, err := s.Get(sessionID)
	if err == nil {
		err = s.updateIndex(record, dir)
	}
	return turn, err
}

func (s *SessionStore) AddAttempt(sessionID string, attempt RunAttemptRecord) error {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return err
	}
	return withAdvisoryFileLock(s.lockPath(sessionID), func() error {
		record, err := s.readSession(dir)
		if err != nil {
			return err
		}
		now := time.Now().UTC()
		attempt.SchemaVersion = SessionSchemaVersion
		attempt.SessionID = sessionID
		attempt.StartedAt = now
		attempt.State = StateRunning
		attemptPath := filepath.Join(dir, "turns", attempt.TurnID, "attempts", attempt.RunID+".json")
		if err := os.MkdirAll(filepath.Dir(attemptPath), 0o700); err != nil {
			return err
		}
		if err := writeJSONAtomic(attemptPath, attempt); err != nil {
			return err
		}
		record.LastSequence++
		record.RunCount++
		record.Providers = appendUnique(record.Providers, attempt.Provider)
		record.UpdatedAt = now
		if err := appendJSONL(filepath.Join(dir, "events.jsonl"), SessionEvent{SchemaVersion: SessionSchemaVersion,
			EventID: newEntryID("event"), SessionID: sessionID, TurnID: attempt.TurnID, ExecutionID: attempt.ExecutionID,
			RunID: attempt.RunID, Sequence: record.LastSequence, Timestamp: now, Type: "run.started",
			Data: map[string]any{"provider": attempt.Provider, "profile": attempt.Profile, "attempt": attempt.Attempt}}); err != nil {
			return err
		}
		return s.writeSession(dir, record)
	})
}

func (s *SessionStore) UpsertExecution(sessionID string, execution ExecutionRecord) error {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return err
	}
	return withAdvisoryFileLock(s.lockPath(sessionID), func() error {
		path := filepath.Join(dir, "executions", execution.ExecutionID+".json")
		var existing ExecutionRecord
		if readErr := readJSON(path, &existing); readErr == nil {
			execution.StartedAt = existing.StartedAt
			if execution.Kind == "" {
				execution.Kind = existing.Kind
			}
			if execution.Profile == "" {
				execution.Profile = existing.Profile
			}
			if execution.Provider == "" {
				execution.Provider = existing.Provider
			}
			if execution.State == "" {
				execution.State = existing.State
			}
			if execution.CaptureQuality == "" {
				execution.CaptureQuality = existing.CaptureQuality
			}
			if execution.TmuxSession == "" {
				execution.TmuxSession = existing.TmuxSession
			}
			execution.RunIDs = appendUnique(existing.RunIDs, execution.RunIDs...)
			execution.TurnIDs = appendUnique(existing.TurnIDs, execution.TurnIDs...)
		}
		now := time.Now().UTC()
		execution.SchemaVersion = SessionSchemaVersion
		execution.SessionID = sessionID
		if execution.StartedAt.IsZero() {
			execution.StartedAt = now
		}
		execution.UpdatedAt = now
		if terminalStateValue(execution.State) && execution.CompletedAt.IsZero() {
			execution.CompletedAt = now
		}
		return writeJSONAtomic(path, execution)
	})
}

func (s *SessionStore) ContextManifestPath(sessionID, turnID string) (string, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return "", err
	}
	if err := validateRunID(turnID); err != nil {
		return "", fmt.Errorf("invalid turn_id: %w", err)
	}
	return filepath.Join(dir, "turns", turnID, "context-manifest.json"), nil
}

func (s *SessionStore) CompleteRun(sessionID, turnID, runID, state, failureReason, output string, resultRef *ResultRef) error {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return err
	}
	err = withAdvisoryFileLock(s.lockPath(sessionID), func() error {
		record, err := s.readSession(dir)
		if err != nil {
			return err
		}
		turnPath := filepath.Join(dir, "turns", turnID, "turn.json")
		var turn TurnRecord
		if err := readJSON(turnPath, &turn); err != nil {
			return err
		}
		attemptPath := filepath.Join(dir, "turns", turnID, "attempts", runID+".json")
		var attempt RunAttemptRecord
		if err := readJSON(attemptPath, &attempt); err != nil {
			return err
		}
		if terminalStateValue(attempt.State) && attempt.State == state && !attempt.CompletedAt.IsZero() {
			return nil
		}
		now := time.Now().UTC()
		attempt.State, attempt.FailureReason, attempt.CompletedAt, attempt.ResultRef = state, failureReason, now, resultRef
		if err := writeJSONAtomic(attemptPath, attempt); err != nil {
			return err
		}
		turn.State, turn.UpdatedAt = state, now
		if state == StateDone {
			turn.WinningRunID, turn.ResultRef = runID, resultRef
		}
		if record.RecordMode == RecordFull && strings.TrimSpace(output) != "" {
			record.LastSequence++
			message := SessionMessage{SchemaVersion: SessionSchemaVersion, MessageID: newEntryID("message"), SessionID: sessionID,
				TurnID: turnID, Sequence: record.LastSequence, Timestamp: now, Role: "assistant", Kind: "message", Content: output,
				Metadata: map[string]any{"run_id": runID, "capture_quality": turn.CaptureQuality}}
			turn.OutputMessageID = message.MessageID
			if err := appendJSONL(filepath.Join(dir, "messages.jsonl"), message); err != nil {
				return err
			}
		}
		if err := writeJSONAtomic(turnPath, turn); err != nil {
			return err
		}
		record.LastSequence++
		eventType := "run.completed"
		if state != StateDone {
			eventType = "run." + state
		}
		if err := appendJSONL(filepath.Join(dir, "events.jsonl"), SessionEvent{SchemaVersion: SessionSchemaVersion,
			EventID: newEntryID("event"), SessionID: sessionID, TurnID: turnID, ExecutionID: attempt.ExecutionID,
			RunID: runID, Sequence: record.LastSequence, Timestamp: now, Type: eventType,
			Data: map[string]any{"state": state, "failure_reason": failureReason, "result_ref": resultRef}}); err != nil {
			return err
		}
		record.State, record.UpdatedAt = state, now
		if state == StateDone && strings.TrimSpace(output) != "" {
			record.Summary = truncateHistoryText(output, 512)
		}
		return s.writeSession(dir, record)
	})
	if err != nil {
		return err
	}
	record, err := s.Get(sessionID)
	if err != nil {
		return err
	}
	return s.updateIndex(record, dir)
}

func (s *SessionStore) ResumeRun(sessionID, turnID, runID string) error {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return err
	}
	err = withAdvisoryFileLock(s.lockPath(sessionID), func() error {
		record, err := s.readSession(dir)
		if err != nil {
			return err
		}
		turnPath := filepath.Join(dir, "turns", turnID, "turn.json")
		var turn TurnRecord
		if err := readJSON(turnPath, &turn); err != nil {
			return err
		}
		attemptPath := filepath.Join(dir, "turns", turnID, "attempts", runID+".json")
		var attempt RunAttemptRecord
		if err := readJSON(attemptPath, &attempt); err != nil {
			return err
		}
		now := time.Now().UTC()
		attempt.State, attempt.FailureReason, attempt.CompletedAt, attempt.ResultRef = StateRunning, "", time.Time{}, nil
		if err := writeJSONAtomic(attemptPath, attempt); err != nil {
			return err
		}
		turn.State, turn.ResultRef, turn.UpdatedAt = StateRunning, nil, now
		if err := writeJSONAtomic(turnPath, turn); err != nil {
			return err
		}
		record.LastSequence++
		record.State, record.Summary, record.UpdatedAt = StateRunning, "", now
		if err := appendJSONL(filepath.Join(dir, "events.jsonl"), SessionEvent{SchemaVersion: SessionSchemaVersion,
			EventID: newEntryID("event"), SessionID: sessionID, TurnID: turnID, ExecutionID: attempt.ExecutionID,
			RunID: runID, Sequence: record.LastSequence, Timestamp: now, Type: "run.resumed"}); err != nil {
			return err
		}
		return s.writeSession(dir, record)
	})
	if err != nil {
		return err
	}
	record, err := s.Get(sessionID)
	if err != nil {
		return err
	}
	return s.updateIndex(record, dir)
}

func (s *SessionStore) AppendEvent(sessionID string, event SessionEvent) error {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return err
	}
	return withAdvisoryFileLock(s.lockPath(sessionID), func() error {
		record, err := s.readSession(dir)
		if err != nil {
			return err
		}
		record.LastSequence++
		record.UpdatedAt = time.Now().UTC()
		event.SchemaVersion = SessionSchemaVersion
		event.EventID = newEntryID("event")
		event.SessionID = sessionID
		event.Sequence = record.LastSequence
		if event.Timestamp.IsZero() {
			event.Timestamp = record.UpdatedAt
		}
		event.Data = redactMetadata(event.Data)
		if err := appendJSONL(filepath.Join(dir, "events.jsonl"), event); err != nil {
			return err
		}
		return s.writeSession(dir, record)
	})
}

func (s *SessionStore) AppendMessage(sessionID string, message SessionMessage) (SessionMessage, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return SessionMessage{}, err
	}
	err = withAdvisoryFileLock(s.lockPath(sessionID), func() error {
		record, err := s.readSession(dir)
		if err != nil {
			return err
		}
		if record.RecordMode != RecordFull {
			return nil
		}
		record.LastSequence++
		record.UpdatedAt = time.Now().UTC()
		message.SchemaVersion = SessionSchemaVersion
		if message.MessageID == "" {
			message.MessageID = newEntryID("message")
		}
		message.SessionID = sessionID
		message.Sequence = record.LastSequence
		if message.Timestamp.IsZero() {
			message.Timestamp = record.UpdatedAt
		}
		message.Metadata = redactMetadata(message.Metadata)
		if err := appendJSONL(filepath.Join(dir, "messages.jsonl"), message); err != nil {
			return err
		}
		return s.writeSession(dir, record)
	})
	return message, err
}

func (s *SessionStore) CompleteTurn(sessionID, turnID, state, output, kind string) error {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return err
	}
	err = withAdvisoryFileLock(s.lockPath(sessionID), func() error {
		record, err := s.readSession(dir)
		if err != nil {
			return err
		}
		turnPath := filepath.Join(dir, "turns", turnID, "turn.json")
		var turn TurnRecord
		if err := readJSON(turnPath, &turn); err != nil {
			return err
		}
		now := time.Now().UTC()
		if record.RecordMode == RecordFull && strings.TrimSpace(output) != "" {
			record.LastSequence++
			message := SessionMessage{SchemaVersion: SessionSchemaVersion, MessageID: newEntryID("message"), SessionID: sessionID,
				TurnID: turnID, Sequence: record.LastSequence, Timestamp: now, Role: "assistant", Kind: kind,
				Content: output, Metadata: map[string]any{"capture_quality": turn.CaptureQuality}}
			turn.OutputMessageID = message.MessageID
			if err := appendJSONL(filepath.Join(dir, "messages.jsonl"), message); err != nil {
				return err
			}
		}
		turn.State, turn.UpdatedAt = state, now
		if err := writeJSONAtomic(turnPath, turn); err != nil {
			return err
		}
		record.LastSequence++
		record.State, record.UpdatedAt = state, now
		if err := appendJSONL(filepath.Join(dir, "events.jsonl"), SessionEvent{SchemaVersion: SessionSchemaVersion,
			EventID: newEntryID("event"), SessionID: sessionID, TurnID: turnID, Sequence: record.LastSequence,
			Timestamp: now, Type: "turn." + state, Data: map[string]any{"capture_quality": turn.CaptureQuality}}); err != nil {
			return err
		}
		return s.writeSession(dir, record)
	})
	if err != nil {
		return err
	}
	record, err := s.Get(sessionID)
	if err != nil {
		return err
	}
	return s.updateIndex(record, dir)
}

func (s *SessionStore) UpdateSessionState(sessionID, state, summary string) error {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return err
	}
	err = withAdvisoryFileLock(s.lockPath(sessionID), func() error {
		record, err := s.readSession(dir)
		if err != nil {
			return err
		}
		record.State = state
		record.UpdatedAt = time.Now().UTC()
		if summary != "" {
			record.Summary = truncateHistoryText(summary, 512)
		}
		return s.writeSession(dir, record)
	})
	if err != nil {
		return err
	}
	record, err := s.Get(sessionID)
	if err != nil {
		return err
	}
	return s.updateIndex(record, dir)
}

func (s *SessionStore) Import(input SessionImport) (SessionRecord, error) {
	if input.Session.SessionID == "" {
		return SessionRecord{}, fmt.Errorf("import session_id is required")
	}
	if _, err := s.Get(input.Session.SessionID); err == nil {
		return SessionRecord{}, fmt.Errorf("session already exists: %s", input.Session.SessionID)
	}
	record, err := s.Create(input.Session)
	if err != nil {
		return SessionRecord{}, err
	}
	for _, message := range input.Messages {
		message.Sequence = 0
		if _, err := s.AppendMessage(record.SessionID, message); err != nil {
			return SessionRecord{}, err
		}
	}
	for _, event := range input.Events {
		event.Sequence = 0
		if err := s.AppendEvent(record.SessionID, event); err != nil {
			return SessionRecord{}, err
		}
	}
	return s.Get(record.SessionID)
}

func (s *SessionStore) ConfigureSession(sessionID, runtime, profile string) (SessionRecord, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	var record SessionRecord
	err = withAdvisoryFileLock(s.lockPath(sessionID), func() error {
		var readErr error
		record, readErr = s.readSession(dir)
		if readErr != nil {
			return readErr
		}
		record.Runtime, record.Profile = runtime, profile
		record.UpdatedAt = time.Now().UTC()
		return s.writeSession(dir, record)
	})
	if err != nil {
		return SessionRecord{}, err
	}
	return record, s.updateIndex(record, dir)
}

func (s *SessionStore) SetTags(sessionID string, tags []string) (SessionRecord, error) {
	if err := ValidateSessionTags(tags); err != nil {
		return SessionRecord{}, err
	}
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return SessionRecord{}, err
	}
	var record SessionRecord
	err = withAdvisoryFileLock(s.lockPath(sessionID), func() error {
		var readErr error
		record, readErr = s.readSession(dir)
		if readErr != nil {
			return readErr
		}
		record.Tags = appendUnique(nil, tags...)
		record.UpdatedAt = time.Now().UTC()
		return s.writeSession(dir, record)
	})
	if err != nil {
		return SessionRecord{}, err
	}
	return record, s.updateIndex(record, dir)
}

func ValidateSessionTags(tags []string) error {
	if len(tags) > 64 {
		return fmt.Errorf("session supports at most 64 tags")
	}
	for index, tag := range tags {
		if strings.TrimSpace(tag) != tag || tag == "" || len([]rune(tag)) > 64 || strings.ContainsRune(tag, '\x00') {
			return fmt.Errorf("tag[%d] is invalid", index)
		}
	}
	return nil
}

func (s *SessionStore) Trash(sessionID string) (string, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return "", err
	}
	root, err := filepath.Abs(s.sessionsDir)
	if err != nil {
		return "", err
	}
	resolved, err := filepath.Abs(dir)
	if err != nil || resolved == root || !strings.HasPrefix(resolved, root+string(filepath.Separator)) {
		return "", fmt.Errorf("refuse to trash session outside sessions root")
	}
	if _, err := os.Stat(resolved); err != nil {
		return "", err
	}
	trashDir := filepath.Join(s.historyDir, "trash", time.Now().UTC().Format("20060102-150405"), sessionID)
	if err := os.MkdirAll(filepath.Dir(trashDir), 0o700); err != nil {
		return "", err
	}
	if err := os.Rename(resolved, trashDir); err != nil {
		return "", err
	}
	err = withAdvisoryFileLock(s.indexPath()+".lock", func() error {
		index, loadErr := s.loadIndex()
		if loadErr != nil {
			return loadErr
		}
		delete(index.Sessions, sessionID)
		index.UpdatedAt = time.Now().UTC()
		return writeJSONAtomic(s.indexPath(), index)
	})
	return trashDir, err
}

func (s *SessionStore) Messages(sessionID string, after int64, limit int) ([]SessionMessage, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return nil, err
	}
	var values []SessionMessage
	if err := readJSONL(filepath.Join(dir, "messages.jsonl"), &values); err != nil {
		return nil, err
	}
	return filterMessages(values, after, limit), nil
}

func (s *SessionStore) Events(sessionID string, after int64, limit int) ([]SessionEvent, error) {
	dir, err := s.sessionDir(sessionID)
	if err != nil {
		return nil, err
	}
	var values []SessionEvent
	if err := readJSONL(filepath.Join(dir, "events.jsonl"), &values); err != nil {
		return nil, err
	}
	out := make([]SessionEvent, 0, len(values))
	for _, value := range values {
		if value.Sequence > after {
			out = append(out, value)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out, nil
}

func (s *SessionStore) View(sessionID string) (SessionView, error) {
	record, err := s.Get(sessionID)
	if err != nil {
		return SessionView{}, err
	}
	dir, _ := s.sessionDir(sessionID)
	view := SessionView{Session: record}
	view.Messages, _ = s.Messages(sessionID, 0, 0)
	view.Events, _ = s.Events(sessionID, 0, 0)
	turnFiles, _ := filepath.Glob(filepath.Join(dir, "turns", "*", "turn.json"))
	for _, path := range turnFiles {
		var turn TurnRecord
		if readJSON(path, &turn) == nil {
			view.Turns = append(view.Turns, turn)
		}
		attemptFiles, _ := filepath.Glob(filepath.Join(filepath.Dir(path), "attempts", "*.json"))
		for _, attemptPath := range attemptFiles {
			var attempt RunAttemptRecord
			if readJSON(attemptPath, &attempt) == nil {
				view.Attempts = append(view.Attempts, attempt)
			}
		}
	}
	executionFiles, _ := filepath.Glob(filepath.Join(dir, "executions", "*.json"))
	for _, path := range executionFiles {
		var execution ExecutionRecord
		if readJSON(path, &execution) == nil {
			view.Executions = append(view.Executions, execution)
		}
	}
	sort.Slice(view.Turns, func(i, j int) bool { return view.Turns[i].Sequence < view.Turns[j].Sequence })
	sort.Slice(view.Attempts, func(i, j int) bool { return view.Attempts[i].StartedAt.Before(view.Attempts[j].StartedAt) })
	sort.Slice(view.Executions, func(i, j int) bool { return view.Executions[i].StartedAt.Before(view.Executions[j].StartedAt) })
	return view, nil
}

func (s *SessionStore) Export(sessionID, outputPath string) error {
	view, err := s.View(sessionID)
	if err != nil {
		return err
	}
	return writeJSONAtomic(outputPath, map[string]any{"schema_version": SessionSchemaVersion, "exported_at": time.Now().UTC(), "view": view})
}

func (s *SessionStore) readSession(dir string) (SessionRecord, error) {
	var record SessionRecord
	err := readJSON(filepath.Join(dir, "session.json"), &record)
	return record, err
}

func (s *SessionStore) writeSession(dir string, record SessionRecord) error {
	return writeJSONAtomic(filepath.Join(dir, "session.json"), record)
}

func appendJSONL(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	return json.NewEncoder(file).Encode(value)
}

func readJSONL[T any](path string, output *[]T) error {
	file, err := os.Open(path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var value T
		if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
			return fmt.Errorf("decode %s: %w", path, err)
		}
		*output = append(*output, value)
	}
	return scanner.Err()
}

func newEntryID(prefix string) string {
	return fmt.Sprintf("%s-%d-%s", prefix, time.Now().UnixNano(), randomID(6))
}

func appendUnique(values []string, additions ...string) []string {
	seen := make(map[string]struct{}, len(values)+len(additions))
	for _, value := range values {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	for _, value := range additions {
		if value != "" {
			seen[value] = struct{}{}
		}
	}
	out := make([]string, 0, len(seen))
	for value := range seen {
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}

func truncateHistoryText(value string, limit int) string {
	runes := []rune(strings.TrimSpace(value))
	if len(runes) <= limit {
		return string(runes)
	}
	return string(runes[:limit]) + "…"
}

func filterMessages(values []SessionMessage, after int64, limit int) []SessionMessage {
	out := make([]SessionMessage, 0, len(values))
	for _, value := range values {
		if value.Sequence > after {
			out = append(out, value)
			if limit > 0 && len(out) >= limit {
				break
			}
		}
	}
	return out
}

func redactMetadata(input map[string]any) map[string]any {
	if len(input) == 0 {
		return input
	}
	out := make(map[string]any, len(input))
	for key, value := range input {
		lower := strings.ToLower(key)
		if strings.Contains(lower, "token") || strings.Contains(lower, "secret") || strings.Contains(lower, "password") || strings.Contains(lower, "authorization") || strings.Contains(lower, "cookie") {
			out[key] = "[REDACTED]"
			continue
		}
		out[key] = redactMetadataValue(value)
	}
	return out
}

func redactMetadataValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return redactMetadata(typed)
	case map[string]string:
		converted := make(map[string]any, len(typed))
		for key, item := range typed {
			converted[key] = item
		}
		return redactMetadata(converted)
	case []any:
		redacted := make([]any, len(typed))
		for index, item := range typed {
			redacted[index] = redactMetadataValue(item)
		}
		return redacted
	default:
		return value
	}
}
