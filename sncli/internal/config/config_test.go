package config

import (
	"os"
	"path/filepath"
	"testing"
)

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

func TestFindRootRecognizesRepositoryLayout(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"sncli/cmd/sn-cli/main.go", "sncli/conf/default.json"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("{}"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	nested := filepath.Join(root, "nested", "dir")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	original, err := os.Getwd()
	if err != nil {
		t.Fatal(err)
	}
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	t.Setenv("SN_CLI_ROOT", "")
	got, err := FindRoot()
	want, _ := filepath.EvalSymlinks(root)
	got, _ = filepath.EvalSymlinks(got)
	if err != nil || got != want {
		t.Fatalf("FindRoot()=(%q,%v), want %q", got, err, want)
	}
}
