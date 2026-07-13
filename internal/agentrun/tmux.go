package agentrun

import (
	"context"
	"fmt"
	"os"
	"os/exec"
	"regexp"
	"sort"
	"strings"
	"time"

	"agent-arch/internal/provider"
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

func (s *Service) executeTmuxTask(ctx context.Context, profile provider.Config, prompt string, request Request, paths Paths, providerStatus map[string]any) (provider.Result, error) {
	session, err := startTmuxShell(ctx, profile, request.RunID, request.CWD, tmuxTaskShellCommand(profile, request, paths))
	if err != nil {
		return provider.Result{ExitCode: 1}, err
	}
	providerStatus["tmux_session"] = session
	providerStatus["alive"] = true
	providerStatus["done_file"] = paths.DoneFile
	_, _ = s.store.WriteStatus(paths, request, StateRunning, "", "tmux running", providerStatus)
	defer func() { _ = tmuxCommand(context.Background(), "kill-session", "-t", session).Run() }()
	taskPrompt := tmuxDonePrompt(prompt, paths.ResultFile, paths.DoneFile)
	if err := tmuxSendBracketed(ctx, session, taskPrompt, true, profile.CLI.Tmux.PasteBracketed); err != nil {
		return provider.Result{ExitCode: 1}, err
	}
	poll := tmuxPoll(profile)
	lastOutput := ""
	for {
		if ctx.Err() != nil {
			return provider.Result{Stdout: lastOutput, ExitCode: 1}, ctx.Err()
		}
		if _, err := os.Stat(paths.DoneFile); err == nil {
			providerStatus["done_file_exists"] = true
			_, resultErr := os.Stat(paths.ResultFile)
			providerStatus["result_file_exists"] = resultErr == nil
			output, _ := tmuxCapture(ctx, session, 200)
			return provider.Result{Stdout: output, FinalText: strings.TrimSpace(output), ExitCode: 0}, nil
		}
		output, captureErr := tmuxCapture(ctx, session, 200)
		if captureErr == nil {
			lastOutput = output
		}
		if !tmuxHasSession(ctx, session) {
			providerStatus["alive"] = false
			providerStatus["reason"] = "exited_without_done_file"
			return provider.Result{Stdout: lastOutput, FinalText: strings.TrimSpace(lastOutput), ExitCode: 1}, nil
		}
		timer := time.NewTimer(poll)
		select {
		case <-ctx.Done():
			timer.Stop()
			return provider.Result{Stdout: lastOutput, ExitCode: 1}, ctx.Err()
		case <-timer.C:
		}
	}
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
			alive := tmuxHasSession(ctx, session)
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
	session, err := startTmux(ctx, profile, options.RunID, resolvedCWD)
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
	alive := tmuxHasSession(ctx, session)
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
	paths, _ := RunPaths(s.RunsDir, RunSession, runID)
	if request, readErr := s.store.ReadRequest(paths); readErr == nil {
		if profiles, loadErr := s.Profiles(); loadErr == nil {
			if profile, ok := provider.Resolve(profiles, request.ProviderProfile); ok && profile.CLI != nil && profile.CLI.Tmux != nil {
				bracketed = profile.CLI.Tmux.PasteBracketed
			}
		}
	}
	if err := tmuxSendBracketed(ctx, summary.Session, text, submit, bracketed); err != nil {
		return summary, err
	}
	return s.SessionStatus(ctx, runID)
}

func (s *Service) SessionLogs(ctx context.Context, runID string, tail int) (Logs, error) {
	summary, err := s.SessionStatus(ctx, runID)
	if err != nil {
		return Logs{}, err
	}
	content, captureErr := tmuxCapture(ctx, summary.Session, tail)
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
		err = tmuxCommand(ctx, "send-keys", "-t", summary.Session, "C-c").Run()
	}
	return summary, err
}

