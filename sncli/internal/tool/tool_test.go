package tool

import (
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-arch/sncli/internal/config"
)

func TestRunnerPassesArgsAndEnv(t *testing.T) {
	tmp := t.TempDir()
	bin := filepath.Join(tmp, "fake-tool")
	out := filepath.Join(tmp, "out.txt")
	script := "#!/usr/bin/env bash\nprintf 'root=%s\\nfoo=%s\\nargs=%s\\n' \"$SN_CLI_ROOT\" \"$FOO\" \"$*\" > \"$OUT_FILE\"\n"
	if err := os.WriteFile(bin, []byte(script), 0755); err != nil {
		t.Fatalf("write fake tool: %v", err)
	}
	t.Setenv("PATH", tmp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("OUT_FILE", out)

	code, err := (Runner{Root: "/runtime"}).Run("fake", config.ToolConfig{
		Command: "fake-tool",
		Args:    []string{"base"},
		Env: map[string]string{
			"FOO": "bar",
		},
	}, []string{"extra"})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if code != 0 {
		t.Fatalf("code = %d", code)
	}
	data, err := os.ReadFile(out)
	if err != nil {
		t.Fatalf("read output: %v", err)
	}
	got := string(data)
	for _, want := range []string{"root=/runtime", "foo=bar", "args=base extra"} {
		if !strings.Contains(got, want) {
			t.Fatalf("output missing %q: %s", want, got)
		}
	}
}
