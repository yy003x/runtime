package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"agent-runtime/internal/agentrun"
	"agent-runtime/internal/capability"
	"agent-runtime/internal/cli/config"
	"agent-runtime/internal/daemon"
	"agent-runtime/internal/provider"
)

func TestRuntimeDoctorReportsContractVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := `{"type":"native","label":"Native Mock","native":{"mock":{"responses":["ok"],"done_after":1}}}`
	if err := os.WriteFile(filepath.Join(home, "configs", "native-mock.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}

	code, output := captureMain(t, []string{"doctor", "--json"})
	if code != 0 {
		t.Fatalf("doctor code=%d output=%q", code, output)
	}
	var payload struct {
		ContractVersion int                    `json:"contract_version"`
		Features        map[string]int         `json:"features"`
		Scheduler       agentrun.QueueSnapshot `json:"scheduler"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatalf("decode doctor output: %v\n%s", err, output)
	}
	if payload.ContractVersion != agentrun.ContractVersion {
		t.Fatalf("contract_version=%d want=%d", payload.ContractVersion, agentrun.ContractVersion)
	}
	if payload.Features["durable_queue"] != 1 || !payload.Scheduler.Healthy {
		t.Fatalf("doctor payload=%#v", payload)
	}
}

func TestParseRunOptionsMergesTypedOverrides(t *testing.T) {
	options, err := parseRunOptions(agentrun.RunTask, []string{
		"-c", "cx", "--model", "first", "--image", "one.png", "--provider-overrides", `{"model":"final","verbosity":"high"}`, "prompt", "--", "--search",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Profile != "cx" || options.Prompt != "prompt" || options.ProviderOverrides["model"] != "final" || options.ProviderOverrides["verbosity"] != "high" {
		t.Fatalf("options=%#v", options)
	}
	if !reflect.DeepEqual(options.ProviderOverrides["images"], []string{"one.png"}) {
		t.Fatalf("images=%#v", options.ProviderOverrides["images"])
	}
	if !reflect.DeepEqual(options.RawCLIArgs, []string{"--search"}) {
		t.Fatalf("raw_cli_args=%#v", options.RawCLIArgs)
	}
}

func TestParseRunOptionsAcceptsQueueTimeout(t *testing.T) {
	options, err := parseRunOptions(agentrun.RunTask, []string{"-c", "cx", "--queue-timeout-seconds", "45", "prompt"})
	if err != nil || options.QueueTimeout != 45 {
		t.Fatalf("options=%#v err=%v", options, err)
	}
	if _, err := parseRunOptions(agentrun.RunTask, []string{"-c", "cx", "--queue-timeout-seconds", "-1", "prompt"}); err == nil {
		t.Fatal("negative queue timeout was accepted")
	}
}

func TestMainCoversLocalControlPlaneCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	for _, dir := range []string{"configs", "configs/skills/review", "configs/tools"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	profile := `{"type":"native","label":"Native Mock","native":{"system_prompt":"test","max_rounds":2,"mock":{"responses":["ok"],"done_after":1}}}`
	if err := os.WriteFile(filepath.Join(home, "configs", "native-mock.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	shellProfile := `{"type":"cli","label":"Shell","cli":{"driver":"generic","executor":"command","command":{"binary":"/bin/sh","args":["-c","printf 'ready\\n'; while IFS= read -r line; do printf 'reply:%s\\n' \"$line\"; done"],"model":""},"runtime":{"prompt_delivery":"stdin","result_contract":"optional"}}}`
	if err := os.WriteFile(filepath.Join(home, "configs", "shell.json"), []byte(shellProfile), 0o644); err != nil {
		t.Fatal(err)
	}
	skill := "name: review\ndescription: review code\nkeywords: [review]\ndefault_profile: native-mock\nprompt_template: 'Review {{input}}'\n"
	if err := os.WriteFile(filepath.Join(home, "configs", "skills", "review", "skill.yaml"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	candidates, err := capability.OpenMemory(filepath.Join(home, "memory", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := candidates.Write([]capability.MemoryItem{{ID: "candidate-1", Type: "fact", Content: "promote me", Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	historyExport := filepath.Join(home, "history-export.json")

	commands := [][]string{
		{}, {"--help"}, {"version"}, {"profiles"}, {"providers"},
		{"config", "choices"}, {"config", "validate", "-c", "native-mock"}, {"config", "command", "-c", "shell"}, {"doctor"},
		{"runs", "list", "--limit", "5"}, {"runs", "reconcile", "--dry-run"},
		{"capabilities", "tools", "schemas"}, {"tools", "call", "echo", "--args", `{"value":"ok"}`},
		{"capabilities", "skills", "list"}, {"capabilities", "skills", "route", "--query", "review this"},
		{"capabilities", "memory", "write", "fact-1", "runtime fact", "--source", "test"},
		{"capabilities", "memory", "recall", "runtime"}, {"capabilities", "memory", "sources"},
		{"capabilities", "memory", "candidates"}, {"capabilities", "memory", "promote", "candidate-1"},
		{"capabilities", "memory", "forget", "fact-1"}, {"clean"},
		{"history", "create", "--session-id", "session-20260716-165900-cli", "--project", "project", "--runtime", "api", "--profile", "native-mock", "--tag", "workbench"},
		{"history", "list", "--project", "project", "--tag", "workbench"},
		{"history", "show", "--session-id", "session-20260716-165900-cli"},
		{"history", "messages", "--session-id", "session-20260716-165900-cli"},
		{"history", "events", "--session-id", "session-20260716-165900-cli"},
		{"history", "configure", "--session-id", "session-20260716-165900-cli", "--runtime", "cli", "--profile", "shell"},
		{"history", "export", "--session-id", "session-20260716-165900-cli", "--output", historyExport},
		{"history", "rebuild"},
		{"update", "--dry-run", "--version", "v1.2.3"},
		{"task", "run", "-c", "native-mock", "--run-id", "task-20260716-170000-clitest", "hello"},
		{"task", "status", "--run-id", "task-20260716-170000-clitest"},
		{"task", "logs", "--run-id", "task-20260716-170000-clitest", "--tail", "5"},
		{"task", "result", "--run-id", "task-20260716-170000-clitest"},
		{"task", "watch", "--run-id", "task-20260716-170000-clitest", "--seconds", "1", "--poll-seconds", "0.01"},
		{"native-mock", "--run-id", "task-20260716-170001-profile", "hello"},
		{"capabilities", "skills", "run", "review", "--input", "main.go", "--run-id", "task-20260716-170002-skill"},
		{"loop", "run", "--loop-id", "loop-20260716-170003-cli", "--input", "hello", "--actions-json", `[{"type":"respond","content":"done"}]`},
		{"loop", "status", "--loop-id", "loop-20260716-170003-cli"},
		{"loop", "logs", "--loop-id", "loop-20260716-170003-cli", "--tail", "5"},
	}
	for _, command := range commands {
		code, output := captureMain(t, command)
		if code != 0 {
			t.Fatalf("Main(%q) code=%d output=%q", command, code, output)
		}
	}
	if _, err := exec.LookPath("tmux"); err == nil {
		service := agentrun.New(home)
		daemonContext, cancelDaemon := context.WithCancel(context.Background())
		daemonDone := make(chan error, 1)
		server := daemon.NewServer(service.DaemonConfig())
		go func() { daemonDone <- server.Run(daemonContext) }()
		for deadline := time.Now().Add(3 * time.Second); ; {
			if _, statusErr := service.DaemonClient().Status(context.Background()); statusErr == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("CLI test daemon did not start")
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Cleanup(func() {
			_, _ = service.SessionStop(context.Background(), "session-20260716-170010-cli")
			_, _ = service.CommandStop(context.Background(), "command-20260716-170011-cli")
			cancelDaemon()
			select {
			case <-daemonDone:
			case <-time.After(time.Second):
			}
		})
		tmuxCommands := [][]string{
			{"daemon", "status"}, {"doctor", "daemon", "--json"},
			{"session", "start", "-c", "shell", "--run-id", "session-20260716-170010-cli"},
			{"session", "status", "--run-id", "session-20260716-170010-cli"},
			{"session", "list"},
			{"session", "send", "--run-id", "session-20260716-170010-cli", "hello"},
			{"session", "logs", "--run-id", "session-20260716-170010-cli", "--tail", "10"},
			{"session", "stop", "--run-id", "session-20260716-170010-cli"},
			{"session", "watch", "--run-id", "session-20260716-170010-cli", "--seconds", "1", "--poll-seconds", "0.01"},
			{"command", "start", "-c", "shell", "--run-id", "command-20260716-170011-cli", "--", "/bin/sh", "-c", "printf command-ok"},
			{"command", "status", "--run-id", "command-20260716-170011-cli"},
			{"command", "logs", "--run-id", "command-20260716-170011-cli", "--tail", "10"},
			{"command", "watch", "--run-id", "command-20260716-170011-cli", "--seconds", "1", "--poll-seconds", "0.01"},
		}
		for _, command := range tmuxCommands {
			code, output := captureMain(t, command)
			if code != 0 {
				t.Fatalf("Main(%q) code=%d output=%q", command, code, output)
			}
		}
	}
	if code, output := captureMain(t, []string{"unknown-command"}); code != 1 || !strings.Contains(output, "unknown command") {
		t.Fatalf("unknown code=%d output=%q", code, output)
	}
	invalidCommands := [][]string{
		{"config"}, {"config", "unknown"}, {"capabilities"}, {"capabilities", "unknown"},
		{"tools", "unknown"}, {"capabilities", "skills"}, {"capabilities", "skills", "unknown"},
		{"capabilities", "memory", "unknown"}, {"task"}, {"task", "unknown"},
		{"task", "run", "hello"}, {"task", "status"}, {"loop"}, {"loop", "unknown"},
		{"history"}, {"history", "unknown"}, {"history", "create", "--unknown", "value"},
		{"history", "show"}, {"history", "rebuild", "extra"},
		{"loop", "step"}, {"session"}, {"session", "unknown"}, {"session", "list", "extra"},
		{"session", "send"}, {"command"}, {"command", "unknown"}, {"command", "start", "-c", "shell"},
		{"clean", "--unknown"}, {"update", "--unknown"},
	}
	durable, err := capability.OpenMemory(filepath.Join(home, "memory", "durable.json"))
	if err != nil {
		t.Fatal(err)
	}
	promoted := durable.Recall("promote", "fact", 5)
	if len(promoted) != 1 || promoted[0].PromotedAt == nil {
		t.Fatalf("promoted memory=%#v", durable.Items())
	}
	remaining, err := capability.OpenMemory(filepath.Join(home, "memory", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Items()) != 0 {
		t.Fatalf("remaining candidates=%#v", remaining.Items())
	}
	for _, command := range invalidCommands {
		if code, output := captureMain(t, command); code != 1 || !strings.Contains(output, "error:") {
			t.Fatalf("Main(%q) code=%d output=%q", command, code, output)
		}
	}
	if code, output := captureMain(t, []string{"update", "--help"}); code != 0 || !strings.Contains(output, "sn-cli update") {
		t.Fatalf("update help code=%d output=%q", code, output)
	}
}

func captureMain(t *testing.T, args []string) (int, string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = writer, writer
	code := Main(args)
	os.Stdout, os.Stderr = stdout, stderr
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	return code, string(data)
}

func TestConfigCommandPreviewsManagedArgvWithoutExecutionAndRedactsSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	t.Setenv("MY_API_KEY", "environment-secret")
	if err := os.MkdirAll(filepath.Join(home, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := `{"type":"cli","label":"Preview","cli":{"driver":"generic","executor":"command","command":{"binary":"never-execute","args":["--api-key","literal-secret","--endpoint=https://user:pass@example.test/v1?access=query-secret","environment-secret"],"model":"preview-model"},"runtime":{"prompt_delivery":"stdin","managed_args":["managed"],"result_contract":"optional"}}}`
	if err := os.WriteFile(filepath.Join(home, "configs", "preview.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	nativeProfile := `{"type":"native","native":{"mock":{"responses":["ok"],"done_after":1}}}`
	if err := os.WriteFile(filepath.Join(home, "configs", "native-mock.json"), []byte(nativeProfile), 0o644); err != nil {
		t.Fatal(err)
	}

	code, output := captureMain(t, []string{"config", "command", "-c", "preview", "--json"})
	if code != 0 {
		t.Fatalf("code=%d output=%q", code, output)
	}
	for _, secret := range []string{"literal-secret", "query-secret", "environment-secret", "user:pass"} {
		if strings.Contains(output, secret) {
			t.Fatalf("preview leaked %q: %s", secret, output)
		}
	}
	for _, expected := range []string{"never-execute", "--model", "preview-model", "managed", "[REDACTED]"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("preview missing %q: %s", expected, output)
		}
	}
	if _, err := exec.LookPath("never-execute"); err == nil {
		t.Fatal("test binary unexpectedly exists; non-execution assertion is invalid")
	}

	if code, output := captureMain(t, []string{"config", "command", "-c", "native-mock"}); code != 1 || !strings.Contains(output, "not a CLI profile") {
		t.Fatalf("non-CLI preview code=%d output=%q", code, output)
	}
}

func TestCLIEnvironmentDiagnosticsReportHomesAndAuthConflict(t *testing.T) {
	t.Setenv("CODEX_HOME", "/tmp/codex-aip")
	t.Setenv("ANTHROPIC_API_KEY", "api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "auth-token")

	codexResult := map[string]any{}
	addCLIEnvironmentDiagnostics(provider.Config{CLI: &provider.CLIConfig{
		Driver: "codex", Command: provider.CommandConfig{},
	}}, codexResult)
	codexEnvironment, _ := codexResult["environment"].(map[string]any)
	if codexEnvironment["CODEX_HOME"] != "/tmp/codex-aip" {
		t.Fatalf("codex environment=%#v", codexEnvironment)
	}

	claudeResult := map[string]any{}
	addCLIEnvironmentDiagnostics(provider.Config{CLI: &provider.CLIConfig{
		Driver: "claude", Command: provider.CommandConfig{Env: map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/claude-aip"}},
	}}, claudeResult)
	claudeEnvironment, _ := claudeResult["environment"].(map[string]any)
	if claudeEnvironment["CLAUDE_CONFIG_DIR"] != "/tmp/claude-aip" {
		t.Fatalf("claude environment=%#v", claudeEnvironment)
	}
	if warnings, _ := claudeResult["warnings"].([]string); len(warnings) != 1 {
		t.Fatalf("warnings=%#v", claudeResult["warnings"])
	}
}

func TestCLIEnvironmentDiagnosticsHonorEnvUnset(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "auth-token")
	result := map[string]any{}
	addCLIEnvironmentDiagnostics(provider.Config{CLI: &provider.CLIConfig{
		Driver: "claude", Command: provider.CommandConfig{EnvUnset: []string{"ANTHROPIC_AUTH_TOKEN"}},
	}}, result)
	if _, exists := result["warnings"]; exists {
		t.Fatalf("unexpected warnings=%#v", result["warnings"])
	}
	environment, _ := result["environment"].(map[string]any)
	if got := environment["active_auth_env"]; !reflect.DeepEqual(got, []string{"ANTHROPIC_API_KEY"}) {
		t.Fatalf("active_auth_env=%#v", got)
	}
}

func TestParsersRejectRemovedProfileOption(t *testing.T) {
	if _, err := parseRunOptions(agentrun.RunTask, []string{"--profile", "cx", "hello"}); err == nil {
		t.Fatal("task run accepted removed --profile option")
	}
	if _, err := parseCommandOptions([]string{"--profile", "cx", "--", "true"}); err == nil {
		t.Fatal("command start accepted removed --profile option")
	}
}

func TestParseCommandOptionsPreservesRemainder(t *testing.T) {
	options, err := parseCommandOptions([]string{"-c", "cx", "--label", "smoke", "--", "printf", "%s", "hello world"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Profile != "cx" || options.Label != "smoke" || !reflect.DeepEqual(options.Argv, []string{"printf", "%s", "hello world"}) {
		t.Fatalf("options=%#v", options)
	}
}

func TestParseSessionStartOptionsRequiresConfigAndSeparatesRawArgs(t *testing.T) {
	options, err := parseSessionStartOptions([]string{"-c", "cx", "review", "repo", "--", "--no-alt-screen"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Profile != "cx" || options.Prompt != "review repo" || !reflect.DeepEqual(options.RawCLIArgs, []string{"--no-alt-screen"}) {
		t.Fatalf("options=%#v", options)
	}
	if _, err := parseSessionStartOptions([]string{"hello"}); err == nil {
		t.Fatal("session start accepted missing -c/--config")
	}
}

func TestSessionLifecycleRequiresNamedRunID(t *testing.T) {
	if _, err := parseRequiredID([]string{"session-old-positional"}, "--run-id", nil, nil); err == nil {
		t.Fatal("positional run id was accepted")
	}
	got, err := parseRequiredID([]string{"--run-id", "session-1"}, "--run-id", nil, nil)
	if err != nil || got != "session-1" {
		t.Fatalf("run_id=%q err=%v", got, err)
	}
}

func TestTurnRunRequiresExistingSessionBoundary(t *testing.T) {
	err := runTaskCommand(&config.Config{Home: t.TempDir()}, "turn", []string{"run", "-c", "missing", "hello"})
	if err == nil || !strings.Contains(err.Error(), "turn run requires --session-id") {
		t.Fatalf("err=%v", err)
	}
}

func TestSessionExecCreatesMetadataExecutionOnlyWhenExplicit(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, "direct.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nexit 0\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIProfile(t, home, "direct-session", fmt.Sprintf(`{"type":"cli","cli":{"driver":"generic","executor":"command","command":{"binary":%q,"model":""},"runtime":{"prompt_delivery":"none","result_contract":"optional"}}}`, script))
	if err := runRuntimeSession(&config.Config{Home: home}, []string{"exec", "-c", "direct-session", "--", "--help"}); err != nil {
		t.Fatal(err)
	}
	values, err := agentrun.NewSessionManager(agentrun.New(home)).Store().List(agentrun.SessionFilter{})
	if err != nil || len(values) != 1 || values[0].RecordMode != agentrun.RecordMetadata || values[0].State != agentrun.SessionStateIdle {
		t.Fatalf("sessions=%#v err=%v", values, err)
	}
}

func TestParseLoopOptionsSupportsContractFields(t *testing.T) {
	options, err := parseLoopOptions([]string{"--input", "work", "--actions-json", `[{"type":"respond","content":"ok"}]`, "--result-schema", "custom.yaml", "--deadline-seconds", "9"})
	if err != nil {
		t.Fatal(err)
	}
	if options.ResultSchema != "custom.yaml" || options.DeadlineSeconds != 9 || len(options.Actions) != 1 {
		t.Fatalf("options=%#v", options)
	}
}

func TestParseLoopOptionsRejectsRemovedAliases(t *testing.T) {
	if _, err := parseLoopOptions([]string{"--actions", `[{"type":"respond"}]`}); err == nil {
		t.Fatal("loop accepted removed --actions option")
	}
	if _, err := parseLoopOptions([]string{"--planner-profile", "cx"}); err == nil {
		t.Fatal("loop accepted removed --planner-profile option")
	}
}

func TestOpenURLRejectsNonHTTPURL(t *testing.T) {
	if err := openURL("file:///tmp/private"); err == nil {
		t.Fatal("openURL accepted non-http URL")
	}
}

func TestDaemonRestartRejectsActiveRuntimeState(t *testing.T) {
	status := &daemon.Status{
		Processes:    []daemon.ProcessStatus{{ID: "task/active", Alive: true}},
		Dependencies: []daemon.DependencyStatus{{Command: "service", Healthy: true}},
	}
	if err := ensureDaemonRestartSafe(status); err == nil {
		t.Fatal("active daemon state was accepted for restart")
	}
	status.Processes = nil
	status.Dependencies = nil
	status.UptimeSeconds = int64(time.Second.Seconds())
	if err := ensureDaemonRestartSafe(status); err != nil {
		t.Fatal(err)
	}
}

func TestValidateNativeProfileDistinguishesDependencyAndCredentialFailures(t *testing.T) {
	nativeProfile := provider.Config{ID: "native", Type: provider.TypeNative, Native: &provider.NativeConfig{ModelProfile: "api"}}
	result := validateProfile(nativeProfile, map[string]provider.Config{"native": nativeProfile}, false)
	if result["message"] != "native model_profile 不存在: api" {
		t.Fatalf("result=%#v", result)
	}
	apiProfile := provider.Config{ID: "api", Type: provider.TypeAPI, API: &provider.APIConfig{APIKey: "${NATIVE_TEST_KEY}"}}
	result = validateProfile(nativeProfile, map[string]provider.Config{"native": nativeProfile, "api": apiProfile}, false)
	if !strings.Contains(result["message"], "环境变量未设置: NATIVE_TEST_KEY") {
		t.Fatalf("result=%#v", result)
	}
	t.Setenv("NATIVE_TEST_KEY", "secret")
	result = validateProfile(nativeProfile, map[string]provider.Config{"native": nativeProfile, "api": apiProfile}, false)
	if result["ok"] != true {
		t.Fatalf("result=%#v", result)
	}
}

func TestValidateAPIProfileRejectsProgrammaticPlaintextKey(t *testing.T) {
	profile := provider.Config{ID: "api", Type: provider.TypeAPI, API: &provider.APIConfig{APIKey: "secret"}}
	result := validateProfile(profile, map[string]provider.Config{"api": profile}, false)
	if result["ok"] == true || !strings.Contains(fmt.Sprint(result["message"]), "${ENV_VAR}") {
		t.Fatalf("result=%#v", result)
	}
}

func TestValidateAPIAgentRuntimeReportsCapabilitiesAndMissingMCPCommand(t *testing.T) {
	profile := provider.Config{ID: "api-agent", Type: provider.TypeAPI, API: &provider.APIConfig{
		Mock: true, Runtime: &provider.APIRuntimeConfig{
			Enabled: true, AutoRouteSkills: true, Skills: []string{"review"}, Memory: &provider.APIMemoryConfig{Enabled: true},
			MCPServers: []provider.MCPServerConfig{{Name: "fixture", Command: "/definitely/missing/mcp-server"}},
		},
	}}
	result := validateProfile(profile, map[string]provider.Config{"api-agent": profile}, false)
	if result["ok"] == true || !strings.Contains(fmt.Sprint(result["message"]), "MCP server fixture 命令不可用") {
		t.Fatalf("result=%#v", result)
	}
	profile.API.Runtime.MCPServers[0].Command = "/bin/sh"
	result = validateProfile(profile, map[string]provider.Config{"api-agent": profile}, false)
	if result["ok"] != true || result["agent_runtime"] != true || result["memory_enabled"] != true {
		t.Fatalf("result=%#v", result)
	}
}
