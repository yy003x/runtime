package agentrun

import (
	"bufio"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sync"
	"time"
)

type Store struct {
	eventMu sync.Mutex
}

func (s *Store) WriteRequest(paths Paths, request Request) error {
	return withAdvisoryFileLock(paths.RequestFile+".lock", func() error {
		return writeJSONAtomic(paths.RequestFile, request)
	})
}

func (s *Store) WriteStatus(paths Paths, request Request, state, failureReason, message string, providerStatus map[string]any) (Status, error) {
	status := Status{
		SchemaVersion: 1, RunID: request.RunID, RunType: request.RunType,
		ProjectID: request.ProjectID, State: state, FailureReason: failureReason,
		Provider: request.Provider, ProviderStatus: providerStatus, Message: message,
		UpdatedAt: time.Now().UTC(),
	}
	status.Attempt = 1
	status.QueuedAt = request.CreatedAt
	if status.QueuedAt.IsZero() {
		status.QueuedAt = status.UpdatedAt
	}
	if state == StateRunning {
		status.StartedAt = status.UpdatedAt
	}
	if terminalStateValue(state) || state == StateBlocked {
		status.CompletedAt = status.UpdatedAt
		status.ErrorCode = failureReason
		status.Retryable = failureReason == "queue_timeout" || failureReason == "orphaned"
	}
	if status.ProviderStatus == nil {
		status.ProviderStatus = map[string]any{}
	}
	err := withAdvisoryFileLock(paths.StatusFile+".lock", func() error {
		var existing Status
		if err := readJSON(paths.StatusFile, &existing); err == nil {
			if immutableTerminalState(existing.State) && existing.State != state {
				status = existing
				return nil
			}
			if !existing.QueuedAt.IsZero() {
				status.QueuedAt = existing.QueuedAt
			}
			if status.StartedAt.IsZero() {
				status.StartedAt = existing.StartedAt
			}
			if existing.Attempt > 0 {
				status.Attempt = existing.Attempt
			}
		}
		return writeJSONAtomic(paths.StatusFile, status)
	})
	return status, err
}

func (s *Store) ReadStatus(paths Paths) (Status, error) {
	var status Status
	err := withAdvisoryFileLock(paths.StatusFile+".lock", func() error {
		return readJSON(paths.StatusFile, &status)
	})
	return status, err
}

func (s *Store) ReadRequest(paths Paths) (Request, error) {
	var request Request
	err := withAdvisoryFileLock(paths.RequestFile+".lock", func() error {
		return readJSON(paths.RequestFile, &request)
	})
	return request, err
}

func (s *Store) ReadEvents(paths Paths) ([]Event, error) {
	var events []Event
	err := withAdvisoryFileLock(paths.EventsFile+".lock", func() error {
		var readErr error
		events, readErr = readEventsUnlocked(paths.EventsFile)
		return readErr
	})
	return events, err
}

func readEventsUnlocked(path string) ([]Event, error) {
	file, err := os.Open(path)
	if err != nil {
		if os.IsNotExist(err) {
			return []Event{}, nil
		}
		return nil, err
	}
	defer file.Close()
	var events []Event
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		var event Event
		if err := json.Unmarshal(scanner.Bytes(), &event); err != nil {
			return nil, fmt.Errorf("parse events: %w", err)
		}
		events = append(events, event)
	}
	return events, scanner.Err()
}

func (s *Store) WriteResult(paths Paths, result Result) error {
	return withAdvisoryFileLock(paths.ResultFile+".lock", func() error {
		return writeJSONAtomic(paths.ResultFile, result)
	})
}

func (s *Store) ReadResult(paths Paths) (Result, error) {
	var result Result
	err := withAdvisoryFileLock(paths.ResultFile+".lock", func() error {
		return readJSON(paths.ResultFile, &result)
	})
	return result, err
}

func (s *Store) ValidateResult(paths Paths, runID, schemaRef string) (Result, string) {
	data, readErr := os.ReadFile(paths.ResultFile)
	if readErr != nil {
		if os.IsNotExist(readErr) {
			return Result{}, "result_missing"
		}
		return Result{}, "schema_invalid"
	}
	var raw map[string]any
	if json.Unmarshal(data, &raw) != nil || !validBuiltinResult(raw, runID) {
		return Result{}, "schema_invalid"
	}
	result, err := s.ReadResult(paths)
	if err != nil {
		return Result{}, "schema_invalid"
	}
	if err := validateResultSchema(paths.ResultFile, schemaRef); err != nil {
		return Result{}, "schema_invalid"
	}
	return result, ""
}

