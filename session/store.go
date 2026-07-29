package session

import (
	"bufio"
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/internal/identity"
	"github.com/yy003x/runtime/profile"
)

const (
	maxFactFileBytes = 16 << 20
	maxFactLineBytes = 2 << 20
)

type Store struct {
	sessionsDir string
	historyDir  string
	stateDir    string
	lockDir     string
	now         func() time.Time
}

func NewStore(sessionsDir, stateDir string) (*Store, error) {
	for label, value := range map[string]string{
		"sessions directory": sessionsDir,
		"state directory":    stateDir,
	} {
		if strings.TrimSpace(value) == "" {
			return nil, fmt.Errorf("%s is required", label)
		}
	}
	store := &Store{
		sessionsDir: filepath.Clean(sessionsDir),
		historyDir:  filepath.Join(filepath.Clean(sessionsDir), "_system"),
		stateDir:    filepath.Clean(stateDir),
		lockDir:     filepath.Join(filepath.Clean(stateDir), "session-locks"),
		now:         time.Now,
	}
	if info, err := os.Lstat(store.stateDir); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return nil, fmt.Errorf(
				"state directory must be a directory, not a symlink",
			)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	if err := store.validateExistingSchema(); err != nil {
		return nil, err
	}
	return store, nil
}

func (store *Store) ensure() error {
	for _, directory := range []string{
		store.sessionsDir, store.historyDir,
		filepath.Join(store.historyDir, "trash"), store.lockDir,
	} {
		if err := ensureDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) validateExistingSchema() error {
	info, err := os.Lstat(store.sessionsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("sessions directory must be a directory, not a symlink")
	}
	entries, err := os.ReadDir(store.sessionsDir)
	if err != nil {
		return err
	}
	if historyInfo, err := os.Lstat(store.historyDir); err == nil {
		if historyInfo.Mode()&os.ModeSymlink != 0 || !historyInfo.IsDir() {
			return fmt.Errorf(
				"session store history must be a directory, not a symlink",
			)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	indexPath := filepath.Join(store.historyDir, "index.json")
	if _, err := os.Lstat(indexPath); err == nil {
		var index struct {
			SchemaVersion int       `json:"schema_version"`
			Sessions      []Session `json:"sessions"`
		}
		if err := readStrictJSON(indexPath, &index); err != nil {
			return err
		}
		if index.SchemaVersion != SchemaVersion {
			return unsupportedFactSchema(indexPath, index.SchemaVersion)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "_system" {
			if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
				return fmt.Errorf(
					"session store history must be a directory, not a symlink",
				)
			}
			continue
		}
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf(
				"session store contains unsupported entry %q",
				entry.Name(),
			)
		}
		if err := identity.Validate(entry.Name(), "session"); err != nil {
			return fmt.Errorf(
				"session store contains invalid directory %q: %w",
				entry.Name(), err,
			)
		}
		if err := store.validateSessionFacts(entry.Name()); err != nil {
			return err
		}
	}
	return nil
}

func (store *Store) validateSessionFacts(sessionID string) error {
	sessionValue, err := store.loadSession(sessionID)
	if err != nil {
		return err
	}
	if sessionValue.ID != sessionID {
		return fmt.Errorf(
			"%s session_id=%q does not match its directory",
			store.sessionFile(sessionID), sessionValue.ID,
		)
	}
	if err := validateSessionRootLayout(store.sessionDir(sessionID)); err != nil {
		return err
	}
	if err := store.validateTurnFacts(sessionID); err != nil {
		return err
	}
	if err := store.validateExecutionFacts(sessionID); err != nil {
		return err
	}
	if err := store.validateContextFacts(sessionID); err != nil {
		return err
	}
	messages, err := store.messages(sessionID)
	if err != nil {
		return err
	}
	for index, value := range messages {
		if value.Sequence != uint64(index+1) {
			return fmt.Errorf(
				"session %s message sequence=%d, want %d",
				sessionID, value.Sequence, index+1,
			)
		}
	}
	if sessionValue.MessageCount != uint64(len(messages)) {
		return fmt.Errorf(
			"session %s message_count=%d, facts=%d",
			sessionID, sessionValue.MessageCount, len(messages),
		)
	}
	events, err := store.events(sessionID)
	if err != nil {
		return err
	}
	for index, value := range events {
		if value.Sequence != uint64(index+1) {
			return fmt.Errorf(
				"session %s event sequence=%d, want %d",
				sessionID, value.Sequence, index+1,
			)
		}
	}
	if sessionValue.EventCount != uint64(len(events)) {
		return fmt.Errorf(
			"session %s event_count=%d, facts=%d",
			sessionID, sessionValue.EventCount, len(events),
		)
	}
	return nil
}

func validateSessionRootLayout(root string) error {
	entries, err := os.ReadDir(root)
	if err != nil {
		return err
	}
	files := map[string]bool{
		"session.json": true, "messages.jsonl": true, "events.jsonl": true,
	}
	directories := map[string]bool{
		"turns": true, "executions": true, "context": true,
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 {
			return fmt.Errorf("%s contains a symlink", root)
		}
		switch {
		case files[entry.Name()] && !entry.IsDir():
		case directories[entry.Name()] && entry.IsDir():
		default:
			return fmt.Errorf(
				"%s contains unsupported Session fact %q",
				root, entry.Name(),
			)
		}
	}
	return nil
}

func (store *Store) validateTurnFacts(sessionID string) error {
	root := filepath.Join(store.sessionDir(sessionID), "turns")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || !entry.IsDir() {
			return fmt.Errorf("%s contains unsupported turn entry %q", root, entry.Name())
		}
		if err := identity.Validate(entry.Name(), "turn"); err != nil {
			return fmt.Errorf("%s contains invalid turn %q: %w", root, entry.Name(), err)
		}
		turnRoot := filepath.Join(root, entry.Name())
		children, err := os.ReadDir(turnRoot)
		if err != nil {
			return err
		}
		hasTurn := false
		for _, child := range children {
			if child.Type()&os.ModeSymlink != 0 || child.IsDir() {
				return fmt.Errorf("%s contains unsupported fact %q", turnRoot, child.Name())
			}
			path := filepath.Join(turnRoot, child.Name())
			switch child.Name() {
			case "turn.json":
				hasTurn = true
				var value Turn
				if err := readStrictJSON(path, &value); err != nil {
					return err
				}
				if value.SchemaVersion != SchemaVersion {
					return unsupportedFactSchema(path, value.SchemaVersion)
				}
				if value.ID != entry.Name() || value.SessionID != sessionID {
					return fmt.Errorf("%s identity does not match its path", path)
				}
			case "context-manifest.json":
				var value ContextManifest
				if err := readStrictJSON(path, &value); err != nil {
					return err
				}
				if value.SchemaVersion != SchemaVersion {
					return unsupportedFactSchema(path, value.SchemaVersion)
				}
				if value.TurnID != entry.Name() || value.SessionID != sessionID {
					return fmt.Errorf("%s identity does not match its path", path)
				}
			default:
				return fmt.Errorf("%s contains unsupported fact %q", turnRoot, child.Name())
			}
		}
		if !hasTurn {
			return fmt.Errorf("%s is missing turn.json", turnRoot)
		}
	}
	return nil
}

func (store *Store) validateExecutionFacts(sessionID string) error {
	root := filepath.Join(store.sessionDir(sessionID), "executions")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Type()&os.ModeSymlink != 0 || entry.IsDir() ||
			filepath.Ext(entry.Name()) != ".json" {
			return fmt.Errorf("%s contains unsupported execution fact %q", root, entry.Name())
		}
		executionID := strings.TrimSuffix(entry.Name(), ".json")
		if err := identity.Validate(executionID, "execution"); err != nil {
			return fmt.Errorf("%s contains invalid execution %q: %w", root, entry.Name(), err)
		}
		value, err := store.loadExecution(sessionID, executionID)
		if err != nil {
			return err
		}
		if value.ID != executionID || value.SessionID != sessionID {
			return fmt.Errorf(
				"%s identity does not match its path",
				filepath.Join(root, entry.Name()),
			)
		}
	}
	return nil
}

