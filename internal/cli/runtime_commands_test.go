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

	code, output := captureMain(t, []string{"system", "doctor", "--json"})
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
	shellProfile := `{"type":"cli","label":"Shell","cli":{"driver":"generic","executor":"command","command":{"binary":"/bin/sh","args":["-c","printf 'ready\\n'; while IFS= read -r line; do printf 'reply:%s\\n' \"$line\"; done"],"model":""},"runtime":{"prompt_delivery":"stdin"}}}`
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
		{}, {"--help"}, {"--version"},
		{"profile", "list"}, {"profile", "show", "native-mock"}, {"profile", "validate", "native-mock"}, {"profile", "command", "shell"},
		{"system", "doctor"}, {"run", "list", "--limit", "5"}, {"run", "reconcile", "--dry-run"},
		{"tool", "list"}, {"tool", "show", "echo"}, {"tool", "call", "echo", "--args", `{"value":"ok"}`},
		{"skill", "list"}, {"skill", "show", "review"},
		{"memory", "add", "fact-1", "runtime fact", "--source", "test"},
		{"memory", "recall", "runtime"}, {"memory", "list"},
		{"memory", "list", "--state", "candidate"}, {"memory", "promote", "candidate-1"},
		{"memory", "remove", "fact-1"},
		{"session", "run", "native-mock", "--session-id", "session-20260716-165900-cli", "--project", "project", "hello"},
		{"session", "list", "--project", "project"},
		{"session", "show", "--session-id", "session-20260716-165900-cli"},
		{"session", "messages", "--session-id", "session-20260716-165900-cli"},
		{"session", "events", "--session-id", "session-20260716-165900-cli"},
		{"session", "configure", "--session-id", "session-20260716-165900-cli", "--runtime", "cli", "--profile", "shell"},
		{"session", "configure", "--session-id", "session-20260716-165900-cli", "--runtime", "terminal", "--profile", "shell"},
		{"session", "export", "--session-id", "session-20260716-165900-cli", "--output", historyExport},
		{"system", "update", "--dry-run", "--version", "v1.2.3"},
		{"session", "run", "native-mock", "--session-id", "session-20260716-170000-clitest", "--run-id", "turn-20260716-170000-clitest", "hello"},
		{"run", "show", "--run-id", "turn-20260716-170000-clitest"},
		{"run", "logs", "--run-id", "turn-20260716-170000-clitest", "--tail", "5"},
		{"run", "result", "--run-id", "turn-20260716-170000-clitest"},
		{"run", "watch", "--run-id", "turn-20260716-170000-clitest", "--seconds", "1", "--poll-seconds", "0.01"},
		{"native-mock", "hello"},
		{"skill", "run", "review", "--input", "main.go"},
		{"loop", "run", "--loop-id", "loop-20260716-170003-cli", "--input", "hello", "--actions-json", `[{"type":"respond","content":"done"}]`},
		{"loop", "show", "--loop-id", "loop-20260716-170003-cli"},
		{"loop", "list", "--limit", "5"},
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
			_, _ = service.SessionStop(context.Background(), "session-20260716-170010-exec")
			cancelDaemon()
			select {
			case <-daemonDone:
			case <-time.After(time.Second):
			}
		})
		tmuxCommands := [][]string{
			{"system", "status"},
			{"session", "open", "shell", "--carrier", "tmux", "--session-id", "session-20260716-170010-cli", "--run-id", "session-20260716-170010-exec"},
			{"session", "show", "--session-id", "session-20260716-170010-cli"},
			{"session", "send", "--session-id", "session-20260716-170010-cli", "hello"},
			{"session", "logs", "--session-id", "session-20260716-170010-cli", "--tail", "10"},
			{"session", "stop", "--session-id", "session-20260716-170010-cli"},
			{"run", "watch", "--run-id", "session-20260716-170010-exec", "--seconds", "1", "--poll-seconds", "0.01"},
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
		{"profile"}, {"profile", "unknown"}, {"profile", "show"}, {"profile", "command"},
		{"profile", "validate", "-c", "native-mock"}, {"profile", "validate", "native-mock", "--provider", "native"},
		{"profile", "command", "shell", "-c", "other"},
		{"run"}, {"run", "unknown"}, {"run", "show"}, {"run", "command"},
		{"run", "command", "shell", "-c", "other", "--", "/bin/true"},
		{"session"}, {"session", "unknown"}, {"session", "list", "extra"}, {"session", "run"}, {"session", "run", "native-mock", "-c", "shell", "hello"}, {"session", "run", "native-mock", "--mode", "capture", "hello"}, {"session", "send"},
		{"system"}, {"system", "unknown"}, {"system", "status", "extra"}, {"system", "update", "--unknown"},
		{"skill"}, {"skill", "unknown"}, {"skill", "route"}, {"skill", "run-auto"},
		{"tool"}, {"tool", "unknown"}, {"tool", "schemas"}, {"tool", "open-url"}, {"tool", "describe-external"},
		{"memory"}, {"memory", "unknown"}, {"memory", "write"}, {"memory", "forget"}, {"memory", "candidates"}, {"memory", "sources"}, {"memory", "demo"},
		{"loop"}, {"loop", "unknown"}, {"loop", "start"}, {"loop", "step"}, {"loop", "status"},
		{"help"}, {"version"}, {"task"}, {"turn"}, {"runs"}, {"history"}, {"config"}, {"profiles"},
		{"providers"}, {"upgrade"}, {"capabilities"}, {"tools"}, {"command"}, {"clean"}, {"update"}, {"daemon"}, {"doctor"},
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
	if code, output := captureMain(t, []string{"system", "update", "--help"}); code != 0 || !strings.Contains(output, "sn-cli system update") {
		t.Fatalf("update help code=%d output=%q", code, output)
	}
}

