package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"strings"
	"time"
)

type Engine struct {
	root     string
	registry *Registry
}

func NewEngine(root string) *Engine {
	return &Engine{
		root:     root,
		registry: DefaultRegistry(),
	}
}

func (e *Engine) Run(ctx context.Context, opts RunOptions) (*RunResult, error) {
	startedAt := time.Now().UTC()
	runID := newRunID(startedAt)
	events := make([]Event, 0, 8)
	emit := func(state, message string) {
		events = append(events, Event{
			At:      time.Now().UTC(),
			State:   state,
			Message: message,
		})
	}

	profileName := strings.TrimSpace(opts.Profile)
	req := RunRequest{
		RunID:     runID,
		Profile:   profileName,
		Prompt:    opts.Prompt,
		CWD:       opts.CWD,
		SessionID: opts.SessionID,
		StartedAt: startedAt,
	}
	emit(StateCreated, "")

	emit(StateLoadingConfig, profileName)
	profile, err := LoadProfile(e.root, profileName)
	artifactsRoot := DefaultArtifactsRoot
	if err == nil {
		artifactsRoot = profile.Artifacts.Root
	}
	paths, pathErr := createRunPaths(e.root, artifactsRoot, runID, startedAt)
	if pathErr != nil {
		return nil, pathErr
	}
	if err != nil {
		return e.fail(paths, req, nil, events, startedAt, profileName, "", fmt.Errorf("load profile: %w", err))
	}

	emit(StatePreparingContext, "")
	if strings.TrimSpace(opts.Prompt) == "" {
		return e.fail(paths, req, &profile, events, startedAt, profile.Name, profile.Provider.Type, fmt.Errorf("prompt is required"))
	}

	emit(StateResolvingProvider, profile.Provider.Type)
	provider, err := e.registry.Resolve(profile.Provider.Type)
	if err != nil {
		return e.fail(paths, req, &profile, events, startedAt, profile.Name, profile.Provider.Type, err)
	}

	runCtx := ctx
	cancel := func() {}
	if profile.Runtime.TimeoutSeconds > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(profile.Runtime.TimeoutSeconds)*time.Second)
	}
	defer cancel()

	emit(StateRunning, "")
	providerResult, runErr := provider.Run(runCtx, profile, req)
	if runErr != nil {
		status := StatusFailed
		if runCtx.Err() == context.DeadlineExceeded {
			status = StatusTimeout
		} else if runCtx.Err() == context.Canceled {
			status = StatusCanceled
		}
		return e.finish(paths, req, &profile, events, providerResult, startedAt, status, runErr)
	}

	status := StatusSucceeded
	if providerResult.ExitCode != 0 {
		status = StatusFailed
	}
	return e.finish(paths, req, &profile, events, providerResult, startedAt, status, nil)
}

func (e *Engine) fail(paths RunPaths, req RunRequest, profile *Profile, events []Event, startedAt time.Time, profileName, providerType string, err error) (*RunResult, error) {
	providerResult := ProviderResult{
		Stderr:   err.Error() + "\n",
		ExitCode: 1,
	}
	result, writeErr := e.finishWithNames(paths, req, profile, events, providerResult, startedAt, StatusFailed, profileName, providerType, err)
	if writeErr != nil {
		return result, writeErr
	}
	return result, err
}

func (e *Engine) finish(paths RunPaths, req RunRequest, profile *Profile, events []Event, providerResult ProviderResult, startedAt time.Time, status string, runErr error) (*RunResult, error) {
	profileName := req.Profile
	providerType := ""
	if profile != nil {
		profileName = profile.Name
		providerType = profile.Provider.Type
	}
	return e.finishWithNames(paths, req, profile, events, providerResult, startedAt, status, profileName, providerType, runErr)
}

func (e *Engine) finishWithNames(paths RunPaths, req RunRequest, profile *Profile, events []Event, providerResult ProviderResult, startedAt time.Time, status, profileName, providerType string, runErr error) (*RunResult, error) {
	emit := func(state, message string) {
		events = append(events, Event{
			At:      time.Now().UTC(),
			State:   state,
			Message: message,
		})
	}
	emit(StateExtractingOutput, "")
	emit(StateWritingArtifacts, "")

	completedAt := time.Now().UTC()
	result := &RunResult{
		RunID:     req.RunID,
		Profile:   profileName,
		Provider:  providerType,
		Status:    status,
		FinalText: providerResult.FinalText,
		ExitCode:  providerResult.ExitCode,
		Artifacts: map[string]string{
			"run_dir":         paths.RunDir,
			"request":         paths.RequestPath,
			"resolved_config": paths.ConfigPath,
			"events":          paths.EventsPath,
			"stdout":          paths.StdoutPath,
			"stderr":          paths.StderrPath,
			"output":          paths.OutputPath,
			"result":          paths.ResultPath,
			"artifacts_dir":   paths.ArtifactsDir,
		},
		StartedAt:   startedAt,
		CompletedAt: completedAt,
		DurationMS:  completedAt.Sub(startedAt).Milliseconds(),
	}
	if runErr != nil {
		result.Error = runErr.Error()
	}

	if err := writeRunArtifacts(paths, req, profile, events, providerResult.Stdout, providerResult.Stderr, result); err != nil {
		return result, err
	}
	if status == StatusSucceeded {
		return result, nil
	}
	if runErr != nil {
		return result, runErr
	}
	return result, fmt.Errorf("runtime status=%s exit_code=%d", status, providerResult.ExitCode)
}

func newRunID(now time.Time) string {
	var b [4]byte
	if _, err := rand.Read(b[:]); err != nil {
		return fmt.Sprintf("run_%d", now.UnixNano())
	}
	return "run_" + now.Format("20060102T150405Z") + "_" + hex.EncodeToString(b[:])
}
