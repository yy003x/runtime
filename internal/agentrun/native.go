package agentrun

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"

	"agent-runtime/internal/provider"
)

func (s *Service) ResumeNative(ctx context.Context, runType, runID string, patch *provider.NativePatch) (RunSummary, error) {
	paths, request, status, profile, profiles, err := s.nativeRun(runType, runID)
	if err != nil {
		return RunSummary{}, err
	}
	if status.State != StateBlocked {
		return RunSummary{}, fmt.Errorf("native run %s must be blocked before resume, got %s", runID, status.State)
	}
	selected, err := provider.Select(profile)
	if err != nil {
		return RunSummary{}, err
	}
	providerStatus := cloneStatusMap(status.ProviderStatus)
	providerStatus["kind"] = provider.TypeNative
	_, _ = s.store.WriteStatus(paths, request, StateRunning, "", "native resuming", providerStatus)
	_ = s.store.Event(paths, request, "status.changed", map[string]any{"state": StateRunning, "transport": provider.TypeNative})
	prepared, err := selected.Prepare(ctx, profile, provider.Request{
		Overrides: request.ProviderOverrides, CWD: request.CWD, HTTPClient: s.HTTPClient, Profiles: profiles,
		RunID: runID, SnapshotFile: filepath.Join(paths.RunDir, "native-snapshot.json"),
		PersonaDir: rooted(s.Root, "configs/personas"), NativeResume: true, NativePatch: patch,
	})
	if err != nil {
		return s.fail(paths, request, providerStatus, "provider_error", err)
	}
	sink := &runProviderSink{service: s, paths: paths, request: request, status: providerStatus}
	result, err := selected.Execute(ctx, prepared, sink)
	if writeErr := writeOutputLog(paths.OutputLog, prepared, result); writeErr != nil {
		return RunSummary{}, writeErr
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
	_, _ = s.store.WriteStatus(paths, request, StateResultPending, "", "validating result", providerStatus)
	text := strings.TrimSpace(result.FinalText)
	if text == "" {
		text = captureSummary(result.Stdout)
	}
	contract := Result{SchemaVersion: 1, RunID: runID, Outcome: OutcomeSucceeded, Summary: text,
		Artifacts: []map[string]any{{"type": "log", "path": paths.OutputLog}, {"type": "native_snapshot", "path": filepath.Join(paths.RunDir, "native-snapshot.json")}},
		Errors:    []map[string]any{}, Validation: Validation{Commands: []string{}, Passed: true}}
	if err := s.store.WriteResult(paths, contract); err != nil {
		return RunSummary{}, err
	}
	_ = s.store.Event(paths, request, "result.synthesized", map[string]any{"execution_mode": request.ExecutionMode, "summary_chars": len(text)})
	return s.markDone(paths, request, providerStatus)
}

func (s *Service) ControlNative(runType, runID, action, reason string) (RunSummary, error) {
	paths, request, status, _, _, err := s.nativeRun(runType, runID)
	if err != nil {
		return RunSummary{}, err
	}
	if terminalStateValue(status.State) {
		return summary(paths, status, true), nil
	}
	detail, err := provider.ControlNative(filepath.Join(paths.RunDir, "native-snapshot.json"), action, reason)
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
	_ = s.store.Event(paths, request, "provider."+action, map[string]any{"transport": provider.TypeNative, "reason": reason})
	return summary(paths, status, false), err
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
	if request.Provider != provider.TypeNative {
		return Paths{}, Request{}, Status{}, provider.Config{}, nil, fmt.Errorf("run %s is not a native provider run", runID)
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
	return paths, request, status, profile, profiles, nil
}

func cloneStatusMap(input map[string]any) map[string]any {
	output := make(map[string]any, len(input))
	for key, value := range input {
		output[key] = value
	}
	return output
}
