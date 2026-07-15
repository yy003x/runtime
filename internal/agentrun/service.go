package agentrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	"agent-runtime/internal/daemon"
	"agent-runtime/internal/layout"
	"agent-runtime/internal/provider"
)

var Version = "dev"

type Service struct {
	Home           string
	ConfigDir      string
	PersonaDir     string
	RunsDir        string
	StateDir       string
	paths          layout.Paths
	DefaultProject string
	DefaultProfile string
	RuntimeVersion string
	MaxConcurrency int
	HTTPClient     *http.Client
	store          Store
	configErr      error
}

type runProviderSink struct {
	service *Service
	paths   Paths
	request Request
	status  map[string]any
	stream  sync.Mutex
}

func (s *runProviderSink) Stdout(value []byte) error {
	return s.appendStream("stdout", value)
}

func (s *runProviderSink) Stderr(value []byte) error {
	return s.appendStream("stderr", value)
}

func (s *runProviderSink) Event(event provider.Event) error {
	return s.service.store.Event(s.paths, s.request, event.Type, event.Data)
}

func (s *runProviderSink) StatusPatch(patch provider.StatusPatch) error {
	if current, err := s.service.store.ReadStatus(s.paths); err == nil && terminalStateValue(current.State) {
		return nil
	}
	for key, value := range patch.Values {
		s.status[key] = value
	}
	_, err := s.service.store.WriteStatus(s.paths, s.request, StateRunning, "", patch.Message, s.status)
	return err
}

func (s *runProviderSink) appendStream(name string, value []byte) error {
	if len(value) == 0 {
		return nil
	}
	s.stream.Lock()
	defer s.stream.Unlock()
	file, err := os.OpenFile(s.paths.OutputLog, os.O_CREATE|os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		return err
	}
	defer file.Close()
	for _, line := range strings.SplitAfter(string(value), "\n") {
		if line != "" {
			if _, err := fmt.Fprintf(file, "[%s] %s", name, line); err != nil {
				return err
			}
		}
	}
	return nil
}

func New(home string) *Service {
	paths, pathErr := layout.FromHome(home)
	if pathErr != nil {
		return &Service{Home: home, RuntimeVersion: Version, configErr: pathErr}
	}
	settings, err := loadSettings(paths.ConfigDir)
	return &Service{
		Home: paths.Home, ConfigDir: paths.ConfigDir, PersonaDir: paths.PersonaDir,
		RunsDir: paths.RunsDir, StateDir: paths.StateDir, paths: paths, DefaultProject: settings.DefaultProject,
		DefaultProfile: settings.DefaultProfile, MaxConcurrency: settings.MaxConcurrency,
		RuntimeVersion: Version, configErr: err,
	}
}

func (s *Service) DaemonConfig() daemon.Config {
	version := os.Getenv("AGENT_RUNTIME_VERSION")
	if version == "" {
		version = s.RuntimeVersion
	}
	executable := ""
	if info, err := os.Stat(s.paths.Binary); err == nil && !info.IsDir() {
		executable = s.paths.Binary
	}
	return daemon.Config{
		Home: s.Home, Dir: s.paths.DaemonDir, LogFile: s.paths.DaemonLog,
		Executable: executable, Version: version,
	}
}

