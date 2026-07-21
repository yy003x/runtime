package agentrun

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"time"

	"agent-runtime/internal/provider"
)

type SessionSummary struct {
	RunID       string         `json:"run_id"`
	SessionID   string         `json:"session_id,omitempty"`
	ExecutionID string         `json:"execution_id,omitempty"`
	ProjectID   string         `json:"project_id"`
	State       string         `json:"state"`
	RunDir      string         `json:"run_dir"`
	Carrier     string         `json:"carrier,omitempty"`
	Session     string         `json:"carrier_id,omitempty"`
	Attach      string         `json:"attach,omitempty"`
	Alive       bool           `json:"alive"`
	Idempotent  bool           `json:"idempotent,omitempty"`
	Status      map[string]any `json:"provider_status,omitempty"`
}

type SessionOptions struct {
	Profile          string
	ProjectID        string
	CWD              string
	SessionID        string
	RunID            string
	Carrier          string
	Prompt           string
	PromptFile       string
	RawCLIArgs       []string
	AllowedActions   []string
	ForbiddenActions []string
	Force            bool
	RecordMode       string
	Retention        string
}

// StartSession is retained as a Go API convenience. CLI routing uses
// StartSessionWithOptions so logical Session and execution Run IDs stay
// independent.
func (s *Service) StartSession(ctx context.Context, profileName, projectID, cwd, runID string, force bool) (SessionSummary, error) {
	return s.StartSessionWithOptions(ctx, SessionOptions{Profile: profileName, ProjectID: projectID, CWD: cwd, RunID: runID, Carrier: "tmux", Force: force})
}

