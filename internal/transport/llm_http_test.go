package transport

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/runtime/internal/agentrun"
	"github.com/yy003x/runtime/llmruntime"
	"github.com/yy003x/runtime/runtimeapi"
)

func TestLLMGenerateUsesConfiguredRuntime(t *testing.T) {
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input["max_tokens"] != float64(512) {
			t.Fatalf("max_tokens=%v", input["max_tokens"])
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": "ok"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer providerServer.Close()

	home := t.TempDir()
	configDir := filepath.Join(home, "configs")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	profile, _ := json.Marshal(map[string]any{
		"protocol": "openai", "base_url": providerServer.URL,
		"model": "test-model", "api_key": "${TEST_HTTP_LLM_KEY}",
	})
	if err := os.WriteFile(filepath.Join(configDir, "test.json"), profile, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_HTTP_LLM_KEY", "secret")
	runtime, err := llmruntime.New(llmruntime.Options{ProfileDir: configDir})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandlerWithOptions(agentrun.New(home), HTTPOptions{LLMRuntime: runtime})
	body, _ := json.Marshal(runtimeapi.Request{Profile: "test", Prompt: "hello", MaxTokens: 512})
	request := httptest.NewRequest(http.MethodPost, "/v1/llm/generate", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusOK {
		t.Fatalf("unexpected status %d: %s", response.Code, response.Body.String())
	}
	var output runtimeapi.Response
	if err := json.Unmarshal(response.Body.Bytes(), &output); err != nil {
		t.Fatal(err)
	}
	if output.Message.Content != "ok" || !output.Done {
		t.Fatalf("unexpected response: %#v", output)
	}
}

func TestLLMGenerateStreamsRuntimeEvents(t *testing.T) {
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var input map[string]any
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			t.Fatal(err)
		}
		if input["stream"] != true || request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("input=%#v headers=%v", input, request.Header)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"streamed\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer providerServer.Close()

	home := t.TempDir()
	configDir := filepath.Join(home, "configs")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	profile, _ := json.Marshal(map[string]any{
		"protocol": "openai", "base_url": providerServer.URL,
		"model": "test-model", "api_key": "${TEST_HTTP_STREAM_KEY}",
	})
	if err := os.WriteFile(filepath.Join(configDir, "test.json"), profile, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_HTTP_STREAM_KEY", "secret")
	runtime, err := llmruntime.New(llmruntime.Options{ProfileDir: configDir})
	if err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandlerWithOptions(agentrun.New(home), HTTPOptions{LLMRuntime: runtime})
	body, _ := json.Marshal(runtimeapi.Request{Profile: "test", Prompt: "hello"})
	request := httptest.NewRequest(http.MethodPost, "/v1/llm/generate", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	if response.Code != http.StatusOK ||
		!strings.HasPrefix(response.Header().Get("Content-Type"), "text/event-stream") {
		t.Fatalf("status=%d headers=%v body=%s", response.Code, response.Header(), response.Body.String())
	}
	for _, expected := range []string{
		`event: request.started`,
		`event: context.compiled`,
		`event: provider.started`,
		`event: output.delta`,
		`"delta":"streamed"`,
		`event: response.completed`,
		`"content":"streamed"`,
	} {
		if !strings.Contains(response.Body.String(), expected) {
			t.Fatalf("stream missing %q:\n%s", expected, response.Body.String())
		}
	}
}
