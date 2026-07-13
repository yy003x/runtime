package agentrun

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"agent-arch/internal/provider"
)

type Service struct {
	Root           string
	ConfigDir      string
	RunsDir        string
	DefaultProject string
	DefaultProfile string
	RuntimeVersion string
	MaxConcurrency int
	HTTPClient     *http.Client
	store          Store
	configErr      error
}

func New(root string) *Service {
	settings, err := loadSettings(root)
	return &Service{
		Root: root, ConfigDir: rooted(root, settings.ProviderConfigDir),
		RunsDir: rooted(root, settings.RunsDir), DefaultProject: settings.DefaultProject,
		DefaultProfile: settings.DefaultProfile, MaxConcurrency: settings.MaxConcurrency,
		RuntimeVersion: "go-runtime", configErr: err,
	}
}

func (s *Service) Profiles() (map[string]provider.Config, error) {
	if s.configErr != nil {
		return nil, s.configErr
	}
	return provider.LoadDir(s.ConfigDir)
}

func (s *Service) Run(ctx context.Context, options RunOptions) (RunSummary, error) {
	runType := options.RunType
	if runType == "" {
		runType = RunTask
	}
	if !validRunType(runType) || runType == RunSession || runType == RunCommand {
		return RunSummary{}, fmt.Errorf("Run 仅支持 task|turn，得到 %q", runType)
	}
	mode := options.ExecutionMode
	if mode == "" {
		mode = ModeManaged
	}
	if mode != ModeManaged && mode != ModeCapture {
		return RunSummary{}, fmt.Errorf("execution_mode 必须是 managed|capture")
	}
	profiles, err := s.Profiles()
	if err != nil {
		return RunSummary{}, err
	}
	profileName := options.Profile
	if profileName == "" {
		profileName = s.DefaultProfile
	}
	profile, ok := provider.Resolve(profiles, profileName)
	if !ok {
		return RunSummary{}, fmt.Errorf("unknown provider profile: %s", profileName)
	}
	cwd, err := resolveCWD(options.CWD)
	if err != nil {
		return RunSummary{}, err
	}
	prompt, promptFile, err := resolvePrompt(cwd, options.Prompt, options.PromptFile)
	if err != nil {
		return RunSummary{}, err
	}
	overrides := cloneOverrides(options.ProviderOverrides)
	if _, err := provider.Prepare(profile, "", overrides); err != nil {
		return RunSummary{}, err
	}
	runID := options.RunID
	if runID == "" {
		runID = newRunID(runType)
	}
	paths, err := RunPaths(s.RunsDir, runType, runID)
	if err != nil {
		return RunSummary{}, err
	}
	projectID := options.ProjectID
	if projectID == "" {
		projectID = s.DefaultProject
	}
	if options.RunID != "" && !options.Force {
		if status, err := s.store.ReadStatus(paths); err == nil {
			return summary(paths, status, true), nil
		}
	}
	if err := paths.Ensure(); err != nil {
		return RunSummary{}, err
	}
	if options.Force {
		s.resetRun(paths)
	}
	now := time.Now().UTC()
	request := Request{
		SchemaVersion: 1, ContractVersion: ContractVersion, RuntimeVersion: s.RuntimeVersion,
		ProjectID: projectID, RunType: runType, RunID: runID, Caller: options.Caller,
		ProviderProfile: profile.ID, Provider: profile.Transport(), CWD: cwd,
		PromptFile: promptFile, DeadlineSeconds: options.DeadlineSeconds,
		ResultFile: paths.ResultFile, ResultSchema: options.ResultSchema, ExecutionMode: mode,
		ProviderOverrides: overrides, AllowedActions: append([]string(nil), options.AllowedActions...),
		ForbiddenActions: append([]string(nil), options.ForbiddenActions...), CreatedAt: now, UpdatedAt: now,
	}
	if request.ResultSchema != "" && request.ResultSchema != "result" && request.ResultSchema != "builtin:result" && !filepath.IsAbs(request.ResultSchema) {
		request.ResultSchema = filepath.Join(cwd, request.ResultSchema)
	}
	if model, ok := overrides["model"].(string); ok {
		request.ModelOverride = model
	}
	if effort, ok := overrides["reasoning_effort"].(string); ok {
		request.ReasoningEffortOverride = effort
	}
	if err := s.store.WriteRequest(paths, request); err != nil {
		return RunSummary{}, err
	}
	_, _ = s.store.WriteStatus(paths, request, StatePending, "", "queued", nil)
	s.register(paths, request, StatePending)
	_ = s.store.Event(paths, request, "status.changed", map[string]any{"state": StatePending})

	timeout := options.DeadlineSeconds
	if timeout == 0 {
		timeout = profile.TimeoutSeconds
	}
	runCtx := ctx
	cancel := func() {}
	if timeout > 0 {
		runCtx, cancel = context.WithTimeout(ctx, time.Duration(timeout)*time.Second)
	}
	defer cancel()
	providerStatus := map[string]any{"execution_mode": mode, "profile": profile.ID}
	_, _ = s.store.WriteStatus(paths, request, StateRunning, "", "running", providerStatus)
	_ = s.store.Event(paths, request, "status.changed", map[string]any{"state": StateRunning, "transport": profile.Transport(), "execution_mode": mode})

	providerPrompt := prompt
	if mode == ModeManaged && profile.ResultContract() == "required" {
		providerPrompt = managedPrompt(prompt, request, paths)
	}
	prepared, err := provider.Prepare(profile, providerPrompt, overrides)
	if err != nil {
		return s.fail(paths, request, providerStatus, "provider_error", err)
	}
	var providerResult provider.Result
	isTmux := profile.Transport() == provider.ExecutorTmux
	if isTmux {
		providerResult, err = s.executeTmuxTask(runCtx, profile, providerPrompt, request, paths, providerStatus)
	} else if prepared.CLI != nil {
		providerStatus["argv"] = prepared.CLI.Argv
		providerStatus["driver"] = prepared.CLI.Driver
		providerStatus["effective_options"] = prepared.CLI.EffectiveOptions
		extraEnv := map[string]string{
			"AGENTRUN_RESULT_FILE":  paths.ResultFile,
			"AGENTRUN_REQUEST_FILE": paths.RequestFile,
			"AGENTRUN_RUN_DIR":      paths.RunDir,
			"AGENTRUN_RUN_ID":       request.RunID,
		}
		providerResult, err = provider.ExecuteCLIWithObserver(runCtx, profile, *prepared.CLI, cwd, extraEnv, func(info provider.ExecutionInfo) {
			providerStatus["pid"] = info.PID
			providerStatus["pgid"] = info.PGID
			_, _ = s.store.WriteStatus(paths, request, StateRunning, "", "cli running", providerStatus)
		})
	} else {
		providerStatus["protocol"] = prepared.API.Protocol
		providerStatus["effective_options"] = prepared.API.EffectiveOptions
		providerResult, err = provider.ExecuteAPI(runCtx, s.HTTPClient, profile, *prepared.API)
	}
	if writeErr := writeOutputLog(paths.OutputLog, prepared, providerResult); writeErr != nil {
		return RunSummary{}, writeErr
	}
	if current, readErr := s.store.ReadStatus(paths); readErr == nil && current.State == StateCancelled {
		return summary(paths, current, false), context.Canceled
	}
	if runCtx.Err() != nil {
		if ctx.Err() != nil {
			return s.cancelled(paths, request, providerStatus, ctx.Err())
		}
		return s.fail(paths, request, providerStatus, "timeout", runCtx.Err())
	}
	if err != nil {
		return s.fail(paths, request, providerStatus, "provider_error", err)
	}
	providerStatus["returncode"] = providerResult.ExitCode
	_ = s.store.Event(paths, request, "provider.exited", map[string]any{"returncode": providerResult.ExitCode, "execution_mode": mode})

	_, _ = s.store.WriteStatus(paths, request, StateResultPending, "", "validating result", providerStatus)
	_ = s.store.Event(paths, request, "status.changed", map[string]any{"state": StateResultPending})
	_, validationReason := s.store.ValidateResult(paths, request.RunID, request.ResultSchema)
	if validationReason == "" {
		return s.markDone(paths, request, providerStatus)
	}
	if validationReason == "schema_invalid" {
		return s.fail(paths, request, providerStatus, validationReason, fmt.Errorf("result validation failed: %s", validationReason))
	}
	if !isTmux && (prepared.API != nil || (providerResult.ExitCode == 0 && (mode == ModeCapture || profile.ResultContract() != "required"))) {
		text := strings.TrimSpace(providerResult.FinalText)
		if text == "" {
			text = captureSummary(providerResult.Stdout)
		}
		result := Result{SchemaVersion: 1, RunID: runID, Outcome: OutcomeSucceeded, Summary: text,
			Artifacts: []map[string]any{{"type": "log", "path": paths.OutputLog}}, Errors: []map[string]any{}, Validation: Validation{Commands: []string{}, Passed: true}}
		if err := s.store.WriteResult(paths, result); err != nil {
			return RunSummary{}, err
		}
		_ = s.store.Event(paths, request, "result.synthesized", map[string]any{"execution_mode": mode, "summary_chars": len(text)})
		return s.markDone(paths, request, providerStatus)
	}
	reason := "result_missing"
	if providerResult.ExitCode != 0 {
		reason = "exited"
	}
	return s.fail(paths, request, providerStatus, reason, fmt.Errorf("provider exited %d without valid result", providerResult.ExitCode))
}

