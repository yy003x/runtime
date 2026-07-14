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
		},
		Tmux: &TmuxConfig{SessionName: "test"},
	}}
	command := tmuxShellCommandArgv(cfg, []string{"agent cli", "--model", "override"}, map[string]string{"AGENTRUN_RUN_ID": "task-1"})
	for _, expected := range []string{"exec env", "'AGENTRUN_RUN_ID=task-1'", "'BASE=configured'", "'agent cli'", "'--model'", "'override'"} {
		if !strings.Contains(command, expected) {
			t.Fatalf("command %q missing %q", command, expected)
		}
	}
	if strings.Contains(command, "--base") {
		t.Fatalf("command must use prepared argv: %q", command)
	}
}

func TestShellQuote(t *testing.T) {
	if got := ShellQuote("a'b"); got != `'a'"'"'b'` {
		t.Fatalf("ShellQuote=%q", got)
	}
}
