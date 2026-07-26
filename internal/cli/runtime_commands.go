package cli

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yy003x/runtime/internal/agentrun"
	"github.com/yy003x/runtime/internal/cli/config"
	"github.com/yy003x/runtime/internal/daemon"
	"github.com/yy003x/runtime/internal/provider"
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
	scheduler, schedulerErr := service.QueueSnapshot(false)
	if schedulerErr != nil {
		ok = false
	}
	return printJSON(map[string]any{
		"ok": ok, "version": service.RuntimeVersion, "contract_version": agentrun.ContractVersion, "runs_dir": service.RunsDir,
		"default_profile": service.DefaultProfile, "profiles": len(profiles), "providers": items,
		"features": agentrun.SupportedFeatures(), "scheduler": scheduler,
	})
}

func runDaemonCommand(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: system start|status|stop|restart")
	}
	service := agentrun.New(cfg.Home)
	client := service.DaemonClient()
	switch args[0] {
	case "serve":
		signalCtx, stop := signal.NotifyContext(context.Background(), os.Interrupt, syscall.SIGTERM)
		defer stop()
		ctx, cancel := context.WithCancel(signalCtx)
		defer cancel()
		config := service.DaemonConfig()
		config.Busy = service.QueueBusy
		dispatchErr := make(chan error, 1)
		go func() {
			dispatchErr <- service.DispatchQueue(ctx)
		}()
		serverErr := daemon.NewServer(config).Run(ctx)
		cancel()
		return errors.Join(serverErr, <-dispatchErr)
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
	if len(status.Processes) > 0 || len(status.Dependencies) > 0 || status.Busy {
		return fmt.Errorf("daemon has active work (processes=%d dependencies=%d queue_busy=%t); stop or cancel it before restart", len(status.Processes), len(status.Dependencies), status.Busy)
	}
	return nil
}

