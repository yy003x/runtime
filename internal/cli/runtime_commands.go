package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"syscall"
	"time"

	"agent-runtime/internal/agentrun"
	"agent-runtime/internal/capability"
	"agent-runtime/internal/cli/config"
	"agent-runtime/internal/daemon"
	"agent-runtime/internal/provider"
)

func runRuntimeDoctor(cfg *config.Config, args []string) error {
	service := agentrun.New(cfg.Home)
	if len(args) > 0 && args[0] == "daemon" {
		status, notice, err := service.DaemonClient().EnsureRunning(context.Background())
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "daemon": status, "notice": notice})
	}
	profiles, err := service.Profiles()
	if err != nil {
		return err
	}
	items := make(map[string]any, len(profiles))
	ok := true
	for id, profile := range profiles {
		validation := validateProfile(profile, profiles, false)
		items[id] = validation
		if valid, _ := validation["ok"].(bool); !valid {
			ok = false
		}
	}
	return printJSON(map[string]any{
		"ok": ok, "version": service.RuntimeVersion, "runs_dir": service.RunsDir,
		"default_profile": service.DefaultProfile, "profiles": len(profiles), "providers": items,
	})
}

func runDaemonCommand(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: daemon start|status|stop|restart")
	}
	service := agentrun.New(cfg.Home)
	client := service.DaemonClient()
	switch args[0] {
	case "serve":
		ctx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		return daemon.NewServer(service.DaemonConfig()).Run(ctx)
	case "start":
		status, notice, err := client.EnsureRunning(context.Background())
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "daemon": status, "notice": notice})
	case "status":
		status, err := client.Status(context.Background())
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "daemon": status})
	case "stop":
		if err := client.Stop(context.Background(), true, 5*time.Second); err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "stopped": true})
	case "restart":
		if current, err := client.Status(context.Background()); err == nil {
			if err := ensureDaemonRestartSafe(current); err != nil {
				return err
			}
			if err := client.Stop(context.Background(), false, 5*time.Second); err != nil {
				return err
			}
		}
		status, notice, err := client.EnsureRunning(context.Background())
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "daemon": status, "notice": notice})
	default:
		return fmt.Errorf("unknown daemon command: %s", args[0])
	}
}

func ensureDaemonRestartSafe(status *daemon.Status) error {
	if status == nil {
		return nil
	}
	if len(status.Processes) > 0 || len(status.Dependencies) > 0 {
		return fmt.Errorf("daemon has %d active process(es) and %d dependency process(es); stop or cancel them before restart", len(status.Processes), len(status.Dependencies))
	}
	return nil
}

func runConfigCommand(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: config choices|validate|command")
	}
	service := agentrun.New(cfg.Home)
	profiles, err := service.Profiles()
	if err != nil {
		return err
	}
	switch args[0] {
	case "choices":
		includeAll := containsArg(args[1:], "--all")
		var choices []map[string]any
		for _, profile := range profiles {
			validation := validateProfile(profile, profiles, false)
			valid, _ := validation["ok"].(bool)
			if !includeAll && !valid {
				continue
			}
			choices = append(choices, map[string]any{
				"id": profile.ID, "label": profile.Label, "provider_type": profile.Type,
				"transport": profile.Transport(), "validated": valid, "validation": validation,
			})
		}
		return printJSON(map[string]any{"ok": true, "only_valid": !includeAll, "choices": choices})
	case "validate":
		name := optionValue(args[1:], "--name")
		if containsArg(args[1:], "--profile") {
			return fmt.Errorf("unknown config validate option: --profile; use -c/--config")
		}
		profileID := optionValue(args[1:], "-c")
		if profileID == "" {
			profileID = optionValue(args[1:], "--config")
		}
		providerType := optionValue(args[1:], "--provider")
		live := containsArg(args[1:], "--live")
		var results []map[string]any
		allOK := true
		for _, profile := range profiles {
			if name != "" && !profileHasName(profile, name) {
				continue
			}
			if profileID != "" && profile.ID != profileID {
				continue
			}
			if providerType != "" && profile.Transport() != providerType && profile.Type != providerType {
				continue
			}
			result := validateProfile(profile, profiles, live)
			results = append(results, result)
			if valid, _ := result["ok"].(bool); !valid {
				allOK = false
			}
		}
		if len(results) == 0 {
			allOK = false
		}
		return printJSON(map[string]any{"ok": allOK, "validated": len(results), "results": results})
	case "command":
		return printConfigCommand(profiles, args[1:])
	default:
		return fmt.Errorf("unknown config command: %s", args[0])
	}
}

func printConfigCommand(profiles map[string]provider.Config, args []string) error {
	profileID := ""
	jsonOutput := false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "-c", "--config":
			if index+1 >= len(args) {
				return fmt.Errorf("config command %s requires a value", args[index])
			}
			index++
			profileID = args[index]
		case "--json":
			jsonOutput = true
		case "--profile":
			return fmt.Errorf("unknown config command option: --profile; use -c/--config")
		default:
			return fmt.Errorf("unknown config command option: %s", args[index])
		}
	}
	if strings.TrimSpace(profileID) == "" {
		return fmt.Errorf("config command requires -c/--config")
	}
	profile, ok := provider.Resolve(profiles, profileID)
	if !ok {
		return fmt.Errorf("unknown config %q", profileID)
	}
	if profile.Type != provider.TypeCLI || profile.CLI == nil {
		return fmt.Errorf("config %q is not a CLI profile", profile.ID)
	}
	prepared, err := provider.Prepare(profile, "", nil)
	if err != nil {
		return err
	}
	if prepared.CLI == nil || len(prepared.CLI.Argv) == 0 {
		return fmt.Errorf("config %q did not resolve a CLI command", profile.ID)
	}
	argv := redactCommandArgv(prepared.CLI.Argv)
	command := shellJoin(argv)
	if !jsonOutput {
		fmt.Println(command)
		return nil
	}
	return printJSON(map[string]any{
		"ok": true, "profile": profile.ID, "provider_type": profile.Type,
		"transport": profile.Transport(), "argv": argv, "command": command,
	})
}

func redactCommandArgv(argv []string) []string {
	redacted := append([]string(nil), argv...)
	secretValues := sensitiveEnvironmentValues()
	redactNext := false
	for index, value := range redacted {
		if redactNext {
			redacted[index] = "[REDACTED]"
			redactNext = false
			continue
		}
		name, optionValue, hasValue := strings.Cut(value, "=")
		if (strings.HasPrefix(value, "-") || hasValue) && sensitiveCommandName(name) {
			if hasValue {
				redacted[index] = name + "=[REDACTED]"
			} else {
				redactNext = true
			}
			continue
		}
		lower := strings.ToLower(value)
		if strings.Contains(lower, "authorization:") || strings.Contains(lower, "proxy-authorization:") {
			redacted[index] = "[REDACTED]"
			continue
		}
		if hasValue {
			redacted[index] = name + "=" + redactURLSecrets(optionValue)
		} else {
			redacted[index] = redactURLSecrets(redacted[index])
		}
		for _, secret := range secretValues {
			redacted[index] = strings.ReplaceAll(redacted[index], secret, "[REDACTED]")
		}
	}
	return redacted
}

