package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandEnvPrefersRepoPython(t *testing.T) {
	root := t.TempDir()
	venvBin := filepath.Join(root, ".venv", "bin")
	if err := os.MkdirAll(venvBin, 0755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", "/usr/bin")
	t.Setenv("PYTHONPATH", "/tmp/old")

	cmd := Client{Command: "runtime-provider", Root: root}.command("providers")

	if got := envValue(cmd.Env, "SINAN_ROOT"); got != root {
		t.Fatalf("SINAN_ROOT=%q, want %q", got, root)
	}
	if got := envValue(cmd.Env, "PATH"); !strings.HasPrefix(got, venvBin+string(os.PathListSeparator)) {
		t.Fatalf("PATH=%q does not start with %q", got, venvBin)
	}
	if got := envValue(cmd.Env, "PYTHONPATH"); !strings.HasPrefix(got, root+string(os.PathListSeparator)) {
		t.Fatalf("PYTHONPATH=%q does not start with %q", got, root)
	}
}

func envValue(env []string, key string) string {
	prefix := key + "="
	value := ""
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			value = strings.TrimPrefix(item, prefix)
		}
	}
	return value
}