func runProfileValidation(cfg *config.Config, profileID string, live bool) error {
	service := agentrun.New(cfg.Home)
	profiles, err := service.Profiles()
	if err != nil {
		return err
	}
	if profileID != "" {
		profile, ok := provider.Resolve(profiles, profileID)
		if !ok {
			return fmt.Errorf("unknown profile %q", profileID)
		}
		result := validateProfile(profile, profiles, live)
		valid, _ := result["ok"].(bool)
		return printJSON(map[string]any{"ok": valid, "validated": 1, "results": []map[string]any{result}})
	}
	ids := make([]string, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	results := make([]map[string]any, 0, len(ids))
	allOK := len(ids) > 0
	for _, id := range ids {
		result := validateProfile(profiles[id], profiles, live)
		results = append(results, result)
		if valid, _ := result["ok"].(bool); !valid {
			allOK = false
		}
	}
	return printJSON(map[string]any{"ok": allOK, "validated": len(results), "results": results})
}

func printProfileCommand(cfg *config.Config, profileID, mode string, jsonOutput bool) error {
	profiles, err := agentrun.New(cfg.Home).Profiles()
	if err != nil {
		return err
	}
	profile, ok := provider.Resolve(profiles, profileID)
	if !ok {
		return fmt.Errorf("unknown profile %q", profileID)
	}
	if profile.Type != provider.TypeCLI || profile.CLI == nil {
		return fmt.Errorf("profile %q is not a CLI profile", profile.ID)
	}
	var argv []string
	switch mode {
	case "direct":
		prepared, err := provider.PrepareInteractiveCLI(profile, nil)
		if err != nil {
			return err
		}
		argv = prepared.Argv
	case "exec":
		prepared, err := provider.Prepare(profile, "", nil)
		if err != nil {
			return err
		}
		if prepared.CLI != nil {
			argv = prepared.CLI.Argv
		}
	default:
		return fmt.Errorf("profile command --mode must be direct|exec")
	}
	if len(argv) == 0 {
		return fmt.Errorf("profile %q did not resolve a CLI command", profile.ID)
	}
	argv = redactCommandArgv(argv)
	command := shellJoin(argv)
	if !jsonOutput {
		fmt.Println(command)
		return nil
	}
	return printJSON(map[string]any{
		"ok": true, "profile": profile.ID, "provider_type": profile.Type,
		"transport": profile.Transport(), "mode": mode, "argv": argv, "command": command,
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
		if message := validateAPIKeyEnvironment(modelProfile.API); message != "" {
			result["message"] = "native model_profile 凭据缺失: " + message
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
		if message := validateAPIKeyEnvironment(profile.API); message != "" {
			result["message"] = message
			return result
		}
		if message := validateAPIHeaderEnvironment(profile.API); message != "" {
			result["message"] = message
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
	if message := addCLIEnvironmentDiagnostics(profile, result); message != "" {
		result["message"] = message
		return result
	}
	binary, err := provider.ResolveEnv(profile.CLI.Command.Binary)
	if err != nil {
		result["message"] = "command: " + err.Error()
		return result
	}
	if _, err := exec.LookPath(binary); err != nil {
		if info, statErr := os.Stat(binary); statErr != nil || info.IsDir() {
			result["message"] = "命令不可用: " + binary
			return result
		}
	}
	result["ok"], result["message"] = true, "命令可用"
	return result
}

func validateAPIKeyEnvironment(api *provider.APIConfig) string {
	name, ok := provider.EnvironmentReferenceName(api.APIKey)
	if !ok {
		return "api_key 必须使用完整的 ${ENV_VAR} 环境变量占位符"
	}
	key, err := provider.ResolveEnv(api.APIKey)
	if err != nil {
		return "api_key: " + err.Error()
	}
	if key == "" {
		return "环境变量不能为空: " + name
	}
	return ""
}

func validateAPIHeaderEnvironment(api *provider.APIConfig) string {
	names := make([]string, 0, len(api.Headers))
	for name := range api.Headers {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		if _, err := provider.ResolveEnv(api.Headers[name]); err != nil {
			return "headers." + name + ": " + err.Error()
		}
	}
	return ""
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
		command, err := provider.ResolveEnv(server.Command)
		if err != nil {
			return fmt.Sprintf("MCP server %s command: %s", server.Name, err)
		}
		if _, err := exec.LookPath(command); err != nil {
			if info, statErr := os.Stat(command); statErr != nil || info.IsDir() {
				return fmt.Sprintf("MCP server %s 命令不可用: %s", server.Name, command)
			}
		}
	}
	result["mcp_servers"] = servers
	return ""
}

func addCLIEnvironmentDiagnostics(profile provider.Config, result map[string]any) string {
	if profile.CLI == nil {
		return ""
	}
	environment := make(map[string]string)
	resolvedEnvironment, err := provider.CommandEnvironment(profile.CLI.Command, nil)
	if err != nil {
		return err.Error()
	}
	for _, item := range resolvedEnvironment {
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
			result["warnings"] = []string{"ANTHROPIC_API_KEY 与 ANTHROPIC_AUTH_TOKEN 同时生效；请在对应 profile 的 env 中把不使用的一项设为 null"}
		}
	}
	if len(details) > 0 {
		result["environment"] = details
	}
	return ""
}

func userHomeDir() string {
	home, err := os.UserHomeDir()
	if err != nil {
		return "~"
	}
	return home
}

func validateExecutionEnvironment(profile provider.Config) string {
	for index, upstream := range profile.Execution.Upstreams {
		resolved, err := provider.ResolveEnv(upstream)
		if err != nil {
			return fmt.Sprintf("execution.upstreams[%d]: %s", index, err)
		}
		if strings.TrimSpace(resolved) == "" {
			return fmt.Sprintf("execution.upstreams[%d] 不能为空", index)
		}
	}
	if profile.Execution.Dylib == "" {
		return ""
	}
	path, err := provider.ResolveEnv(profile.Execution.Dylib)
	if err != nil {
		return "execution.dylib: " + err.Error()
	}
	if path == "" {
		return "dylib 路径环境变量未设置: " + profile.Execution.Dylib
	}
	if info, err := os.Stat(path); err != nil || info.IsDir() {
		return "dylib 不可用: " + path
	}
	return ""
}

func runRunRegistryAction(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: run list|reconcile")
	}
	service := agentrun.New(cfg.Home)
	switch args[0] {
	case "list":
		filter := agentrun.RunFilter{}
		for index := 1; index < len(args); index++ {
			switch args[index] {
			case "--active":
				filter.Active = true
			case "--state", "--type", "--project", "--profile", "--limit":
				name := args[index]
				index++
				if index >= len(args) {
					return fmt.Errorf("%s requires value", name)
				}
				switch name {
				case "--state":
					filter.State = args[index]
				case "--type":
					filter.RunType = args[index]
				case "--project":
					filter.ProjectID = args[index]
				case "--profile":
					filter.Profile = args[index]
				case "--limit":
					value, err := strconv.Atoi(args[index])
					if err != nil || value <= 0 {
						return fmt.Errorf("--limit must be a positive integer")
					}
					filter.Limit = value
				}
			default:
				return fmt.Errorf("unknown run list option: %s", args[index])
			}
		}
		runs, err := service.ListRuns(filter)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"runs": runs})
	case "reconcile":
		dryRun := false
		for _, arg := range args[1:] {
			if arg != "--dry-run" {
				return fmt.Errorf("unknown run reconcile option: %s", arg)
			}
			dryRun = true
		}
		before, err := service.QueueSnapshot(true)
		if err != nil {
			return err
		}
		report, err := service.ReconcileQueue(dryRun)
		if err != nil {
			return err
		}
		after, err := service.QueueSnapshot(true)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "reconcile": report, "before": before, "after": after})
	default:
		return fmt.Errorf("unknown run action: %s", args[0])
	}
}

