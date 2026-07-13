package runtime

import (
	"context"
	"crypto/rand"
	"encoding/hex"
	"fmt"
	"os"
	"path/filepath"
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
	cwd := strings.TrimSpace(opts.CWD)
	if cwd == "" {
		cwd = e.root
	}
	if absoluteCWD, err := filepath.Abs(cwd); err == nil {
		cwd = absoluteCWD
	}
	req := RunRequest{
		RunID:     runID,
		Profile:   profileName,
		CWD:       cwd,
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
	prompt, promptFile, images, err := resolveRunInput(e.root, cwd, profile.Input, opts)
	if err != nil {
		return e.fail(paths, req, &profile, events, startedAt, profile.Name, profile.Provider.Type, err)
	}
	req.Prompt = prompt
	req.PromptFile = promptFile
	req.Images = images

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

func resolveRunInput(root, cwd string, defaults InputConfig, opts RunOptions) (string, string, []string, error) {
	if strings.TrimSpace(opts.Prompt) != "" && strings.TrimSpace(opts.PromptFile) != "" {
		return "", "", nil, fmt.Errorf("prompt and prompt file cannot both be set")
	}

	prompt := opts.Prompt
	promptFile := ""
	switch {
	case strings.TrimSpace(opts.PromptFile) != "":
		path, err := resolveInputFile(cwd, opts.PromptFile, "prompt file")
		if err != nil {
			return "", "", nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", nil, fmt.Errorf("read prompt file %s: %w", path, err)
		}
		prompt = string(data)
		promptFile = path
	case strings.TrimSpace(prompt) != "":
	case strings.TrimSpace(defaults.PromptFile) != "":
		path, err := resolveInputFile(root, defaults.PromptFile, "profile input.prompt_file")
		if err != nil {
			return "", "", nil, err
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return "", "", nil, fmt.Errorf("read prompt file %s: %w", path, err)
		}
		prompt = string(data)
		promptFile = path
	default:
		prompt = defaults.Prompt
	}
	if strings.TrimSpace(prompt) == "" {
		return "", "", nil, fmt.Errorf("prompt is required")
	}

	imageBase := root
	images := defaults.Images
	if opts.ImagesSet {
		imageBase = cwd
		images = opts.Images
	}
	resolvedImages := make([]string, 0, len(images))
	for _, image := range images {
		path, err := resolveInputFile(imageBase, image, "image")
		if err != nil {
			return "", "", nil, err
		}
		resolvedImages = append(resolvedImages, path)
	}
	return prompt, promptFile, resolvedImages, nil
}

func resolveInputFile(base, path, label string) (string, error) {
	value := strings.TrimSpace(path)
	if value == "" {
		return "", fmt.Errorf("%s path is required", label)
	}
	if !filepath.IsAbs(value) {
		value = filepath.Join(base, value)
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", fmt.Errorf("resolve %s %q: %w", label, path, err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", fmt.Errorf("stat %s %s: %w", label, absolute, err)
	}
	if info.IsDir() {
		return "", fmt.Errorf("%s is a directory: %s", label, absolute)
	}
	return absolute, nil
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