func sensitiveCommandName(value string) bool {
	name := strings.TrimLeft(strings.ToLower(strings.TrimSpace(value)), "-")
	name = strings.NewReplacer("-", "_", ".", "_").Replace(name)
	for _, token := range []string{"api_key", "access_key", "private_key", "secret", "token", "password", "authorization", "credential", "cookie"} {
		if name == token || strings.HasSuffix(name, "_"+token) {
			return true
		}
	}
	return false
}

func sensitiveEnvironmentValues() []string {
	values := make([]string, 0)
	for _, entry := range os.Environ() {
		name, value, ok := strings.Cut(entry, "=")
		if ok && len(value) >= 4 && sensitiveCommandName(name) {
			values = append(values, value)
		}
	}
	return values
}

func redactURLSecrets(value string) string {
	parsed, err := url.Parse(value)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return value
	}
	hadUser := parsed.User != nil
	if hadUser {
		parsed.User = url.User("[REDACTED]")
	}
	query := parsed.Query()
	changed := hadUser
	for name := range query {
		if sensitiveCommandName(name) || oneOfString(strings.ToLower(name), "access", "key", "api-key", "apikey", "auth", "signature", "sig") {
			query.Set(name, "[REDACTED]")
			changed = true
		}
	}
	if changed {
		parsed.RawQuery = query.Encode()
		return parsed.String()
	}
	return value
}

func oneOfString(value string, allowed ...string) bool {
	for _, item := range allowed {
		if value == item {
			return true
		}
	}
	return false
}

func shellJoin(argv []string) string {
	quoted := make([]string, len(argv))
	for index, value := range argv {
		quoted[index] = shellQuote(value)
	}
	return strings.Join(quoted, " ")
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	if strings.IndexFunc(value, func(char rune) bool {
		return !(char >= 'a' && char <= 'z' || char >= 'A' && char <= 'Z' || char >= '0' && char <= '9' || strings.ContainsRune("_@%+=:,./-", char))
	}) == -1 {
		return value
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func profileHasName(profile provider.Config, name string) bool {
	if profile.ID == name {
		return true
	}
	for _, alias := range profile.Aliases {
		if alias == name {
			return true
		}
	}
	return false
}

func validateProfile(profile provider.Config, profiles map[string]provider.Config, live bool) map[string]any {
	result := map[string]any{"profile": profile.ID, "provider_type": profile.Type, "transport": profile.Transport(), "ok": false}
	if message := validateExecutionEnvironment(profile); message != "" {
		result["message"] = message
		return result
	}
	if profile.Type == provider.TypeNative {
		if profile.Native.Mock != nil {
			result["ok"], result["message"] = true, "native mock profile 可用"
			return result
		}
		modelProfile, exists := provider.Resolve(profiles, profile.Native.ModelProfile)
		if !exists {
			result["message"] = "native model_profile 不存在: " + profile.Native.ModelProfile
			return result
		}
		if modelProfile.Type != provider.TypeAPI || modelProfile.API == nil {
			result["message"] = "native model_profile 必须引用 API profile: " + modelProfile.ID
			return result
		}
		result["model_profile"] = modelProfile.ID
		if os.Getenv(modelProfile.API.APIKeyEnv) == "" {
			result["message"] = "native model_profile 凭据缺失: " + modelProfile.API.APIKeyEnv
			return result
		}
		result["ok"], result["message"] = true, "native profile 与 model_profile 凭据有效；doctor 不发起付费网络请求"
		return result
	}
	if profile.Type == provider.TypeAPI {
		if message := validateAPIRuntimeEnvironment(profile, result); message != "" {
			result["message"] = message
			return result
		}
		if profile.API.Mock {
			result["ok"], result["message"] = true, "mock API profile 可用"
			return result
		}
		if os.Getenv(profile.API.APIKeyEnv) == "" {
			result["message"] = "环境变量未设置: " + profile.API.APIKeyEnv
			return result
		}
		if live {
			result["message"] = "配置有效；真实 API 验证请执行 profile smoke，避免 doctor 产生费用"
		} else {
			result["message"] = "API key 环境变量已设置"
		}
		result["ok"] = true
		return result
	}
	if profile.Transport() == provider.ExecutorTmux {
		if _, err := exec.LookPath("tmux"); err != nil {
			result["message"] = "tmux 不可用"
			return result
		}
	}
	addCLIEnvironmentDiagnostics(profile, result)
	binary := profile.CLI.Command.Binary
	if _, err := exec.LookPath(binary); err != nil {
		if info, statErr := os.Stat(binary); statErr != nil || info.IsDir() {
			result["message"] = "命令不可用: " + binary
			return result
		}
	}
	result["ok"], result["message"] = true, "命令可用"
	return result
}

func validateAPIRuntimeEnvironment(profile provider.Config, result map[string]any) string {
	if profile.API == nil || profile.API.Runtime == nil || !profile.API.Runtime.Enabled {
		return ""
	}
	runtime := profile.API.Runtime
	result["agent_runtime"] = true
	result["context"] = "local snapshot"
	result["memory_enabled"] = runtime.Memory != nil && runtime.Memory.Enabled
	result["skills"] = append([]string{}, runtime.Skills...)
	result["auto_route_skills"] = runtime.AutoRouteSkills
	servers := make([]string, 0, len(runtime.MCPServers))
	for _, server := range runtime.MCPServers {
		servers = append(servers, server.Name)
		command := provider.ExpandEnv(server.Command)
		if _, err := exec.LookPath(command); err != nil {
			if info, statErr := os.Stat(command); statErr != nil || info.IsDir() {
				return fmt.Sprintf("MCP server %s 命令不可用: %s", server.Name, command)
			}
		}
	}
	result["mcp_servers"] = servers
	return ""
}

func addCLIEnvironmentDiagnostics(profile provider.Config, result map[string]any) {
	if profile.CLI == nil {
		return
	}
	environment := make(map[string]string)
	for _, item := range provider.CommandEnvironment(profile.CLI.Command, nil) {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			environment[key] = value
		}
	}
	details := make(map[string]any)
	switch profile.CLI.Driver {
	case "codex":
		home := environment["CODEX_HOME"]
		if home == "" {
			home = filepath.Join(userHomeDir(), ".codex")
		}
		details["CODEX_HOME"] = home
	case "claude":
		home := environment["CLAUDE_CONFIG_DIR"]
		if home == "" {
			home = filepath.Join(userHomeDir(), ".claude")
		}
		details["CLAUDE_CONFIG_DIR"] = home
		var activeAuth []string
		for _, name := range []string{"ANTHROPIC_API_KEY", "ANTHROPIC_AUTH_TOKEN"} {
			if environment[name] != "" {
				activeAuth = append(activeAuth, name)
			}
		}
		details["active_auth_env"] = activeAuth
		if len(activeAuth) > 1 {
			result["warnings"] = []string{"ANTHROPIC_API_KEY 与 ANTHROPIC_AUTH_TOKEN 同时生效；请在 profile/preset 的 env_unset 中删除不使用的一项"}
		}
	}
	if len(details) > 0 {
		result["environment"] = details
	}
}

func userHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~"
	}
	return home
}

func validateExecutionEnvironment(profile provider.Config) string {
	for _, name := range profile.Execution.UpstreamProxyEnv {
		if strings.TrimSpace(os.Getenv(name)) == "" {
			return "上游代理环境变量未设置: " + name
		}
	}
	if profile.Execution.Dylib == "" {
		return ""
	}
	path := provider.ExpandEnv(profile.Execution.Dylib)
	if path == "" {
		return "dylib 路径环境变量未设置: " + profile.Execution.Dylib
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "dylib 不可用: " + path
	}
	return ""
}