func (s *Service) SessionStop(ctx context.Context, runID string) (SessionSummary, error) {
	summary, err := s.SessionStatus(ctx, runID)
	if err != nil {
		return SessionSummary{}, err
	}
	if summary.Alive {
		if err := tmuxCommand(ctx, "kill-session", "-t", summary.Session).Run(); err != nil {
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
	cmd := tmuxCommand(ctx, "attach-session", "-t", summary.Session)
	cmd.Stdin, cmd.Stdout, cmd.Stderr = os.Stdin, os.Stdout, os.Stderr
	return cmd.Run()
}

func startTmux(ctx context.Context, profile provider.Config, runID, cwd string) (string, error) {
	return startTmuxShell(ctx, profile, runID, cwd, tmuxShellCommand(profile))
}

func startTmuxShell(ctx context.Context, profile provider.Config, runID, cwd, command string) (string, error) {
	if _, err := exec.LookPath("tmux"); err != nil {
		return "", fmt.Errorf("tmux not found: %w", err)
	}
	base := profile.CLI.Tmux.SessionName
	if regexp.MustCompile(`^[0-9]+$`).MatchString(base) {
		return "", fmt.Errorf("numeric tmux session_name is not allowed: %s", base)
	}
	suffix := runID
	if len(suffix) > 12 {
		suffix = suffix[len(suffix)-12:]
	}
	session := sanitizeTmuxName(base + "-" + suffix)
	cmd := tmuxCommand(ctx, "new-session", "-d", "-s", session, "-c", cwd, command)
	if output, err := cmd.CombinedOutput(); err != nil {
		return "", fmt.Errorf("tmux new-session: %w: %s", err, strings.TrimSpace(string(output)))
	}
	settle := time.Duration(profile.CLI.Tmux.SessionReadySettleSeconds * float64(time.Second))
	if settle <= 0 {
		settle = 300 * time.Millisecond
	}
	timer := time.NewTimer(settle)
	select {
	case <-ctx.Done():
		timer.Stop()
		return "", ctx.Err()
	case <-timer.C:
	}
	if !tmuxHasSession(ctx, session) {
		return "", fmt.Errorf("tmux session did not stay alive: %s", session)
	}
	return session, nil
}

func tmuxShellCommand(profile provider.Config) string {
	return tmuxShellCommandWithEnv(profile, nil)
}

func tmuxShellCommandWithEnv(profile provider.Config, extra map[string]string) string {
	command := profile.CLI.Command
	commandParts := []string{shellQuote(command.Binary)}
	for _, arg := range command.Args {
		commandParts = append(commandParts, shellQuote(provider.ExpandEnv(arg)))
	}
	if command.Model != "" {
		commandParts = append(commandParts, "--model", shellQuote(command.Model))
	}
	values := make(map[string]string, len(command.Env)+len(command.EnvPassthrough)+len(extra))
	for key := range command.Env {
		values[key] = provider.ExpandEnv(command.Env[key])
	}
	for _, key := range command.EnvPassthrough {
		if _, exists := values[key]; !exists {
			values[key] = os.Getenv(key)
		}
	}
	for key, value := range extra {
		values[key] = value
	}
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := []string{"exec"}
	if len(keys) > 0 {
		parts = append(parts, "env")
		for _, key := range keys {
			parts = append(parts, shellQuote(key+"="+values[key]))
		}
	}
	parts = append(parts, commandParts...)
	return strings.Join(parts, " ")
}

func tmuxTaskShellCommand(profile provider.Config, request Request, paths Paths) string {
	values := map[string]string{
		"AGENTRUN_DONE_FILE":    paths.DoneFile,
		"AGENTRUN_OUTPUT_LOG":   paths.OutputLog,
		"AGENTRUN_REQUEST_FILE": paths.RequestFile,
		"AGENTRUN_RESULT_FILE":  paths.ResultFile,
		"AGENTRUN_RUN_DIR":      paths.RunDir,
		"AGENTRUN_RUN_ID":       request.RunID,
	}
	return tmuxShellCommandWithEnv(profile, values)
}

func tmuxDonePrompt(prompt, resultFile, doneFile string) string {
	return fmt.Sprintf(`%s

## Tmux task completion contract

完成 result.json 写入并重新读取校验后，最后一步创建空完成标记：
- result_file: %s
- done_file: %s

必须先原子写入并校验 result_file，再执行 touch "$AGENTRUN_DONE_FILE"。终端输出不能替代 result_file；未创建 done_file 不视为完成。
`, strings.TrimRight(prompt, "\n"), resultFile, doneFile)
}

func tmuxCommandEnv(profile provider.Config, command string) string {
	configured := profile.CLI.Command
	keys := make([]string, 0, len(configured.Env)+len(configured.EnvPassthrough))
	for key := range configured.Env {
		keys = append(keys, key)
	}
	for _, key := range configured.EnvPassthrough {
		if _, exists := configured.Env[key]; !exists {
			keys = append(keys, key)
		}
	}
	sort.Strings(keys)
	if len(keys) == 0 {
		return command
	}
	parts := []string{"env"}
	for _, key := range keys {
		value, ok := configured.Env[key]
		if !ok {
			value = os.Getenv(key)
		}
		parts = append(parts, shellQuote(key+"="+provider.ExpandEnv(value)))
	}
	parts = append(parts, command)
	return strings.Join(parts, " ")
}

func tmuxSend(ctx context.Context, session, text string, submit bool) error {
	return tmuxSendBracketed(ctx, session, text, submit, false)
}

func tmuxSendBracketed(ctx context.Context, session, text string, submit, bracketed bool) error {
	buffer := "agentrun-" + randomID(4)
	load := tmuxCommand(ctx, "load-buffer", "-b", buffer, "-")
	load.Stdin = strings.NewReader(text)
	if output, err := load.CombinedOutput(); err != nil {
		return fmt.Errorf("tmux load-buffer: %w: %s", err, strings.TrimSpace(string(output)))
	}
	defer func() { _ = tmuxCommand(context.Background(), "delete-buffer", "-b", buffer).Run() }()
	args := []string{"paste-buffer", "-d", "-b", buffer, "-t", session}
	if bracketed {
		args = append([]string{"paste-buffer", "-p"}, args[1:]...)
	}
	if output, err := tmuxCommand(ctx, args...).CombinedOutput(); err != nil {
		return fmt.Errorf("tmux paste-buffer: %w: %s", err, strings.TrimSpace(string(output)))
	}
	if submit {
		if output, err := tmuxCommand(ctx, "send-keys", "-t", session, "Enter").CombinedOutput(); err != nil {
			return fmt.Errorf("tmux send Enter: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func tmuxCapture(ctx context.Context, session string, tail int) (string, error) {
	if tail <= 0 {
		tail = 120
	}
	output, err := tmuxCommand(ctx, "capture-pane", "-p", "-t", session, "-S", fmt.Sprintf("-%d", tail)).CombinedOutput()
	if err != nil {
		return "", fmt.Errorf("tmux capture-pane: %w: %s", err, strings.TrimSpace(string(output)))
	}
	return string(output), nil
}

func tmuxHasSession(ctx context.Context, session string) bool {
	if session == "" {
		return false
	}
	return tmuxCommand(ctx, "has-session", "-t", session).Run() == nil
}

func tmuxCommand(ctx context.Context, args ...string) *exec.Cmd {
	return exec.CommandContext(ctx, "tmux", args...)
}

func tmuxPoll(profile provider.Config) time.Duration {
	value := profile.CLI.Tmux.PollIntervalSeconds
	if value <= 0 {
		value = 0.3
	}
	return time.Duration(value * float64(time.Second))
}

func sanitizeTmuxName(value string) string {
	value = strings.Map(func(r rune) rune {
		if (r >= 'a' && r <= 'z') || (r >= 'A' && r <= 'Z') || (r >= '0' && r <= '9') || r == '-' || r == '_' {
			return r
		}
		return '-'
	}, value)
	return strings.Trim(value, "-")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}
