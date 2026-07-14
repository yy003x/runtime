package agentrun

import (
	"context"
	"fmt"
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
	if profile.Transport() != provider.ExecutorTmux {
		return SessionSummary{}, fmt.Errorf("session 只支持 tmux profile，得到 %s", profile.Transport())
	}
	if options.ProjectID == "" {
		options.ProjectID = s.DefaultProject
	}
	resolvedCWD, err := resolveCWD(options.CWD)
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
	if !options.Force {
		if status, err := s.store.ReadStatus(paths); err == nil {
			session, _ := status.ProviderStatus["tmux_session"].(string)
			alive, _ := s.DaemonClient().HasTmux(ctx, options.RunID, session)
			return SessionSummary{RunID: options.RunID, ProjectID: options.ProjectID, State: status.State, RunDir: paths.RunDir, Session: session, Attach: "tmux attach-session -t " + session, Alive: alive, Idempotent: true, Status: status.ProviderStatus}, nil
		}
	}
	if err := paths.Ensure(); err != nil {
		return SessionSummary{}, err
	}
	if options.Force {
		s.resetRun(paths)
	}
	now := time.Now().UTC()
	request := Request{SchemaVersion: 1, ContractVersion: ContractVersion, RuntimeVersion: s.RuntimeVersion,
		ProjectID: options.ProjectID, RunType: RunSession, RunID: options.RunID, ProviderProfile: profile.ID,
		Provider: provider.ExecutorTmux, CWD: resolvedCWD, ResultFile: paths.ResultFile,
		ExecutionMode: ModeManaged, ProviderOverrides: map[string]any{}, AllowedActions: append([]string(nil), options.AllowedActions...), ForbiddenActions: append([]string(nil), options.ForbiddenActions...), CreatedAt: now, UpdatedAt: now}
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
	session, err := backend.StartShell(ctx, options.RunID, resolvedCWD, provider.TmuxShellCommand(profile, nil))
	if err != nil {
		_, _ = s.store.WriteStatus(paths, request, StateFailed, "provider_error", err.Error(), nil)
		return SessionSummary{}, err
	}
	providerStatus := map[string]any{"tmux_session": session, "alive": true}
	status, err := s.store.WriteStatus(paths, request, StateRunning, "", "session running", providerStatus)
	_ = s.store.Event(paths, request, "status.changed", map[string]any{"state": StateRunning, "transport": provider.ExecutorTmux})
	return SessionSummary{RunID: options.RunID, ProjectID: options.ProjectID, State: status.State, RunDir: paths.RunDir, Session: session, Attach: "tmux attach-session -t " + session, Alive: true, Status: providerStatus}, err
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
	session, _ := status.ProviderStatus["tmux_session"].(string)
	alive, daemonErr := s.DaemonClient().HasTmux(ctx, runID, session)
	if daemonErr != nil {
		return SessionSummary{}, daemonErr
	}
	return SessionSummary{RunID: runID, ProjectID: status.ProjectID, State: status.State, RunDir: paths.RunDir, Session: session, Attach: "tmux attach-session -t " + session, Alive: alive, Status: status.ProviderStatus}, nil
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
			if profile, ok := provider.Resolve(profiles, request.ProviderProfile); ok && profile.CLI != nil && profile.CLI.Tmux != nil {
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
	if err := backend.Send(ctx, summary.Session, text, providertmux.SendOptions{Submit: submit, Bracketed: bracketed, Stabilize: submit}); err != nil {
		return summary, err
	}
	return s.SessionStatus(ctx, runID)
}

func (s *Service) SessionLogs(ctx context.Context, runID string, tail int) (Logs, error) {
	summary, err := s.SessionStatus(ctx, runID)
	if err != nil {
		return Logs{}, err
	}
	content, captureErr := s.DaemonClient().CaptureTmux(ctx, runID, summary.Session, tail)
	paths, _ := RunPaths(s.RunsDir, RunSession, runID)
	events, _ := s.store.ReadEvents(paths)
	if captureErr != nil && summary.Alive {
		return Logs{}, captureErr
	}
	return Logs{RunID: runID, Content: content, Events: events}, nil
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
	paths, _ := RunPaths(s.RunsDir, RunSession, runID)
	request, _ := s.store.ReadRequest(paths)
	status, _ := s.store.WriteStatus(paths, request, StateCancelled, "interrupted", "session stopped", summary.Status)
	s.updateRegistry(paths, status)
	summary.State = status.State
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