func TestSessionRunAndSubmitPersistStructuredCrossProfileTurns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"alpha", "beta"} {
		body := fmt.Sprintf(`{"type":"native","label":%q,"native":{"system_prompt":"test","mock":{"responses":[%q],"done_after":1}}}`, profile, profile+" reply")
		if err := os.WriteFile(filepath.Join(home, "configs", profile+".json"), []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	sessionID := "session-20260721-160000-cross-profile"
	commands := [][]string{
		{"session", "run", "alpha", "--session-id", sessionID, "first"},
		{"session", "submit", "beta", "--session-id", sessionID, "second"},
	}
	for _, command := range commands {
		if code, output := captureMain(t, command); code != 0 {
			t.Fatalf("Main(%q) code=%d output=%q", command, code, output)
		}
	}
	view, err := agentrun.NewSessionManager(agentrun.New(home)).Store().View(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Session.Retention != agentrun.RetentionStandard || view.Session.RecordMode != agentrun.RecordFull || view.Session.CaptureQuality != agentrun.CaptureStructured {
		t.Fatalf("session policy=%#v", view.Session)
	}
	if len(view.Turns) != 2 || view.Turns[0].Profile != "alpha" || view.Turns[1].Profile != "beta" {
		t.Fatalf("turns=%#v", view.Turns)
	}
	if len(view.Messages) != 4 || view.Messages[0].Role != "user" || view.Messages[1].Role != "assistant" || view.Messages[2].Role != "user" || view.Messages[3].Role != "assistant" {
		t.Fatalf("messages=%#v", view.Messages)
	}
	if len(view.Executions) != 2 {
		t.Fatalf("executions=%#v", view.Executions)
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
	profile := `{"type":"cli","label":"Preview","cli":{"driver":"generic","executor":"command","command":{"binary":"never-execute","args":["--api-key","literal-secret","--endpoint=https://user:pass@example.test/v1?access=query-secret","environment-secret"],"model":"preview-model"},"runtime":{"prompt_delivery":"stdin","managed_args":["managed"]}}}`
	if err := os.WriteFile(filepath.Join(home, "configs", "preview.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	nativeProfile := `{"type":"native","native":{"mock":{"responses":["ok"],"done_after":1}}}`
	if err := os.WriteFile(filepath.Join(home, "configs", "native-mock.json"), []byte(nativeProfile), 0o644); err != nil {
		t.Fatal(err)
	}

	code, output := captureMain(t, []string{"profile", "command", "preview", "--json"})
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

	if code, output := captureMain(t, []string{"profile", "command", "native-mock"}); code != 1 || !strings.Contains(output, "not a CLI profile") {
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

func TestParseSessionStartOptionsUsesPositionalProfileAndSeparatesRawArgs(t *testing.T) {
	options, err := parseSessionStartOptions([]string{"cx", "--carrier", "tmux", "--session-id", "session-1", "review", "repo", "--", "--no-alt-screen"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Profile != "cx" || options.Carrier != "tmux" || options.SessionID != "session-1" || options.Prompt != "review repo" || !reflect.DeepEqual(options.RawCLIArgs, []string{"--no-alt-screen"}) {
		t.Fatalf("options=%#v", options)
	}
	if _, err := parseSessionStartOptions(nil); err == nil {
		t.Fatal("session open accepted a missing profile")
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
	if !strings.Contains(fmt.Sprint(result["message"]), "环境变量未设置: NATIVE_TEST_KEY") {
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
