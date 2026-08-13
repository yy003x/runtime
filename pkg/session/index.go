package session

import (
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"sort"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/internal/domain/identity"
)

const (
	sessionIndexFileName = "index.json"
	maxSessionIndexBytes = 64 << 20
)

var errSessionIndexNotReady = errors.New("Session index is not ready")

type sessionIndex struct {
	SchemaVersion int       `json:"schema_version"`
	Sessions      []Session `json:"sessions"`
}

func (store *Store) inspectSessionIndex() (bool, error) {
	history, err := store.openPinnedDirectory(store.historyDir)
	if errors.Is(err, os.ErrNotExist) {
		return false, nil
	}
	if err != nil {
		return false, err
	}
	defer history.close()
	if _, err := history.statEntry(sessionIndexFileName); errors.Is(
		err, os.ErrNotExist,
	) {
		return false, nil
	} else if err != nil {
		return false, err
	}
	_, err = store.readSessionIndexFrom(history)
	return err == nil, err
}

func (store *Store) ensureSessionIndex() error {
	store.indexInitMu.Lock()
	defer store.indexInitMu.Unlock()
	if store.indexReady.Load() {
		return nil
	}
	exists, err := store.inspectSessionIndex()
	if err != nil {
		return err
	}
	if exists {
		store.indexReady.Store(true)
		return nil
	}
	if err := store.rebuildIndex(); err != nil {
		return err
	}
	store.indexReady.Store(true)
	return nil
}

func (store *Store) list(filter ListFilter) ([]Session, error) {
	if err := ValidateListFilter(filter); err != nil {
		return nil, err
	}
	if err := store.ensureSessionIndex(); err != nil {
		return nil, err
	}
	if err := store.recoverPendingIndexUpdates(); err != nil {
		return nil, err
	}
	var index sessionIndex
	err := store.withIndexLock(func() error {
		var err error
		index, err = store.readSessionIndex()
		return err
	})
	if err != nil {
		return nil, err
	}
	values := make([]Session, 0, len(index.Sessions))
	for _, value := range index.Sessions {
		if filter.State != "" && value.State != filter.State {
			continue
		}
		if filter.Interface != "" && value.Interface != filter.Interface {
			continue
		}
		values = append(values, value)
	}
	return values, nil
}

// recoverPendingIndexUpdates lets a long-lived Store observe committed work
// left behind by another process that exited after updating canonical facts but
// before updating the derived Session index. It scans only the bounded journal
// directories; it never walks canonical Session facts.
func (store *Store) recoverPendingIndexUpdates() error {
	store.indexRecoveryMu.Lock()
	defer store.indexRecoveryMu.Unlock()
	if _, err := store.recoverTrashMoves(); err != nil {
		return err
	}
	return store.recoverExistingMutations()
}

func (store *Store) readSessionIndex() (sessionIndex, error) {
	history, err := store.openPinnedDirectory(store.historyDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sessionIndex{}, fmt.Errorf(
				"%w: %s is missing", errSessionIndexNotReady,
				filepath.Join(store.historyDir, sessionIndexFileName),
			)
		}
		return sessionIndex{}, err
	}
	defer history.close()
	return store.readSessionIndexFrom(history)
}

func (store *Store) readSessionIndexFrom(
	history *safeDirectory,
) (sessionIndex, error) {
	var index sessionIndex
	if err := history.readStrictJSON(
		sessionIndexFileName, maxSessionIndexBytes, &index,
	); err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sessionIndex{}, fmt.Errorf(
				"%w: %s is missing", errSessionIndexNotReady,
				filepath.Join(store.historyDir, sessionIndexFileName),
			)
		}
		return sessionIndex{}, err
	}
	if err := store.validateSessionIndex(index); err != nil {
		return sessionIndex{}, err
	}
	return index, nil
}

func (store *Store) validateSessionIndex(index sessionIndex) error {
	path := filepath.Join(store.historyDir, sessionIndexFileName)
	if index.SchemaVersion != SchemaVersion {
		return unsupportedFactSchema(path, index.SchemaVersion)
	}
	seen := make(map[string]struct{}, len(index.Sessions))
	for offset, value := range index.Sessions {
		if value.SchemaVersion != SchemaVersion {
			return unsupportedFactSchema(path, value.SchemaVersion)
		}
		if err := validateSessionFact(value); err != nil {
			return fmt.Errorf(
				"%s sessions[%d]: %w", path, offset, err,
			)
		}
		if _, duplicate := seen[value.ID]; duplicate {
			return fmt.Errorf(
				"%s contains duplicate Session %s", path, value.ID,
			)
		}
		seen[value.ID] = struct{}{}
		if offset > 0 && sessionSummaryBefore(
			value, index.Sessions[offset-1],
		) {
			return fmt.Errorf(
				"%s Sessions are not in canonical order", path,
			)
		}
	}
	return nil
}