func (s *Service) DaemonClient() *daemon.Client {
	return daemon.NewClient(s.DaemonConfig())
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
	selectedProvider, err := provider.Select(profile)
	if err != nil {
		return RunSummary{}, err
	}
	if _, err := selectedProvider.Prepare(ctx, profile, provider.Request{Overrides: overrides, RawCLIArgs: options.RawCLIArgs}); err != nil {
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
		PromptFile: promptFile, RawCLIArgs: append([]string(nil), options.RawCLIArgs...), DeadlineSeconds: options.DeadlineSeconds,
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
	extraEnv := map[string]string{
		"AGENTRUN_DONE_FILE":    paths.DoneFile,
		"AGENTRUN_OUTPUT_LOG":   paths.OutputLog,
		"AGENTRUN_RESULT_FILE":  paths.ResultFile,
		"AGENTRUN_REQUEST_FILE": paths.RequestFile,
		"AGENTRUN_RUN_DIR":      paths.RunDir,
		"AGENTRUN_RUN_ID":       request.RunID,
	}
	providerRequest := provider.Request{
		Prompt:       providerPrompt,
		RawCLIArgs:   append([]string(nil), options.RawCLIArgs...),
		Overrides:    overrides,
		CWD:          cwd,
		Environment:  extraEnv,
		HTTPClient:   s.HTTPClient,
		Daemon:       s.DaemonClient(),
		Profiles:     profiles,
		RunID:        request.RunID,
		RequestFile:  paths.RequestFile,
		ResultFile:   paths.ResultFile,
		DoneFile:     paths.DoneFile,
		OutputLog:    paths.OutputLog,
		SnapshotFile: filepath.Join(paths.RunDir, "native-snapshot.json"),
		PersonaDir:   s.PersonaDir,
	}
	prepared, err := selectedProvider.Prepare(runCtx, profile, providerRequest)
	if err != nil {
		return s.fail(paths, request, providerStatus, "provider_error", err)
	}
	if prepared.CLI != nil {
		providerStatus["argv"] = prepared.CLI.Argv
		providerStatus["driver"] = prepared.CLI.Driver
		providerStatus["effective_options"] = prepared.CLI.EffectiveOptions
	}
	if prepared.API != nil {
		providerStatus["protocol"] = prepared.API.Protocol
		providerStatus["effective_options"] = prepared.API.EffectiveOptions
	}
	if prepared.Native != nil {
		providerStatus["kind"] = provider.TypeNative
		providerStatus["effective_options"] = prepared.Native.EffectiveOptions
		providerStatus["snapshot_file"] = providerRequest.SnapshotFile
	}
	sink := &runProviderSink{service: s, paths: paths, request: request, status: providerStatus}
	providerResult, err := selectedProvider.Execute(runCtx, prepared, sink)
	isTmux := selectedProvider.Kind() == provider.ExecutorTmux
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
	if providerResult.State == "cancelled" {
		return s.cancelled(paths, request, providerStatus, context.Canceled)
	}
	if providerResult.State == "blocked" {
		return s.blocked(paths, request, providerStatus, providerResult.BlockedReason)
	}
	providerStatus["returncode"] = providerResult.ExitCode
	for key, value := range providerResult.Detail {
		providerStatus[key] = value
	}
	_ = s.store.Event(paths, request, "provider.exited", map[string]any{"returncode": providerResult.ExitCode, "execution_mode": mode})

	_, _ = s.store.WriteStatus(paths, request, StateResultPending, "", "validating result", providerStatus)
	_ = s.store.Event(paths, request, "status.changed", map[string]any{"state": StateResultPending})
	_, validationReason := s.store.ValidateResult(paths, request.RunID, request.ResultSchema)
	if validationReason == "" {
		summary, doneErr := s.markDone(paths, request, providerStatus)
		summary.FinalText = strings.TrimSpace(providerResult.FinalText)
		return summary, doneErr
	}
	if validationReason == "schema_invalid" {
		return s.fail(paths, request, providerStatus, validationReason, fmt.Errorf("result validation failed: %s", validationReason))
	}
	if !isTmux && (prepared.API != nil || (providerResult.ExitCode == 0 && (mode == ModeCapture || profile.ResultContract() != "required"))) {
		text := strings.TrimSpace(providerResult.FinalText)
		if text == "" {
			text = captureSummary(providerResult.Stdout)
		}
		artifacts := []map[string]any{{"type": "log", "path": paths.OutputLog}}
		if prepared.Native != nil {
			artifacts = append(artifacts, map[string]any{"type": "native_snapshot", "path": filepath.Join(paths.RunDir, "native-snapshot.json")})
		}
		result := Result{SchemaVersion: 1, RunID: runID, Outcome: OutcomeSucceeded, Summary: text,
			Artifacts: artifacts, Errors: []map[string]any{}, Validation: Validation{Commands: []string{}, Passed: true}}
		if err := s.store.WriteResult(paths, result); err != nil {
			return RunSummary{}, err
		}
		_ = s.store.Event(paths, request, "result.synthesized", map[string]any{"execution_mode": mode, "summary_chars": len(text)})
		summary, doneErr := s.markDone(paths, request, providerStatus)
		summary.FinalText = text
		return summary, doneErr
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
		session, _ := status.ProviderStatus["tmux_session"].(string)
		_ = s.DaemonClient().KillTmux(context.Background(), runID, session)
	} else if request.Provider == provider.TypeNative {
		detail, controlErr := provider.ControlNative(filepath.Join(paths.RunDir, "native-snapshot.json"), "cancel", "cancelled by user")
		if controlErr != nil {
			return RunSummary{}, controlErr
		}
		for key, value := range detail {
			status.ProviderStatus[key] = value
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

func (s *Service) blocked(paths Paths, request Request, providerStatus map[string]any, reason string) (RunSummary, error) {
	if strings.TrimSpace(reason) == "" {
		reason = "native provider is waiting for human input"
	}
	status, err := s.store.WriteStatus(paths, request, StateBlocked, reason, reason, providerStatus)
	s.updateRegistry(paths, status)
	_ = s.store.Event(paths, request, "status.changed", map[string]any{"state": StateBlocked, "failure_reason": reason})
	return summary(paths, status, false), err
}

func (s *Service) resetRun(paths Paths) {
	if status, err := s.store.ReadStatus(paths); err == nil {
		if session, _ := status.ProviderStatus["tmux_session"].(string); session != "" {
			_ = s.DaemonClient().KillTmux(context.Background(), "", session)
		}
	}
	for _, path := range []string{paths.RequestFile, paths.StatusFile, paths.EventsFile, paths.OutputLog, paths.ResultFile, paths.DoneFile, filepath.Join(paths.RunDir, "native-snapshot.json"), filepath.Join(paths.RunDir, "native-snapshot.json.control")} {
		_ = os.Remove(path)
	}
}

func managedPrompt(prompt string, request Request, paths Paths) string {
	example, _ := json.MarshalIndent(Result{
		SchemaVersion: 1,
		RunID:         request.RunID,
		Outcome:       OutcomeSucceeded,
		Summary:       "任务结果摘要",
		Artifacts:     []map[string]any{},
		Errors:        []map[string]any{},
		Validation:    Validation{Commands: []string{}, Passed: true},
	}, "", "  ")
	return fmt.Sprintf(`%s

## AgentRun result contract

本次任务完成信号不是 stdout/stderr。最终必须写入环境变量 AGENTRUN_RESULT_FILE 指向的 JSON 文件：

%s

严格按下面的类型和枚举写入，不能把数字或布尔值写成字符串：

%s

- schema_version 必须是数字 1。
- run_id 必须是字符串 %s。
- outcome 只能是 succeeded、failed、blocked、partial、cancelled 之一。
- artifacts 和 errors 必须是 object 数组；没有内容时写 []。
- validation 必须包含 commands 字符串数组和 passed 布尔值。

只向该文件写入一个 JSON object，不要包含 Markdown code fence 或额外文本。写入后重新读取并确认 JSON 可解析。终端输出只作为过程日志，不能替代 result.json。
`, strings.TrimRight(prompt, "\n"), paths.ResultFile, string(example), request.RunID)
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