func runTaskCommand(cfg *config.Config, command string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s run|status|logs|result|watch|block|continue|patch-resume|stop|cancel", command)
	}
	service := agentrun.New(cfg.Home)
	runType := agentrun.RunTask
	if command == "turn" {
		runType = agentrun.RunTurn
	}
	switch args[0] {
	case "run":
		options, err := parseRunOptions(runType, args[1:])
		if err != nil {
			return err
		}
		if err := applyStdinPrompt(&options.Prompt, options.PromptFile); err != nil {
			return err
		}
		if runType == agentrun.RunTurn && options.SessionID == "" {
			return fmt.Errorf("turn run requires --session-id")
		}
		options.Caller = "cli." + command
		result, runErr := service.Run(context.Background(), options)
		_ = printJSON(result)
		return runErr
	case "status":
		runID, err := parseRequiredID(args[1:], "--run-id", nil, nil)
		if err != nil {
			return err
		}
		status, err := service.Status(runType, runID)
		if err != nil {
			return err
		}
		return printJSON(status)
	case "logs":
		runID, err := parseRequiredID(args[1:], "--run-id", map[string]bool{"--tail": true}, nil)
		if err != nil {
			return err
		}
		logs, err := service.Logs(runType, runID, intOption(args[1:], "--tail", 120))
		if err != nil {
			return err
		}
		return printJSON(logs)
	case "result":
		runID, err := parseRequiredID(args[1:], "--run-id", nil, nil)
		if err != nil {
			return err
		}
		result, err := service.ReadResult(runType, runID)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "watch":
		runID, err := parseRequiredID(args[1:], "--run-id", map[string]bool{"--seconds": true, "--poll-seconds": true, "--tail": true}, nil)
		if err != nil {
			return err
		}
		return watchRun(service, runType, runID, args[1:])
	case "cancel":
		runID, err := parseRequiredID(args[1:], "--run-id", nil, nil)
		if err != nil {
			return err
		}
		result, err := service.Cancel(runType, runID)
		_ = printJSON(result)
		return err
	case "block", "stop":
		runID, err := parseRequiredID(args[1:], "--run-id", map[string]bool{"--reason": true}, nil)
		if err != nil {
			return err
		}
		result, err := service.ControlNative(runType, runID, args[0], optionValue(args[1:], "--reason"))
		_ = printJSON(result)
		return err
	case "continue":
		runID, err := parseRequiredID(args[1:], "--run-id", nil, nil)
		if err != nil {
			return err
		}
		result, err := service.ResumeNative(context.Background(), runType, runID, nil)
		_ = printJSON(result)
		return err
	case "patch-resume":
		runID, err := parseRequiredID(args[1:], "--run-id", map[string]bool{"--patch": true}, nil)
		if err != nil {
			return err
		}
		var patch provider.NativePatch
		value := optionValue(args[1:], "--patch")
		if value == "" {
			return fmt.Errorf("--patch is required")
		}
		if err := json.Unmarshal([]byte(value), &patch); err != nil {
			return fmt.Errorf("decode native patch: %w", err)
		}
		result, err := service.ResumeNative(context.Background(), runType, runID, &patch)
		_ = printJSON(result)
		return err
	default:
		return fmt.Errorf("unknown %s command: %s", command, args[0])
	}
}

func parseRunOptions(runType string, args []string) (agentrun.RunOptions, error) {
	options := agentrun.RunOptions{RunType: runType, ExecutionMode: agentrun.ModeManaged, ProviderOverrides: map[string]any{}}
	var promptParts []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c", "--config":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			options.Profile = args[i]
		case "--project":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--project requires value")
			}
			options.ProjectID = args[i]
		case "--run-id":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--run-id requires value")
			}
			options.RunID = args[i]
		case "--session-id":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--session-id requires value")
			}
			options.SessionID = args[i]
		case "--record-mode":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--record-mode requires value")
			}
			options.RecordMode = args[i]
		case "--retention":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--retention requires value")
			}
			options.Retention = args[i]
		case "--cwd":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--cwd requires value")
			}
			options.CWD = args[i]
		case "--prompt-file":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			options.PromptFile = args[i]
		case "--mode":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--mode requires value")
			}
			options.ExecutionMode = args[i]
		case "--deadline-seconds":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			value, err := strconv.Atoi(args[i])
			if err != nil {
				return options, err
			}
			options.DeadlineSeconds = value
		case "--result-schema":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--result-schema requires value")
			}
			options.ResultSchema = args[i]
		case "--model":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--model requires value")
			}
			options.ProviderOverrides["model"] = args[i]
		case "--reasoning-effort":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			options.ProviderOverrides["reasoning_effort"] = args[i]
		case "--image":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--image requires value")
			}
			images, _ := options.ProviderOverrides["images"].([]string)
			options.ProviderOverrides["images"] = append(images, args[i])
		case "--provider-overrides":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--provider-overrides requires value")
			}
			var overrides map[string]any
			if err := json.Unmarshal([]byte(args[i]), &overrides); err != nil {
				return options, err
			}
			for key, value := range overrides {
				options.ProviderOverrides[key] = value
			}
		case "--allowed-action":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--allowed-action requires value")
			}
			options.AllowedActions = append(options.AllowedActions, args[i])
		case "--forbidden-action":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--forbidden-action requires value")
			}
			options.ForbiddenActions = append(options.ForbiddenActions, args[i])
		case "--force":
			options.Force = true
		case "--json":
		case "--":
			options.RawCLIArgs = append(options.RawCLIArgs, args[i+1:]...)
			i = len(args)
		default:
			if strings.HasPrefix(args[i], "-") {
				return options, fmt.Errorf("unknown %s run option: %s", runType, args[i])
			}
			promptParts = append(promptParts, args[i])
		}
	}
	options.Prompt = strings.TrimSpace(strings.Join(promptParts, " "))
	if options.Profile == "" {
		return options, fmt.Errorf("%s run requires -c/--config", runType)
	}
	if options.Prompt != "" && strings.TrimSpace(options.PromptFile) != "" {
		return options, fmt.Errorf("positional prompt and --prompt-file are mutually exclusive")
	}
	return options, nil
}

