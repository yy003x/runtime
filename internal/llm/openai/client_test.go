package openai

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yy003x/runtime/internal/llm"
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
		Tools: []llm.Tool{{Name: "echo", Parameters: map[string]any{"type": "object"}}}, MaxTokens: 128,
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

func TestClientGenerateStreamEmitsDeltasAndBuildsToolCall(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["stream"] != true || request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("request=%#v headers=%v", body, request.Header)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"Hel\"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"lo\",\"tool_calls\":[{\"index\":0,\"id\":\"call-1\",\"function\":{\"name\":\"echo\",\"arguments\":\"{\\\"value\\\":\"}}]}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"tool_calls\":[{\"index\":0,\"function\":{\"arguments\":\"\\\"ok\\\"}\"}}]},\"finish_reason\":\"tool_calls\"}],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":4}}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	var deltas []string
	response, err := NewClient(server.URL, "secret", server.Client()).GenerateStream(
		context.Background(),
		llm.Request{Model: "model", Messages: []llm.Message{{Role: "user", Content: "hello"}}},
		func(event llm.StreamEvent) error {
			deltas = append(deltas, event.Delta)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "Hello" || response.OutputText != "Hello" {
		t.Fatalf("deltas=%v response=%#v", deltas, response)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Arguments["value"] != "ok" ||
		response.InputTokens != 7 || response.OutputTokens != 4 {
		t.Fatalf("response=%#v", response)
	}
}
