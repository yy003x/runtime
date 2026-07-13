package runtime

import (
	"context"
	"encoding/json"
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

func TestEngineLoadsJSONProfileAndResolvesDefaultInputs(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "default-prompt.md"), []byte("from default file\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "default.png"), []byte("image"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := `{
  "name": "json-profile",
  "provider": {"type": "fake", "echo_prefix": "json: "},
  "runtime": {"timeout_seconds": 30},
  "input": {"prompt_file": "default-prompt.md", "images": ["default.png"]},
  "artifacts": {"root": "runs/global/runtime"}
}`
	if err := os.WriteFile(filepath.Join(root, "configs", "json-profile.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	writeProfile(t, root, "json-profile", `name: yaml-profile
provider:
  type: fake
  echo_prefix: "yaml: "
`)

	result, err := NewEngine(root).Run(context.Background(), RunOptions{
		Profile: "json-profile",
		CWD:     root,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.FinalText != "json: from default file\n" {
		t.Fatalf("final_text=%q", result.FinalText)
	}

	request := readRunRequest(t, result.Artifacts["request"])
	if request.PromptFile != filepath.Join(root, "default-prompt.md") {
		t.Fatalf("prompt_file=%q", request.PromptFile)
	}
	if len(request.Images) != 1 || request.Images[0] != filepath.Join(root, "default.png") {
		t.Fatalf("images=%#v", request.Images)
	}
}

func TestEngineCLIInputsOverrideJSONDefaults(t *testing.T) {
	root := t.TempDir()
	cwd := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := `{
  "provider": {"type": "fake"},
  "input": {"prompt": "default prompt", "images": ["default.png"]}
}`
	if err := os.WriteFile(filepath.Join(root, "configs", "override.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "default.png"), []byte("default"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "prompt.md"), []byte("CLI prompt\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(cwd, "cli.png"), []byte("cli"), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := NewEngine(root).Run(context.Background(), RunOptions{
		Profile:    "override",
		PromptFile: "prompt.md",
		Images:     []string{"cli.png"},
		ImagesSet:  true,
		CWD:        cwd,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.FinalText != "CLI prompt\n" {
		t.Fatalf("final_text=%q", result.FinalText)
	}
	request := readRunRequest(t, result.Artifacts["request"])
	if request.PromptFile != filepath.Join(cwd, "prompt.md") {
		t.Fatalf("prompt_file=%q", request.PromptFile)
	}
	if len(request.Images) != 1 || request.Images[0] != filepath.Join(cwd, "cli.png") {
		t.Fatalf("images=%#v", request.Images)
	}
}

func TestEngineInlinePromptOverridesJSONPromptFile(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "default.md"), []byte("default prompt"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile := `{
  "provider": {"type": "fake"},
  "input": {"prompt_file": "default.md"}
}`
	if err := os.WriteFile(filepath.Join(root, "configs", "inline.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}

	result, err := NewEngine(root).Run(context.Background(), RunOptions{
		Profile: "inline",
		Prompt:  "CLI inline prompt",
		CWD:     root,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	if result.FinalText != "CLI inline prompt" {
		t.Fatalf("final_text=%q", result.FinalText)
	}
	request := readRunRequest(t, result.Artifacts["request"])
	if request.PromptFile != "" {
		t.Fatalf("prompt_file=%q, want empty for inline override", request.PromptFile)
	}
}

func readRunRequest(t *testing.T, path string) RunRequest {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatalf("read request: %v", err)
	}
	var request RunRequest
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatalf("parse request: %v", err)
	}
	return request
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