func (store *Store) validateContextFacts(sessionID string) error {
	root := filepath.Join(store.sessionDir(sessionID), "context")
	entries, err := os.ReadDir(root)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() != "current.json" ||
			entry.Type()&os.ModeSymlink != 0 ||
			entry.IsDir() {
			return fmt.Errorf("%s contains unsupported context fact %q", root, entry.Name())
		}
		path := filepath.Join(root, entry.Name())
		var value ContextManifest
		if err := readStrictJSON(path, &value); err != nil {
			return err
		}
		if value.SchemaVersion != SchemaVersion {
			return unsupportedFactSchema(path, value.SchemaVersion)
		}
		if value.SessionID != sessionID {
			return fmt.Errorf("%s identity does not match its path", path)
		}
	}
	return nil
}

func unsupportedFactSchema(path string, version int) error {
	return fmt.Errorf(
		"%s uses unsupported Session schema_version %d; expected %d; "+
			"export with the previous binary or move the complete state to a "+
			"recoverable backup before initializing schema %d",
		path, version, SchemaVersion, SchemaVersion,
	)
}

func (store *Store) withLock(sessionID string, fn func() error) error {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return err
	}
	if err := store.ensure(); err != nil {
		return err
	}
	lockPath := filepath.Join(store.lockDir, sessionID+".lock")
	file, err := openRegularForLock(lockPath)
	if err != nil {
		return err
	}
	defer file.Close()
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock session %s: %w", sessionID, err)
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN) //nolint:errcheck
	return fn()
}

