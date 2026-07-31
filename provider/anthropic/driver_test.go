package anthropic

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
)

func TestDriverExecutionIdentity(t *testing.T) {
	identity := New(nil).ExecutionIdentity()
	if identity.Driver != model.DriverAnthropicCompatible ||
		identity.Implementation != executionImplementation ||
		identity.ImplementationVersion != executionImplementationVersion {
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
		if request.Header.Get("x-api-key") != "secret" {
			t.Errorf("x-api-key=%q", request.Header.Get("x-api-key"))
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
	catalog, err := model.NewCatalog(map[string]model.Profile{
		"api": {
			Driver:   model.DriverAnthropicCompatible,
			Endpoint: server.URL + "/v1/messages", Model: "fixture",
			Auth:     model.Auth{Header: "x-api-key", FromEnv: "MODEL_API_KEY"},
			Defaults: model.Defaults{MaxTokens: &maxTokens},
			Timeout:  "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := model.NewService(
		catalog,
		map[model.DriverName]model.Driver{model.DriverAnthropicCompatible: New(server.Client())},
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
		Driver:   model.DriverAnthropicCompatible,
		Endpoint: "https://example.invalid/v1/messages", Model: "fixture",
		Auth:    model.Auth{Header: "x-api-key", FromEnv: "MODEL_API_KEY"},
		Timeout: "1m",
	}
	if err := New(nil).Validate(profile); err == nil {
		t.Fatal("profile without max_tokens was accepted")
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
