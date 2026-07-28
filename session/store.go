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
)

const (
	maxFactFileBytes = 16 << 20
	maxFactLineBytes = 2 << 20
)

type Store struct {
	sessionsDir string
	historyDir  string
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
	return &Store{
		sessionsDir: filepath.Clean(sessionsDir),
		historyDir:  filepath.Join(filepath.Clean(sessionsDir), "_system"),
		lockDir:     filepath.Join(filepath.Clean(stateDir), "session-locks"),
		now:         time.Now,
	}, nil
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
