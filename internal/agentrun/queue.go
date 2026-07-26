package agentrun

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
	"time"
)

const queueSchemaVersion = 1

var featureVersions = map[string]int{
	"artifact_durability":        1,
	"async_submit":               1,
	"durable_queue":              1,
	"run_follow":                 1,
	"run_list":                   1,
	"run_reconcile":              1,
	"session_context_checkpoint": 1,
	"session_memory_input":       1,
}

func SupportedFeatures() map[string]int {
	result := make(map[string]int, len(featureVersions))
	for name, version := range featureVersions {
		result[name] = version
	}
	return result
}

type QueueEntry struct {
	SchemaVersion int        `json:"schema_version"`
	Sequence      int64      `json:"sequence"`
	RunID         string     `json:"run_id"`
	RunType       string     `json:"run_type"`
	Profile       string     `json:"profile"`
	ProjectID     string     `json:"project_id,omitempty"`
	State         string     `json:"state"`
	OwnerPID      int        `json:"owner_pid,omitempty"`
	Attempt       int        `json:"attempt"`
	QueuedAt      time.Time  `json:"queued_at"`
	ClaimedAt     *time.Time `json:"claimed_at,omitempty"`
	ExpiresAt     *time.Time `json:"expires_at,omitempty"`
	Options       RunOptions `json:"options"`
}

type QueueSnapshot struct {
	SchemaVersion       int          `json:"schema_version"`
	MaxConcurrency      int          `json:"max_concurrency"`
	MaxQueue            int          `json:"max_queue"`
	QueueTimeoutSeconds int          `json:"queue_timeout_seconds"`
	Running             int          `json:"running"`
	Queued              int          `json:"queued"`
	Healthy             bool         `json:"healthy"`
	Entries             []QueueEntry `json:"entries,omitempty"`
}

type ReconcileAction struct {
	RunID   string `json:"run_id"`
	RunType string `json:"run_type"`
	Action  string `json:"action"`
	Reason  string `json:"reason"`
}

type ReconcileReport struct {
	DryRun  bool              `json:"dry_run"`
	Scanned int               `json:"scanned"`
	Actions []ReconcileAction `json:"actions"`
}

type queueDocument struct {
	SchemaVersion int          `json:"schema_version"`
	NextSequence  int64        `json:"next_sequence"`
	Entries       []QueueEntry `json:"entries"`
}

type RunFilter struct {
	Active    bool
	State     string
	RunType   string
	ProjectID string
	Profile   string
	Limit     int
}

