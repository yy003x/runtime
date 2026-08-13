package session

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/profile"
)

const (
	maxFactFileBytes = 16 << 20
	maxFactLineBytes = 2 << 20
)

type Store struct {
	sessionsDir   string
	historyDir    string
	stateDir      string
	lockDir       string
	journalDir    string
	moveDir       string
	invocationDir string
	now           func() time.Time

	mutationMu         sync.Mutex
	activeMutations    map[string]*sessionMutation
	mutationFailpoint  func(stage, relativePath string)
	mutationErrorpoint func(stage, relativePath string) error
	indexInitMu        sync.Mutex
	indexRecoveryMu    sync.Mutex
	indexReady         atomic.Bool

	directoryIdentityMu sync.Mutex
	directoryIdentities map[string]safeFileIdentity
	lockIdentityMu      sync.Mutex
	lockIdentities      map[string]safeFileIdentity
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
	canonicalSessionsDir, err := canonicalStorePath(sessionsDir)
	if err != nil {
		return nil, fmt.Errorf("resolve sessions directory: %w", err)
	}
	canonicalStateDir, err := canonicalStorePath(stateDir)
	if err != nil {
		return nil, fmt.Errorf("resolve state directory: %w", err)
	}
	store := &Store{
		sessionsDir:         canonicalSessionsDir,
		historyDir:          filepath.Join(canonicalSessionsDir, "_system"),
		stateDir:            canonicalStateDir,
		lockDir:             filepath.Join(canonicalStateDir, "session-locks"),
		journalDir:          filepath.Join(canonicalStateDir, "session-mutations"),
		moveDir:             filepath.Join(canonicalStateDir, "session-trash-moves"),
		invocationDir:       filepath.Join(canonicalStateDir, "session-invocations"),
		now:                 time.Now,
		activeMutations:     make(map[string]*sessionMutation),
		directoryIdentities: make(map[string]safeFileIdentity),
		lockIdentities:      make(map[string]safeFileIdentity),
	}
	indexExists, err := store.inspectSessionIndex()
	if err != nil {
		return nil, fmt.Errorf("validate Session index: %w", err)
	}
	store.indexReady.Store(indexExists)
	if _, err := store.recoverTrashMoves(); err != nil {
		return nil, err
	}
	if err := store.recoverExistingMutations(); err != nil {
		return nil, err
	}
	if !store.indexReady.Load() {
		exists, err := store.sessionsDirectoryExists()
		if err != nil {
			return nil, err
		}
		if !exists {
			return store, nil
		}
		if err := store.rebuildIndex(); err != nil {
			return nil, fmt.Errorf("rebuild missing Session index: %w", err)
		}
		store.indexReady.Store(true)
		// Committed Session mutations and trash moves intentionally retain
		// their journals while the index is missing. Re-run recovery after
		// reconstruction so each journal verifies its idempotent upsert/remove
		// before cleanup.
		if _, err := store.recoverTrashMoves(); err != nil {
			return nil, err
		}
		if err := store.recoverExistingMutations(); err != nil {
			return nil, err
		}
	}
	return store, nil
}

func (store *Store) sessionsDirectoryExists() (bool, error) {
	sessions, err := store.openSessionsDirectory()
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	return true, sessions.close()
}

