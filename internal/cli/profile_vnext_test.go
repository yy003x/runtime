package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/model"
)

func TestVNextProfileManagementAggregatesCatalogs(t *testing.T) {
	paths := prepareVNextHome(t)
	writeVNextCommand(t, paths.ConfigDir, "cx")
	writeVNextModel(t, paths.ConfigDir, "api-cx", "https://example.invalid/v1/chat/completions")
	for _, args := range [][]string{
		{"list"}, {"show", "cx"}, {"show", "api-cx"}, {"check"}, {"check", "cx"},
	} {
		output := captureStdout(t, func() {
			if err := runVNextProfileNamespace(paths, args); err != nil {
				t.Fatalf("args=%q error=%v", args, err)
			}
		})
		if !strings.Contains(output, `"ok": true`) {
			t.Fatalf("args=%q output=%s", args, output)
		}
	}
}

func TestVNextDirectModelReturnsCompletedWithoutRuntimeRecords(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%q", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
		  "id":"req-1",
		  "model":"fixture",
		  "choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
		}`))
	}))
	defer server.Close()

	paths := prepareVNextHome(t)
	writeVNextModel(t, paths.ConfigDir, "api-cx", server.URL+"/v1/chat/completions")
	t.Setenv("MODEL_API_KEY", "secret")
	originalTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	output := captureStdout(t, func() {
		if err := runVNextProfileNamespace(paths, []string{"api-cx", "hello"}); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(output, `"state": "completed"`) ||
		!strings.Contains(output, `"content": "OK"`) {
		t.Fatalf("output=%s", output)
	}
	for _, name := range []string{"sessions", "runs"} {
		entries, err := os.ReadDir(filepath.Join(paths.Home, name))
		if err == nil && len(entries) != 0 {
			t.Fatalf("%s entries=%v", name, entries)
		}
	}
}

func TestParseDirectModelInputRejectsLegacyModelOverride(t *testing.T) {
	openAIProfile := vNextTestModelProfile("openai-compatible")
	if _, _, err := parseDirectModelInput(
		"api", openAIProfile, []string{"--model", "other", "hello"},
	); err == nil {
		t.Fatal("legacy --model override was accepted")
	}
	request, stream, err := parseDirectModelInput(
		"api",
		openAIProfile,
		[]string{
			"--stream", "--temperature", "0.2",
			"--max-completion-tokens", "128", "hello",
		},
	)
	if err != nil || !stream || request.Input.Options.Temperature == nil ||
		request.Input.Options.MaxOutputTokens == nil ||
		*request.Input.Options.MaxOutputTokens != 128 ||
		len(request.Input.Messages) != 1 {
		t.Fatalf("request=%#v stream=%v error=%v", request, stream, err)
	}
	if _, _, err := parseDirectModelInput(
		"api", openAIProfile, []string{"--max-tokens", "128", "hello"},
	); err == nil {
		t.Fatal("Anthropic max_tokens was accepted for OpenAI")
	}
	anthropicProfile := vNextTestModelProfile("anthropic-compatible")
	if _, _, err := parseDirectModelInput(
		"api", anthropicProfile, []string{"--max-tokens", "128", "hello"},
	); err != nil {
		t.Fatalf("Anthropic max_tokens rejected: %v", err)
	}
}

func vNextTestModelProfile(driver string) model.Profile {
	return model.Profile{Driver: model.DriverName(driver)}
}

func prepareVNextHome(t *testing.T) layout.Paths {
	t.Helper()
	paths, err := layout.FromHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.CommandDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return paths
}

func writeVNextCommand(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(dir, id+".json"),
		[]byte(`{"type":"cli","binary":"codex","transport":"tty","prompt_delivery":"manual"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}

func writeVNextModel(t *testing.T, dir, id, endpoint string) {
	t.Helper()
	value := map[string]any{
		"type":   "api",
		"driver": "openai-compatible", "endpoint": endpoint, "model": "fixture",
		"auth": map[string]any{
			"header": "Authorization", "scheme": "Bearer", "from_env": "MODEL_API_KEY",
		},
		"defaults": map[string]any{"max_completion_tokens": 1024},
		"timeout":  "1m",
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s.json", id)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