func (s *Service) StartSessionWithOptions(ctx context.Context, options SessionOptions) (SessionSummary, error) {
	profiles, err := s.Profiles()
	if err != nil {
		return SessionSummary{}, err
	}
	profile, ok := provider.Resolve(profiles, options.Profile)
	if !ok {
		return SessionSummary{}, fmt.Errorf("unknown provider profile: %s", options.Profile)
	}
	carrier, err := s.resolveSessionCarrier(options.Carrier)
	if err != nil {
		return SessionSummary{}, err
	}
	profile, err = carrier.PrepareProfile(profile)
	if err != nil {
		return SessionSummary{}, err
	}
	if options.ProjectID == "" {
		options.ProjectID = s.DefaultProject
	}
	resolvedCWD, err := resolveCWD(options.CWD)
	if err != nil {
		return SessionSummary{}, err
	}
	prompt, promptFile, err := resolveOptionalPrompt(resolvedCWD, options.Prompt, options.PromptFile)
	if err != nil {
		return SessionSummary{}, err
	}
	if carrier.Name() == "terminal" && strings.TrimSpace(prompt) != "" {
		return SessionSummary{}, fmt.Errorf("terminal carrier does not support an injected initial prompt; type in the opened window or use session run")
	}
	if options.SessionID == "" {
		options.SessionID = newRunID(RunSession)
	}
	if options.RunID == "" {
		options.RunID = newRunID(RunSession)
	}
	if options.RunID == options.SessionID {
		return SessionSummary{}, fmt.Errorf("session_id and run_id must be independent")
	}
	decision, err := DecideRecordPolicy("cli.session.open", RunSession, carrier.ExecutionKind(), options.SessionID, options.RecordMode, options.Retention)
	if err != nil {
		return SessionSummary{}, err
	}
	if decision.RecordMode == RecordOff {
		return SessionSummary{}, fmt.Errorf("session open does not allow record_mode=off")
	}
	paths, err := RunPaths(s.RunsDir, RunSession, options.RunID)
	if err != nil {
		return SessionSummary{}, err
	}
	runLock, err := s.acquireRunLock(ctx, options.RunID)
	if err != nil {
		return SessionSummary{}, err
	}
	defer runLock.release()
	now := time.Now().UTC()
	request := Request{
		SchemaVersion: 1, ContractVersion: ContractVersion, RuntimeVersion: s.RuntimeVersion,
		ProjectID: options.ProjectID, RunType: RunSession, RunID: options.RunID,
		ProviderProfile: profile.ID, Provider: carrier.Name(), CWD: resolvedCWD,
		PromptFile: promptFile, RawCLIArgs: append([]string(nil), options.RawCLIArgs...), ResultFile: paths.ResultFile,
		ExecutionMode: ModeManaged, SessionID: options.SessionID, ExecutionID: executionIDForRun(options.RunID), ExecutionKind: carrier.ExecutionKind(),
		RecordMode: decision.RecordMode, Retention: decision.Retention, CaptureQuality: decision.CaptureQuality,
		ProviderOverrides: map[string]any{}, AllowedActions: append([]string(nil), options.AllowedActions...),
		ForbiddenActions: append([]string(nil), options.ForbiddenActions...), CreatedAt: now, UpdatedAt: now,
	}
	request.RequestFingerprint, err = fingerprintRequest(request, prompt, profile)
	if err != nil {
		return SessionSummary{}, err
	}
	if !options.Force {
		if _, found, existingErr := s.existingRun(paths, request); found {
			if existingErr != nil {
				return SessionSummary{}, existingErr
			}
			existing, statusErr := s.SessionStatus(ctx, options.RunID)
			existing.Idempotent = true
			return existing, statusErr
		}
	}
	if options.Force {
		if _, statErr := os.Stat(paths.RequestFile); statErr == nil {
			return SessionSummary{}, fmt.Errorf("cannot --force an existing carrier execution; use a new --run-id")
		}
	}
	if err := paths.Ensure(); err != nil {
		return SessionSummary{}, err
	}
	manager := NewSessionManager(s)
	if _, err := manager.EnsureSession(request.SessionID, request.ProjectID, request.CWD, prompt, decision); err != nil {
		return SessionSummary{}, err
	}
	if _, err := manager.Store().ConfigureSession(request.SessionID, carrier.Name(), profile.ID); err != nil {
		return SessionSummary{}, err
	}
	pendingExecution := carrierExecutionRecord(request, carrier, profile, paths, carrierHandle{}, StatePending)
	if err := manager.Store().UpsertExecution(request.SessionID, pendingExecution); err != nil {
		return SessionSummary{}, err
	}
	if err := s.store.WriteRequest(paths, request); err != nil {
		return SessionSummary{}, err
	}
	pendingStatus, err := s.store.WriteStatus(paths, request, StatePending, "", "carrier queued", map[string]any{"carrier": carrier.Name()})
	if err != nil {
		return SessionSummary{}, err
	}
	s.register(paths, request, pendingStatus.State)
	failBeforeStart := func(cause error, reason string) error {
		failedStatus, statusErr := s.store.WriteStatus(paths, request, StateFailed, reason, cause.Error(), map[string]any{"carrier": carrier.Name()})
		if statusErr == nil {
			s.updateRegistry(paths, failedStatus)
		}
		eventErr := s.store.Event(paths, request, "status.changed", map[string]any{"state": StateFailed, "failure_reason": reason})
		finalizeErr := s.finalizeCarrierRun(paths, request, carrier, map[string]any{"carrier": carrier.Name()}, StateFailed, reason)
		return errors.Join(cause, statusErr, eventErr, finalizeErr)
	}
	handle, err := carrier.Start(ctx, s, profile, request, options, paths)
	if err != nil {
		return SessionSummary{}, failBeforeStart(err, "provider_error")
	}
	providerStatus := handle.Status
	failStarted := func(cause error, reason string) error {
		stopErr := carrier.Stop(context.Background(), s, request, providerStatus)
		failedStatus, statusErr := s.store.WriteStatus(paths, request, StateFailed, reason, cause.Error(), providerStatus)
		if statusErr == nil {
			s.updateRegistry(paths, failedStatus)
		}
		eventErr := s.store.Event(paths, request, "status.changed", map[string]any{"state": StateFailed, "failure_reason": reason})
		finalizeErr := s.finalizeCarrierRun(paths, request, carrier, providerStatus, StateFailed, reason)
		return errors.Join(cause, stopErr, statusErr, eventErr, finalizeErr)
	}
	if err := manager.Store().UpsertExecution(request.SessionID, carrierExecutionRecord(request, carrier, profile, paths, handle, StateRunning)); err != nil {
		return SessionSummary{}, failStarted(err, "history_sync_failed")
	}
	if err := manager.Store().AppendEvent(request.SessionID, SessionEvent{ExecutionID: request.ExecutionID, RunID: request.RunID,
		Type: "execution.started", Data: map[string]any{"kind": carrier.ExecutionKind(), "carrier": carrier.Name(), "carrier_id": handle.ID}}); err != nil {
		return SessionSummary{}, failStarted(err, "history_sync_failed")
	}
	if err := manager.Store().UpdateSessionState(request.SessionID, SessionStateActive, ""); err != nil {
		return SessionSummary{}, failStarted(err, "history_sync_failed")
	}
	status, err := s.store.WriteStatus(paths, request, StateRunning, "", "carrier running", providerStatus)
	if err != nil {
		return SessionSummary{}, failStarted(err, "status_sync_failed")
	}
	if err := s.store.Event(paths, request, "status.changed", map[string]any{"state": StateRunning, "carrier": carrier.Name()}); err != nil {
		return SessionSummary{}, failStarted(err, "event_sync_failed")
	}
	if strings.TrimSpace(prompt) != "" {
		summary, sendErr := s.SessionSend(ctx, options.RunID, prompt, true)
		if sendErr != nil {
			return summary, failStarted(sendErr, "prompt_submit_failed")
		}
		return summary, nil
	}
	return carrierSummary(paths, request, status, handle, true), nil
}