func (store *Store) ensure() error {
	sessions, err := store.ensurePinnedRoot(store.sessionsDir)
	if err != nil {
		return err
	}
	defer sessions.close()
	history, err := sessions.openDirectory("_system", true)
	if err != nil {
		return err
	}
	if err := store.pinOpenedDirectory(history); err != nil {
		history.close()
		return err
	}
	trash, err := history.openDirectory("trash", true)
	if err != nil {
		history.close()
		return err
	}
	if err := store.pinOpenedDirectory(trash); err != nil {
		trash.close()
		history.close()
		return err
	}
	if err := trash.close(); err != nil {
		history.close()
		return err
	}
	if err := history.close(); err != nil {
		return err
	}

	state, err := store.ensurePinnedRoot(store.stateDir)
	if err != nil {
		return err
	}
	defer state.close()
	for _, name := range []string{
		"session-locks", "session-mutations", "session-trash-moves",
		"session-invocations",
	} {
		child, err := state.openDirectory(name, true)
		if err != nil {
			return err
		}
		if err := store.pinOpenedDirectory(child); err != nil {
			child.close()
			return err
		}
		if err := child.close(); err != nil {
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
	if err := store.validateSessionRootLayout(sessionID); err != nil {
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

func (store *Store) validateSessionRootLayout(sessionID string) error {
	rootPath := store.sessionDir(sessionID)
	root, err := store.openSessionRoot(sessionID)
	if err != nil {
		return err
	}
	defer root.close()
	entries, err := root.entries()
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
		if entry.isRegular() && isOwnedAtomicTempName(entry.name) {
			continue
		}
		if entry.isSymlink() {
			return fmt.Errorf("%s contains a symlink", rootPath)
		}
		switch {
		case files[entry.name] && entry.isRegular():
		case directories[entry.name] && entry.isDirectory():
		default:
			return fmt.Errorf(
				"%s contains unsupported Session fact %q",
				rootPath, entry.name,
			)
		}
	}
	return nil
}

func (store *Store) validateTurnFacts(sessionID string) error {
	rootPath := filepath.Join(store.sessionDir(sessionID), "turns")
	sessionRoot, err := store.openSessionRoot(sessionID)
	if err != nil {
		return err
	}
	defer sessionRoot.close()
	root, err := sessionRoot.openDirectory("turns", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.close()
	entries, err := root.entries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if !entry.isDirectory() {
			return fmt.Errorf(
				"%s contains unsupported turn entry %q",
				rootPath, entry.name,
			)
		}
		if err := identity.Validate(entry.name, "turn"); err != nil {
			return fmt.Errorf(
				"%s contains invalid turn %q: %w",
				rootPath, entry.name, err,
			)
		}
		turnRelativePath := filepath.Join("turns", entry.name)
		turnRoot, err := sessionRoot.openDirectory(
			turnRelativePath, false,
		)
		if err != nil {
			return err
		}
		children, err := turnRoot.entries()
		closeErr := turnRoot.close()
		if err != nil {
			return err
		}
		if closeErr != nil {
			return closeErr
		}
		turnPath := filepath.Join(rootPath, entry.name)
		hasTurn := false
		for _, child := range children {
			if child.isRegular() && isOwnedAtomicTempName(child.name) {
				continue
			}
			if !child.isRegular() {
				return fmt.Errorf(
					"%s contains unsupported fact %q",
					turnPath, child.name,
				)
			}
			path := filepath.Join(turnPath, child.name)
			relativePath := filepath.Join(turnRelativePath, child.name)
			switch child.name {
			case "turn.json":
				hasTurn = true
				var value Turn
				if err := sessionRoot.readStrictJSON(
					relativePath, maxFactLineBytes, &value,
				); err != nil {
					return err
				}
				if value.SchemaVersion != SchemaVersion {
					return unsupportedFactSchema(path, value.SchemaVersion)
				}
				if value.ID != entry.name || value.SessionID != sessionID {
					return fmt.Errorf("%s identity does not match its path", path)
				}
			case "context-manifest.json":
				var value ContextManifest
				if err := sessionRoot.readStrictJSON(
					relativePath, maxFactLineBytes, &value,
				); err != nil {
					return err
				}
				if value.SchemaVersion != SchemaVersion {
					return unsupportedFactSchema(path, value.SchemaVersion)
				}
				if value.TurnID != entry.name || value.SessionID != sessionID {
					return fmt.Errorf("%s identity does not match its path", path)
				}
			default:
				return fmt.Errorf(
					"%s contains unsupported fact %q",
					turnPath, child.name,
				)
			}
		}
		if !hasTurn {
			return fmt.Errorf("%s is missing turn.json", turnPath)
		}
	}
	return nil
}

func (store *Store) validateExecutionFacts(sessionID string) error {
	rootPath := filepath.Join(store.sessionDir(sessionID), "executions")
	sessionRoot, err := store.openSessionRoot(sessionID)
	if err != nil {
		return err
	}
	defer sessionRoot.close()
	root, err := sessionRoot.openDirectory("executions", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.close()
	entries, err := root.entries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.isRegular() && isOwnedAtomicTempName(entry.name) {
			continue
		}
		if !entry.isRegular() ||
			filepath.Ext(entry.name) != ".json" {
			return fmt.Errorf(
				"%s contains unsupported execution fact %q",
				rootPath, entry.name,
			)
		}
		executionID := strings.TrimSuffix(entry.name, ".json")
		if err := identity.Validate(executionID, "execution"); err != nil {
			return fmt.Errorf(
				"%s contains invalid execution %q: %w",
				rootPath, entry.name, err,
			)
		}
		value, err := store.loadExecution(sessionID, executionID)
		if err != nil {
			return err
		}
		if value.ID != executionID || value.SessionID != sessionID {
			return fmt.Errorf(
				"%s identity does not match its path",
				filepath.Join(rootPath, entry.name),
			)
		}
	}
	return nil
}

func (store *Store) validateContextFacts(sessionID string) error {
	rootPath := filepath.Join(store.sessionDir(sessionID), "context")
	sessionRoot, err := store.openSessionRoot(sessionID)
	if err != nil {
		return err
	}
	defer sessionRoot.close()
	root, err := sessionRoot.openDirectory("context", false)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	defer root.close()
	entries, err := root.entries()
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.isRegular() && isOwnedAtomicTempName(entry.name) {
			continue
		}
		if entry.name != "current.json" || !entry.isRegular() {
			return fmt.Errorf(
				"%s contains unsupported context fact %q",
				rootPath, entry.name,
			)
		}
		path := filepath.Join(rootPath, entry.name)
		var value ContextManifest
		if err := sessionRoot.readStrictJSON(
			filepath.Join("context", entry.name),
			maxFactLineBytes,
			&value,
		); err != nil {
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
			"move the complete state to a recoverable backup before "+
			"initializing schema %d",
		path, version, SchemaVersion, SchemaVersion,
	)
}

func isOwnedAtomicTempName(name string) bool {
	return strings.HasPrefix(name, ".runtime-") &&
		strings.HasSuffix(name, ".tmp")
}

func (store *Store) withLock(sessionID string, fn func() error) error {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return err
	}
	if err := store.ensureSessionIndex(); err != nil {
		return err
	}
	if err := store.ensure(); err != nil {
		return err
	}
	return store.withSessionFileLock(sessionID, func() error {
		if err := store.beginMutation(sessionID); err != nil {
			return err
		}
		callbackErr := fn()
		if callbackErr == nil {
			callbackErr = store.hitMutationErrorpoint(
				"after_mutation_callback", "",
			)
		}
		finishErr := store.finishMutation(sessionID, callbackErr == nil)
		switch {
		case callbackErr != nil && finishErr != nil:
			return errors.Join(
				callbackErr,
				fmt.Errorf("restore Session mutation: %w", finishErr),
			)
		case callbackErr != nil:
			return callbackErr
		default:
			return finishErr
		}
	})
}

func (store *Store) withSessionFileLock(
	sessionID string,
	fn func() error,
) error {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return err
	}
	if err := store.ensure(); err != nil {
		return err
	}
	lockDirectory, err := store.openPinnedDirectory(store.lockDir)
	if err != nil {
		return err
	}
	defer lockDirectory.close()
	file, err := lockDirectory.openRegularForLock(sessionID + ".lock")
	if err != nil {
		return err
	}
	defer file.Close()
	if err := store.pinLockIdentity(
		filepath.Join(store.lockDir, sessionID+".lock"), file,
	); err != nil {
		return err
	}
	if err := unix.Flock(int(file.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock session %s: %w", sessionID, err)
	}
	defer unix.Flock(int(file.Fd()), unix.LOCK_UN) //nolint:errcheck
	store.hitMutationFailpoint("after_session_lock_acquired", sessionID)
	if err := lockDirectory.verifyVisibleRegular(
		sessionID+".lock", file,
	); err != nil {
		return err
	}
	callbackErr := fn()
	visibleErr := lockDirectory.verifyVisibleRegular(
		sessionID+".lock", file,
	)
	if visibleErr != nil {
		return errors.Join(callbackErr, visibleErr)
	}
	return callbackErr
}

func (store *Store) loadSession(sessionID string) (Session, error) {
	var value Session
	root, err := store.openSessionRoot(sessionID)
	if err != nil {
		return Session{}, err
	}
	defer root.close()
	if err := root.readStrictJSON(
		"session.json", maxFactLineBytes, &value,
	); err != nil {
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
	path := store.sessionFile(value.ID)
	relativePath, err := store.prepareReplace(value.ID, path)
	if err != nil {
		return err
	}
	if err := store.atomicMutationJSON(value.ID, relativePath, value); err != nil {
		return err
	}
	store.hitMutationFailpoint("after_target_write", relativePath)
	return nil
}

func (store *Store) loadTurn(sessionID, turnID string) (Turn, error) {
	if err := identity.Validate(turnID, "turn"); err != nil {
		return Turn{}, err
	}
	var value Turn
	root, err := store.openSessionRoot(sessionID)
	if err != nil {
		return Turn{}, err
	}
	defer root.close()
	relativePath := filepath.Join("turns", turnID, "turn.json")
	if err := root.readStrictJSON(
		relativePath, maxFactLineBytes, &value,
	); err != nil {
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
	path := store.turnFile(value.SessionID, value.ID)
	relativePath, err := store.prepareReplace(value.SessionID, path)
	if err != nil {
		return err
	}
	if err := store.atomicMutationJSON(
		value.SessionID, relativePath, value,
	); err != nil {
		return err
	}
	store.hitMutationFailpoint("after_target_write", relativePath)
	return nil
}

func (store *Store) writeExecution(value Execution) error {
	if err := identity.Validate(value.ID, "execution"); err != nil {
		return err
	}
	path := filepath.Join(
		store.sessionDir(value.SessionID), "executions", value.ID+".json",
	)
	relativePath, err := store.prepareReplace(value.SessionID, path)
	if err != nil {
		return err
	}
	if err := store.atomicMutationJSON(
		value.SessionID, relativePath, value,
	); err != nil {
		return err
	}
	store.hitMutationFailpoint("after_target_write", relativePath)
	return nil
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
	root, err := store.openSessionRoot(sessionID)
	if err != nil {
		return Execution{}, err
	}
	defer root.close()
	if err := root.readStrictJSON(
		filepath.Join("executions", executionID+".json"),
		maxFactLineBytes,
		&value,
	); err != nil {
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
	switch value.Interface {
	case InterfaceManaged, InterfaceNativeTUI:
	default:
		return fmt.Errorf(
			"unsupported Session interface %q", value.Interface,
		)
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
	turnRelativePath, err := store.prepareReplace(value.SessionID, turnPath)
	if err != nil {
		return err
	}
	if err := store.atomicMutationJSON(
		value.SessionID, turnRelativePath, value,
	); err != nil {
		return err
	}
	store.hitMutationFailpoint("after_target_write", turnRelativePath)
	currentPath := filepath.Join(store.sessionDir(value.SessionID), "context", "current.json")
	currentRelativePath, err := store.prepareReplace(value.SessionID, currentPath)
	if err != nil {
		return err
	}
	if err := store.atomicMutationJSON(
		value.SessionID, currentRelativePath, value,
	); err != nil {
		return err
	}
	store.hitMutationFailpoint("after_target_write", currentRelativePath)
	return nil
}

func (store *Store) appendMessage(session *Session, value MessageRecord) error {
	session.MessageCount++
	value.Sequence = session.MessageCount
	path := filepath.Join(store.sessionDir(session.ID), "messages.jsonl")
	relativePath, err := store.prepareAppend(session.ID, path)
	if err != nil {
		return err
	}
	if err := store.appendMutationJSONLine(
		session.ID, relativePath, value,
	); err != nil {
		return err
	}
	store.hitMutationFailpoint("after_target_write", relativePath)
	return nil
}

func (store *Store) appendEvent(session *Session, value EventRecord) error {
	session.EventCount++
	value.Sequence = session.EventCount
	path := filepath.Join(store.sessionDir(session.ID), "events.jsonl")
	relativePath, err := store.prepareAppend(session.ID, path)
	if err != nil {
		return err
	}
	if err := store.appendMutationJSONLine(
		session.ID, relativePath, value,
	); err != nil {
		return err
	}
	store.hitMutationFailpoint("after_target_write", relativePath)
	return nil
}

func (store *Store) messages(sessionID string) ([]MessageRecord, error) {
	var values []MessageRecord
	root, err := store.openSessionRoot(sessionID)
	if err != nil {
		return nil, err
	}
	defer root.close()
	err = root.readJSONLines(
		"messages.jsonl", maxFactFileBytes, maxFactLineBytes,
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
	root, err := store.openSessionRoot(sessionID)
	if err != nil {
		return nil, err
	}
	defer root.close()
	err = root.readJSONLines(
		"events.jsonl", maxFactFileBytes, maxFactLineBytes,
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

func (store *Store) sessionDir(sessionID string) string {
	return filepath.Join(store.sessionsDir, sessionID)
}

func (store *Store) sessionFile(sessionID string) string {
	return filepath.Join(store.sessionDir(sessionID), "session.json")
}

func (store *Store) turnFile(sessionID, turnID string) string {
	return filepath.Join(store.sessionDir(sessionID), "turns", turnID, "turn.json")
}

func decodeStrict(data []byte, value any) error {
	limit := int64(len(data))
	if limit == 0 {
		limit = 1
	}
	return strictjson.Decode(bytes.NewReader(data), limit, value)
}
