package config

import "testing"

func TestResolvePath(t *testing.T) {
	cfg := &Config{Root: "/repo"}
	if got := cfg.ResolvePath("runs/global/sn-cli"); got != "/repo/runs/global/sn-cli" {
		t.Fatalf("unexpected path: %s", got)
	}
}

func TestMergeToolConfig(t *testing.T) {
	base := ToolConfig{
		Command:     "claude",
		Args:        []string{"--dangerously-skip-permissions"},
		Description: "base",
		Env: map[string]string{
			"A": "1",
			"B": "2",
		},
	}
	overlay := ToolConfig{
		Args: []string{"--safe"},
		Env: map[string]string{
			"B": "override",
			"C": "3",
		},
	}
	got := mergeToolConfig(base, overlay)
	if got.Command != "claude" {
		t.Fatalf("Command = %q", got.Command)
	}
	if len(got.Args) != 1 || got.Args[0] != "--safe" {
		t.Fatalf("Args = %#v", got.Args)
	}
	if got.Env["A"] != "1" || got.Env["B"] != "override" || got.Env["C"] != "3" {
		t.Fatalf("Env = %#v", got.Env)
	}
}

func TestUpdateEnabledDefault(t *testing.T) {
	cfg := &Config{}
	applyDefaults(cfg)
	if !cfg.UpdateEnabled() {
		t.Fatal("UpdateEnabled should default to true")
	}
}