func carrierExecutionRecord(request Request, carrier sessionCarrier, profile provider.Config, paths Paths, handle carrierHandle, state string) ExecutionRecord {
	return ExecutionRecord{
		ExecutionID: request.ExecutionID, Kind: carrier.ExecutionKind(), Carrier: carrier.Name(), CarrierID: handle.ID,
		Profile: profile.ID, Provider: request.Provider, State: state, CaptureQuality: request.CaptureQuality,
		CWD: request.CWD, ProcessID: handle.ProcessID, RawArgCount: len(request.RawCLIArgs), TranscriptRef: paths.OutputLog,
		RunIDs: []string{request.RunID},
	}
}

func resolveOptionalPrompt(cwd, inline, file string) (string, string, error) {
	if strings.TrimSpace(inline) == "" && strings.TrimSpace(file) == "" {
		return "", "", nil
	}
	return resolvePrompt(cwd, inline, file)
}

func (s *Service) SessionStatus(ctx context.Context, runID string) (SessionSummary, error) {
	paths, err := RunPaths(s.RunsDir, RunSession, runID)
	if err != nil {
		return SessionSummary{}, err
	}
	request, err := s.store.ReadRequest(paths)
	if err != nil {
		return SessionSummary{}, fmt.Errorf("carrier execution does not exist: %s", runID)
	}
	status, err := s.store.ReadStatus(paths)
	if err != nil {
		return SessionSummary{}, err
	}
	providerStatus := ensureProviderStatus(&status)
	carrier, err := s.carrierForStoredExecution(request, providerStatus)
	if err != nil {
		return SessionSummary{}, err
	}
	alive, carrierErr := carrier.Alive(ctx, s, request, providerStatus, paths)
	if carrierErr != nil {
		return SessionSummary{}, carrierErr
	}
	providerStatus["alive"] = alive
	if !alive && status.State == StateRunning {
		if code, attempts, ok := carrier.Exit(paths); ok {
			state, reason := StateDone, ""
			if code != 0 {
				state, reason = StateFailed, "provider_exit"
			}
			providerStatus["returncode"] = code
			providerStatus["attempts"] = attempts
			releaseErr := carrier.Release(ctx, s, request, providerStatus)
			status, carrierErr = s.store.WriteStatus(paths, request, state, reason, "carrier process exited", providerStatus)
			s.updateRegistry(paths, status)
			eventErr := s.store.Event(paths, request, "provider.exited", map[string]any{"returncode": code, "attempts": attempts, "carrier": carrier.Name()})
			finalizeErr := s.finalizeCarrierRun(paths, request, carrier, providerStatus, state, reason)
			carrierErr = errors.Join(releaseErr, carrierErr, eventErr, finalizeErr)
		} else {
			providerStatus["alive"] = false
			releaseErr := carrier.Release(ctx, s, request, providerStatus)
			status, carrierErr = s.store.WriteStatus(paths, request, StateFailed, "orphaned", "carrier disappeared without exit marker", providerStatus)
			s.updateRegistry(paths, status)
			eventErr := s.store.Event(paths, request, "provider.disappeared", map[string]any{"carrier": carrier.Name(), "carrier_id": carrierID(providerStatus)})
			finalizeErr := s.finalizeCarrierRun(paths, request, carrier, providerStatus, StateFailed, "orphaned")
			carrierErr = errors.Join(releaseErr, carrierErr, eventErr, finalizeErr)
		}
	}
	handle := carrierHandle{ID: carrierID(providerStatus), Status: providerStatus, ProcessID: intStatus(providerStatus, "pid")}
	if carrier.Name() == "tmux" && handle.ID != "" {
		handle.Attach = "tmux attach-session -t " + handle.ID
	}
	return carrierSummary(paths, request, status, handle, alive), carrierErr
}