func (store *Store) loadSession(sessionID string) (Session, error) {
	var value Session
	if err := readStrictJSON(store.sessionFile(sessionID), &value); err != nil {
		return Session{}, err
	}
	if value.SchemaVersion != SchemaVersion {
		return Session{}, unsupportedFactSchema(
			store.sessionFile(sessionID), value.SchemaVersion,
		)
	}
	if err := validateSessionFact(value); err != nil {
		return Session{}, fmt.Errorf("%s: %w", store.sessionFile(sessionID), err)
	}
	return value, nil
}

func (store *Store) writeSession(value Session) error {
	return atomicJSON(store.sessionFile(value.ID), value, 0o600)
}

func (store *Store) loadTurn(sessionID, turnID string) (Turn, error) {
	if err := identity.Validate(turnID, "turn"); err != nil {
		return Turn{}, err
	}
	var value Turn
	if err := readStrictJSON(store.turnFile(sessionID, turnID), &value); err != nil {
		return Turn{}, err
	}
	if value.SchemaVersion != SchemaVersion {
		return Turn{}, unsupportedFactSchema(
			store.turnFile(sessionID, turnID), value.SchemaVersion,
		)
	}
	if err := validateTurnFact(value); err != nil {
		return Turn{}, fmt.Errorf(
			"%s: %w", store.turnFile(sessionID, turnID), err,
		)
	}
	return value, nil
}

func (store *Store) writeTurn(value Turn) error {
	return atomicJSON(store.turnFile(value.SessionID, value.ID), value, 0o600)
}

func (store *Store) writeExecution(value Execution) error {
	if err := identity.Validate(value.ID, "execution"); err != nil {
		return err
	}
	return atomicJSON(
		filepath.Join(store.sessionDir(value.SessionID), "executions", value.ID+".json"),
		value, 0o600,
	)
}

func (store *Store) loadExecution(
	sessionID, executionID string,
) (Execution, error) {
	if err := identity.Validate(executionID, "execution"); err != nil {
		return Execution{}, err
	}
	path := filepath.Join(
		store.sessionDir(sessionID), "executions", executionID+".json",
	)
	var value Execution
	if err := readStrictJSON(path, &value); err != nil {
		return Execution{}, err
	}
	if value.SchemaVersion != SchemaVersion {
		return Execution{}, unsupportedFactSchema(path, value.SchemaVersion)
	}
	if err := validateExecutionFact(value); err != nil {
		return Execution{}, fmt.Errorf("%s: %w", path, err)
	}
	return value, nil
}

func validateSessionFact(value Session) error {
	if err := identity.Validate(value.ID, "session"); err != nil {
		return err
	}
	switch value.State {
	case SessionIdle, SessionActive, SessionBlocked, SessionArchived:
	default:
		return fmt.Errorf("unsupported Session state %q", value.State)
	}
	switch value.Retention {
	case RetentionEphemeral, RetentionStandard, RetentionPinned:
	default:
		return fmt.Errorf("unsupported Session retention %q", value.Retention)
	}
	if value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return fmt.Errorf("Session timestamps are required")
	}
	if value.ActiveTurnID != "" {
		if err := identity.Validate(value.ActiveTurnID, "turn"); err != nil {
			return err
		}
	}
	switch value.LastProfileKind {
	case "", profile.KindCommand, profile.KindModel:
	default:
		return fmt.Errorf(
			"unsupported last_profile_kind %q", value.LastProfileKind,
		)
	}
	return nil
}