func parseRunOptions(runType string, args []string) (agentrun.RunOptions, []string, error) {
	options := agentrun.RunOptions{RunType: runType, ExecutionMode: agentrun.ModeManaged, ProviderOverrides: map[string]any{}}
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			options.Profile = args[i]
			return options, append([]string(nil), args[i+1:]...), nil
		}
		switch args[i] {
		case "--project":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("--project requires value")
			}
			options.ProjectID = args[i]
		case "--run-id":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("--run-id requires value")
			}
			options.RunID = args[i]
		case "--session-id":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("--session-id requires value")
			}
			options.SessionID = args[i]
		case "--record-mode":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("--record-mode requires value")
			}
			options.RecordMode = args[i]
		case "--retention":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("--retention requires value")
			}
			options.Retention = args[i]
		case "--cwd":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("--cwd requires value")
			}
			options.CWD = args[i]
		case "--prompt-file":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("%s requires value", args[i-1])
			}
			options.PromptFile = args[i]
		case "--memory-file":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("--memory-file requires value")
			}
			values, err := readInjectedMemoryFile(args[i])
			if err != nil {
				return options, nil, err
			}
			options.InjectedMemory = append(options.InjectedMemory, values...)
		case "--deadline-seconds":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("%s requires value", args[i-1])
			}
			value, err := strconv.Atoi(args[i])
			if err != nil {
				return options, nil, err
			}
			options.DeadlineSeconds = value
		case "--queue-timeout-seconds":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("--queue-timeout-seconds requires value")
			}
			value, err := strconv.Atoi(args[i])
			if err != nil || value < 0 {
				return options, nil, fmt.Errorf("--queue-timeout-seconds must be a non-negative integer")
			}
			options.QueueTimeout = value
		case "--result-schema":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("--result-schema requires value")
			}
			options.ResultSchema = args[i]
		case "--allowed-action":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("--allowed-action requires value")
			}
			options.AllowedActions = append(options.AllowedActions, args[i])
		case "--forbidden-action":
			i++
			if i >= len(args) {
				return options, nil, fmt.Errorf("--forbidden-action requires value")
			}
			options.ForbiddenActions = append(options.ForbiddenActions, args[i])
		case "--force":
			options.Force = true
		case "--json":
		default:
			return options, nil, fmt.Errorf("unknown Runtime option before provider: %s", args[i])
		}
	}
	return options, nil, fmt.Errorf("%s run requires a provider as its first positional argument", runType)
}

const maxInjectedMemoryFileBytes int64 = 1 << 20