func (s *Service) carrierForStoredExecution(request Request, status map[string]any) (sessionCarrier, error) {
	name, _ := status["carrier"].(string)
	if name == "" {
		switch request.ExecutionKind {
		case ExecutionTerminal:
			name = "terminal"
		default:
			name = "tmux"
		}
	}
	if name == "terminal" {
		driver, _ := status["terminal_driver"].(string)
		if driver == "" {
			driver = s.TerminalDriver
		}
		return terminalSessionCarrier{driver: driver}, nil
	}
	return tmuxSessionCarrier{}, nil
}

func sessionExitFile(paths Paths) string {
	return filepath.Join(paths.RunDir, "session-exit")
}

func readSessionExit(path string) (code, attempts int, ok bool) {
	data, err := os.ReadFile(path)
	if err != nil {
		return 0, 0, false
	}
	fields := strings.Fields(string(data))
	if len(fields) != 2 {
		return 0, 0, false
	}
	code, err = strconv.Atoi(fields[0])
	if err != nil {
		return 0, 0, false
	}
	attempts, err = strconv.Atoi(fields[1])
	return code, attempts, err == nil
}

func (s *Service) SessionSend(ctx context.Context, runID, text string, submit bool) (SessionSummary, error) {
	summary, err := s.SessionStatus(ctx, runID)
	if err != nil {
		return SessionSummary{}, err
	}
	if !summary.Alive {
		return summary, fmt.Errorf("carrier is not alive: %s", summary.Session)
	}
	paths, _ := RunPaths(s.RunsDir, RunSession, runID)
	request, err := s.store.ReadRequest(paths)
	if err != nil {
		return summary, err
	}
	carrier, err := s.carrierForStoredExecution(request, summary.Status)
	if err != nil {
		return summary, err
	}
	if carrier.Name() == "terminal" {
		return summary, fmt.Errorf("%w: terminal carrier does not support input injection; type in the opened window", errCarrierOperationUnsupported)
	}
	profiles, err := s.Profiles()
	if err != nil {
		return summary, err
	}
	profile, ok := provider.Resolve(profiles, request.ProviderProfile)
	if !ok {
		return summary, fmt.Errorf("carrier profile is unavailable: %s", request.ProviderProfile)
	}
	profile, err = carrier.PrepareProfile(profile)
	if err != nil {
		return summary, err
	}
	manager := NewSessionManager(s)
	turnID := ""
	if submit && request.RecordMode != RecordOff {
		turnID = newRunID(RunTurn)
		manifest, compileErr := manager.CompileContext(request.SessionID, turnID, request.CWD, profile, text, request.AllowedActions, request.ForbiddenActions)
		if compileErr != nil {
			return summary, compileErr
		}
		if _, addErr := manager.Store().AddTurn(request.SessionID, TurnRecord{TurnID: turnID, Runtime: carrier.Name(),
			Provider: request.Provider, Profile: request.ProviderProfile, Model: requestModel(request, profile),
			RecordMode: request.RecordMode, CaptureQuality: request.CaptureQuality}, text, manifest); addErr != nil {
			return summary, addErr
		}
	}
	if err := carrier.Send(ctx, s, profile, request, summary.Status, text, submit); err != nil {
		if turnID != "" {
			if completeErr := manager.Store().CompleteTurn(request.SessionID, turnID, StateFailed, err.Error(), "error"); completeErr != nil {
				return summary, errors.Join(err, completeErr)
			}
		}
		return summary, err
	}
	if submit {
		status, statusErr := s.store.ReadStatus(paths)
		if statusErr != nil {
			return summary, statusErr
		}
		providerStatus := ensureProviderStatus(&status)
		providerStatus["prompt_submitted"] = true
		providerStatus["last_prompt_submitted_at"] = time.Now().UTC()
		updated, writeErr := s.store.WriteStatus(paths, request, status.State, status.FailureReason, status.Message, providerStatus)
		if writeErr != nil {
			return summary, writeErr
		}
		s.updateRegistry(paths, updated)
		if eventErr := s.store.Event(paths, request, "prompt.submitted", map[string]any{"submitted": true, "turn_id": turnID}); eventErr != nil {
			return summary, eventErr
		}
		if err := manager.Store().CompleteTurn(request.SessionID, turnID, TurnStateSubmitted, "", "transcript_snapshot"); err != nil {
			return summary, err
		}
		if err := manager.Store().UpsertExecution(request.SessionID, ExecutionRecord{ExecutionID: request.ExecutionID,
			Kind: carrier.ExecutionKind(), Carrier: carrier.Name(), CarrierID: summary.Session, Profile: request.ProviderProfile,
			Provider: request.Provider, State: StateRunning, CaptureQuality: request.CaptureQuality,
			CWD: request.CWD, RawArgCount: len(request.RawCLIArgs), TranscriptRef: paths.OutputLog,
			RunIDs: []string{request.RunID}, TurnIDs: []string{turnID}}); err != nil {
			return summary, err
		}
		if err := manager.Store().UpdateSessionState(request.SessionID, SessionStateActive, ""); err != nil {
			return summary, err
		}
	}
	return s.SessionStatus(ctx, runID)
}

