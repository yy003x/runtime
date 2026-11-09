package codingbootstrap

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yy003x/sn-code/internal/infrastructure/layout"
)

func TestLoadIsIndependentOfRuntimeExecutionState(t *testing.T) {
	for _, brokenRuntime := range []bool{false, true} {
		name := "absent"
		if brokenRuntime {
			name = "invalid"
		}
		t.Run(name, func(t *testing.T) {
			paths, err := layout.FromHome(t.TempDir())
			if err != nil {
				t.Fatal(err)
			}
			if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
				t.Fatal(err)
			}
			profiles := map[string]string{
				"cli.json": `{"type":"cli","command":"codex","model":"fixture"}`,
				"api.json": `{"type":"api","driver":"openai-chat-completions","base_url":"https://example.invalid","model":"fixture","headers":{"Authorization":"${SN_BOUNDARY_TEST_KEY}"},"timeout":"1m"}`,
			}
			for name, value := range profiles {
				if err := os.WriteFile(filepath.Join(paths.ConfigDir, name), []byte(value), 0o600); err != nil {
					t.Fatal(err)
				}
			}
			if brokenRuntime {
				if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
					t.Fatal(err)
				}
				for _, path := range []string{paths.RuntimeConfigFile, paths.ToolsDir, paths.SessionsDir, paths.RunDBFile} {
					if err := os.WriteFile(path, []byte("invalid runtime state"), 0o600); err != nil {
						t.Fatal(err)
					}
				}
			}
			services, err := Load(paths)
			if err != nil {
				t.Fatal(err)
			}
			ids := services.Profiles.IDs()
			if len(ids) != 1 || ids[0] != "api" {
				t.Fatalf("API profiles=%v", ids)
			}
			if _, ok := services.Profiles.Get("cli"); ok {
				t.Fatal("CLI profile exposed to coding product")
			}
			if services.AgentDir != filepath.Join(paths.Home, "sncode") {
				t.Fatalf("agent dir=%s", services.AgentDir)
			}
			if !brokenRuntime {
				for _, path := range []string{paths.RunDBFile, paths.SessionsDir, paths.LogsDir, paths.AuthFile} {
					if _, err := os.Stat(path); !os.IsNotExist(err) {
						t.Fatalf("composition created %s: %v", path, err)
					}
				}
			}
		})
	}
}
