package agentrun

import (
	"os"
	"path/filepath"
	"testing"
)

func TestNewLoadsRuntimeSettings(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	data := []byte("runs_dir: var/runs\ndefault_project: demo\ndefault_profile: fake\nmax_concurrency: 2\nprovider_config_dir: providers\n")
	if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	if service.RunsDir != filepath.Join(root, "var/runs") || service.ConfigDir != filepath.Join(root, "providers") {
		t.Fatalf("service paths: %#v", service)
	}
	if service.DefaultProject != "demo" || service.DefaultProfile != "fake" || service.MaxConcurrency != 2 {
		t.Fatalf("service settings: %#v", service)
	}
}

func TestMalformedRuntimeSettingsBlocksProfileLoading(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), []byte("max_concurrency: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root).Profiles(); err == nil {
		t.Fatal("Profiles accepted malformed runtime settings")
	}
}