func (s *Service) SessionList(ctx context.Context) ([]SessionSummary, error) {
	type listedSession struct {
		summary   SessionSummary
		updatedAt time.Time
	}
	var entries []registryEntry
	if err := s.withRegistry(false, func(document *registryDocument) {
		for _, entry := range document.Runs {
			if entry.RunType == RunSession {
				entries = append(entries, entry)
			}
		}
	}); err != nil {
		return nil, err
	}
	listed := make([]listedSession, 0, len(entries))
	var reconcileErrors []error
	for _, entry := range entries {
		var status Status
		if err := readJSON(filepath.Join(entry.RunDir, "status.json"), &status); err != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("read carrier status %s: %w", entry.RunDir, err))
			continue
		}
		current, statusErr := s.SessionStatus(ctx, status.RunID)
		if statusErr != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("reconcile carrier run %s: %w", status.RunID, statusErr))
			continue
		}
		listed = append(listed, listedSession{summary: current, updatedAt: entry.UpdatedAt})
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].updatedAt.After(listed[j].updatedAt) })
	result := make([]SessionSummary, len(listed))
	for i := range listed {
		result[i] = listed[i].summary
	}
	return result, errors.Join(reconcileErrors...)
}

// ResolveSessionCarrierRun maps a logical Session to its newest carrier Run.
// The relationship is read from Execution records; IDs are never inferred
// from prefixes or assumed to be equal.
func (s *Service) ResolveSessionCarrierRun(ctx context.Context, sessionID string, activeOnly bool) (string, error) {
	view, err := NewSessionManager(s).Store().View(sessionID)
	if err != nil {
		return "", err
	}
	executions := append([]ExecutionRecord(nil), view.Executions...)
	sort.SliceStable(executions, func(i, j int) bool { return executions[i].StartedAt.After(executions[j].StartedAt) })
	var statusErrors []error
	for _, execution := range executions {
		if execution.Kind != ExecutionTmux && execution.Kind != ExecutionTerminal {
			continue
		}
		for index := len(execution.RunIDs) - 1; index >= 0; index-- {
			runID := execution.RunIDs[index]
			summary, statusErr := s.SessionStatus(ctx, runID)
			if statusErr != nil {
				statusErrors = append(statusErrors, statusErr)
				continue
			}
			if !activeOnly || (summary.Alive && !terminalStateValue(summary.State)) {
				return runID, nil
			}
		}
	}
	if activeOnly {
		return "", errors.Join(append([]error{fmt.Errorf("session has no active carrier execution: %s", sessionID)}, statusErrors...)...)
	}
	return "", errors.Join(append([]error{fmt.Errorf("session has no carrier execution: %s", sessionID)}, statusErrors...)...)
}

