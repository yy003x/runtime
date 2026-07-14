package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"testing"

	"agent-runtime/internal/llm"
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
		Model: "test-model", System: "system", Messages: []llm.Message{{Role: "user", Content: "hello"}}, MaxOutputTokens: 128,
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.OutputText != "ok after retry" || attempts != 5 {
		t.Fatalf("response=%#v attempts=%d", response, attempts)
	}
}

type roundTripperFunc func(*http.Request) (*http.Response, error)

func (function roundTripperFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
