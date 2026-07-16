package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"agent-runtime/internal/llm"
)

func TestClientUsesCompatibleChatCompletionsAndParsesToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Fatalf("path=%q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("X-Test") != "runtime" {
			t.Fatalf("headers=%v", request.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if len(body["tools"].([]any)) != 1 || len(body["messages"].([]any)) != 2 {
			t.Fatalf("body=%#v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":"","tool_calls":[{"id":"call-1","type":"function","function":{"name":"echo","arguments":"{\"value\":\"ok\"}"}}]},"finish_reason":"tool_calls"}],"usage":{"prompt_tokens":12,"completion_tokens":3}}`))
	}))
	defer server.Close()

	client := NewClientWithOptions(server.URL+"/v1", "secret", server.Client(), llm.HTTPOptions{Headers: map[string]string{"X-Test": "runtime"}})
	response, err := client.Generate(context.Background(), llm.Request{
		Model: "model", System: "system", Messages: []llm.Message{{Role: "user", Content: "hello"}},
		Tools: []llm.Tool{{Name: "echo", Parameters: map[string]any{"type": "object"}}}, MaxOutputTokens: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Done || response.FinishReason != "tool_calls" || len(response.ToolCalls) != 1 {
		t.Fatalf("response=%#v", response)
	}
	if response.ToolCalls[0].Name != "echo" || response.ToolCalls[0].Arguments["value"] != "ok" {
		t.Fatalf("tool_call=%#v", response.ToolCalls[0])
	}
}

func TestClientRejectsOversizedErrorBody(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		writer.WriteHeader(http.StatusBadGateway)
		_, _ = writer.Write([]byte(strings.Repeat("x", 70<<10)))
	}))
	defer server.Close()

	_, err := NewClient(server.URL, "secret", server.Client()).Generate(context.Background(), llm.Request{Model: "model"})
	if err == nil || !strings.Contains(err.Error(), "exceeds") {
		t.Fatalf("err=%v", err)
	}
}