// ReconcileSession synchronizes every carrier execution referenced by a
// logical Session. Managed/API executions are reconciled by their own Run
// lifecycle and are skipped here.
func (s *Service) ReconcileSession(ctx context.Context, sessionID string) error {
	view, err := NewSessionManager(s).Store().View(sessionID)
	if err != nil {
		return err
	}
	var reconcileErrors []error
	for _, execution := range view.Executions {
		if execution.Kind != ExecutionTmux && execution.Kind != ExecutionTerminal {
			continue
		}
		for _, runID := range execution.RunIDs {
			if _, statusErr := s.SessionStatus(ctx, runID); statusErr != nil {
				reconcileErrors = append(reconcileErrors, statusErr)
			}
		}
	}
	return errors.Join(reconcileErrors...)
}

func (s *Service) SessionLogs(ctx context.Context, runID string, tail int) (Logs, error) {
	summary, err := s.SessionStatus(ctx, runID)
	if err != nil {
		return Logs{}, err
	}
	paths, _ := RunPaths(s.RunsDir, RunSession, runID)
	content, readErr := os.ReadFile(paths.OutputLog)
	if readErr != nil && !os.IsNotExist(readErr) {
		return Logs{}, readErr
	}
	if len(content) == 0 && summary.Alive {
		request, _ := s.store.ReadRequest(paths)
		carrier, carrierErr := s.carrierForStoredExecution(request, summary.Status)
		if carrierErr == nil {
			if live, captureErr := carrier.Capture(ctx, s, request, summary.Status, tail); captureErr == nil {
				content = []byte(live)
			}
		}
	}
	events, eventErr := s.store.ReadEvents(paths)
	if eventErr != nil {
		return Logs{}, eventErr
	}
	return Logs{RunID: runID, Content: tailLines(string(content), tail), Events: events}, nil
}

func (s *Service) SessionInterrupt(ctx context.Context, runID string) (SessionSummary, error) {
	summary, err := s.SessionStatus(ctx, runID)
	if err != nil {
		return SessionSummary{}, err
	}
	if summary.Alive {
		paths, _ := RunPaths(s.RunsDir, RunSession, runID)
		request, _ := s.store.ReadRequest(paths)
		carrier, carrierErr := s.carrierForStoredExecution(request, summary.Status)
		if carrierErr != nil {
			return summary, carrierErr
		}
		err = carrier.Interrupt(ctx, s, request, summary.Status)
	}
	return summary, err
}

func (s *Service) SessionStop(ctx context.Context, runID string) (SessionSummary, error) {
	summary, err := s.SessionStatus(ctx, runID)
	if err != nil {
		return SessionSummary{}, err
	}
	paths, _ := RunPaths(s.RunsDir, RunSession, runID)
	request, readErr := s.store.ReadRequest(paths)
	if readErr != nil {
		return summary, readErr
	}
	carrier, carrierErr := s.carrierForStoredExecution(request, summary.Status)
	if carrierErr != nil {
		return summary, carrierErr
	}
	if summary.Alive {
		if err := carrier.Stop(ctx, s, request, summary.Status); err != nil {
			return summary, err
		}
	}
	if !terminalStateValue(summary.State) {
		summary.Status["alive"] = false
		releaseErr := carrier.Release(ctx, s, request, summary.Status)
		status, writeErr := s.store.WriteStatus(paths, request, StateCancelled, "interrupted", "session carrier stopped", summary.Status)
		if writeErr != nil {
			return summary, errors.Join(releaseErr, writeErr)
		}
		s.updateRegistry(paths, status)
		summary.State = status.State
		if eventErr := NewSessionManager(s).Store().AppendEvent(request.SessionID, SessionEvent{ExecutionID: request.ExecutionID,
			RunID: request.RunID, Type: "execution.cancelled", Data: map[string]any{"carrier": carrier.Name(), "carrier_id": summary.Session}}); eventErr != nil {
			return summary, errors.Join(releaseErr, eventErr)
		}
		if finalizeErr := s.finalizeCarrierRun(paths, request, carrier, summary.Status, StateCancelled, "interrupted"); finalizeErr != nil {
			return summary, errors.Join(releaseErr, finalizeErr)
		}
		if releaseErr != nil {
			return summary, releaseErr
		}
	}
	summary.Alive = false
	return summary, nil
}

