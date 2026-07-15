package agentrun

import (
	"context"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"
	"time"

	"agent-runtime/internal/provider"
	providertmux "agent-runtime/internal/provider/tmux"
)

const commandExitMarker = "__AGENTRUN_COMMAND_EXIT__="

type CommandOptions struct {
	Profile         string
	ProjectID       string
	RunID           string
	CWD             string
	Label           string
	Input           string
	DeadlineSeconds int
	Argv            []string
	Force           bool
}

func (s *Service) StartCommand(ctx context.Context, options CommandOptions) (SessionSummary, error) {
	if len(options.Argv) == 0 {
		return SessionSummary{}, fmt.Errorf("command argv is required")
	}
	if options.Profile == "" {
		return SessionSummary{}, fmt.Errorf("command config is required")
	}
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
	if options.Label == "" {
		options.Label = "command"
	}
	cwd, err := resolveCWD(options.CWD)
	if err != nil {
		return SessionSummary{}, err
	}
	if options.RunID == "" {
		options.RunID = newRunID(RunCommand)
	}
	paths, err := RunPaths(s.RunsDir, RunCommand, options.RunID)
	if err != nil {
		return SessionSummary{}, err
	}
	if !options.Force {
		if _, err := s.store.ReadStatus(paths); err == nil {
			existing, statusErr := s.CommandStatus(ctx, options.RunID)
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
	now := time.Now().UTC()
	request := Request{
		SchemaVersion: 1, ContractVersion: ContractVersion, RuntimeVersion: s.RuntimeVersion,
		ProjectID: options.ProjectID, RunType: RunCommand, RunID: options.RunID,
		ProviderProfile: profile.ID, Provider: provider.ExecutorTmux, CWD: cwd,
		DeadlineSeconds: options.DeadlineSeconds, ResultFile: paths.ResultFile, ExecutionMode: ModeCapture,
		ProviderOverrides: map[string]any{"argv": append([]string(nil), options.Argv...), "label": options.Label},
		AllowedActions:    []string{}, ForbiddenActions: []string{}, CreatedAt: now, UpdatedAt: now,
	}
	if err := s.store.WriteRequest(paths, request); err != nil {
		return SessionSummary{}, err
	}
	_, _ = s.store.WriteStatus(paths, request, StatePending, "", "queued", nil)
	s.register(paths, request, StatePending)
	commandParts := make([]string, 0, len(options.Argv))
	for _, arg := range options.Argv {
		commandParts = append(commandParts, provider.ShellQuote(arg))
	}
	command := provider.TmuxCommandEnv(profile, strings.Join(commandParts, " "))
	command += "; rc=$?; printf '\n" + commandExitMarker + "%s\n' \"$rc\"; while :; do sleep 3600; done"
	backend, err := provider.NewTmuxBackend(profile, s.DaemonClient())
	if err != nil {
		_, _ = s.store.WriteStatus(paths, request, StateFailed, "provider_error", err.Error(), nil)
		return SessionSummary{}, err
	}
	session, err := backend.StartShell(ctx, options.RunID, cwd, command)
	if err != nil {
		_, _ = s.store.WriteStatus(paths, request, StateFailed, "provider_error", err.Error(), nil)
		return SessionSummary{}, err
	}
	providerStatus := map[string]any{"tmux_session": session, "alive": true, "label": options.Label, "argv": options.Argv}
	status, err := s.store.WriteStatus(paths, request, StateRunning, "", "command running", providerStatus)
	_ = s.store.Event(paths, request, "status.changed", map[string]any{"state": StateRunning, "transport": provider.ExecutorTmux})
	if options.Input != "" {
		if sendErr := backend.Send(ctx, session, options.Input, providertmux.SendOptions{}); sendErr != nil {
			return SessionSummary{}, sendErr
		}
	}
	return SessionSummary{RunID: options.RunID, ProjectID: options.ProjectID, State: status.State, RunDir: paths.RunDir, Session: session, Attach: "tmux attach-session -t " + session, Alive: true, Status: providerStatus}, err
}

func (s *Service) CommandStatus(ctx context.Context, runID string) (SessionSummary, error) {
	paths, err := RunPaths(s.RunsDir, RunCommand, runID)
	if err != nil {
		return SessionSummary{}, err
	}
	request, err := s.store.ReadRequest(paths)
	if err != nil {
		return SessionSummary{}, fmt.Errorf("command 不存在: %s", runID)
	}
	status, err := s.store.ReadStatus(paths)
	if err != nil {
		return SessionSummary{}, err
	}
	session, _ := status.ProviderStatus["tmux_session"].(string)
	alive, daemonErr := s.DaemonClient().HasTmux(ctx, runID, session)
	if daemonErr != nil {
		return SessionSummary{}, daemonErr
	}
	content := ""
	if alive {
		content, _ = s.DaemonClient().CaptureTmux(ctx, runID, session, 500)
		_ = os.WriteFile(paths.OutputLog, []byte(content), 0o644)
	}
	if terminalStateValue(status.State) {
		return tmuxSummary(paths, status, session, alive), nil
	}
	if code, found := commandExitCode(content); found {
		state, reason := StateDone, ""
		if code != 0 {
			state, reason = StateFailed, "exited"
		}
		status.ProviderStatus["returncode"] = code
		status.ProviderStatus["alive"] = alive
		status, err = s.store.WriteStatus(paths, request, state, reason, "command exited", status.ProviderStatus)
		s.updateRegistry(paths, status)
		_ = s.store.Event(paths, request, "provider.exited", map[string]any{"returncode": code})
		return tmuxSummary(paths, status, session, alive), err
	}
	if !alive {
		status, err = s.store.WriteStatus(paths, request, StateFailed, "orphaned", "tmux command disappeared", status.ProviderStatus)
		s.updateRegistry(paths, status)
	}
	return tmuxSummary(paths, status, session, alive), err
}

func (s *Service) CommandLogs(ctx context.Context, runID string, tail int) (Logs, error) {
	summary, err := s.CommandStatus(ctx, runID)
	if err != nil {
		return Logs{}, err
	}
	paths, _ := RunPaths(s.RunsDir, RunCommand, runID)
	content, _ := os.ReadFile(paths.OutputLog)
	if summary.Alive {
		if live, captureErr := s.DaemonClient().CaptureTmux(ctx, runID, summary.Session, tail); captureErr == nil {
			if strings.TrimSpace(live) != "" {
				content = []byte(live)
				_ = os.WriteFile(paths.OutputLog, content, 0o644)
			}
		}
	}
	events, _ := s.store.ReadEvents(paths)
	return Logs{RunID: runID, Content: tailLines(string(content), tail), Events: events}, nil
}

func (s *Service) CommandInterrupt(ctx context.Context, runID string) (SessionSummary, error) {
	summary, err := s.CommandStatus(ctx, runID)
	if err != nil {
		return summary, err
	}
	if summary.Alive && !terminalStateValue(summary.State) {
		err = s.DaemonClient().InterruptTmux(ctx, runID, summary.Session)
	}
	return summary, err
}

func (s *Service) CommandStop(ctx context.Context, runID string) (SessionSummary, error) {
	summary, err := s.CommandStatus(ctx, runID)
	if err != nil {
		return summary, err
	}
	if summary.Alive {
		if err := s.DaemonClient().KillTmux(ctx, runID, summary.Session); err != nil {
			return summary, err
		}
	}
	if !terminalStateValue(summary.State) {
		paths, _ := RunPaths(s.RunsDir, RunCommand, runID)
		request, _ := s.store.ReadRequest(paths)
		status, writeErr := s.store.WriteStatus(paths, request, StateCancelled, "interrupted", "command stopped", summary.Status)
		s.updateRegistry(paths, status)
		summary.State = status.State
		err = writeErr
	}
	summary.Alive = false
	return summary, err
}

func (s *Service) CommandAttach(ctx context.Context, runID string) error {
	summary, err := s.CommandStatus(ctx, runID)
	if err != nil {
		return err
	}
	return providertmux.Attach(ctx, summary.Session)
}

func commandExitCode(content string) (int, bool) {
	pattern := regexp.MustCompile(regexp.QuoteMeta(commandExitMarker) + `([0-9]+)`)
	matches := pattern.FindAllStringSubmatch(content, -1)
	if len(matches) == 0 {
		return 0, false
	}
	code, err := strconv.Atoi(matches[len(matches)-1][1])
	return code, err == nil
}

func tmuxSummary(paths Paths, status Status, session string, alive bool) SessionSummary {
	return SessionSummary{RunID: status.RunID, ProjectID: status.ProjectID, State: status.State, RunDir: paths.RunDir, Session: session, Attach: "tmux attach-session -t " + session, Alive: alive, Status: status.ProviderStatus}
}

func terminalStateValue(state string) bool {
	return state == StateDone || state == StateFailed || state == StateBlocked || state == StateCancelled
}

func tailLines(content string, tail int) string {
	if tail <= 0 {
		tail = 120
	}
	lines := strings.Split(strings.TrimRight(content, "\r\n "), "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	return strings.Join(lines, "\n")
}