type RunListEntry struct {
	RunID         string    `json:"run_id"`
	RunType       string    `json:"run_type"`
	ProjectID     string    `json:"project_id,omitempty"`
	SessionID     string    `json:"session_id,omitempty"`
	Profile       string    `json:"profile,omitempty"`
	State         string    `json:"state"`
	QueuePosition int       `json:"queue_position,omitempty"`
	QueuedAt      time.Time `json:"queued_at,omitzero"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	CompletedAt   time.Time `json:"completed_at,omitzero"`
	ErrorCode     string    `json:"error_code,omitempty"`
	Retryable     bool      `json:"retryable,omitempty"`
	UpdatedAt     time.Time `json:"updated_at"`
	RunDir        string    `json:"run_dir,omitempty"`
}

func (s *Service) queuePath() string {
	return filepath.Join(s.StateDir, "runs", "queue.json")
}

func (s *Service) queueLockPath() string {
	return filepath.Join(s.StateDir, "runs", "queue.lock")
}

func (s *Service) withQueue(persist bool, update func(*queueDocument) error) error {
	return withAdvisoryFileLock(s.queueLockPath(), func() error {
		document := queueDocument{SchemaVersion: queueSchemaVersion, NextSequence: 1, Entries: []QueueEntry{}}
		if data, err := os.ReadFile(s.queuePath()); err == nil {
			if err := json.Unmarshal(data, &document); err != nil {
				return fmt.Errorf("parse queue: %w", err)
			}
		} else if !os.IsNotExist(err) {
			return err
		}
		if document.SchemaVersion != queueSchemaVersion {
			return fmt.Errorf("unsupported queue schema_version=%d", document.SchemaVersion)
		}
		if document.NextSequence <= 0 {
			document.NextSequence = 1
		}
		if document.Entries == nil {
			document.Entries = []QueueEntry{}
		}
		if err := update(&document); err != nil {
			return err
		}
		if !persist {
			return nil
		}
		if err := writeJSONAtomic(s.queuePath(), document); err != nil {
			return err
		}
		return os.Chmod(s.queuePath(), 0o600)
	})
}

func (s *Service) Submit(ctx context.Context, options RunOptions) (RunSummary, error) {
	if s.configErr != nil {
		return RunSummary{}, s.configErr
	}
	if options.RunType == "" {
		options.RunType = RunTask
	}
	if !validRunType(options.RunType) || options.RunType == RunSession || options.RunType == RunCommand {
		return RunSummary{}, fmt.Errorf("Run 仅支持 task|turn，得到 %q", options.RunType)
	}
	if options.RunID == "" {
		options.RunID = newRunID(options.RunType)
	}
	if err := validateRunID(options.RunID); err != nil {
		return RunSummary{}, err
	}
	if options.CreateSession && options.SessionID == "" {
		options.SessionID = sessionIDForRun(options.RunID)
	}
	if options.Profile == "" {
		options.Profile = s.DefaultProfile
	}
	if options.ProjectID == "" {
		options.ProjectID = s.DefaultProject
	}
	if options.QueueTimeout == 0 {
		options.QueueTimeout = s.QueueTimeout
	}
	if options.QueueTimeout < 0 {
		return RunSummary{}, fmt.Errorf("queue_timeout_seconds must be non-negative")
	}
	paths, err := RunPaths(s.RunsDir, options.RunType, options.RunID)
	if err != nil {
		return RunSummary{}, err
	}
	queuedAt := time.Now().UTC()
	entry := QueueEntry{
		SchemaVersion: queueSchemaVersion, RunID: options.RunID, RunType: options.RunType,
		Profile: options.Profile, ProjectID: options.ProjectID, State: StatePending,
		OwnerPID: os.Getpid(), Attempt: 1, QueuedAt: queuedAt, Options: options,
	}
	if options.QueueTimeout > 0 {
		expires := queuedAt.Add(time.Duration(options.QueueTimeout) * time.Second)
		entry.ExpiresAt = &expires
	}
	queuePosition := 0
	err = s.withQueue(true, func(document *queueDocument) error {
		pending := 0
		for _, existing := range document.Entries {
			if existing.State == StatePending {
				pending++
			}
			if existing.RunID != entry.RunID || existing.RunType != entry.RunType {
				continue
			}
			left, _ := json.Marshal(existing.Options)
			right, _ := json.Marshal(entry.Options)
			if bytes.Equal(left, right) {
				entry = existing
				if existing.State == StatePending {
					queuePosition = pending
				}
				return nil
			}
			return fmt.Errorf("idempotency conflict for run_id %s", entry.RunID)
		}
		if pending >= s.MaxQueue {
			return fmt.Errorf("queue_full: max_queue=%d reached", s.MaxQueue)
		}
		entry.Sequence = document.NextSequence
		queuePosition = pending + 1
		document.NextSequence++
		document.Entries = append(document.Entries, entry)
		return nil
	})
	if err != nil {
		return RunSummary{}, err
	}
	if options.CreateSession {
		decision, decisionErr := DecideRecordPolicy(options.Caller, options.RunType, options.ExecutionKind,
			options.SessionID, options.RecordMode, options.Retention)
		if decisionErr != nil {
			_ = s.removeQueueEntry(entry.RunType, entry.RunID)
			return RunSummary{}, decisionErr
		}
		if decision.RecordMode == RecordOff {
			_ = s.removeQueueEntry(entry.RunType, entry.RunID)
			return RunSummary{}, fmt.Errorf("session submit does not allow record_mode=off")
		}
		cwd, cwdErr := resolveCWD(options.CWD)
		if cwdErr != nil {
			_ = s.removeQueueEntry(entry.RunType, entry.RunID)
			return RunSummary{}, cwdErr
		}
		if _, sessionErr := NewSessionManager(s).EnsureSession(options.SessionID, options.ProjectID, cwd, options.Prompt, decision); sessionErr != nil {
			_ = s.removeQueueEntry(entry.RunType, entry.RunID)
			return RunSummary{}, sessionErr
		}
	}
	if queueInlineMode() {
		return s.executeInlineQueued(ctx, entry)
	}
	if _, _, err := s.DaemonClient().EnsureRunning(ctx); err != nil {
		_ = s.removeQueueEntry(entry.RunType, entry.RunID)
		return RunSummary{}, fmt.Errorf("start runtime dispatcher: %w", err)
	}
	return RunSummary{RunID: entry.RunID, RunType: entry.RunType, ProjectID: entry.ProjectID,
		SessionID: entry.Options.SessionID, State: StatePending, QueuePosition: queuePosition,
		QueuedAt: entry.QueuedAt, RunDir: paths.RunDir, ResultFile: paths.ResultFile}, nil
}

func queueInlineMode() bool {
	if strings.EqualFold(strings.TrimSpace(os.Getenv("SN_RUNTIME_QUEUE_MODE")), "inline") {
		return true
	}
	return strings.HasSuffix(os.Args[0], ".test")
}

func (s *Service) executeInlineQueued(ctx context.Context, entry QueueEntry) (RunSummary, error) {
	claimed, err := s.claimEntry(entry.RunType, entry.RunID)
	if err != nil {
		return RunSummary{}, err
	}
	result, runErr := s.runImmediate(ctx, claimed.Options)
	removeErr := s.removeQueueEntry(claimed.RunType, claimed.RunID)
	return result, errors.Join(runErr, removeErr)
}

func (s *Service) Wait(ctx context.Context, runType, runID string) (RunSummary, error) {
	paths, err := RunPaths(s.RunsDir, runType, runID)
	if err != nil {
		return RunSummary{}, err
	}
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		status, statusErr := s.Status(runType, runID)
		if statusErr == nil && (terminalStateValue(status.State) || status.State == StateBlocked) {
			return summary(paths, status, false), terminalStatusError(status)
		}
		select {
		case <-ctx.Done():
			_, cancelErr := s.Cancel(runType, runID)
			return RunSummary{RunID: runID, RunType: runType, State: StateCancelled, RunDir: paths.RunDir}, errors.Join(ctx.Err(), cancelErr)
		case <-ticker.C:
		}
	}
}

func terminalStatusError(status Status) error {
	if status.State == StateDone {
		return nil
	}
	if status.Message != "" {
		return errors.New(status.Message)
	}
	return fmt.Errorf("run %s ended in state %s", status.RunID, status.State)
}

func (s *Service) claimEntry(runType, runID string) (QueueEntry, error) {
	var claimed QueueEntry
	err := s.withQueue(true, func(document *queueDocument) error {
		sort.SliceStable(document.Entries, func(i, j int) bool { return document.Entries[i].Sequence < document.Entries[j].Sequence })
		for index := range document.Entries {
			entry := &document.Entries[index]
			if entry.State != StatePending {
				continue
			}
			if entry.RunType != runType || entry.RunID != runID {
				return fmt.Errorf("run %s is not at the head of the queue", runID)
			}
			now := time.Now().UTC()
			entry.State, entry.OwnerPID, entry.ClaimedAt = StateRunning, os.Getpid(), &now
			claimed = *entry
			return nil
		}
		return fmt.Errorf("queued run not found: %s", runID)
	})
	return claimed, err
}

func (s *Service) claimNext() (QueueEntry, bool, error) {
	var claimed QueueEntry
	found := false
	err := s.withQueue(true, func(document *queueDocument) error {
		sort.SliceStable(document.Entries, func(i, j int) bool { return document.Entries[i].Sequence < document.Entries[j].Sequence })
		now := time.Now().UTC()
		active := document.Entries[:0]
		for _, entry := range document.Entries {
			if entry.State == StatePending && entry.ExpiresAt != nil && !entry.ExpiresAt.After(now) {
				if _, err := s.writeQueuedTerminal(entry, StateFailed, "queue_timeout", "queue wait timeout exceeded"); err != nil {
					return err
				}
				continue
			}
			active = append(active, entry)
		}
		document.Entries = active
		for index := range document.Entries {
			entry := &document.Entries[index]
			if entry.State != StatePending {
				continue
			}
			entry.State, entry.OwnerPID, entry.ClaimedAt = StateRunning, os.Getpid(), &now
			claimed, found = *entry, true
			return nil
		}
		return nil
	})
	return claimed, found, err
}

func (s *Service) removeQueueEntry(runType, runID string) error {
	return s.withQueue(true, func(document *queueDocument) error {
		filtered := document.Entries[:0]
		for _, entry := range document.Entries {
			if entry.RunType != runType || entry.RunID != runID {
				filtered = append(filtered, entry)
			}
		}
		document.Entries = filtered
		return nil
	})
}

func (s *Service) cancelQueued(runType, runID string) (RunSummary, bool, error) {
	var result RunSummary
	found := false
	err := s.withQueue(true, func(document *queueDocument) error {
		filtered := document.Entries[:0]
		for _, entry := range document.Entries {
			if !found && entry.RunType == runType && entry.RunID == runID && entry.State == StatePending {
				var writeErr error
				result, writeErr = s.writeQueuedTerminal(entry, StateCancelled, "cancelled", "cancelled while queued")
				if writeErr != nil {
					return writeErr
				}
				found = true
				continue
			}
			filtered = append(filtered, entry)
		}
		document.Entries = filtered
		return nil
	})
	return result, found, err
}

func (s *Service) writeQueuedTerminal(entry QueueEntry, state, reason, message string) (RunSummary, error) {
	paths, err := RunPaths(s.RunsDir, entry.RunType, entry.RunID)
	if err != nil {
		return RunSummary{}, err
	}
	if err := paths.Ensure(); err != nil {
		return RunSummary{}, err
	}
	request := Request{SchemaVersion: 1, ContractVersion: ContractVersion, RuntimeVersion: s.RuntimeVersion,
		ProjectID: entry.ProjectID, RunType: entry.RunType, RunID: entry.RunID, Caller: entry.Options.Caller,
		ProviderProfile: entry.Profile, Provider: "queue", CWD: entry.Options.CWD,
		ExecutionMode: entry.Options.ExecutionMode, ProviderOverrides: map[string]any{},
		AllowedActions: []string{}, ForbiddenActions: []string{}, CreatedAt: entry.QueuedAt, UpdatedAt: time.Now().UTC()}
	if err := s.store.WriteRequest(paths, request); err != nil {
		return RunSummary{}, err
	}
	status, err := s.store.WriteStatus(paths, request, state, reason, message, map[string]any{"profile": entry.Profile})
	if err != nil {
		return RunSummary{}, err
	}
	s.updateRegistry(paths, status)
	return summary(paths, status, false), nil
}

func (s *Service) QueueSnapshot(includeEntries bool) (QueueSnapshot, error) {
	snapshot := QueueSnapshot{SchemaVersion: queueSchemaVersion, MaxConcurrency: s.MaxConcurrency, MaxQueue: s.MaxQueue,
		QueueTimeoutSeconds: s.QueueTimeout, Healthy: true}
	err := s.withQueue(false, func(document *queueDocument) error {
		for _, entry := range document.Entries {
			if entry.State == StatePending {
				snapshot.Queued++
			} else if entry.State == StateRunning {
				snapshot.Running++
			}
			if includeEntries {
				snapshot.Entries = append(snapshot.Entries, entry)
			}
		}
		return nil
	})
	if err != nil {
		snapshot.Healthy = false
	}
	return snapshot, err
}

func (s *Service) queuedStatus(runType, runID string) (Status, bool, error) {
	var status Status
	found := false
	err := s.withQueue(false, func(document *queueDocument) error {
		position := 0
		for _, entry := range document.Entries {
			if entry.State == StatePending {
				position++
			}
			if entry.RunType == runType && entry.RunID == runID {
				state := entry.State
				if state == StateRunning {
					state = StatePending
				}
				status = Status{SchemaVersion: 1, RunID: runID, RunType: runType, ProjectID: entry.ProjectID,
					State: state, Provider: "queue", ProviderStatus: map[string]any{"profile": entry.Profile},
					Message: "queued", QueuedAt: entry.QueuedAt, QueuePosition: position, Attempt: entry.Attempt, UpdatedAt: entry.QueuedAt}
				found = true
				return nil
			}
		}
		return nil
	})
	return status, found, err
}

func (s *Service) DispatchQueue(ctx context.Context) error {
	if err := s.waitForDaemonReady(ctx); err != nil {
		return err
	}
	if _, err := s.ReconcileQueue(false); err != nil {
		return err
	}
	sem := make(chan struct{}, s.MaxConcurrency)
	var workers sync.WaitGroup
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	defer workers.Wait()
	for {
		select {
		case <-ctx.Done():
			return nil
		default:
		}
		if len(sem) < cap(sem) {
			entry, found, err := s.claimNext()
			if err != nil {
				return err
			}
			if found {
				sem <- struct{}{}
				workers.Add(1)
				go func(item QueueEntry) {
					defer workers.Done()
					defer func() { <-sem }()
					_, runErr := s.runImmediate(ctx, item.Options)
					if runErr != nil {
						status, statusErr := s.Status(item.RunType, item.RunID)
						if statusErr != nil || !terminalStateValue(status.State) {
							if _, terminalErr := s.writeQueuedTerminal(item, StateFailed, "dispatch_error", runErr.Error()); terminalErr != nil {
								return
							}
						}
					}
					_ = s.removeQueueEntry(item.RunType, item.RunID)
				}(entry)
				continue
			}
		}
		select {
		case <-ctx.Done():
			return nil
		case <-ticker.C:
		}
	}
}

func (s *Service) waitForDaemonReady(ctx context.Context) error {
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	timer := time.NewTimer(5 * time.Second)
	defer timer.Stop()
	for {
		if _, err := s.DaemonClient().Status(ctx); err == nil {
			return nil
		}
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-timer.C:
			return fmt.Errorf("runtime daemon did not become ready")
		case <-ticker.C:
		}
	}
}

func (s *Service) QueueBusy() bool {
	snapshot, err := s.QueueSnapshot(false)
	return err != nil || snapshot.Queued > 0 || snapshot.Running > 0
}

func (s *Service) ReconcileQueue(dryRun bool) (ReconcileReport, error) {
	report := ReconcileReport{DryRun: dryRun, Actions: []ReconcileAction{}}
	err := s.withQueue(!dryRun, func(document *queueDocument) error {
		report.Scanned = len(document.Entries)
		now := time.Now().UTC()
		kept := make([]QueueEntry, 0, len(document.Entries))
		for _, entry := range document.Entries {
			if entry.State == StatePending && entry.ExpiresAt != nil && !entry.ExpiresAt.After(now) {
				report.Actions = append(report.Actions, ReconcileAction{RunID: entry.RunID, RunType: entry.RunType, Action: "fail", Reason: "queue_timeout"})
				if !dryRun {
					if _, err := s.writeQueuedTerminal(entry, StateFailed, "queue_timeout", "queue wait timeout exceeded"); err != nil {
						return err
					}
				}
				continue
			}
			if entry.State != StateRunning {
				kept = append(kept, entry)
				continue
			}
			paths, err := RunPaths(s.RunsDir, entry.RunType, entry.RunID)
			if err != nil {
				return err
			}
			status, statusErr := s.store.ReadStatus(paths)
			if statusErr == nil && terminalStateValue(status.State) {
				report.Actions = append(report.Actions, ReconcileAction{RunID: entry.RunID, RunType: entry.RunType, Action: "remove", Reason: "terminal_artifact"})
				continue
			}
			if processAliveForQueue(entry.OwnerPID) {
				kept = append(kept, entry)
				continue
			}
			switch {
			case statusErr == nil && (status.State == StateRunning || status.State == StateResultPending):
				report.Actions = append(report.Actions, ReconcileAction{RunID: entry.RunID, RunType: entry.RunType, Action: "fail", Reason: "orphaned"})
				if !dryRun {
					request, readErr := s.store.ReadRequest(paths)
					if readErr != nil {
						return readErr
					}
					failed, writeErr := s.store.WriteStatus(paths, request, StateFailed, "orphaned", "provider process disappeared", status.ProviderStatus)
					if writeErr != nil {
						return writeErr
					}
					s.updateRegistry(paths, failed)
				}
				continue
			default:
				report.Actions = append(report.Actions, ReconcileAction{RunID: entry.RunID, RunType: entry.RunType, Action: "requeue", Reason: "unstarted_claim"})
				entry.State, entry.OwnerPID, entry.ClaimedAt = StatePending, 0, nil
				kept = append(kept, entry)
			}
		}
		if !dryRun {
			document.Entries = kept
		}
		return nil
	})
	return report, err
}

func processAliveForQueue(pid int) bool {
	return pid > 0 && syscall.Kill(pid, 0) == nil
}

func (s *Service) ListRuns(filter RunFilter) ([]RunListEntry, error) {
	if filter.Limit <= 0 {
		filter.Limit = 100
	}
	var result []RunListEntry
	snapshot, err := s.QueueSnapshot(true)
	if err != nil {
		return nil, err
	}
	position := 0
	for _, entry := range snapshot.Entries {
		if entry.State == StatePending {
			position++
		}
		state := entry.State
		if !matchesRunFilter(filter, state, entry.RunType, entry.ProjectID, entry.Profile) {
			continue
		}
		paths, _ := RunPaths(s.RunsDir, entry.RunType, entry.RunID)
		result = append(result, RunListEntry{RunID: entry.RunID, RunType: entry.RunType, ProjectID: entry.ProjectID,
			SessionID: entry.Options.SessionID, Profile: entry.Profile, State: state, QueuePosition: position,
			QueuedAt: entry.QueuedAt, UpdatedAt: entry.QueuedAt, RunDir: paths.RunDir})
	}
	registryErr := s.withRegistry(false, func(document *registryDocument) {
		for runID, entry := range document.Runs {
			state, projectID, updatedAt := entry.State, entry.ProjectID, entry.UpdatedAt
			profile := ""
			status := Status{}
			if readErr := readJSON(filepath.Join(entry.RunDir, "status.json"), &status); readErr == nil {
				state, projectID, updatedAt = status.State, status.ProjectID, status.UpdatedAt
			}
			request := Request{}
			if readErr := readJSON(filepath.Join(entry.RunDir, "request.json"), &request); readErr == nil {
				profile = request.ProviderProfile
			}
			if !matchesRunFilter(filter, state, entry.RunType, projectID, profile) {
				continue
			}
			result = append(result, RunListEntry{RunID: runID, RunType: entry.RunType, ProjectID: projectID, SessionID: request.SessionID, Profile: profile,
				State: state, QueuedAt: status.QueuedAt, StartedAt: status.StartedAt, CompletedAt: status.CompletedAt,
				ErrorCode: status.ErrorCode, Retryable: status.Retryable, UpdatedAt: updatedAt, RunDir: entry.RunDir})
		}
	})
	if registryErr != nil {
		return nil, registryErr
	}
	sort.SliceStable(result, func(i, j int) bool { return result[i].UpdatedAt.After(result[j].UpdatedAt) })
	seen := map[string]bool{}
	filtered := result[:0]
	for _, item := range result {
		key := item.RunType + "/" + item.RunID
		if seen[key] {
			continue
		}
		seen[key] = true
		filtered = append(filtered, item)
		if len(filtered) >= filter.Limit {
			break
		}
	}
	return filtered, nil
}

func matchesRunFilter(filter RunFilter, state, runType, projectID, profile string) bool {
	if filter.Active && terminalStateValue(state) {
		return false
	}
	return (filter.State == "" || filter.State == state) && (filter.RunType == "" || filter.RunType == runType) &&
		(filter.ProjectID == "" || filter.ProjectID == projectID) && (filter.Profile == "" || filter.Profile == profile)
}
