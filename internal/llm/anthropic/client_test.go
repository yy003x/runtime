package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yy003x/runtime/internal/llm"
)

func TestClientRetriesOn529(t *testing.T) {
	attempts := 0
	client := NewClient("https://anthropic.test", "test-token", &http.Client{
		Transport: roundTripperFunc(func(request *http.Request) (*http.Response, error) {
			attempts++
			status := http.StatusOK
			body := map[string]any{"content": []map[string]any{{"type": "text", "text": "ok after retry"}}, "usage": map[string]any{"input_tokens": 10, "output_tokens": 10}}
			if attempts < 5 {
				status = 529
				body = map[string]any{"type": "error", "error": map[string]any{"type": "overloaded_error", "message": "overloaded"}}
			}
			payload, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}
			return &http.Response{StatusCode: status, Header: make(http.Header), Body: io.NopCloser(bytes.NewReader(payload)), Request: request}, nil
		}),
	})
	response, err := client.Generate(context.Background(), llm.Request{
		Model: "test-model", System: "system", Messages: []llm.Message{{Role: "user", Content: "hello"}}, MaxTokens: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.OutputText != "ok after retry" || attempts != 5 {
		t.Fatalf("response=%#v attempts=%d", response, attempts)
	}
}

func TestClientAvoidsDuplicateV1AndParsesToolUse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/api/v1/messages" {
			t.Fatalf("path=%q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Token secret" || request.Header.Get("x-api-key") != "" || request.Header.Get("X-Test") != "runtime" {
			t.Fatalf("headers=%v", request.Header)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		messages := body["messages"].([]any)
		last := messages[len(messages)-1].(map[string]any)
		if last["role"] != "user" || len(last["content"].([]any)) != 2 {
			t.Fatalf("messages=%#v", messages)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"content":[{"type":"tool_use","id":"tool-2","name":"echo","input":{"value":"ok"}}],"stop_reason":"tool_use","usage":{"input_tokens":9,"output_tokens":2}}`))
	}))
	defer server.Close()

	client := NewClientWithOptions(server.URL+"/api/v1", "secret", server.Client(), llm.HTTPOptions{
		Headers: map[string]string{"X-Test": "runtime"}, AuthHeader: "Authorization", AuthPrefix: "Token ",
	})
	response, err := client.Generate(context.Background(), llm.Request{
		Model: "model", Messages: []llm.Message{
			{Role: "assistant", ToolCalls: []llm.ToolCall{{ID: "one", Name: "echo", Arguments: map[string]any{"a": 1}}}},
			{Role: "tool", ToolCallID: "one", Content: `{"a":1}`},
			{Role: "tool", ToolCallID: "two", Content: `{"b":2}`},
		},
		Tools: []llm.Tool{{Name: "echo", Parameters: map[string]any{"type": "object"}}}, MaxTokens: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Done || response.FinishReason != "tool_use" || len(response.ToolCalls) != 1 || response.ToolCalls[0].ID != "tool-2" {
		t.Fatalf("response=%#v", response)
	}
}

func TestClientGenerateStreamEmitsDeltasAndBuildsToolUse(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if body["stream"] != true || request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("request=%#v headers=%v", body, request.Header)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("event: message_start\ndata: {\"type\":\"message_start\",\"message\":{\"usage\":{\"input_tokens\":8}}}\n\n"))
		_, _ = writer.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":0,\"content_block\":{\"type\":\"text\",\"text\":\"Hi \"}}\n\n"))
		_, _ = writer.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":0,\"delta\":{\"type\":\"text_delta\",\"text\":\"there\"}}\n\n"))
		_, _ = writer.Write([]byte("event: content_block_start\ndata: {\"type\":\"content_block_start\",\"index\":1,\"content_block\":{\"type\":\"tool_use\",\"id\":\"tool-1\",\"name\":\"echo\",\"input\":{}}}\n\n"))
		_, _ = writer.Write([]byte("event: content_block_delta\ndata: {\"type\":\"content_block_delta\",\"index\":1,\"delta\":{\"type\":\"input_json_delta\",\"partial_json\":\"{\\\"value\\\":\\\"ok\\\"}\"}}\n\n"))
		_, _ = writer.Write([]byte("event: message_delta\ndata: {\"type\":\"message_delta\",\"delta\":{\"stop_reason\":\"tool_use\"},\"usage\":{\"output_tokens\":5}}\n\n"))
		_, _ = writer.Write([]byte("event: message_stop\ndata: {\"type\":\"message_stop\"}\n\n"))
	}))
	defer server.Close()

	var deltas []string
	response, err := NewClient(server.URL, "secret", server.Client()).GenerateStream(
		context.Background(),
		llm.Request{Model: "model", Messages: []llm.Message{{Role: "user", Content: "hello"}}, MaxTokens: 64},
		func(event llm.StreamEvent) error {
			deltas = append(deltas, event.Delta)
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(deltas, "") != "Hi there" || response.OutputText != "Hi there" {
		t.Fatalf("deltas=%v response=%#v", deltas, response)
	}
	if len(response.ToolCalls) != 1 || response.ToolCalls[0].Arguments["value"] != "ok" ||
		response.InputTokens != 8 || response.OutputTokens != 5 {
		t.Fatalf("response=%#v", response)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
