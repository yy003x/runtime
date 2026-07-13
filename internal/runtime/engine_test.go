package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestEngineRunFakeProfileWritesArtifacts(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "fake", `name: fake
provider:
  type: fake
  echo_prefix: "echo: "
runtime:
  timeout_seconds: 30
artifacts:
  root: runs/global/runtime
`)

	result, err := NewEngine(root).Run(context.Background(), RunOptions{
		Profile: "fake",
		Prompt:  "hello",
		CWD:     root,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.Status != StatusSucceeded {
		t.Fatalf("status=%s, want %s", result.Status, StatusSucceeded)
	}
	if result.FinalText != "echo: hello" {
		t.Fatalf("final_text=%q, want %q", result.FinalText, "echo: hello")
	}

	for name, path := range map[string]string{
		"run_dir":         result.Artifacts["run_dir"],
		"request":         result.Artifacts["request"],
		"resolved_config": result.Artifacts["resolved_config"],
		"events":          result.Artifacts["events"],
		"stdout":          result.Artifacts["stdout"],
		"stderr":          result.Artifacts["stderr"],
		"output":          result.Artifacts["output"],
		"result":          result.Artifacts["result"],
		"artifacts_dir":   result.Artifacts["artifacts_dir"],
	} {
		if path == "" {
			t.Fatalf("artifact %s path is empty", name)
		}
		if _, err := os.Stat(path); err != nil {
			t.Fatalf("artifact %s not found at %s: %v", name, path, err)
		}
	}

	output, err := os.ReadFile(result.Artifacts["output"])
	if err != nil {
		t.Fatalf("read output artifact: %v", err)
	}
	if string(output) != "echo: hello" {
		t.Fatalf("output artifact=%q, want %q", string(output), "echo: hello")
	}
}

func TestEngineRunMissingProfileWritesFailedResult(t *testing.T) {
	root := t.TempDir()

	result, err := NewEngine(root).Run(context.Background(), RunOptions{
		Profile: "missing",
		Prompt:  "hello",
		CWD:     root,
	})
	if err == nil {
		t.Fatal("Run returned nil error for missing profile")
	}
	if result == nil {
		t.Fatal("Run returned nil result for missing profile")
	}
	if result.Status != StatusFailed {
		t.Fatalf("status=%s, want %s", result.Status, StatusFailed)
	}
	if result.Artifacts["result"] == "" {
		t.Fatal("result artifact path is empty")
	}
	if _, statErr := os.Stat(result.Artifacts["result"]); statErr != nil {
		t.Fatalf("result artifact not found: %v", statErr)
	}

	stderr, readErr := os.ReadFile(result.Artifacts["stderr"])
	if readErr != nil {
		t.Fatalf("read stderr artifact: %v", readErr)
	}
	if !strings.Contains(string(stderr), "read profile") {
		t.Fatalf("stderr artifact=%q, want read profile error", string(stderr))
	}
}

func TestEngineRejectsArtifactsRootOutsideRuns(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "bad", `name: bad
provider:
  type: fake
artifacts:
  root: ../outside
`)

	result, err := NewEngine(root).Run(context.Background(), RunOptions{
		Profile: "bad",
		Prompt:  "hello",
		CWD:     root,
	})
	if err == nil {
		t.Fatal("Run returned nil error for unsafe artifacts root")
	}
	if result != nil {
		t.Fatalf("result=%#v, want nil when artifact paths cannot be created", result)
	}
}

func writeProfile(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "configs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create configs dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}