func readInjectedMemoryFile(path string) ([]provider.InjectedMemory, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("stat memory file: %w", err)
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("memory file must be a regular file")
	}
	if info.Size() > maxInjectedMemoryFileBytes {
		return nil, fmt.Errorf("memory file exceeds %d bytes", maxInjectedMemoryFileBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read memory file: %w", err)
	}
	var values []provider.InjectedMemory
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&values); err != nil {
		return nil, fmt.Errorf("decode memory file: %w", err)
	}
	if err := decoder.Decode(&struct{}{}); err != io.EOF {
		if err == nil {
			return nil, fmt.Errorf("memory file must contain one JSON array")
		}
		return nil, fmt.Errorf("decode memory file: %w", err)
	}
	return values, nil
}

func runSessionHistory(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: session list|show|messages|events|configure|export|delete|gc")
	}
	service := agentrun.New(cfg.Home)
	store := agentrun.NewSessionManager(service).Store()
	switch args[0] {
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
			return fmt.Errorf("session export requires --output")
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
			return fmt.Errorf("session configure requires runtime/profile or record-mode/retention")
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
	case "gc":
		hours, limit, apply, err := parseSessionGCOptions(args[1:])
		if err != nil {
			return err
		}
		result, err := store.GC(agentrun.SessionGCOptions{
			Before: time.Now().UTC().Add(-time.Duration(hours) * time.Hour),
			Limit:  limit, Apply: apply,
		})
		if err != nil {
			return err
		}
		return printJSON(result)
	default:
		return fmt.Errorf("unknown session action: %s", args[0])
	}
}

func parseSessionGCOptions(args []string) (hours, limit int, apply bool, err error) {
	hours, limit = 24, 100
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--older-than-hours":
			index++
			if index >= len(args) {
				return 0, 0, false, fmt.Errorf("--older-than-hours requires value")
			}
			hours, err = strconv.Atoi(args[index])
			if err != nil || hours < 1 || hours > 10*365*24 {
				return 0, 0, false, fmt.Errorf("--older-than-hours must be between 1 and %d", 10*365*24)
			}
		case "--limit":
			index++
			if index >= len(args) {
				return 0, 0, false, fmt.Errorf("--limit requires value")
			}
			limit, err = strconv.Atoi(args[index])
			if err != nil || limit < 1 || limit > 1000 {
				return 0, 0, false, fmt.Errorf("--limit must be between 1 and 1000")
			}
		case "--apply":
			apply = true
		default:
			return 0, 0, false, fmt.Errorf("unknown session gc option: %s", args[index])
		}
	}
	return hours, limit, apply, nil
}

func validateSessionRuntimeProfile(service *agentrun.Service, runtimeName, profileName string) (string, string, error) {
	if runtimeName != "" && runtimeName != "api" && runtimeName != "cli" && runtimeName != "tmux" && runtimeName != "terminal" {
		return "", "", fmt.Errorf("runtime must be api|cli|tmux|terminal")
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
	if runtimeName == "terminal" {
		if profile.Type != provider.TypeCLI || profile.CLI == nil || profile.CLI.Executor != provider.ExecutorCommand {
			return "", "", fmt.Errorf("terminal runtime requires a command CLI profile")
		}
		return runtimeName, profile.ID, nil
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
	for i := 0; i < len(args); i++ {
		if !strings.HasPrefix(args[i], "-") {
			options.Profile = args[i]
			options.RawCLIArgs = append([]string(nil), args[i+1:]...)
			return options, nil
		}
		switch args[i] {
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
		case "--session-id":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--session-id requires value")
			}
			options.SessionID = args[i]
		case "--carrier":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--carrier requires value")
			}
			options.Carrier = args[i]
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
		default:
			return options, fmt.Errorf("unknown Runtime option before provider: %s", args[i])
		}
	}
	return options, fmt.Errorf("session open requires a provider as its first positional argument")
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

func terminalState(state string) bool {
	return state == agentrun.StateDone || state == agentrun.StateFailed || state == agentrun.StateBlocked || state == agentrun.StateCancelled
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
