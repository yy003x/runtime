package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
)

func TestDriverExecutionIdentity(t *testing.T) {
	identity := New(nil).ExecutionIdentity()
	if identity.Driver != model.DriverAnthropicMessages ||
		identity.Implementation != "runtime.provider.anthropic-messages" ||
		identity.ImplementationVersion != 1 {
		t.Fatalf("identity=%#v", identity)
	}
	if err := identity.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDriverStreamsToolUseWithoutRetry(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		if request.URL.Path != "/v1/messages" {
			t.Errorf("path=%q", request.URL.Path)
		}
		if request.Header.Get("x-api-key") != "secret" {
			t.Errorf("x-api-key=%q", request.Header.Get("x-api-key"))
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["top_p"] != float64(0.9) {
			t.Errorf("top_p=%v", body["top_p"])
		}
		stop, ok := body["stop_sequences"].([]any)
		if !ok || len(stop) != 1 || stop[0] != "END" {
			t.Errorf("stop_sequences=%v", body["stop_sequences"])
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(writer, `event: message_start`)
		fmt.Fprintln(writer, `data: {"type":"message_start","message":{"id":"msg-1","model":"wire-model","usage":{"input_tokens":8}}}`)
		fmt.Fprintln(writer, `event: content_block_start`)
		fmt.Fprintln(writer, `data: {"type":"content_block_start","index":0,"content_block":{"type":"text","text":"hello"}}`)
		fmt.Fprintln(writer, `event: content_block_start`)
		fmt.Fprintln(writer, `data: {"type":"content_block_start","index":1,"content_block":{"type":"tool_use","id":"call-1","name":"weather","input":{}}}`)
		fmt.Fprintln(writer, `event: content_block_delta`)
		fmt.Fprintln(writer, `data: {"type":"content_block_delta","index":1,"delta":{"type":"input_json_delta","partial_json":"{\"city\":\"Shanghai\"}"}}`)
		fmt.Fprintln(writer, `event: message_delta`)
		fmt.Fprintln(writer, `data: {"type":"message_delta","delta":{"stop_reason":"tool_use"},"usage":{"output_tokens":5}}`)
	}))
	defer server.Close()

	maxTokens := int64(1024)
	topP := 0.9
	catalog, err := model.NewCatalog(map[string]model.Profile{
		"api": {
			Driver:  model.DriverAnthropicMessages,
			BaseURL: server.URL + "/v1", Model: "fixture",
			Headers: map[string]string{"x-api-key": "${MODEL_API_KEY}"},
			Parameters: model.Parameters{
				MaxTokens: &maxTokens, TopP: &topP,
				StopSequences: []string{"END"},
			},
			Timeout: "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := model.NewService(
		catalog,
		map[model.DriverName]model.Driver{model.DriverAnthropicMessages: New(server.Client())},
		model.ServiceOptions{Getenv: func(name string) (string, bool) {
			return "secret", name == "MODEL_API_KEY"
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	result, runtimeErr := service.Generate(context.Background(), contract.GenerateRequest{
		ModelProfile: "api",
		Input: contract.ModelRequest{
			Messages: []contract.Message{{Role: contract.RoleUser, Content: "hello"}},
		},
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if attempts.Load() != 1 || result.FinishReason != contract.FinishToolCall ||
		len(result.Message.ToolCalls) != 1 ||
		string(result.Message.ToolCalls[0].Arguments) != `{"city":"Shanghai"}` ||
		result.Provider.RequestID != "msg-1" {
		t.Fatalf("attempts=%d result=%#v", attempts.Load(), result)
	}
}

func TestDriverRequiresMaxTokens(t *testing.T) {
	profile := model.Profile{
		Driver:   model.DriverAnthropicMessages,
		Endpoint: "https://example.invalid/v1/messages", Model: "fixture",
		Headers: map[string]string{"x-api-key": "${MODEL_API_KEY}"},
		Timeout: "1m",
	}
	if err := New(nil).Validate(profile); err == nil {
		t.Fatal("profile without max_tokens was accepted")
	}
}

func TestEncodeRequestProjectsImageContentParts(t *testing.T) {
	encoded, err := contract.DataImageURL("image/png", []byte{
		0x89, 0x50, 0x4e, 0x47, 0x0d, 0x0a, 0x1a, 0x0a,
		0x00, 0x00, 0x00, 0x0d, 0x49, 0x48, 0x44, 0x52,
		0x00, 0x00, 0x00, 0x01, 0x00, 0x00, 0x00, 0x01,
		0x08, 0x06, 0x00, 0x00, 0x00, 0x1f, 0x15, 0xc4,
		0x89, 0x00, 0x00, 0x00, 0x0a, 0x49, 0x44, 0x41,
		0x54, 0x78, 0x9c, 0x63, 0x00, 0x01, 0x00, 0x00,
		0x05, 0x00, 0x01, 0x0d, 0x0a, 0x2d, 0xb4, 0x00,
		0x00, 0x00, 0x00, 0x49, 0x45, 0x4e, 0x44, 0xae,
		0x42, 0x60, 0x82,
	})
	if err != nil {
		t.Fatal(err)
	}
	maxTokens := int64(64)
	payload, err := encodeRequest(model.ResolvedModel{Model: "fixture"}, contract.ModelRequest{
		Messages: []contract.Message{{
			Role: contract.RoleUser,
			ContentParts: []contract.ContentPart{
				{Type: contract.ContentPartText, Text: "what is this"},
				{Type: contract.ContentPartImageURL, ImageURL: &contract.ImageURL{URL: encoded}},
			},
		}},
		Options: contract.GenerateOptions{MaxOutputTokens: &maxTokens},
	})
	if err != nil {
		t.Fatal(err)
	}
	var body struct {
		Messages []struct {
			Content []map[string]any `json:"content"`
		} `json:"messages"`
	}
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 1 || len(body.Messages[0].Content) != 2 {
		t.Fatalf("payload=%s", payload)
	}
	if body.Messages[0].Content[0]["type"] != "text" {
		t.Fatalf("text part=%v", body.Messages[0].Content[0])
	}
	source, _ := body.Messages[0].Content[1]["source"].(map[string]any)
	if body.Messages[0].Content[1]["type"] != "image" || source["type"] != "base64" || source["media_type"] != "image/png" {
		t.Fatalf("image part=%v", body.Messages[0].Content[1])
	}
}

func TestEncodeRequestStreamModes(t *testing.T) {
	for _, name := range []string{"omitted", "true", "false"} {
		t.Run(name, func(t *testing.T) {
			var stream *bool
			if name != "omitted" {
				value := name == "true"
				stream = &value
			}
			maxTokens := int64(64)
			payload, err := encodeRequest(model.ResolvedModel{
				Model: "fixture", Parameters: model.Parameters{Stream: stream},
			}, contract.ModelRequest{
				Messages: []contract.Message{{Role: contract.RoleUser, Content: "hello"}},
				Options:  contract.GenerateOptions{MaxOutputTokens: &maxTokens},
			})
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Fatal(err)
			}
			if string(body["stream"]) != fmt.Sprint(name != "false") {
				t.Fatalf("stream=%s mode=%s", body["stream"], name)
			}
			if _, exists := body["stream_options"]; exists {
				t.Fatal("unexpected OpenAI stream_options")
			}
		})
	}
}

func TestEncodeRequestPreservesToolResultError(t *testing.T) {
	maxTokens := int64(64)
	payload, err := encodeRequest(model.ResolvedModel{
		Model: "fixture",
	}, contract.ModelRequest{
		Messages: []contract.Message{
			{Role: contract.RoleUser, Content: "start"},
			{
				Role: contract.RoleAssistant,
				ToolCalls: []contract.ToolCall{{
					ID: "call-1", Name: "fixture",
					Arguments: json.RawMessage(`{}`),
				}},
			},
			{
				Role: contract.RoleTool, ToolCallID: "call-1",
				Content: "failed", IsError: true,
			},
		},
		Options: contract.GenerateOptions{MaxOutputTokens: &maxTokens},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range [][]byte{
		[]byte(`"temperature"`), []byte(`"top_p"`),
		[]byte(`"stop_sequences"`),
	} {
		if bytes.Contains(payload, field) {
			t.Fatalf("unset field %s was sent: %s", field, payload)
		}
	}
	var body requestBody
	if err := json.Unmarshal(payload, &body); err != nil {
		t.Fatal(err)
	}
	if len(body.Messages) != 3 ||
		len(body.Messages[2].Content) != 1 ||
		body.Messages[2].Content[0].Type != "tool_result" ||
		!body.Messages[2].Content[0].IsError {
		t.Fatalf("body=%#v", body)
	}
}