func watchRun(service *agentrun.Service, runType, runID string, args []string) error {
	seconds := intOption(args, "--seconds", 0)
	poll := floatOption(args, "--poll-seconds", 1)
	deadline := time.Time{}
	if seconds > 0 {
		deadline = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	for {
		status, err := service.Status(runType, runID)
		if err != nil {
			return err
		}
		if terminalState(status.State) || (!deadline.IsZero() && time.Now().After(deadline)) {
			logs, _ := service.Logs(runType, runID, intOption(args, "--tail", 120))
			return printJSON(map[string]any{"status": status, "logs": logs})
		}
		time.Sleep(time.Duration(poll * float64(time.Second)))
	}
}

func runRuntimeSession(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: session run|exec|start|list|status|logs|watch|send|interrupt|stop|attach")
	}
	service := agentrun.New(cfg.Home)
	ctx := context.Background()
	switch args[0] {
	case "history":
		return runSessionHistory(cfg, args[1:])
	case "run":
		options, err := parseRunOptions(agentrun.RunTask, args[1:])
		if err != nil {
			return err
		}
		if err := applyStdinPrompt(&options.Prompt, options.PromptFile); err != nil {
			return err
		}
		options.Caller = "cli.session"
		options.CreateSession = true
		if options.RecordMode == agentrun.RecordOff {
			return fmt.Errorf("session run does not allow --record-mode off; use task run for a Run without Session")
		}
		if options.Retention == "" {
			options.Retention = agentrun.RetentionEphemeral
		}
		result, runErr := service.Run(ctx, options)
		_ = printJSON(result)
		return runErr
	case "exec":
		return runDirectSession(cfg, service, args[1:])
	case "start":
		options, err := parseSessionStartOptions(args[1:])
		if err != nil {
			return err
		}
		if err := applyStdinPrompt(&options.Prompt, options.PromptFile); err != nil {
			return err
		}
		result, err := service.StartSessionWithOptions(ctx, options)
		_ = printJSON(result)
		return err
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("session list does not accept arguments")
		}
		result, err := service.SessionList(ctx)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"sessions": result})
	case "status":
		runID, err := parseRequiredID(args[1:], "--run-id", nil, nil)
		if err != nil {
			return err
		}
		result, err := service.SessionStatus(ctx, runID)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "logs":
		runID, err := parseRequiredID(args[1:], "--run-id", map[string]bool{"--tail": true}, nil)
		if err != nil {
			return err
		}
		result, err := service.SessionLogs(ctx, runID, intOption(args[1:], "--tail", 120))
		if err != nil {
			return err
		}
		return printJSON(result)
	case "watch":
		runID, err := parseRequiredID(args[1:], "--run-id", map[string]bool{"--seconds": true, "--poll-seconds": true, "--tail": true}, nil)
		if err != nil {
			return err
		}
		return watchSession(service, runID, args[1:])
	case "send":
		runID, textValue, promptFile, submit, err := parseSessionSendOptions(args[1:])
		if err != nil {
			return err
		}
		if err := applyStdinPrompt(&textValue, promptFile); err != nil {
			return err
		}
		if promptFile != "" {
			resolved, _, resolveErr := resolvePromptForCLI(promptFile)
			if resolveErr != nil {
				return resolveErr
			}
			textValue = resolved
		}
		if strings.TrimSpace(textValue) == "" {
			return fmt.Errorf("session send prompt is required")
		}
		result, err := service.SessionSend(ctx, runID, textValue, submit)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "interrupt":
		runID, err := parseRequiredID(args[1:], "--run-id", nil, nil)
		if err != nil {
			return err
		}
		result, err := service.SessionInterrupt(ctx, runID)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "stop":
		runID, err := parseRequiredID(args[1:], "--run-id", nil, nil)
		if err != nil {
			return err
		}
		result, err := service.SessionStop(ctx, runID)
		_ = printJSON(result)
		return err
	case "attach":
		runID, err := parseRequiredID(args[1:], "--run-id", nil, nil)
		if err != nil {
			return err
		}
		return service.SessionAttach(ctx, runID)
	default:
		return fmt.Errorf("unknown session command: %s", args[0])
	}
}

func runDirectSession(_ *config.Config, service *agentrun.Service, args []string) error {
	profileID, projectID, cwd, sessionID, recordMode, retention := "", "", "", "", "", ""
	var rawArgs []string
	for index := 0; index < len(args); index++ {
		if args[index] == "--" {
			rawArgs = append(rawArgs, args[index+1:]...)
			break
		}
		var target *string
		switch args[index] {
		case "-c", "--config":
			target = &profileID
		case "--project":
			target = &projectID
		case "--cwd":
			target = &cwd
		case "--session-id":
			target = &sessionID
		case "--record-mode":
			target = &recordMode
		case "--retention":
			target = &retention
		default:
			return fmt.Errorf("unknown session exec option: %s; native CLI args must follow --", args[index])
		}
		index++
		if index >= len(args) {
			return fmt.Errorf("%s requires value", args[index-1])
		}
		*target = args[index]
	}
	if profileID == "" {
		return fmt.Errorf("session exec requires -c/--config")
	}
	profiles, err := service.Profiles()
	if err != nil {
		return err
	}
	profile, ok := provider.Resolve(profiles, profileID)
	if !ok {
		return fmt.Errorf("unknown provider profile: %s", profileID)
	}
	prepared, err := provider.PrepareInteractiveCLI(profile, rawArgs)
	if err != nil {
		return err
	}
	if cwd == "" {
		cwd, err = os.Getwd()
	} else {
		cwd, err = filepath.Abs(cwd)
	}
	if err != nil {
		return err
	}
	if info, statErr := os.Stat(cwd); statErr != nil || !info.IsDir() {
		return fmt.Errorf("cwd 不存在或不是目录: %s", cwd)
	}
	decision, err := agentrun.DecideRecordPolicy("cli.session", agentrun.RunTask, agentrun.ExecutionCLIDirect, sessionID, recordMode, retention)
	if err != nil {
		return err
	}
	manager := agentrun.NewSessionManager(service)
	session, execution, err := manager.BeginDirectExecutionInSession(profile, sessionID, projectID, cwd, len(rawArgs), decision)
	if err != nil {
		return err
	}
	result, runErr := provider.ExecuteCLIInteractive(context.Background(), profile, prepared, cwd, service.DaemonClient())
	if completeErr := manager.CompleteDirectExecution(session.SessionID, execution, result.ExitCode, runErr); completeErr != nil && runErr == nil {
		runErr = completeErr
	}
	if runErr != nil {
		return runErr
	}
	if result.ExitCode != 0 {
		return fmt.Errorf("direct CLI exited with code %d", result.ExitCode)
	}
	return printJSON(map[string]any{"ok": true, "session_id": session.SessionID, "execution_id": execution.ExecutionID, "capture_quality": decision.CaptureQuality})
}