func (s *Service) Status(runType, runID string) (Status, error) {
	paths, err := RunPaths(s.RunsDir, runType, runID)
	if err != nil {
		return Status{}, err
	}
	return s.store.ReadStatus(paths)
}

type Logs struct {
	RunID   string  `json:"run_id"`
	Content string  `json:"content"`
	Events  []Event `json:"events"`
}

func (s *Service) Logs(runType, runID string, tail int) (Logs, error) {
	paths, err := RunPaths(s.RunsDir, runType, runID)
	if err != nil {
		return Logs{}, err
	}
	content, err := os.ReadFile(paths.OutputLog)
	if err != nil && !os.IsNotExist(err) {
		return Logs{}, err
	}
	events, err := s.store.ReadEvents(paths)
	if err != nil {
		return Logs{}, err
	}
	if tail <= 0 {
		tail = 120
	}
	lines := strings.Split(string(content), "\n")
	if len(lines) > tail {
		lines = lines[len(lines)-tail:]
	}
	if len(events) > tail {
		events = events[len(events)-tail:]
	}
	return Logs{RunID: runID, Content: strings.Join(lines, "\n"), Events: events}, nil
}

func (s *Service) ReadResult(runType, runID string) (Result, error) {
	paths, err := RunPaths(s.RunsDir, runType, runID)
	if err != nil {
		return Result{}, err
	}
	return s.store.ReadResult(paths)
}

