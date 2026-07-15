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
	command := tmuxShellCommandArgv(cfg, []string{"agent cli", "--model", "override"}, map[string]string{"AGENTRUN_RUN_ID": "task-1"})
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
	command := TmuxCommandEnv(cfg, "agent run")
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
	command := TmuxShellCommand(cfg, nil)
	for _, expected := range []string{"'/tmp/sn-test-home/agent'", "'instructions=/tmp/sn-test-home/AGENTS.md'", "'configured-model'"} {
		if !strings.Contains(command, expected) {
			t.Fatalf("command %q missing %q", command, expected)
		}
	}
}

func TestShellQuote(t *testing.T) {
	if got := ShellQuote("a'b"); got != `'a'"'"'b'` {
		t.Fatalf("ShellQuote=%q", got)
	}
}