func validBuiltinResult(raw map[string]any, runID string) bool {
	for _, key := range []string{"schema_version", "run_id", "outcome", "summary", "artifacts", "errors", "validation"} {
		if _, exists := raw[key]; !exists {
			return false
		}
	}
	version, ok := raw["schema_version"].(float64)
	if !ok || version != 1 {
		return false
	}
	id, ok := raw["run_id"].(string)
	if !ok || id != runID {
		return false
	}
	outcome, ok := raw["outcome"].(string)
	if !ok || !validOutcome(outcome) {
		return false
	}
	if _, ok := raw["summary"].(string); !ok {
		return false
	}
	if _, ok := raw["artifacts"].([]any); !ok {
		return false
	}
	if _, ok := raw["errors"].([]any); !ok {
		return false
	}
	if _, ok := raw["validation"].(map[string]any); !ok {
		return false
	}
	return true
}

func (s *Store) Event(paths Paths, request Request, eventType string, data map[string]any) error {
	s.eventMu.Lock()
	defer s.eventMu.Unlock()
	return withAdvisoryFileLock(paths.EventsFile+".lock", func() error {
		sequence := 1
		if file, err := os.Open(paths.EventsFile); err == nil {
			scanner := bufio.NewScanner(file)
			for scanner.Scan() {
				sequence++
			}
			scanErr := scanner.Err()
			closeErr := file.Close()
			if scanErr != nil || closeErr != nil {
				return errors.Join(scanErr, closeErr)
			}
		}
		record := Event{
			SchemaVersion: 1, EventID: randomID(16), RunID: request.RunID,
			RunType: request.RunType, Type: eventType, Timestamp: time.Now().UTC(),
			Sequence: sequence, Data: data,
		}
		file, err := os.OpenFile(paths.EventsFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o644)
		if err != nil {
			return fmt.Errorf("open events: %w", err)
		}
		if err := json.NewEncoder(file).Encode(record); err != nil {
			_ = file.Close()
			return err
		}
		syncErr := file.Sync()
		closeErr := file.Close()
		return errors.Join(syncErr, closeErr)
	})
}

func writeJSONAtomic(path string, value any) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	data = append(data, '\n')
	tmp := path + ".tmp-" + randomID(8)
	file, err := os.OpenFile(tmp, os.O_CREATE|os.O_EXCL|os.O_WRONLY, 0o644)
	if err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if _, err := file.Write(data); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(tmp)
		return fmt.Errorf("sync %s: %w", filepath.Base(path), err)
	}
	if err := file.Close(); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("close %s: %w", filepath.Base(path), err)
	}
	if err := os.Rename(tmp, path); err != nil {
		_ = os.Remove(tmp)
		return fmt.Errorf("replace %s: %w", filepath.Base(path), err)
	}
	directory, err := os.Open(filepath.Dir(path))
	if err != nil {
		return fmt.Errorf("open parent for %s: %w", filepath.Base(path), err)
	}
	syncErr := directory.Sync()
	closeErr := directory.Close()
	if syncErr != nil || closeErr != nil {
		return fmt.Errorf("sync parent for %s: %w", filepath.Base(path), errors.Join(syncErr, closeErr))
	}
	return nil
}

func readJSON(path string, value any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return err
	}
	if err := json.Unmarshal(data, value); err != nil {
		return fmt.Errorf("parse %s: %w", filepath.Base(path), err)
	}
	return nil
}

func randomID(bytesCount int) string {
	data := make([]byte, bytesCount)
	if _, err := rand.Read(data); err != nil {
		return fmt.Sprintf("%d", time.Now().UnixNano())
	}
	return hex.EncodeToString(data)
}

func validOutcome(value string) bool {
	return value == OutcomeSucceeded || value == OutcomeFailed || value == OutcomeBlocked || value == OutcomePartial || value == OutcomeCancelled
}

func immutableTerminalState(state string) bool {
	return state == StateDone || state == StateFailed || state == StateCancelled
}