func validateTurnFact(value Turn) error {
	if err := identity.Validate(value.ID, "turn"); err != nil {
		return err
	}
	if err := identity.Validate(value.SessionID, "session"); err != nil {
		return err
	}
	if err := identity.Validate(value.RunID, "run"); err != nil {
		return err
	}
	if err := identity.Validate(value.ExecutionID, "execution"); err != nil {
		return err
	}
	switch value.ProfileKind {
	case profile.KindCommand, profile.KindModel:
	default:
		return fmt.Errorf("unsupported profile_kind %q", value.ProfileKind)
	}
	switch value.State {
	case TurnRunning, TurnRequiresAction, TurnCompleted, TurnFailed, TurnCancelled:
	default:
		return fmt.Errorf("unsupported Turn state %q", value.State)
	}
	if value.RequestDigest == "" || value.ConfigDigest == "" {
		return fmt.Errorf("Turn request_digest and config_digest are required")
	}
	if value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return fmt.Errorf("Turn timestamps are required")
	}
	return nil
}

func validateExecutionFact(value Execution) error {
	if err := identity.Validate(value.ID, "execution"); err != nil {
		return err
	}
	if err := identity.Validate(value.SessionID, "session"); err != nil {
		return err
	}
	if err := identity.Validate(value.TurnID, "turn"); err != nil {
		return err
	}
	if err := identity.Validate(value.RunID, "run"); err != nil {
		return err
	}
	switch value.ProfileKind {
	case profile.KindCommand, profile.KindModel:
	default:
		return fmt.Errorf("unsupported profile_kind %q", value.ProfileKind)
	}
	switch value.State {
	case ExecutionSpawnIntent, ExecutionRunning:
		if value.Outcome != "" {
			return fmt.Errorf(
				"nonterminal Execution must not have outcome %q",
				value.Outcome,
			)
		}
	case ExecutionSettled:
		switch value.Outcome {
		case OutcomeCompleted, OutcomeFailed, OutcomeCancelled, OutcomeUnknown:
		default:
			return fmt.Errorf(
				"settled Execution has unsupported outcome %q",
				value.Outcome,
			)
		}
	default:
		return fmt.Errorf("unsupported Execution state %q", value.State)
	}
	if value.RequestDigest == "" || value.ConfigDigest == "" {
		return fmt.Errorf(
			"Execution request_digest and config_digest are required",
		)
	}
	if value.CreatedAt.IsZero() || value.UpdatedAt.IsZero() {
		return fmt.Errorf("Execution timestamps are required")
	}
	return nil
}

func (store *Store) writeManifest(value ContextManifest) error {
	turnPath := filepath.Join(
		store.sessionDir(value.SessionID), "turns", value.TurnID, "context-manifest.json",
	)
	if err := atomicJSON(turnPath, value, 0o600); err != nil {
		return err
	}
	currentPath := filepath.Join(store.sessionDir(value.SessionID), "context", "current.json")
	return atomicJSON(currentPath, value, 0o600)
}

func (store *Store) appendMessage(session *Session, value MessageRecord) error {
	session.MessageCount++
	value.Sequence = session.MessageCount
	return appendJSONLine(filepath.Join(store.sessionDir(session.ID), "messages.jsonl"), value)
}

func (store *Store) appendEvent(session *Session, value EventRecord) error {
	session.EventCount++
	value.Sequence = session.EventCount
	return appendJSONLine(filepath.Join(store.sessionDir(session.ID), "events.jsonl"), value)
}

func (store *Store) messages(sessionID string) ([]MessageRecord, error) {
	var values []MessageRecord
	err := readJSONLines(
		filepath.Join(store.sessionDir(sessionID), "messages.jsonl"),
		func(data []byte) error {
			var value MessageRecord
			if err := decodeStrict(data, &value); err != nil {
				return err
			}
			values = append(values, value)
			return nil
		},
	)
	return values, err
}