func runSessionHistory(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: history create|list|show|messages|events|configure|delete|export|import|rebuild")
	}
	service := agentrun.New(cfg.Home)
	store := agentrun.NewSessionManager(service).Store()
	switch args[0] {
	case "create":
		if err := validateValueOptions(args[1:], map[string]bool{
			"--session-id": true, "--project": true, "--cwd": true, "--title": true,
			"--runtime": true, "--profile": true, "--record-mode": true, "--retention": true, "--tag": true,
		}); err != nil {
			return err
		}
		tags := repeatOption(args[1:], "--tag")
		if err := agentrun.ValidateSessionTags(tags); err != nil {
			return err
		}
		service := agentrun.New(cfg.Home)
		manager := agentrun.NewSessionManager(service)
		runtimeName, profileName, err := validateSessionRuntimeProfile(service, optionValue(args[1:], "--runtime"), optionValue(args[1:], "--profile"))
		if err != nil {
			return err
		}
		decision, err := agentrun.DecideRecordPolicy("cli.history", agentrun.RunTurn, agentrun.ExecutionAPI,
			optionValue(args[1:], "--session-id"), optionValue(args[1:], "--record-mode"), optionValue(args[1:], "--retention"))
		if err != nil {
			return err
		}
		record, err := manager.EnsureSession(optionValue(args[1:], "--session-id"), optionValue(args[1:], "--project"),
			optionValue(args[1:], "--cwd"), optionValue(args[1:], "--title"), decision)
		if err != nil {
			return err
		}
		if runtimeName != "" || profileName != "" {
			record, err = store.ConfigureSession(record.SessionID, runtimeName, profileName)
			if err != nil {
				return err
			}
		}
		if len(tags) > 0 {
			record, err = store.SetTags(record.SessionID, tags)
			if err != nil {
				return err
			}
		}
		return printJSON(record)
	case "list":
		if err := validateValueOptions(args[1:], map[string]bool{
			"--project": true, "--state": true, "--retention": true, "--tag": true, "--limit": true,
		}); err != nil {
			return err
		}
		if _, err := service.SessionList(context.Background()); err != nil {
			return err
		}
		filter := agentrun.SessionFilter{
			ProjectID: optionValue(args[1:], "--project"), State: optionValue(args[1:], "--state"),
			Retention: optionValue(args[1:], "--retention"), Tags: repeatOption(args[1:], "--tag"),
			Limit: intOption(args[1:], "--limit", 100),
		}
		values, err := store.List(filter)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"sessions": values})
	case "show":
		sessionID, err := parseRequiredID(args[1:], "--session-id", nil, nil)
		if err != nil {
			return err
		}
		if err := service.ReconcileSession(context.Background(), sessionID); err != nil {
			return err
		}
		value, err := store.View(sessionID)
		if err != nil {
			return err
		}
		return printJSON(value)
	case "messages":
		sessionID, err := parseRequiredID(args[1:], "--session-id", map[string]bool{"--after-seq": true, "--limit": true}, nil)
		if err != nil {
			return err
		}
		values, err := store.Messages(sessionID, int64(intOption(args[1:], "--after-seq", 0)), intOption(args[1:], "--limit", 200))
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"session_id": sessionID, "messages": values})
	case "events":
		sessionID, err := parseRequiredID(args[1:], "--session-id", map[string]bool{"--after-seq": true, "--limit": true}, nil)
		if err != nil {
			return err
		}
		values, err := store.Events(sessionID, int64(intOption(args[1:], "--after-seq", 0)), intOption(args[1:], "--limit", 200))
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"session_id": sessionID, "events": values})
	case "export":
		sessionID, err := parseRequiredID(args[1:], "--session-id", map[string]bool{"--output": true}, nil)
		if err != nil {
			return err
		}
		output := optionValue(args[1:], "--output")
		if output == "" {
			return fmt.Errorf("history export requires --output")
		}
		if err := store.Export(sessionID, output); err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "session_id": sessionID, "output": output})
	case "configure":
		sessionID, err := parseRequiredID(args[1:], "--session-id", map[string]bool{
			"--runtime": true, "--profile": true, "--record-mode": true, "--retention": true,
		}, nil)
		if err != nil {
			return err
		}
		runtimeValue, profileValue := optionValue(args[1:], "--runtime"), optionValue(args[1:], "--profile")
		recordMode, retention := optionValue(args[1:], "--record-mode"), optionValue(args[1:], "--retention")
		if runtimeValue == "" && profileValue == "" && recordMode == "" && retention == "" {
			return fmt.Errorf("history configure requires runtime/profile or record-mode/retention")
		}
		runtimeName, profileName, err := validateSessionRuntimeProfile(service, runtimeValue, profileValue)
		if err != nil {
			return err
		}
		record, err := store.Get(sessionID)
		if err != nil {
			return err
		}
		if runtimeName != "" || profileName != "" {
			record, err = store.ConfigureSession(sessionID, runtimeName, profileName)
			if err != nil {
				return err
			}
		}
		if recordMode != "" || retention != "" {
			record, err = store.ConfigurePolicy(sessionID, recordMode, retention)
			if err != nil {
				return err
			}
		}
		return printJSON(record)
	case "delete":
		sessionID, err := parseRequiredID(args[1:], "--session-id", nil, nil)
		if err != nil {
			return err
		}
		trash, err := store.Trash(sessionID)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "session_id": sessionID, "trash_path": trash, "recoverable": true})
	case "import":
		if err := validateValueOptions(args[1:], map[string]bool{"--input": true}); err != nil {
			return err
		}
		inputPath := optionValue(args[1:], "--input")
		if inputPath == "" {
			return fmt.Errorf("history import requires --input")
		}
		data, err := os.ReadFile(inputPath)
		if err != nil {
			return err
		}
		var input agentrun.SessionImport
		if err := json.Unmarshal(data, &input); err != nil {
			return fmt.Errorf("decode session import: %w", err)
		}
		record, err := store.Import(input)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "session": record, "messages": len(input.Messages), "events": len(input.Events)})
	case "rebuild":
		if len(args) != 1 {
			return fmt.Errorf("history rebuild does not accept arguments")
		}
		index, err := store.RebuildIndex()
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "sessions": len(index.Sessions), "updated_at": index.UpdatedAt})
	default:
		return fmt.Errorf("unknown history command: %s", args[0])
	}
}

func validateSessionRuntimeProfile(service *agentrun.Service, runtimeName, profileName string) (string, string, error) {
	if runtimeName != "" && runtimeName != "api" && runtimeName != "cli" && runtimeName != "tmux" {
		return "", "", fmt.Errorf("runtime must be api|cli|tmux")
	}
	if profileName == "" {
		return runtimeName, profileName, nil
	}
	profiles, err := service.Profiles()
	if err != nil {
		return "", "", err
	}
	profile, ok := provider.Resolve(profiles, profileName)
	if !ok {
		return "", "", fmt.Errorf("unknown provider profile: %s", profileName)
	}
	expected := "api"
	if profile.Type == provider.TypeCLI {
		expected = "cli"
		if profile.Transport() == provider.ExecutorTmux {
			expected = "tmux"
		}
	}
	if runtimeName == "" {
		runtimeName = expected
	} else if runtimeName != expected {
		return "", "", fmt.Errorf("runtime does not match provider profile")
	}
	return runtimeName, profile.ID, nil
}

func parseSessionStartOptions(args []string) (agentrun.SessionOptions, error) {
	options := agentrun.SessionOptions{}
	var promptParts []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-c", "--config":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			options.Profile = args[i]
		case "--project":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--project requires value")
			}
			options.ProjectID = args[i]
		case "--cwd":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--cwd requires value")
			}
			options.CWD = args[i]
		case "--run-id":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--run-id requires value")
			}
			options.RunID = args[i]
		case "--prompt-file":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--prompt-file requires value")
			}
			options.PromptFile = args[i]
		case "--allowed-action":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--allowed-action requires value")
			}
			options.AllowedActions = append(options.AllowedActions, args[i])
		case "--forbidden-action":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--forbidden-action requires value")
			}
			options.ForbiddenActions = append(options.ForbiddenActions, args[i])
		case "--force":
			options.Force = true
		case "--record-mode":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--record-mode requires value")
			}
			options.RecordMode = args[i]
		case "--retention":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--retention requires value")
			}
			options.Retention = args[i]
		case "--":
			options.RawCLIArgs = append(options.RawCLIArgs, args[i+1:]...)
			i = len(args)
		default:
			if strings.HasPrefix(args[i], "-") {
				return options, fmt.Errorf("unknown session start option: %s", args[i])
			}
			promptParts = append(promptParts, args[i])
		}
	}
	if options.Profile == "" {
		return options, fmt.Errorf("session start requires -c/--config")
	}
	options.Prompt = strings.TrimSpace(strings.Join(promptParts, " "))
	if options.Prompt != "" && strings.TrimSpace(options.PromptFile) != "" {
		return options, fmt.Errorf("positional prompt and --prompt-file are mutually exclusive")
	}
	return options, nil
}

