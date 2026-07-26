package agentrun

import (
	"context"
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"github.com/yy003x/runtime/internal/provider"
)

func (s *Service) ResumeNative(ctx context.Context, runType, runID string, patch *provider.NativePatch) (RunSummary, error) {
	if err := validateRunID(runID); err != nil {
		return RunSummary{}, err
	}
	runLock, err := s.acquireRunLock(ctx, runID)
	if err != nil {
		return RunSummary{}, err
	}
	defer runLock.release()
	paths, request, status, profile, profiles, err := s.nativeRun(runType, runID)
	if err != nil {
		return RunSummary{}, err
	}
	if status.State != StateBlocked {
		return RunSummary{}, fmt.Errorf("agent run %s must be blocked before resume, got %s", runID, status.State)
	}
	concurrencySlot, err := s.acquireConcurrencySlot()
	if err != nil {
		return RunSummary{}, err
	}
	defer concurrencySlot.release()
	selected, err := provider.Select(profile)
	if err != nil {
		return RunSummary{}, err
	}
	providerStatus := cloneStatusMap(status.ProviderStatus)
	snapshotFile := agentSnapshotFile(paths, profile)
	kind := provider.TypeNative
	if profile.Type == provider.TypeAPI {
		kind = "api-agent"
	}
	providerStatus["kind"] = kind
	runningStatus, err := s.store.WriteStatus(paths, request, StateRunning, "", "agent resuming", providerStatus)
	if err != nil {
		return s.fail(paths, request, providerStatus, "status_sync_failed", err)
	}
	s.updateRegistry(paths, runningStatus)
	if err := s.store.Event(paths, request, "status.changed", map[string]any{"state": StateRunning, "transport": profile.Type}); err != nil {
		return s.fail(paths, request, providerStatus, "event_sync_failed", err)
	}
	if err := NewSessionManager(s).ResumeRun(request); err != nil {
		return s.fail(paths, request, providerStatus, "history_sync_failed", fmt.Errorf("resume session history: %w", err))
	}
	manager := NewSessionManager(s)
	memoryFile, candidateFile := manager.MemoryPaths(request.SessionID)
	prepared, err := selected.Prepare(ctx, profile, provider.Request{
		Overrides: request.ProviderOverrides, CWD: request.CWD, HTTPClient: s.HTTPClient, Profiles: profiles,
		RunID: runID, SnapshotFile: snapshotFile,
		PersonaDir: s.PersonaDir, SkillDir: s.paths.SkillsDir, ToolDir: s.paths.ToolsDir, MemoryFile: memoryFile,
		MemoryCandidateFile: candidateFile, SessionID: request.SessionID, TurnID: request.TurnID,
		Allowed: append([]string(nil), request.AllowedActions...), Forbidden: append([]string(nil), request.ForbiddenActions...),
		NativeResume: true, NativePatch: patch,
	})
	if err != nil {
		return s.fail(paths, request, providerStatus, "provider_error", err)
	}
	if err := appendOutputLogHeader(paths.OutputLog, prepared, "resuming"); err != nil {
		return s.fail(paths, request, providerStatus, "output_sync_failed", err)
	}
	sink := &runProviderSink{service: s, paths: paths, request: request, status: providerStatus}
	result, err := selected.Execute(ctx, prepared, sink)
	if writeErr := finalizeOutputLog(paths.OutputLog, sink, result); writeErr != nil {
		return s.fail(paths, request, providerStatus, "output_sync_failed", writeErr)
	}
	if ctx.Err() != nil {
		return s.cancelled(paths, request, providerStatus, ctx.Err())
	}
	if err != nil {
		return s.fail(paths, request, providerStatus, "provider_error", err)
	}
	for key, value := range result.Detail {
		providerStatus[key] = value
	}
	if result.State == "blocked" {
		return s.blocked(paths, request, providerStatus, result.BlockedReason)
	}
	if result.State == "cancelled" {
		return s.cancelled(paths, request, providerStatus, context.Canceled)
	}
	providerStatus["returncode"] = result.ExitCode
	if err := s.store.Event(paths, request, "provider.exited", map[string]any{"returncode": result.ExitCode, "execution_mode": request.ExecutionMode}); err != nil {
		return s.fail(paths, request, providerStatus, "event_sync_failed", err)
	}
	resultPendingStatus, err := s.store.WriteStatus(paths, request, StateResultPending, "", "validating result", providerStatus)
	if err != nil {
		return s.fail(paths, request, providerStatus, "status_sync_failed", err)
	}
	s.updateRegistry(paths, resultPendingStatus)
	if err := s.store.Event(paths, request, "status.changed", map[string]any{"state": StateResultPending}); err != nil {
		return s.fail(paths, request, providerStatus, "event_sync_failed", err)
	}
	text := strings.TrimSpace(result.FinalText)
	if text == "" {
		text = captureSummary(result.Stdout)
	}
	snapshotType := "native_snapshot"
	if profile.Type == provider.TypeAPI {
		snapshotType = "context_snapshot"
	}
	contract := Result{
		SchemaVersion: 1, RunID: runID, Outcome: OutcomeSucceeded,
		AssistantMessage: text, Summary: text,
		Artifacts: []map[string]any{{"type": "log", "path": paths.OutputLog}, {"type": snapshotType, "path": snapshotFile}},
		Errors:    []map[string]any{}, Validation: Validation{Commands: []string{}, Passed: true}}
	if err := s.store.WriteResult(paths, contract); err != nil {
		return s.fail(paths, request, providerStatus, "result_sync_failed", err)
	}
	if err := s.store.Event(paths, request, "result.synthesized", map[string]any{"execution_mode": request.ExecutionMode, "summary_chars": len(text)}); err != nil {
		return s.fail(paths, request, providerStatus, "event_sync_failed", err)
	}
	return s.markDone(paths, request, providerStatus)
}