func sessionSummaryBefore(left, right Session) bool {
	if left.UpdatedAt.Equal(right.UpdatedAt) {
		return left.ID < right.ID
	}
	return left.UpdatedAt.After(right.UpdatedAt)
}

func sortSessionSummaries(values []Session) {
	sort.Slice(values, func(left, right int) bool {
		return sessionSummaryBefore(values[left], values[right])
	})
}

func (store *Store) updateIndexForCommittedSession(sessionID string) error {
	if !store.indexReady.Load() {
		return errSessionIndexNotReady
	}
	value, err := store.loadSession(sessionID)
	if err != nil {
		return err
	}
	return store.mutateSessionIndex(sessionID, &value)
}

func (store *Store) removeSessionFromIndex(sessionID string) error {
	if !store.indexReady.Load() {
		return errSessionIndexNotReady
	}
	return store.mutateSessionIndex(sessionID, nil)
}

func (store *Store) mutateSessionIndex(
	sessionID string,
	upsert *Session,
) error {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return err
	}
	if upsert != nil {
		if upsert.ID != sessionID {
			return fmt.Errorf("Session index upsert identity does not match")
		}
		if upsert.SchemaVersion != SchemaVersion {
			return unsupportedFactSchema(store.sessionFile(sessionID), upsert.SchemaVersion)
		}
		if err := validateSessionFact(*upsert); err != nil {
			return err
		}
	}
	return store.withIndexLock(func() error {
		store.hitMutationFailpoint("before_index_update", sessionID)
		if err := store.hitMutationErrorpoint(
			"before_index_update", sessionID,
		); err != nil {
			return err
		}
		index, err := store.readSessionIndex()
		if err != nil {
			return err
		}
		values := make([]Session, 0, len(index.Sessions)+1)
		for _, value := range index.Sessions {
			if value.ID != sessionID {
				values = append(values, value)
			}
		}
		if upsert != nil {
			position := sort.Search(len(values), func(offset int) bool {
				return sessionSummaryBefore(*upsert, values[offset])
			})
			values = append(values, Session{})
			copy(values[position+1:], values[position:])
			values[position] = *upsert
		}
		if !reflect.DeepEqual(values, index.Sessions) {
			if err := store.writeSessionIndex(sessionIndex{
				SchemaVersion: SchemaVersion,
				Sessions:      values,
			}); err != nil {
				return err
			}
		}
		store.hitMutationFailpoint("after_index_update", sessionID)
		return store.hitMutationErrorpoint("after_index_update", sessionID)
	})
}