func parseRequiredID(args []string, idOption string, valueOptions, boolOptions map[string]bool) (string, error) {
	id := ""
	for i := 0; i < len(args); i++ {
		if args[i] == idOption {
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s requires value", idOption)
			}
			id = args[i]
			continue
		}
		if valueOptions[args[i]] {
			i++
			if i >= len(args) {
				return "", fmt.Errorf("%s requires value", args[i-1])
			}
			continue
		}
		if boolOptions[args[i]] {
			continue
		}
		return "", fmt.Errorf("unknown option: %s", args[i])
	}
	if id == "" {
		return "", fmt.Errorf("%s is required", idOption)
	}
	return id, nil
}

func parseSessionSendOptions(args []string) (runID, prompt, promptFile string, submit bool, err error) {
	submit = true
	var promptParts []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--run-id":
			i++
			if i >= len(args) {
				return "", "", "", false, fmt.Errorf("--run-id requires value")
			}
			runID = args[i]
		case "--prompt-file":
			i++
			if i >= len(args) {
				return "", "", "", false, fmt.Errorf("--prompt-file requires value")
			}
			promptFile = args[i]
		case "--no-submit":
			submit = false
		default:
			if strings.HasPrefix(args[i], "-") {
				return "", "", "", false, fmt.Errorf("unknown session send option: %s", args[i])
			}
			promptParts = append(promptParts, args[i])
		}
	}
	if runID == "" {
		return "", "", "", false, fmt.Errorf("--run-id is required")
	}
	prompt = strings.TrimSpace(strings.Join(promptParts, " "))
	if prompt != "" && strings.TrimSpace(promptFile) != "" {
		return "", "", "", false, fmt.Errorf("positional prompt and --prompt-file are mutually exclusive")
	}
	return runID, prompt, promptFile, submit, nil
}

func resolvePromptForCLI(path string) (string, string, error) {
	cwd, err := os.Getwd()
	if err != nil {
		return "", "", err
	}
	if !filepath.IsAbs(path) {
		path = filepath.Join(cwd, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", "", err
	}
	data, err := os.ReadFile(absolute)
	if err != nil {
		return "", "", fmt.Errorf("read prompt file: %w", err)
	}
	return string(data), absolute, nil
}

func runLoopCommand(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: loop run|start|step|status|logs|cancel")
	}
	service := agentrun.New(cfg.Home)
	switch args[0] {
	case "run", "start":
		options, err := parseLoopOptions(args[1:])
		if err != nil {
			return err
		}
		var status agentrun.PersistentLoopStatus
		if args[0] == "run" {
			status, err = service.LoopRun(context.Background(), options)
		} else {
			status, err = service.LoopStart(options)
		}
		_ = printJSON(status)
		return err
	case "step":
		loopID, err := parseRequiredID(args[1:], "--loop-id", nil, nil)
		if err != nil {
			return err
		}
		status, err := service.LoopStep(context.Background(), loopID)
		_ = printJSON(status)
		return err
	case "status":
		loopID, err := parseRequiredID(args[1:], "--loop-id", nil, nil)
		if err != nil {
			return err
		}
		status, err := service.LoopStatus(loopID)
		if err != nil {
			return err
		}
		return printJSON(status)
	case "logs":
		loopID, err := parseRequiredID(args[1:], "--loop-id", map[string]bool{"--tail": true}, nil)
		if err != nil {
			return err
		}
		logs, err := service.LoopLogs(loopID, intOption(args[1:], "--tail", 120))
		if err != nil {
			return err
		}
		return printJSON(logs)
	case "cancel":
		loopID, err := parseRequiredID(args[1:], "--loop-id", nil, nil)
		if err != nil {
			return err
		}
		status, err := service.LoopCancel(loopID)
		_ = printJSON(status)
		return err
	default:
		return fmt.Errorf("unknown loop command: %s", args[0])
	}
}

func watchSession(service *agentrun.Service, runID string, args []string) error {
	seconds := intOption(args, "--seconds", 0)
	poll := floatOption(args, "--poll-seconds", 1)
	deadline := time.Time{}
	if seconds > 0 {
		deadline = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	for {
		status, err := service.SessionStatus(context.Background(), runID)
		if err != nil {
			return err
		}
		if terminalState(status.State) || !status.Alive || (!deadline.IsZero() && time.Now().After(deadline)) {
			logs, _ := service.SessionLogs(context.Background(), runID, intOption(args, "--tail", 120))
			return printJSON(map[string]any{"status": status, "logs": logs})
		}
		time.Sleep(time.Duration(poll * float64(time.Second)))
	}
}

func runCommandCommand(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: command start|status|logs|watch|interrupt|stop|attach")
	}
	service := agentrun.New(cfg.Home)
	ctx := context.Background()
	switch args[0] {
	case "start":
		options, err := parseCommandOptions(args[1:])
		if err != nil {
			return err
		}
		result, err := service.StartCommand(ctx, options)
		_ = printJSON(result)
		return err
	case "status":
		runID, err := parseRequiredID(args[1:], "--run-id", nil, nil)
		if err != nil {
			return err
		}
		result, err := service.CommandStatus(ctx, runID)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "logs":
		runID, err := parseRequiredID(args[1:], "--run-id", map[string]bool{"--tail": true}, nil)
		if err != nil {
			return err
		}
		result, err := service.CommandLogs(ctx, runID, intOption(args[1:], "--tail", 120))
		if err != nil {
			return err
		}
		return printJSON(result)
	case "watch":
		runID, err := parseRequiredID(args[1:], "--run-id", map[string]bool{"--seconds": true, "--poll-seconds": true, "--tail": true}, nil)
		if err != nil {
			return err
		}
		return watchCommand(service, runID, args[1:])
	case "interrupt":
		runID, err := parseRequiredID(args[1:], "--run-id", nil, nil)
		if err != nil {
			return err
		}
		result, err := service.CommandInterrupt(ctx, runID)
		_ = printJSON(result)
		return err
	case "stop":
		runID, err := parseRequiredID(args[1:], "--run-id", nil, nil)
		if err != nil {
			return err
		}
		result, err := service.CommandStop(ctx, runID)
		_ = printJSON(result)
		return err
	case "attach":
		runID, err := parseRequiredID(args[1:], "--run-id", nil, nil)
		if err != nil {
			return err
		}
		return service.CommandAttach(ctx, runID)
	default:
		return fmt.Errorf("unknown command lifecycle action: %s", args[0])
	}
}

