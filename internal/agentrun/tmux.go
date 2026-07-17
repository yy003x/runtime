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
	providertmux "agent-runtime/internal/provider/tmux"
)

type SessionSummary struct {
	RunID      string         `json:"run_id"`
	ProjectID  string         `json:"project_id"`
	State      string         `json:"state"`
	RunDir     string         `json:"run_dir"`
	Session    string         `json:"tmux_session"`
	Attach     string         `json:"attach"`
	Alive      bool           `json:"alive"`
	Idempotent bool           `json:"idempotent,omitempty"`
	Status     map[string]any `json:"provider_status,omitempty"`
}

type SessionOptions struct {
	Profile          string
	ProjectID        string
	CWD              string
	RunID            string
	Prompt           string
	PromptFile       string
	RawCLIArgs       []string
	AllowedActions   []string
	ForbiddenActions []string
	Force            bool
	RecordMode       string
	Retention        string
}

func (s *Service) StartSession(ctx context.Context, profileName, projectID, cwd, runID string, force bool) (SessionSummary, error) {
	return s.StartSessionWithOptions(ctx, SessionOptions{Profile: profileName, ProjectID: projectID, CWD: cwd, RunID: runID, Force: force})
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
	profile, err = provider.AsTmuxSessionProfile(profile)
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
	if options.RunID == "" {
		options.RunID = newRunID(RunSession)
	}
	decision, err := DecideRecordPolicy("cli.session", RunSession, ExecutionTmux, options.RunID, options.RecordMode, options.Retention)
	if err != nil {
		return SessionSummary{}, err
	}
	if decision.RecordMode == RecordOff {
		return SessionSummary{}, fmt.Errorf("session start does not allow record_mode=off")
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
	request := Request{SchemaVersion: 1, ContractVersion: ContractVersion, RuntimeVersion: s.RuntimeVersion,
		ProjectID: options.ProjectID, RunType: RunSession, RunID: options.RunID, ProviderProfile: profile.ID,
		Provider: provider.ExecutorTmux, CWD: resolvedCWD, PromptFile: promptFile, RawCLIArgs: append([]string(nil), options.RawCLIArgs...), ResultFile: paths.ResultFile,
		ExecutionMode: ModeManaged, SessionID: options.RunID, ExecutionID: executionIDForRun(options.RunID), ExecutionKind: ExecutionTmux,
		RecordMode: decision.RecordMode, Retention: decision.Retention, CaptureQuality: decision.CaptureQuality,
		ProviderOverrides: map[string]any{}, AllowedActions: append([]string(nil), options.AllowedActions...), ForbiddenActions: append([]string(nil), options.ForbiddenActions...), CreatedAt: now, UpdatedAt: now}
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
			return SessionSummary{}, fmt.Errorf("cannot --force an existing tmux Session; use a new --run-id to preserve execution history")
		}
	}
	if err := paths.Ensure(); err != nil {
		return SessionSummary{}, err
	}
	if options.Force {
		s.resetRun(paths)
	}
	manager := NewSessionManager(s)
	if _, err := manager.EnsureSession(request.SessionID, request.ProjectID, request.CWD, prompt, decision); err != nil {
		return SessionSummary{}, err
	}
	if _, err := manager.Store().ConfigureSession(request.SessionID, "tmux", profile.ID); err != nil {
		return SessionSummary{}, err
	}
	if err := manager.Store().UpsertExecution(request.SessionID, ExecutionRecord{ExecutionID: request.ExecutionID,
		Kind: ExecutionTmux, Profile: profile.ID, Provider: provider.ExecutorTmux, State: StatePending,
		CaptureQuality: request.CaptureQuality, RunIDs: []string{request.RunID}}); err != nil {
		return SessionSummary{}, err
	}
	if err := s.store.WriteRequest(paths, request); err != nil {
		return SessionSummary{}, err
	}
	pendingStatus, err := s.store.WriteStatus(paths, request, StatePending, "", "queued", nil)
	if err != nil {
		return SessionSummary{}, err
	}
	s.register(paths, request, pendingStatus.State)
	failBeforeStart := func(cause error, reason string) error {
		failedStatus, statusErr := s.store.WriteStatus(paths, request, StateFailed, reason, cause.Error(), nil)
		if statusErr == nil {
			s.updateRegistry(paths, failedStatus)
		}
		eventErr := s.store.Event(paths, request, "status.changed", map[string]any{"state": StateFailed, "failure_reason": reason})
		finalizeErr := s.finalizeTmuxRun(paths, request, StateFailed, reason, "")
		return errors.Join(cause, statusErr, eventErr, finalizeErr)
	}
	backend, err := provider.NewTmuxBackend(profile, s.DaemonClient())
	if err != nil {
		return SessionSummary{}, failBeforeStart(err, "provider_error")
	}
	command, err := provider.TmuxShellCommandWithRawArgs(profile, options.RawCLIArgs, nil)
	if err != nil {
		return SessionSummary{}, failBeforeStart(err, "provider_error")
	}
	tmuxConfig := profile.CLI.Tmux
	session, err := backend.StartShellWithOptions(ctx, options.RunID, resolvedCWD, command, providertmux.StartOptions{
		LogFile: paths.OutputLog, ExitFile: sessionExitFile(paths),
		WaitForOutput:      true,
		RestartMaxAttempts: tmuxConfig.RestartMaxAttempts, RestartDelaySeconds: tmuxConfig.RestartDelaySeconds,
	})
	if err != nil {
		return SessionSummary{}, failBeforeStart(err, "provider_error")
	}
	providerStatus := map[string]any{"tmux_session": session, "alive": true}
	failStartedSession := func(cause error, reason string) error {
		killErr := s.DaemonClient().KillTmux(context.Background(), options.RunID, session)
		failedStatus, statusErr := s.store.WriteStatus(paths, request, StateFailed, reason, cause.Error(), providerStatus)
		if statusErr == nil {
			s.updateRegistry(paths, failedStatus)
		}
		eventErr := s.store.Event(paths, request, "status.changed", map[string]any{"state": StateFailed, "failure_reason": reason})
		finalizeErr := s.finalizeTmuxRun(paths, request, StateFailed, reason, session)
		return errors.Join(cause, killErr, statusErr, eventErr, finalizeErr)
	}
	if err := manager.Store().UpsertExecution(request.SessionID, ExecutionRecord{ExecutionID: request.ExecutionID,
		Kind: ExecutionTmux, Profile: profile.ID, Provider: provider.ExecutorTmux, State: StateRunning,
		CaptureQuality: request.CaptureQuality, TmuxSession: session, RunIDs: []string{request.RunID}}); err != nil {
		return SessionSummary{}, failStartedSession(err, "history_sync_failed")
	}
	if err := manager.Store().AppendEvent(request.SessionID, SessionEvent{ExecutionID: request.ExecutionID, RunID: request.RunID,
		Type: "execution.started", Data: map[string]any{"kind": ExecutionTmux, "tmux_session": session}}); err != nil {
		return SessionSummary{}, failStartedSession(err, "history_sync_failed")
	}
	if err := manager.Store().UpdateSessionState(request.SessionID, SessionStateActive, ""); err != nil {
		return SessionSummary{}, failStartedSession(err, "history_sync_failed")
	}
	status, err := s.store.WriteStatus(paths, request, StateRunning, "", "session running", providerStatus)
	if err != nil {
		return SessionSummary{}, failStartedSession(err, "status_sync_failed")
	}
	if err := s.store.Event(paths, request, "status.changed", map[string]any{"state": StateRunning, "transport": provider.ExecutorTmux}); err != nil {
		return SessionSummary{}, failStartedSession(err, "event_sync_failed")
	}
	if strings.TrimSpace(prompt) != "" {
		summary, sendErr := s.SessionSend(ctx, options.RunID, prompt, true)
		if sendErr != nil {
			return summary, failStartedSession(sendErr, "prompt_submit_failed")
		}
		return summary, nil
	}
	return SessionSummary{RunID: options.RunID, ProjectID: options.ProjectID, State: status.State, RunDir: paths.RunDir, Session: session, Attach: "tmux attach-session -t " + session, Alive: true, Status: providerStatus}, nil
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
	status, err := s.store.ReadStatus(paths)
	if err != nil {
		return SessionSummary{}, fmt.Errorf("session 不存在: %s", runID)
	}
	providerStatus := ensureProviderStatus(&status)
	session, _ := providerStatus["tmux_session"].(string)
	alive, daemonErr := s.DaemonClient().HasTmux(ctx, runID, session)
	if daemonErr != nil {
		return SessionSummary{}, daemonErr
	}
	providerStatus["alive"] = alive
	if !alive && status.State == StateRunning {
		if code, attempts, ok := readSessionExit(sessionExitFile(paths)); ok {
			state, reason := StateDone, ""
			if code != 0 {
				state, reason = StateFailed, "provider_exit"
			}
			request, readErr := s.store.ReadRequest(paths)
			if readErr != nil {
				return SessionSummary{}, readErr
			}
			providerStatus["returncode"] = code
			providerStatus["attempts"] = attempts
			status, daemonErr = s.store.WriteStatus(paths, request, state, reason, "session process exited", providerStatus)
			s.updateRegistry(paths, status)
			eventErr := s.store.Event(paths, request, "provider.exited", map[string]any{"returncode": code, "attempts": attempts})
			finalizeErr := s.finalizeTmuxRun(paths, request, state, reason, session)
			daemonErr = errors.Join(daemonErr, eventErr, finalizeErr)
		} else {
			request, readErr := s.store.ReadRequest(paths)
			if readErr != nil {
				return SessionSummary{}, readErr
			}
			status, daemonErr = s.store.WriteStatus(paths, request, StateFailed, "orphaned", "tmux session disappeared without exit marker", providerStatus)
			s.updateRegistry(paths, status)
			eventErr := s.store.Event(paths, request, "provider.disappeared", map[string]any{"tmux_session": session})
			finalizeErr := s.finalizeTmuxRun(paths, request, StateFailed, "orphaned", session)
			daemonErr = errors.Join(daemonErr, eventErr, finalizeErr)
		}
	}
	return SessionSummary{RunID: runID, ProjectID: status.ProjectID, State: status.State, RunDir: paths.RunDir, Session: session, Attach: "tmux attach-session -t " + session, Alive: alive, Status: status.ProviderStatus}, daemonErr
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
		return summary, fmt.Errorf("tmux session is not alive: %s", summary.Session)
	}
	bracketed := false
	var backend *providertmux.Backend
	paths, _ := RunPaths(s.RunsDir, RunSession, runID)
	request, readRequestErr := s.store.ReadRequest(paths)
	if readRequestErr != nil {
		return summary, readRequestErr
	}
	var profile provider.Config
	if profiles, loadErr := s.Profiles(); loadErr == nil {
		if resolved, ok := provider.Resolve(profiles, request.ProviderProfile); ok {
			profile, err = provider.AsTmuxSessionProfile(resolved)
			if err == nil {
				bracketed = profile.CLI.Tmux.PasteBracketed
				backend, err = provider.NewTmuxBackend(profile, s.DaemonClient())
			}
		}
	}
	if err != nil {
		return summary, err
	}
	if backend == nil {
		return summary, fmt.Errorf("tmux profile is unavailable for session: %s", runID)
	}
	manager := NewSessionManager(s)
	turnID := ""
	if submit && request.RecordMode != RecordOff {
		turnID = newRunID(RunTurn)
		manifest, compileErr := manager.CompileContext(request.SessionID, turnID, request.CWD, profile, text, request.AllowedActions, request.ForbiddenActions)
		if compileErr != nil {
			return summary, compileErr
		}
		if _, addErr := manager.Store().AddTurn(request.SessionID, TurnRecord{TurnID: turnID, Runtime: "tmux",
			Provider: request.Provider, Profile: request.ProviderProfile, Model: requestModel(request, profile),
			RecordMode: request.RecordMode, CaptureQuality: request.CaptureQuality}, text, manifest); addErr != nil {
			return summary, addErr
		}
	}
	if err := backend.Send(ctx, summary.Session, text, providertmux.SendOptions{Submit: submit, Bracketed: bracketed, Stabilize: submit}); err != nil {
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
		if status.ProviderStatus == nil {
			status.ProviderStatus = map[string]any{}
		}
		status.ProviderStatus["prompt_submitted"] = true
		status.ProviderStatus["last_prompt_submitted_at"] = time.Now().UTC()
		updated, writeErr := s.store.WriteStatus(paths, request, status.State, status.FailureReason, status.Message, status.ProviderStatus)
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
			Kind: ExecutionTmux, Profile: request.ProviderProfile, Provider: request.Provider, State: StateRunning,
			CaptureQuality: request.CaptureQuality, TmuxSession: summary.Session, RunIDs: []string{request.RunID}, TurnIDs: []string{turnID}}); err != nil {
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
			reconcileErrors = append(reconcileErrors, fmt.Errorf("read tmux session status %s: %w", entry.RunDir, err))
			continue
		}
		current, statusErr := s.SessionStatus(ctx, status.RunID)
		if statusErr != nil {
			reconcileErrors = append(reconcileErrors, fmt.Errorf("reconcile tmux session %s: %w", status.RunID, statusErr))
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

// ReconcileSession 在读取逻辑 Session 前同步同 ID 的 tmux 执行状态。
// API/CLI managed Session 没有 runs/session artifact，因此会直接跳过。
func (s *Service) ReconcileSession(ctx context.Context, sessionID string) error {
	paths, err := RunPaths(s.RunsDir, RunSession, sessionID)
	if err != nil {
		return err
	}
	if _, err := os.Stat(paths.RequestFile); os.IsNotExist(err) {
		return nil
	} else if err != nil {
		return err
	}
	request, err := s.store.ReadRequest(paths)
	if err != nil {
		return err
	}
	if request.ExecutionKind != ExecutionTmux && request.Provider != provider.ExecutorTmux {
		return nil
	}
	_, err = s.SessionStatus(ctx, sessionID)
	return err
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
		if live, captureErr := s.DaemonClient().CaptureTmux(ctx, runID, summary.Session, tail); captureErr == nil {
			content = []byte(live)
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
		err = s.DaemonClient().InterruptTmux(ctx, runID, summary.Session)
	}
	return summary, err
}

func (s *Service) SessionStop(ctx context.Context, runID string) (SessionSummary, error) {
	summary, err := s.SessionStatus(ctx, runID)
	if err != nil {
		return SessionSummary{}, err
	}
	if summary.Alive {
		if err := s.DaemonClient().KillTmux(ctx, runID, summary.Session); err != nil {
			return summary, err
		}
	}
	if !terminalStateValue(summary.State) {
		paths, _ := RunPaths(s.RunsDir, RunSession, runID)
		request, readErr := s.store.ReadRequest(paths)
		if readErr != nil {
			return summary, readErr
		}
		status, writeErr := s.store.WriteStatus(paths, request, StateCancelled, "interrupted", "session stopped", summary.Status)
		if writeErr != nil {
			return summary, writeErr
		}
		s.updateRegistry(paths, status)
		summary.State = status.State
		if eventErr := NewSessionManager(s).Store().AppendEvent(request.SessionID, SessionEvent{ExecutionID: request.ExecutionID,
			RunID: request.RunID, Type: "execution.cancelled", Data: map[string]any{"tmux_session": summary.Session}}); eventErr != nil {
			return summary, eventErr
		}
		if finalizeErr := s.finalizeTmuxRun(paths, request, StateCancelled, "interrupted", summary.Session); finalizeErr != nil {
			return summary, finalizeErr
		}
	}
	summary.Alive = false
	return summary, nil
}

func (s *Service) finalizeTmuxRun(paths Paths, request Request, state, reason, tmuxSession string) error {
	outcome := OutcomeSucceeded
	summary := "tmux 执行容器已退出；transcript 仅作为日志 artifact，不代表结构化 assistant final。"
	if tmuxSession == "" {
		summary = "tmux 执行未能启动；未产生结构化 assistant final。"
	}
	errors := []map[string]any{}
	if state != StateDone {
		outcome = OutcomeFailed
		if state == StateCancelled {
			outcome = OutcomeCancelled
		}
		errors = append(errors, map[string]any{"reason": reason})
	}
	result := Result{SchemaVersion: 1, RunID: request.RunID, Outcome: outcome, Summary: summary,
		ResultKind: "execution_summary", CaptureQuality: CaptureTranscriptOnly,
		Artifacts: []map[string]any{{"type": "transcript_log", "path": paths.OutputLog}}, Errors: errors,
		Validation: Validation{Commands: []string{}, Passed: state == StateDone}}
	if err := s.store.WriteResult(paths, result); err != nil {
		return err
	}
	digest, _ := digestFile(paths.ResultFile)
	ref := &ResultRef{RunID: request.RunID, RunType: RunSession, ResultFile: paths.ResultFile, ResultDigest: digest}
	if request.SessionID == "" || request.ExecutionID == "" {
		return s.store.Event(paths, request, "result.synthesized", map[string]any{
			"result_kind": "execution_summary", "capture_quality": CaptureTranscriptOnly,
		})
	}
	manager := NewSessionManager(s)
	if err := manager.Store().UpsertExecution(request.SessionID, ExecutionRecord{ExecutionID: request.ExecutionID,
		Kind: ExecutionTmux, Profile: request.ProviderProfile, Provider: request.Provider, State: state,
		CaptureQuality: request.CaptureQuality, TmuxSession: tmuxSession, RunIDs: []string{request.RunID}, ResultRef: ref}); err != nil {
		return err
	}
	if err := manager.Store().UpdateSessionState(request.SessionID, SessionStateIdle, summary); err != nil {
		return err
	}
	return s.store.Event(paths, request, "result.synthesized", map[string]any{
		"result_kind": "execution_summary", "capture_quality": CaptureTranscriptOnly,
	})
}

func (s *Service) SessionAttach(ctx context.Context, runID string) error {
	summary, err := s.SessionStatus(ctx, runID)
	if err != nil {
		return err
	}
	return providertmux.Attach(ctx, summary.Session)
}
