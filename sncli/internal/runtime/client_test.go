package runtime

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestClientRunsInProcessMockProfile(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "fake", `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key_env":"UNSET","mock":true}}`)
	result, err := (Client{Root: root}).Run(RunOptions{Provider: "fake", Prompt: "hello", CWD: root, Timeout: 30})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.Outcome != "succeeded" || !strings.Contains(result.FinalText, "[mock openai:mock]") {
		t.Fatalf("result=%#v", result)
	}
	if !strings.HasPrefix(result.Artifacts["run_dir"], filepath.Join(root, "runs", "global", "runtime")) {
		t.Fatalf("run_dir=%q", result.Artifacts["run_dir"])
	}
}

func TestProvidersJSONUsesRepositoryConfigs(t *testing.T) {
	root := t.TempDir()
	writeProfile(t, root, "fake", `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key_env":"UNSET","mock":true}}`)
	data, err := (Client{Root: root}).ProvidersJSON()
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"source":"configs"`) || !strings.Contains(string(data), `"fake"`) {
		t.Fatalf("providers=%s", data)
	}
}

func writeProfile(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "configs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