func parseCommandOptions(args []string) (agentrun.CommandOptions, error) {
	options := agentrun.CommandOptions{Label: "command"}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			options.Argv = append(options.Argv, args[i+1:]...)
			break
		}
		switch args[i] {
		case "-c", "--config":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			options.Profile = args[i]
		case "--project":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--project requires value")
			}
			options.ProjectID = args[i]
		case "--run-id":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--run-id requires value")
			}
			options.RunID = args[i]
		case "--cwd":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--cwd requires value")
			}
			options.CWD = args[i]
		case "--label":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--label requires value")
			}
			options.Label = args[i]
		case "--input":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--input requires value")
			}
			options.Input = args[i]
		case "--input-file":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--input-file requires value")
			}
			data, err := os.ReadFile(args[i])
			if err != nil {
				return options, err
			}
			options.Input = string(data)
		case "--deadline-seconds":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			value, err := strconv.Atoi(args[i])
			if err != nil {
				return options, err
			}
			options.DeadlineSeconds = value
		case "--force":
			options.Force = true
		default:
			return options, fmt.Errorf("unknown command start option: %s", args[i])
		}
	}
	if options.Profile == "" {
		return options, fmt.Errorf("command start requires -c/--config")
	}
	if len(options.Argv) == 0 {
		return options, fmt.Errorf("command argv is required; use -- <command> [args...]")
	}
	return options, nil
}

func watchCommand(service *agentrun.Service, runID string, args []string) error {
	seconds := intOption(args, "--seconds", 0)
	poll := floatOption(args, "--poll-seconds", 1)
	deadline := time.Time{}
	if seconds > 0 {
		deadline = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	for {
		status, err := service.CommandStatus(context.Background(), runID)
		if err != nil {
			return err
		}
		if terminalState(status.State) || !status.Alive || (!deadline.IsZero() && time.Now().After(deadline)) {
			logs, _ := service.CommandLogs(context.Background(), runID, intOption(args, "--tail", 120))
			return printJSON(map[string]any{"status": status, "logs": logs})
		}
		time.Sleep(time.Duration(poll * float64(time.Second)))
	}
}

func runCleanCommand(cfg *config.Config, args []string) error {
	for _, arg := range args {
		if arg != "--apply" && arg != "--json" {
			return fmt.Errorf("unknown clean argument: %s", arg)
		}
	}
	result, err := agentrun.New(cfg.Home).Prune(!containsArg(args, "--apply"))
	if err != nil {
		return err
	}
	return printJSON(result)
}

func parseLoopOptions(args []string) (agentrun.LoopStartOptions, error) {
	options := agentrun.LoopStartOptions{MaxSteps: 10}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--input":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--input requires value")
			}
			options.Input = args[i]
		case "--input-file":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--input-file requires value")
			}
			options.InputFile = args[i]
		case "--actions-file":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			data, err := os.ReadFile(args[i])
			if err != nil {
				return options, err
			}
			if err := json.Unmarshal(data, &options.Actions); err != nil {
				return options, err
			}
		case "--actions-json":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--actions-json requires value")
			}
			if err := json.Unmarshal([]byte(args[i]), &options.Actions); err != nil {
				return options, err
			}
		case "--planner-config":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--planner-config requires value")
			}
			options.PlannerProfile = args[i]
		case "--max-steps":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--max-steps requires value")
			}
			value, err := strconv.Atoi(args[i])
			if err != nil {
				return options, err
			}
			options.MaxSteps = value
		case "--result-schema":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--result-schema requires value")
			}
			options.ResultSchema = args[i]
		case "--deadline-seconds":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			value, err := strconv.Atoi(args[i])
			if err != nil {
				return options, err
			}
			options.DeadlineSeconds = value
		case "--capability":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--capability requires value")
			}
			options.Capabilities = append(options.Capabilities, args[i])
		case "--forbidden-action":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--forbidden-action requires value")
			}
			options.Forbidden = append(options.Forbidden, args[i])
		case "--session-id":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--session-id requires value")
			}
			options.SessionID = args[i]
		case "--project":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--project requires value")
			}
			options.ProjectID = args[i]
		case "--cwd":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--cwd requires value")
			}
			options.CWD = args[i]
		case "--loop-id":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--loop-id requires value")
			}
			options.LoopID = args[i]
		case "--force":
			options.Force = true
		default:
			if strings.HasPrefix(args[i], "-") {
				return options, fmt.Errorf("unknown loop option: %s", args[i])
			}
			if options.Input == "" {
				options.Input = args[i]
			} else {
				options.Input += " " + args[i]
			}
		}
	}
	return options, nil
}

func runCapabilitiesCommand(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: capabilities skills|tools|memory")
	}
	switch args[0] {
	case "tools":
		return runCapabilityTools(cfg, args[1:])
	case "skills":
		return runCapabilitySkills(cfg, args[1:])
	case "memory":
		return runCapabilityMemory(cfg, args[1:])
	default:
		return fmt.Errorf("unknown capability: %s", args[0])
	}
}

func runCapabilityTools(cfg *config.Config, args []string) error {
	manager := capability.NewToolManager()
	dir := optionValue(args, "--dir")
	if dir == "" {
		dir = optionValue(args, "--tools-dir")
	}
	if dir == "" {
		dir = cfg.Paths.ToolsDir
	}
	manager.RegisterDir(dir)
	command := "schemas"
	if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
		command = args[0]
	}
	switch command {
	case "schemas":
		return printJSON(map[string]any{"ok": true, "tools": manager.Schemas(), "doctor": manager.Doctor()})
	case "open-url":
		if len(args) < 2 {
			return fmt.Errorf("usage: tools open-url <http-or-https-url>")
		}
		return openURL(args[1])
	case "call", "describe-external":
		if len(args) < 2 {
			return fmt.Errorf("usage: capabilities tools %s <name>", command)
		}
		arguments := map[string]any{}
		if raw := optionValue(args[2:], "--args"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
				return err
			}
		}
		var output any
		var err error
		if command == "describe-external" {
			output, err = manager.DescribeExternal(args[1], arguments, repeatOption(args[2:], "--capability"), repeatOption(args[2:], "--forbidden-action"))
		} else {
			output, err = manager.Call(args[1], arguments, repeatOption(args[2:], "--capability"), repeatOption(args[2:], "--forbidden-action"))
		}
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "output": output})
	default:
		return fmt.Errorf("unknown tools command: %s", command)
	}
}

func openURL(rawURL string) error {
	parsed, err := url.Parse(rawURL)
	if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
		return fmt.Errorf("open-url only accepts http/https URLs")
	}
	candidates := []string{"/usr/bin/xdg-open", "/usr/local/bin/xdg-open"}
	if runtime.GOOS == "darwin" {
		candidates = []string{"/usr/bin/open"}
	}
	for _, candidate := range candidates {
		if info, statErr := os.Stat(candidate); statErr == nil && !info.IsDir() {
			return exec.Command(candidate, rawURL).Start()
		}
	}
	return fmt.Errorf("system browser launcher not found")
}

