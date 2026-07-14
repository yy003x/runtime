package config

import (
	"os"
	"path/filepath"
	"testing"
)

func TestResolvePathAndUpdateDefaults(t *testing.T) {
	config := &Config{Root: "/repo"}
	if got := config.ResolvePath("runs/global/sn-cli"); got != "/repo/runs/global/sn-cli" {
		t.Fatalf("ResolvePath=%s", got)
	}
	if !config.UpdateEnabled() {
		t.Fatal("updates should default to enabled")
	}
}

func TestFindRootRecognizesUnifiedLayout(t *testing.T) {
	root := t.TempDir()
	for _, path := range []string{"go.mod", "cmd/sn-cli/main.go", "configs/runtime.yaml"} {
		full := filepath.Join(root, path)
		if err := os.MkdirAll(filepath.Dir(full), 0o755); err != nil {
			t.Fatal(err)
		}
		if err := os.WriteFile(full, []byte("test"), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	nested := filepath.Join(root, "nested", "dir")
	if err := os.MkdirAll(nested, 0o755); err != nil {
		t.Fatal(err)
	}
	original, _ := os.Getwd()
	if err := os.Chdir(nested); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Chdir(original) })
	t.Setenv("SN_CLI_ROOT", "")
	got, err := FindRoot()
	got, _ = filepath.EvalSymlinks(got)
	want, _ := filepath.EvalSymlinks(root)
	if err != nil || got != want {
		t.Fatalf("FindRoot=(%q,%v), want %q", got, err, root)
	}
}
