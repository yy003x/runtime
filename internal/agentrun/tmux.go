package agentrun

import (
	"context"
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
		ExecutionMode: ModeManaged, ProviderOverrides: map[string]any{}, AllowedActions: append([]string(nil), options.AllowedActions...), ForbiddenActions: append([]string(nil), options.ForbiddenActions...), CreatedAt: now, UpdatedAt: now}
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
	if err := paths.Ensure(); err != nil {
		return SessionSummary{}, err
	}
	if options.Force {
		s.resetRun(paths)
	}
	if err := s.store.WriteRequest(paths, request); err != nil {
		return SessionSummary{}, err
	}
	_, _ = s.store.WriteStatus(paths, request, StatePending, "", "queued", nil)
	s.register(paths, request, StatePending)
	backend, err := provider.NewTmuxBackend(profile, s.DaemonClient())
	if err != nil {
		_, _ = s.store.WriteStatus(paths, request, StateFailed, "provider_error", err.Error(), nil)
		return SessionSummary{}, err
	}
	command, err := provider.TmuxShellCommandWithRawArgs(profile, options.RawCLIArgs, nil)
	if err != nil {
		_, _ = s.store.WriteStatus(paths, request, StateFailed, "provider_error", err.Error(), nil)
		return SessionSummary{}, err
	}
	tmuxConfig := profile.CLI.Tmux
	session, err := backend.StartShellWithOptions(ctx, options.RunID, resolvedCWD, command, providertmux.StartOptions{
		LogFile: paths.OutputLog, ExitFile: sessionExitFile(paths),
		WaitForOutput:      true,
		RestartMaxAttempts: tmuxConfig.RestartMaxAttempts, RestartDelaySeconds: tmuxConfig.RestartDelaySeconds,
	})
	if err != nil {
		_, _ = s.store.WriteStatus(paths, request, StateFailed, "provider_error", err.Error(), nil)
		return SessionSummary{}, err
	}
	providerStatus := map[string]any{"tmux_session": session, "alive": true}
	status, err := s.store.WriteStatus(paths, request, StateRunning, "", "session running", providerStatus)
	_ = s.store.Event(paths, request, "status.changed", map[string]any{"state": StateRunning, "transport": provider.ExecutorTmux})
	if err != nil {
		return SessionSummary{}, err
	}
	if strings.TrimSpace(prompt) != "" {
		summary, sendErr := s.SessionSend(ctx, options.RunID, prompt, true)
		if sendErr != nil {
			_ = s.DaemonClient().KillTmux(context.Background(), options.RunID, session)
			_, _ = s.store.WriteStatus(paths, request, StateFailed, "prompt_submit_failed", sendErr.Error(), providerStatus)
			return summary, sendErr
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
			request, _ := s.store.ReadRequest(paths)
			providerStatus["returncode"] = code
			providerStatus["attempts"] = attempts
			status, daemonErr = s.store.WriteStatus(paths, request, state, reason, "session process exited", providerStatus)
			s.updateRegistry(paths, status)
			_ = s.store.Event(paths, request, "provider.exited", map[string]any{"returncode": code, "attempts": attempts})
		} else {
			request, _ := s.store.ReadRequest(paths)
			status, daemonErr = s.store.WriteStatus(paths, request, StateFailed, "orphaned", "tmux session disappeared without exit marker", providerStatus)
			s.updateRegistry(paths, status)
			_ = s.store.Event(paths, request, "provider.disappeared", map[string]any{"tmux_session": session})
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
	if request, readErr := s.store.ReadRequest(paths); readErr == nil {
		if profiles, loadErr := s.Profiles(); loadErr == nil {
			if profile, ok := provider.Resolve(profiles, request.ProviderProfile); ok {
				profile, err = provider.AsTmuxSessionProfile(profile)
				if err == nil {
					bracketed = profile.CLI.Tmux.PasteBracketed
					backend, err = provider.NewTmuxBackend(profile, s.DaemonClient())
				}
			}
		}
	}
	if err != nil {
		return summary, err
	}
	if backend == nil {
		return summary, fmt.Errorf("tmux profile is unavailable for session: %s", runID)
	}
	if err := backend.Send(ctx, summary.Session, text, providertmux.SendOptions{Submit: submit, Bracketed: bracketed, Stabilize: submit}); err != nil {
		return summary, err
	}
	if submit {
		request, readErr := s.store.ReadRequest(paths)
		status, statusErr := s.store.ReadStatus(paths)
		if readErr != nil {
			return summary, readErr
		}
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
		if eventErr := s.store.Event(paths, request, "prompt.submitted", map[string]any{"submitted": true}); eventErr != nil {
			return summary, eventErr
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
	for _, entry := range entries {
		var status Status
		if err := readJSON(filepath.Join(entry.RunDir, "status.json"), &status); err != nil {
			continue
		}
		session, _ := status.ProviderStatus["tmux_session"].(string)
		alive, _ := s.DaemonClient().HasTmux(ctx, status.RunID, session)
		listed = append(listed, listedSession{summary: SessionSummary{
			RunID: status.RunID, ProjectID: status.ProjectID, State: status.State, RunDir: entry.RunDir,
			Session: session, Attach: "tmux attach-session -t " + session, Alive: alive, Status: status.ProviderStatus,
		}, updatedAt: entry.UpdatedAt})
	}
	sort.Slice(listed, func(i, j int) bool { return listed[i].updatedAt.After(listed[j].updatedAt) })
	result := make([]SessionSummary, len(listed))
	for i := range listed {
		result[i] = listed[i].summary
	}
	return result, nil
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
	events, _ := s.store.ReadEvents(paths)
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
		request, _ := s.store.ReadRequest(paths)
		status, writeErr := s.store.WriteStatus(paths, request, StateCancelled, "interrupted", "session stopped", summary.Status)
		if writeErr != nil {
			return summary, writeErr
		}
		s.updateRegistry(paths, status)
		summary.State = status.State
	}
	summary.Alive = false
	return summary, nil
}

func (s *Service) SessionAttach(ctx context.Context, runID string) error {
	summary, err := s.SessionStatus(ctx, runID)
	if err != nil {
		return err
	}
	return providertmux.Attach(ctx, summary.Session)
}