func (store *Store) events(sessionID string) ([]EventRecord, error) {
	var values []EventRecord
	err := readJSONLines(
		filepath.Join(store.sessionDir(sessionID), "events.jsonl"),
		func(data []byte) error {
			var value EventRecord
			if err := decodeStrict(data, &value); err != nil {
				return err
			}
			values = append(values, value)
			return nil
		},
	)
	return values, err
}

func (store *Store) list(filter ListFilter) ([]Session, error) {
	if err := store.ensure(); err != nil {
		return nil, err
	}
	entries, err := os.ReadDir(store.sessionsDir)
	if err != nil {
		return nil, err
	}
	values := make([]Session, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if err := identity.Validate(entry.Name(), "session"); err != nil {
			continue
		}
		value, err := store.loadSession(entry.Name())
		if err != nil {
			return nil, err
		}
		if filter.State != "" && value.State != filter.State {
			continue
		}
		values = append(values, value)
	}
	sort.Slice(values, func(left, right int) bool {
		return values[left].UpdatedAt.After(values[right].UpdatedAt)
	})
	return values, nil
}

func (store *Store) rebuildIndex() error {
	if err := store.ensure(); err != nil {
		return err
	}
	lock, err := openRegularForLock(filepath.Join(store.lockDir, "index.lock"))
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock session index: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN) //nolint:errcheck
	values, err := store.list(ListFilter{})
	if err != nil {
		return err
	}
	return atomicJSON(
		filepath.Join(store.historyDir, "index.json"),
		map[string]any{"schema_version": SchemaVersion, "sessions": values},
		0o600,
	)
}

func (store *Store) sessionDir(sessionID string) string {
	return filepath.Join(store.sessionsDir, sessionID)
}

func (store *Store) sessionFile(sessionID string) string {
	return filepath.Join(store.sessionDir(sessionID), "session.json")
}

func (store *Store) turnFile(sessionID, turnID string) string {
	return filepath.Join(store.sessionDir(sessionID), "turns", turnID, "turn.json")
}

func ensureDirectory(path string) error {
	info, err := os.Lstat(path)
	if err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("%s must be a directory, not a symlink", path)
		}
		return nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	if err := os.MkdirAll(path, 0o700); err != nil {
		return fmt.Errorf("create directory %s: %w", path, err)
	}
	return nil
}

func openRegularForLock(path string) (*os.File, error) {
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf("lock path %s must be a regular file", path)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return nil, fmt.Errorf("open lock %s: %w", path, err)
	}
	return file, nil
}

func atomicJSON(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	parent := filepath.Dir(path)
	if err := ensureDirectory(parent); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("refusing to replace symlink %s", path)
	} else if err != nil && !errors.Is(err, os.ErrNotExist) {
		return err
	}
	temp, err := os.CreateTemp(parent, ".runtime-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(data); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return syncDirectory(parent)
}

func appendJSONLine(path string, value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(data) > maxFactLineBytes {
		return fmt.Errorf("fact exceeds %d bytes", maxFactLineBytes)
	}
	data = append(data, '\n')
	parent := filepath.Dir(path)
	if err := ensureDirectory(parent); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%s must be a regular file, not a symlink", path)
		}
		if info.Size()+int64(len(data)) > maxFactFileBytes {
			return fmt.Errorf("%s exceeds %d bytes", path, maxFactFileBytes)
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	file, err := os.OpenFile(path, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600)
	if err != nil {
		return err
	}
	defer file.Close()
	if _, err := file.Write(data); err != nil {
		return err
	}
	return file.Sync()
}

func readStrictJSON(path string, value any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file, not a symlink", path)
	}
	if info.Size() > maxFactLineBytes {
		return fmt.Errorf("%s exceeds %d bytes", path, maxFactLineBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	return decodeStrict(data, value)
}

func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON")
		}
		return err
	}
	return nil
}

func readJSONLines(path string, accept func([]byte) error) error {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file, not a symlink", path)
	}
	if info.Size() > maxFactFileBytes {
		return fmt.Errorf("%s exceeds %d bytes", path, maxFactFileBytes)
	}
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64<<10), maxFactLineBytes)
	line := 0
	for scanner.Scan() {
		line++
		if err := accept(scanner.Bytes()); err != nil {
			return fmt.Errorf("%s line %d: %w", path, line, err)
		}
	}
	return scanner.Err()
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
