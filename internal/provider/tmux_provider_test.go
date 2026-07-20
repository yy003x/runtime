package provider

import (
	"strings"
	"testing"
)

func TestTmuxShellCommandUsesPreparedEnvironment(t *testing.T) {
	cfg := Config{CLI: &CLIConfig{
		Command: CommandConfig{
			Binary:         "agent cli",
			Args:           []string{"--base", "value"},
			Env:            map[string]string{"BASE": "configured"},
			EnvPassthrough: []string{},
			EnvUnset:       []string{"ANTHROPIC_AUTH_TOKEN"},
		},
		Tmux: &TmuxConfig{SessionName: "test"},
	}}
	command, err := tmuxShellCommandArgv(cfg, []string{"agent cli", "--model", "override"}, map[string]string{"AGENTRUN_RUN_ID": "task-1"})
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"exec env", "-u 'ANTHROPIC_AUTH_TOKEN'", "'AGENTRUN_RUN_ID=task-1'", "'BASE=configured'", "'agent cli'", "'--model'", "'override'"} {
		if !strings.Contains(command, expected) {
			t.Fatalf("command %q missing %q", command, expected)
		}
	}
	if strings.Contains(command, "--base") {
		t.Fatalf("command must use prepared argv: %q", command)
	}
}

func TestTmuxCommandEnvSupportsUnsetWithoutConfiguredValues(t *testing.T) {
	cfg := Config{CLI: &CLIConfig{Command: CommandConfig{EnvUnset: []string{"CODEX_HOME"}}}}
	command, err := TmuxCommandEnv(cfg, "agent run")
	if err != nil {
		t.Fatal(err)
	}
	if command != "env -u 'CODEX_HOME' agent run" {
		t.Fatalf("command=%q", command)
	}
}

func TestTmuxShellCommandExpandsConfiguredValues(t *testing.T) {
	t.Setenv("SN_TEST_HOME", "/tmp/sn-test-home")
	t.Setenv("SN_TEST_MODEL", "configured-model")
	cfg := Config{CLI: &CLIConfig{
		Command: CommandConfig{
			Binary: "${SN_TEST_HOME}/agent",
			Args:   []string{"--config", "instructions=${SN_TEST_HOME}/AGENTS.md"},
			Model:  "${SN_TEST_MODEL}",
		},
	}}
	command, err := TmuxShellCommand(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, expected := range []string{"'/tmp/sn-test-home/agent'", "'instructions=/tmp/sn-test-home/AGENTS.md'", "'configured-model'"} {
		if !strings.Contains(command, expected) {
			t.Fatalf("command %q missing %q", command, expected)
		}
	}
}

func TestDaemonExecutionResolvesOnlyBracedUpstreamReferences(t *testing.T) {
	t.Setenv("SN_TEST_PROXY", "http://127.0.0.1:8080")
	cfg := Config{ID: "proxy", Execution: ExecutionConfig{
		AuditProxy: true,
		Upstreams:  []string{"${SN_TEST_PROXY}", "SN_TEST_PROXY"},
	}}

	execution, err := daemonExecution(cfg)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(execution.Upstreams, "|"); got != "http://127.0.0.1:8080|SN_TEST_PROXY" {
		t.Fatalf("upstreams=%q", got)
	}
}

func TestShellQuote(t *testing.T) {
	if got := ShellQuote("a'b"); got != `'a'"'"'b'` {
		t.Fatalf("ShellQuote=%q", got)
	}
}

func TestTmuxSessionProfileUsesSnAgentNamespace(t *testing.T) {
	cfg, err := AsTmuxSessionProfile(Config{Type: TypeCLI, CLI: &CLIConfig{Driver: "generic"}})
	if err != nil {
		t.Fatal(err)
	}
	if cfg.CLI.Tmux.SessionName != DefaultTmuxSessionName {
		t.Fatalf("session_name=%q, want %q", cfg.CLI.Tmux.SessionName, DefaultTmuxSessionName)
	}
	if strings.Contains(cfg.CLI.Tmux.SessionName, "mz-cli-agent") {
		t.Fatalf("session_name must not use legacy namespace: %q", cfg.CLI.Tmux.SessionName)
	}
	if cfg.CLI.Tmux.PasteBracketed {
		t.Fatal("generic CLI must use line-oriented paste by default")
	}
	codex, err := AsTmuxSessionProfile(Config{Type: TypeCLI, CLI: &CLIConfig{Driver: "codex"}})
	if err != nil || !codex.CLI.Tmux.PasteBracketed {
		t.Fatalf("codex tmux=%#v err=%v", codex.CLI.Tmux, err)
	}
}
