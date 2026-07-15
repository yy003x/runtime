package cli

import (
	"reflect"
	"testing"
	"time"

	"agent-runtime/internal/agentrun"
	"agent-runtime/internal/daemon"
	"agent-runtime/internal/provider"
)

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
