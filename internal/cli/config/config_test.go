package config

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yy003x/runtime/internal/layout"
)

func TestResolvePathAndUpdateDefaults(t *testing.T) {
	config := &Config{Home: "/home/test/.sn"}
	if got := config.ResolvePath("runs/task"); got != "/home/test/.sn/runs/task" {
		t.Fatalf("ResolvePath=%s", got)
	}
	if !config.UpdateEnabled() {
		t.Fatal("updates should default to enabled")
	}
}

func TestLoadUsesSNCLIHomeWithoutRepository(t *testing.T) {
	home := filepath.Join(t.TempDir(), ".sn")
	t.Setenv("SN_CLI_HOME", home)
	config, err := Load()
	if err != nil {
		t.Fatal(err)
	}
	home, err = layout.CanonicalHome(home)
	if err != nil {
		t.Fatal(err)
	}
	if config.Home != home ||
		config.Paths.ConfigDir != filepath.Join(home, "configs") ||
		config.Paths.RuntimeConfigFile != filepath.Join(home, "runtime.json") {
		t.Fatalf("unexpected config: %#v", config)
	}
	if _, err := os.Stat(filepath.Join(home, "sessions")); err != nil {
		t.Fatalf("runtime tree not created: %v", err)
	}
}