func (s *Service) Cancel(runType, runID string) (RunSummary, error) {
	paths, err := RunPaths(s.RunsDir, runType, runID)
	if err != nil {
		return RunSummary{}, err
	}
	request, err := s.store.ReadRequest(paths)
	if err != nil {
		return RunSummary{}, fmt.Errorf("run 不存在: %s", runID)
	}
	status, err := s.store.ReadStatus(paths)
	if err != nil {
		return RunSummary{}, err
	}
	if status.State == StateDone || status.State == StateFailed || status.State == StateBlocked || status.State == StateCancelled {
		return summary(paths, status, true), nil
	}
	if request.Provider == provider.ExecutorTmux {
		if session, _ := status.ProviderStatus["tmux_session"].(string); session != "" {
			_ = tmuxCommand(context.Background(), "kill-session", "-t", session).Run()
		}
	} else {
		if pgid := numberValue(status.ProviderStatus["pgid"]); pgid > 0 {
			_ = syscall.Kill(-pgid, syscall.SIGINT)
		}
	}
	status, err = s.store.WriteStatus(paths, request, StateCancelled, "interrupted", "cancelled", status.ProviderStatus)
	s.updateRegistry(paths, status)
	_ = s.store.Event(paths, request, "provider.cancelled", map[string]any{"transport": request.Provider})
	return summary(paths, status, false), err
}

func numberValue(value any) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	default:
		return 0
	}
}

func (s *Service) markDone(paths Paths, request Request, providerStatus map[string]any) (RunSummary, error) {
	if _, reason := s.store.ValidateResult(paths, request.RunID, request.ResultSchema); reason != "" {
		return s.fail(paths, request, providerStatus, reason, fmt.Errorf("result validation failed: %s", reason))
	}
	status, err := s.store.WriteStatus(paths, request, StateDone, "", "result 校验通过", providerStatus)
	if err != nil {
		return RunSummary{}, err
	}
	result, _ := s.store.ReadResult(paths)
	s.updateRegistry(paths, status)
	_ = s.store.Event(paths, request, "result.written", map[string]any{"outcome": result.Outcome})
	return summary(paths, status, false), nil
}

func (s *Service) fail(paths Paths, request Request, providerStatus map[string]any, reason string, cause error) (RunSummary, error) {
	status, writeErr := s.store.WriteStatus(paths, request, StateFailed, reason, cause.Error(), providerStatus)
	s.updateRegistry(paths, status)
	_ = s.store.Event(paths, request, "status.changed", map[string]any{"state": StateFailed, "failure_reason": reason})
	if writeErr != nil {
		return RunSummary{}, writeErr
	}
	return summary(paths, status, false), cause
}