func (s *Service) ControlNative(runType, runID, action, reason string) (RunSummary, error) {
	paths, request, status, profile, _, err := s.nativeRun(runType, runID)
	if err != nil {
		return RunSummary{}, err
	}
	if terminalStateValue(status.State) {
		return summary(paths, status, true), nil
	}
	detail, err := provider.ControlNative(agentSnapshotFile(paths, profile), action, reason)
	if err != nil {
		return RunSummary{}, err
	}
	providerStatus := cloneStatusMap(status.ProviderStatus)
	for key, value := range detail {
		providerStatus[key] = value
	}
	state := StateBlocked
	failureReason := strings.TrimSpace(reason)
	if action == "stop" || action == "cancel" {
		state = StateCancelled
		if failureReason == "" {
			failureReason = "interrupted"
		}
	}
	status, err = s.store.WriteStatus(paths, request, state, failureReason, action, providerStatus)
	s.updateRegistry(paths, status)
	eventErr := s.store.Event(paths, request, "provider."+action, map[string]any{"transport": profile.Type, "reason": reason})
	historyErr := NewSessionManager(s).CompleteRun(request, state, failureReason, failureReason, nil)
	return summary(paths, status, false), errors.Join(err, eventErr, historyErr)
}

func (s *Service) nativeRun(runType, runID string) (Paths, Request, Status, provider.Config, map[string]provider.Config, error) {
	paths, err := RunPaths(s.RunsDir, runType, runID)
	if err != nil {
		return Paths{}, Request{}, Status{}, provider.Config{}, nil, err
	}
	request, err := s.store.ReadRequest(paths)
	if err != nil {
		return Paths{}, Request{}, Status{}, provider.Config{}, nil, fmt.Errorf("run 不存在: %s", runID)
	}
	status, err := s.store.ReadStatus(paths)
	if err != nil {
		return Paths{}, Request{}, Status{}, provider.Config{}, nil, err
	}
	profiles, err := s.Profiles()
	if err != nil {
		return Paths{}, Request{}, Status{}, provider.Config{}, nil, err
	}
	profile, ok := provider.Resolve(profiles, request.ProviderProfile)
	if !ok {
		return Paths{}, Request{}, Status{}, provider.Config{}, nil, fmt.Errorf("unknown provider profile: %s", request.ProviderProfile)
	}
	apiAgent := profile.Type == provider.TypeAPI && profile.API != nil && profile.API.Runtime != nil && profile.API.Runtime.Enabled
	if request.Provider != provider.TypeNative && !apiAgent {
		return Paths{}, Request{}, Status{}, provider.Config{}, nil, fmt.Errorf("run %s is not an agent provider run", runID)
	}
	return paths, request, status, profile, profiles, nil
}

func agentSnapshotFile(paths Paths, profile provider.Config) string {
	if profile.Type == provider.TypeAPI {
		return filepath.Join(paths.RunDir, "context-snapshot.json")
	}
	return filepath.Join(paths.RunDir, "native-snapshot.json")
}

func cloneStatusMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