func (s *Service) finalizeCarrierRun(paths Paths, request Request, carrier sessionCarrier, status map[string]any, state, reason string) error {
	outcome := OutcomeSucceeded
	summary := carrier.Name() + " carrier 已退出；transcript 仅作为日志 artifact，不代表结构化 assistant final。"
	errorsValue := []map[string]any{}
	if state != StateDone {
		outcome = OutcomeFailed
		if state == StateCancelled {
			outcome = OutcomeCancelled
		}
		errorsValue = append(errorsValue, map[string]any{"reason": reason})
	}
	result := Result{SchemaVersion: 1, RunID: request.RunID, Outcome: outcome, Summary: summary,
		ResultKind: "execution_summary", CaptureQuality: CaptureTranscriptOnly,
		Artifacts: []map[string]any{{"type": "transcript_log", "path": paths.OutputLog}}, Errors: errorsValue,
		Validation: Validation{Commands: []string{}, Passed: state == StateDone}}
	if err := s.store.WriteResult(paths, result); err != nil {
		return err
	}
	digest, _ := digestFile(paths.ResultFile)
	ref := &ResultRef{RunID: request.RunID, RunType: RunSession, ResultFile: paths.ResultFile, ResultDigest: digest}
	if request.SessionID == "" || request.ExecutionID == "" {
		return s.store.Event(paths, request, "result.synthesized", map[string]any{"result_kind": "execution_summary", "capture_quality": CaptureTranscriptOnly})
	}
	manager := NewSessionManager(s)
	execution := ExecutionRecord{ExecutionID: request.ExecutionID, Kind: carrier.ExecutionKind(), Carrier: carrier.Name(),
		CarrierID: carrierID(status), Profile: request.ProviderProfile, Provider: request.Provider, State: state,
		CaptureQuality: request.CaptureQuality, CWD: request.CWD, ProcessID: intStatus(status, "pid"),
		RawArgCount: len(request.RawCLIArgs), TranscriptRef: paths.OutputLog, RunIDs: []string{request.RunID}, ResultRef: ref}
	if err := manager.Store().UpsertExecution(request.SessionID, execution); err != nil {
		return err
	}
	view, viewErr := manager.Store().View(request.SessionID)
	if viewErr != nil {
		return viewErr
	}
	sessionState := SessionStateIdle
	for _, current := range view.Executions {
		if current.ExecutionID != request.ExecutionID && !terminalStateValue(current.State) {
			sessionState = SessionStateActive
			break
		}
	}
	if err := manager.Store().UpdateSessionState(request.SessionID, sessionState, summary); err != nil {
		return err
	}
	return s.store.Event(paths, request, "result.synthesized", map[string]any{"result_kind": "execution_summary", "capture_quality": CaptureTranscriptOnly})
}

func (s *Service) SessionAttach(ctx context.Context, runID string) error {
	summary, err := s.SessionStatus(ctx, runID)
	if err != nil {
		return err
	}
	paths, _ := RunPaths(s.RunsDir, RunSession, runID)
	request, err := s.store.ReadRequest(paths)
	if err != nil {
		return err
	}
	carrier, err := s.carrierForStoredExecution(request, summary.Status)
	if err != nil {
		return err
	}
	return carrier.Attach(ctx, s, request, summary.Status)
}

func carrierSummary(paths Paths, request Request, status Status, handle carrierHandle, alive bool) SessionSummary {
	carrier, _ := handle.Status["carrier"].(string)
	return SessionSummary{
		RunID: request.RunID, SessionID: request.SessionID, ExecutionID: request.ExecutionID,
		ProjectID: status.ProjectID, State: status.State, RunDir: paths.RunDir,
		Carrier: carrier, Session: handle.ID, Attach: handle.Attach, Alive: alive, Status: handle.Status,
	}
}