func (s *Service) cancelled(paths Paths, request Request, providerStatus map[string]any, cause error) (RunSummary, error) {
	status, writeErr := s.store.WriteStatus(paths, request, StateCancelled, "interrupted", "cancelled", providerStatus)
	s.updateRegistry(paths, status)
	_ = s.store.Event(paths, request, "provider.cancelled", map[string]any{"transport": request.Provider})
	if writeErr != nil {
		return RunSummary{}, writeErr
	}
	return summary(paths, status, false), cause
}

func (s *Service) resetRun(paths Paths) {
	if status, err := s.store.ReadStatus(paths); err == nil {
		if session, _ := status.ProviderStatus["tmux_session"].(string); session != "" {
			_ = tmuxCommand(context.Background(), "kill-session", "-t", session).Run()
		}
	}
	for _, path := range []string{paths.RequestFile, paths.StatusFile, paths.EventsFile, paths.OutputLog, paths.ResultFile, paths.DoneFile} {
		_ = os.Remove(path)
	}
}

func managedPrompt(prompt string, request Request, paths Paths) string {
	return fmt.Sprintf(`%s

## AgentRun result contract

本次任务完成信号不是 stdout/stderr。最终必须写入环境变量 AGENTRUN_RESULT_FILE 指向的 JSON 文件：

%s

JSON 必须包含 schema_version、run_id、outcome、summary、artifacts、errors、validation；run_id 必须为 %s。写入后请重新读取并确认 JSON 可解析。终端输出只作为过程日志，不能替代 result.json。
`, strings.TrimRight(prompt, "\n"), paths.ResultFile, request.RunID)
}

func writeOutputLog(path string, prepared provider.PreparedRequest, result provider.Result) error {
	var builder strings.Builder
	if prepared.CLI != nil {
		fmt.Fprintf(&builder, "argv=%q\nrunning\n--- stream ---\n", prepared.CLI.Argv)
	}
	if result.Stdout != "" {
		for _, line := range strings.SplitAfter(result.Stdout, "\n") {
			if line != "" {
				builder.WriteString("[stdout] " + line)
			}
		}
	}
	if result.Stderr != "" {
		for _, line := range strings.SplitAfter(result.Stderr, "\n") {
			if line != "" {
				builder.WriteString("[stderr] " + line)
			}
		}
	}
	fmt.Fprintf(&builder, "returncode=%d\n", result.ExitCode)
	return os.WriteFile(path, []byte(builder.String()), 0o644)
}

func resolveCWD(value string) (string, error) {
	if value == "" {
		return os.Getwd()
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	info, err := os.Stat(absolute)
	if err != nil || !info.IsDir() {
		return "", fmt.Errorf("cwd 不存在或不是目录: %s", absolute)
	}
	return absolute, nil
}

func resolvePrompt(cwd, inline, file string) (string, string, error) {
	if strings.TrimSpace(inline) != "" && strings.TrimSpace(file) != "" {
		return "", "", fmt.Errorf("inline prompt and prompt_file cannot both be set")
	}
	if file != "" {
		path := file
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		absolute, err := filepath.Abs(path)
		if err != nil {
			return "", "", err
		}
		data, err := os.ReadFile(absolute)
		if err != nil {
			return "", "", fmt.Errorf("read prompt_file: %w", err)
		}
		return string(data), absolute, nil
	}
	if strings.TrimSpace(inline) == "" {
		return "", "", fmt.Errorf("prompt is required")
	}
	return inline, "", nil
}

func captureSummary(stdout string) string {
	text := strings.TrimSpace(stdout)
	if text == "" {
		return "cli 退出 0,无 stdout 输出"
	}
	lines := strings.Split(text, "\n")
	if len(lines) > 20 {
		lines = lines[len(lines)-20:]
	}
	text = strings.Join(lines, "\n")
	if len(text) > 4000 {
		text = text[len(text)-4000:]
	}
	return text
}

func cloneOverrides(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}

func summary(paths Paths, status Status, idempotent bool) RunSummary {
	return RunSummary{RunID: status.RunID, ProjectID: status.ProjectID, RunType: status.RunType,
		State: status.State, FailureReason: status.FailureReason, ResultFile: paths.ResultFile, RunDir: paths.RunDir, Idempotent: idempotent}
}

func newRunID(runType string) string {
	return fmt.Sprintf("%s-%s-%s", runType, time.Now().Format("20060102-150405"), randomID(3))
}
