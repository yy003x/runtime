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
	data := []byte("default_project: demo\ndefault_profile: fake\nmax_concurrency: 2\nmax_queue: 12\nqueue_timeout_seconds: 45\ndefault_deadline_seconds: 90\nsession:\n  default_carrier: terminal\n  terminal:\n    driver: iterm2\n")
	if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	if service.RunsDir != filepath.Join(root, "runs") || service.ConfigDir != filepath.Join(root, "configs") {
		t.Fatalf("service paths: %#v", service)
	}
	if service.DefaultProject != "demo" || service.DefaultProfile != "fake" || service.MaxConcurrency != 2 || service.MaxQueue != 12 || service.QueueTimeout != 45 || service.DefaultDeadline != 90 {
		t.Fatalf("service settings: %#v", service)
	}
	if service.DefaultCarrier != "terminal" || service.TerminalDriver != "iterm2" {
		t.Fatalf("carrier settings: %#v", service)
	}
}

func TestRuntimeSettingsRejectUnknownCarrierAndTerminalDriver(t *testing.T) {
	for name, body := range map[string]string{
		"carrier":          "session:\n  default_carrier: screen\n",
		"driver":           "session:\n  terminal:\n    driver: auto\n",
		"unknown":          "runs_dir: ignored/legacy\n",
		"negative-timeout": "default_deadline_seconds: -1\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := New(root).Profiles(); err == nil {
				t.Fatal("invalid carrier settings were accepted")
			}
		})
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

func TestEmptyRuntimeSettingsUseDefaults(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	if service.configErr != nil || service.DefaultProfile != "cx" || service.DefaultDeadline != 300 || service.DefaultCarrier != "tmux" {
		t.Fatalf("service=%#v", service)
	}
}