func runCapabilitySkills(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: capabilities skills list|route|run")
	}
	manager := capability.NewSkillManager()
	dir := optionValue(args[1:], "--dir")
	if dir == "" {
		dir = optionValue(args[1:], "--skills-dir")
	}
	if dir == "" {
		dir = cfg.Paths.SkillsDir
	}
	manager.RegisterDir(dir)
	switch args[0] {
	case "list":
		return printJSON(map[string]any{"skills": manager.List(), "doctor": manager.Doctor()})
	case "route":
		query := optionValue(args[1:], "--query")
		if query == "" {
			query = strings.Join(nonOptionArgs(args[1:]), " ")
		}
		if strings.TrimSpace(query) == "" {
			return fmt.Errorf("--query is required")
		}
		skill, ok := manager.Route(query)
		return printJSON(map[string]any{"ok": true, "query": query, "matched": nullableSkill(skill, ok), "doctor": manager.Doctor()})
	case "run", "run-auto":
		query := optionValue(args[1:], "--query")
		var skill capability.Skill
		var err error
		if args[0] == "run-auto" {
			var ok bool
			skill, ok = manager.Route(query)
			if !ok {
				return fmt.Errorf("未命中 skill: %s", query)
			}
		} else {
			if len(args) < 2 {
				return fmt.Errorf("skill name is required")
			}
			skill, err = manager.Get(args[1])
			if err != nil {
				return err
			}
		}
		input := optionValue(args[1:], "--input")
		if inputFile := optionValue(args[1:], "--input-file"); inputFile != "" {
			data, readErr := os.ReadFile(inputFile)
			if readErr != nil {
				return readErr
			}
			input = string(data)
		}
		variables := map[string]any{}
		if raw := optionValue(args[1:], "--vars"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &variables); err != nil {
				return fmt.Errorf("parse --vars: %w", err)
			}
		}
		prompt, err := skill.Render(input, query, variables)
		if err != nil {
			return err
		}
		if containsArg(args[1:], "--profile") {
			return fmt.Errorf("unknown skill run option: --profile; use -c/--config")
		}
		profile := optionValue(args[1:], "-c")
		if profile == "" {
			profile = optionValue(args[1:], "--config")
		}
		if profile == "" {
			profile = skill.DefaultProfile
		}
		service := agentrun.New(cfg.Home)
		run, runErr := service.Run(context.Background(), agentrun.RunOptions{
			RunType: agentrun.RunTask, Profile: profile, ProjectID: optionValue(args[1:], "--project"),
			RunID: optionValue(args[1:], "--run-id"), SessionID: optionValue(args[1:], "--session-id"), CWD: optionValue(args[1:], "--cwd"), Prompt: prompt,
			ExecutionMode: agentrun.ModeManaged, ResultSchema: optionValue(args[1:], "--result-schema"),
			DeadlineSeconds: intOption(args[1:], "--deadline-seconds", 0),
			AllowedActions:  repeatOption(args[1:], "--allowed-action"), ForbiddenActions: repeatOption(args[1:], "--forbidden-action"),
			Force: containsArg(args[1:], "--force"), Caller: "cli.skill:" + skill.Name,
		})
		_ = printJSON(run)
		return runErr
	default:
		return fmt.Errorf("unknown skills command: %s", args[0])
	}
}

func runCapabilityMemory(cfg *config.Config, args []string) error {
	command := "demo"
	if len(args) > 0 {
		command = args[0]
	}
	path := optionValue(args, "--memory-file")
	candidatePath := cfg.Paths.MemoryCandidatesFile
	sessionID := optionValue(args, "--session-id")
	defaultScope := "global"
	if sessionID != "" {
		defaultScope = "session"
		manager := agentrun.NewSessionManager(agentrun.New(cfg.Home))
		if _, err := manager.Store().Get(sessionID); err != nil {
			return err
		}
		path, candidatePath = manager.MemoryPaths(sessionID)
	}
	if path == "" && command != "demo" {
		path = cfg.Paths.MemoryFile
	}
	memory, err := capability.OpenMemory(path)
	if err != nil {
		return err
	}
	switch command {
	case "candidates":
		candidates, err := capability.OpenMemory(candidatePath)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "items": candidates.Items(), "promotion_required": true})
	case "promote":
		if len(args) < 2 {
			return fmt.Errorf("usage: memory promote <candidate-id...>")
		}
		candidates, err := capability.OpenMemory(candidatePath)
		if err != nil {
			return err
		}
		requested := make(map[string]struct{}, len(args)-1)
		for _, id := range args[1:] {
			if strings.HasPrefix(id, "--") {
				break
			}
			requested[id] = struct{}{}
		}
		var promoted []capability.MemoryItem
		for _, item := range candidates.Items() {
			if _, ok := requested[item.ID]; ok {
				now := time.Now().UTC()
				item.PromotedAt = &now
				item.Status = "working"
				promoted = append(promoted, item)
			}
		}
		if len(promoted) != len(requested) {
			return fmt.Errorf("one or more memory candidates were not found")
		}
		if err := memory.Write(promoted); err != nil {
			return err
		}
		ids := make([]string, 0, len(promoted))
		for _, item := range promoted {
			ids = append(ids, item.ID)
		}
		if err := candidates.Forget(ids); err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "promoted": ids, "durable_memory": path})
	case "demo":
		query := optionValue(args[1:], "--query")
		if query == "" {
			return fmt.Errorf("memory demo --query is required")
		}
		items := []capability.MemoryItem{}
		if raw := optionValue(args[1:], "--items"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &items); err != nil {
				return fmt.Errorf("parse --items: %w", err)
			}
		}
		if err := memory.Write(items); err != nil {
			return err
		}
		filters := map[string]any{}
		if raw := optionValue(args[1:], "--filters"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &filters); err != nil {
				return fmt.Errorf("parse --filters: %w", err)
			}
		}
		kind, _ := filters["type"].(string)
		return printJSON(map[string]any{"ok": true, "recall": memory.Recall(query, kind, intOption(args[1:], "--top-k", 5)), "sources": memory.Sources()})
	case "write":
		if len(args) < 3 {
			return fmt.Errorf("usage: memory write <id> <content>")
		}
		err := memory.Write([]capability.MemoryItem{{ID: args[1], Type: optionDefault(args[3:], "--type", "fact"), Content: args[2],
			Source: optionValue(args[3:], "--source"), SessionID: sessionID, Scope: optionDefault(args[3:], "--scope", defaultScope), Status: "working"}})
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true})
	case "recall":
		if len(args) < 2 {
			return fmt.Errorf("usage: memory recall <query>")
		}
		return printJSON(map[string]any{"ok": true, "items": memory.Recall(args[1], optionValue(args[2:], "--type"), intOption(args[2:], "--top-k", 5))})
	case "forget":
		if len(args) < 2 {
			return fmt.Errorf("usage: memory forget <id...>")
		}
		if err := memory.Forget(args[1:]); err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true})
	case "sources":
		return printJSON(map[string]any{"ok": true, "sources": memory.Sources()})
	default:
		return fmt.Errorf("unknown memory command: %s", command)
	}
}

func terminalState(state string) bool {
	return state == agentrun.StateDone || state == agentrun.StateFailed || state == agentrun.StateBlocked || state == agentrun.StateCancelled
}
func containsArg(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}
func optionValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}

func validateValueOptions(args []string, allowed map[string]bool) error {
	for i := 0; i < len(args); i++ {
		if !allowed[args[i]] {
			return fmt.Errorf("unknown option: %s", args[i])
		}
		i++
		if i >= len(args) {
			return fmt.Errorf("%s requires value", args[i-1])
		}
	}
	return nil
}
func optionDefault(args []string, name, fallback string) string {
	if value := optionValue(args, name); value != "" {
		return value
	}
	return fallback
}
func intOption(args []string, name string, fallback int) int {
	value := optionValue(args, name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
func floatOption(args []string, name string, fallback float64) float64 {
	value := optionValue(args, name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
func repeatOption(args []string, name string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}
func nonOptionArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}
func nullableSkill(skill capability.Skill, ok bool) any {
	if !ok {
		return nil
	}
	return skill
}