func (store *Store) writeSessionIndex(index sessionIndex) error {
	if err := store.validateSessionIndex(index); err != nil {
		return err
	}
	data, err := json.MarshalIndent(index, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if len(data) > maxSessionIndexBytes {
		return fmt.Errorf(
			"Session index exceeds %d bytes", maxSessionIndexBytes,
		)
	}
	history, err := store.openPinnedDirectory(store.historyDir)
	if err != nil {
		return err
	}
	defer history.close()
	return history.atomicBytes(sessionIndexFileName, data, 0o600, nil)
}

func (store *Store) withIndexLock(fn func() error) (resultErr error) {
	if err := store.ensure(); err != nil {
		return err
	}
	lockDirectory, err := store.openPinnedDirectory(store.lockDir)
	if err != nil {
		return err
	}
	defer lockDirectory.close()
	lock, err := lockDirectory.openRegularForLock("index.lock")
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := store.pinLockIdentity(
		filepath.Join(store.lockDir, "index.lock"), lock,
	); err != nil {
		return err
	}
	if err := unix.Flock(int(lock.Fd()), unix.LOCK_EX); err != nil {
		return fmt.Errorf("lock session index: %w", err)
	}
	defer unix.Flock(int(lock.Fd()), unix.LOCK_UN) //nolint:errcheck
	store.hitMutationFailpoint("after_index_lock_acquired", "index.lock")
	if err := lockDirectory.verifyVisibleRegular("index.lock", lock); err != nil {
		return err
	}
	defer func() {
		visibleErr := lockDirectory.verifyVisibleRegular("index.lock", lock)
		if visibleErr != nil {
			resultErr = errors.Join(resultErr, visibleErr)
		}
	}()
	return fn()
}

// rebuildIndex reconstructs the derived Session list read model from
// canonical session.json facts. It never reads the existing index as its data
// source and is only used for deterministic missing-index repair and explicit
// maintenance.
func (store *Store) rebuildIndex() error {
	for attempt := 0; attempt < 4; attempt++ {
		baseline, baselineExists, err := store.readSessionIndexSnapshot()
		if err != nil {
			return err
		}
		values, err := store.scanSessionSummaries(false)
		if err != nil {
			return err
		}
		retry := false
		err = store.withIndexLock(func() error {
			current, currentExists, err := store.readSessionIndexSnapshotUnlocked()
			if err != nil {
				return err
			}
			if baselineExists != currentExists ||
				(baselineExists && !reflect.DeepEqual(baseline, current)) {
				retry = true
				return nil
			}
			return store.writeSessionIndex(sessionIndex{
				SchemaVersion: SchemaVersion,
				Sessions:      values,
			})
		})
		if err != nil {
			return err
		}
		if !retry {
			return nil
		}
	}
	return fmt.Errorf("Session index changed repeatedly during rebuild")
}

func (store *Store) readSessionIndexSnapshot() (
	sessionIndex,
	bool,
	error,
) {
	var index sessionIndex
	var exists bool
	err := store.withIndexLock(func() error {
		var err error
		index, exists, err = store.readSessionIndexSnapshotUnlocked()
		return err
	})
	return index, exists, err
}

func (store *Store) readSessionIndexSnapshotUnlocked() (
	sessionIndex,
	bool,
	error,
) {
	history, err := store.openPinnedDirectory(store.historyDir)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return sessionIndex{}, false, nil
		}
		return sessionIndex{}, false, err
	}
	defer history.close()
	if _, err := history.statEntry(sessionIndexFileName); errors.Is(
		err, os.ErrNotExist,
	) {
		return sessionIndex{}, false, nil
	} else if err != nil {
		return sessionIndex{}, false, err
	}
	index, err := store.readSessionIndexFrom(history)
	return index, err == nil, err
}

func (store *Store) scanSessionSummaries(
	validateFacts bool,
) ([]Session, error) {
	sessions, err := store.openSessionsDirectory()
	if errors.Is(err, os.ErrNotExist) {
		return []Session{}, nil
	}
	if err != nil {
		return nil, err
	}
	defer sessions.close()
	entries, err := sessions.entries()
	if err != nil {
		return nil, err
	}
	values := make([]Session, 0, len(entries))
	for _, entry := range entries {
		if entry.name == "_system" {
			if !entry.isDirectory() {
				return nil, fmt.Errorf(
					"session store history must be a directory, not a symlink",
				)
			}
			continue
		}
		if !entry.isDirectory() {
			return nil, fmt.Errorf(
				"session store contains unsupported entry %q", entry.name,
			)
		}
		if err := identity.Validate(entry.name, "session"); err != nil {
			return nil, fmt.Errorf(
				"session store contains invalid directory %q: %w",
				entry.name, err,
			)
		}
		var value Session
		err := store.withSessionFileLock(entry.name, func() error {
			root, err := store.openSessionRoot(entry.name)
			if errors.Is(err, os.ErrNotExist) {
				return nil
			}
			if err != nil {
				return err
			}
			if err := root.close(); err != nil {
				return err
			}
			if validateFacts {
				if err := store.recoverMutationLocked(entry.name); err != nil {
					return err
				}
				if err := store.validateSessionFacts(entry.name); err != nil {
					return err
				}
			}
			value, err = store.loadSession(entry.name)
			return err
		})
		if err != nil {
			return nil, err
		}
		if value.ID != "" {
			values = append(values, value)
		}
	}
	sortSessionSummaries(values)
	return values, nil
}

// Validate performs an explicit full verification of every canonical Session
// fact and checks that the derived list index exactly matches session.json.
// Ordinary Store construction deliberately does not perform this O(N) scan.
func (store *Store) Validate() error {
	if !store.indexReady.Load() {
		sessions, err := store.openSessionsDirectory()
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		if err := sessions.close(); err != nil {
			return err
		}
		if err := store.ensureSessionIndex(); err != nil {
			return err
		}
	}
	if err := store.recoverPendingIndexUpdates(); err != nil {
		return err
	}
	values, err := store.scanSessionSummaries(true)
	if err != nil {
		return err
	}
	var index sessionIndex
	if err := store.withIndexLock(func() error {
		var err error
		index, err = store.readSessionIndex()
		return err
	}); err != nil {
		return err
	}
	if !reflect.DeepEqual(values, index.Sessions) {
		return fmt.Errorf(
			"%s does not match canonical Session facts",
			filepath.Join(store.historyDir, sessionIndexFileName),
		)
	}
	return nil
}
