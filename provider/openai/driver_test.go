package openai

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
)

func TestDriverExecutionIdentity(t *testing.T) {
	identity := New(nil).ExecutionIdentity()
	if identity.Driver != model.DriverOpenAICompatible ||
		identity.Implementation != executionImplementation ||
		identity.ImplementationVersion != executionImplementationVersion {
		t.Fatalf("identity=%#v", identity)
	}
	if err := identity.Validate(); err != nil {
		t.Fatal(err)
	}
}

func TestDriverStreamsTextAndFragmentedToolCallInOneAttempt(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%q", request.URL.Path)
		}
		if request.Header.Get("Authorization") != "Bearer secret" {
			t.Errorf("authorization=%q", request.Header.Get("Authorization"))
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if body["max_completion_tokens"] != float64(1024) {
			t.Errorf("max_completion_tokens=%v", body["max_completion_tokens"])
		}
		if _, exists := body["max_tokens"]; exists {
			t.Errorf("deprecated max_tokens was sent")
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(writer, `data: {"id":"req-1","model":"wire-model","choices":[{"delta":{"content":"hello "}}]}`)
		fmt.Fprintln(writer, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"weather","arguments":"{\"city\":"}}]}}]}`)
		fmt.Fprintln(writer, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Shanghai\"}"}}]},"finish_reason":"tool_calls"}]}`)
		fmt.Fprintln(writer, `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`)
		fmt.Fprintln(writer, "data: [DONE]")
	}))
	defer server.Close()

	service := newService(t, server.URL+"/v1/chat/completions", server.Client())
	var events []contract.Event
	result, runtimeErr := service.GenerateStream(
		context.Background(),
		contract.GenerateRequest{
			ModelProfile: "api",
			Input: contract.ModelRequest{
				Messages: []contract.Message{{Role: contract.RoleUser, Content: "hello"}},
			},
		},
		func(event contract.Event) error {
			events = append(events, event)
			return nil
		},
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if attempts.Load() != 1 || result.FinishReason != contract.FinishToolCall ||
		len(result.Message.ToolCalls) != 1 ||
		string(result.Message.ToolCalls[0].Arguments) != `{"city":"Shanghai"}` ||
		result.Provider.RequestID != "req-1" {
		t.Fatalf("attempts=%d result=%#v", attempts.Load(), result)
	}
	if len(events) != 6 || events[0].Type != contract.EventModelStarted ||
		events[len(events)-1].Type != contract.EventModelCompleted {
		t.Fatalf("events=%#v", events)
	}
}

func TestDriverMapsRateLimitWithoutRetry(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
		attempts.Add(1)
		writer.Header().Set("Retry-After", "2")
		http.Error(writer, "rate limited", http.StatusTooManyRequests)
	}))
	defer server.Close()
	service := newService(t, server.URL+"/v1/chat/completions", server.Client())
	_, runtimeErr := service.Generate(context.Background(), request())
	if runtimeErr == nil || runtimeErr.Code != contract.ErrorRateLimited ||
		runtimeErr.RetryAfterMS != 2000 || attempts.Load() != 1 {
		t.Fatalf("attempts=%d error=%#v", attempts.Load(), runtimeErr)
	}
}

func newService(t *testing.T, endpoint string, client *http.Client) *model.Service {
	t.Helper()
	maxTokens := int64(1024)
	catalog, err := model.NewCatalog(map[string]model.Profile{
		"api": {
			Driver: model.DriverOpenAICompatible, Endpoint: endpoint, Model: "fixture",
			Auth: model.Auth{
				Header: "Authorization", Scheme: "Bearer", FromEnv: "MODEL_API_KEY",
			},
			Defaults: model.Defaults{MaxCompletionTokens: &maxTokens},
			Timeout:  "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	service, err := model.NewService(
		catalog,
		map[model.DriverName]model.Driver{model.DriverOpenAICompatible: New(client)},
		model.ServiceOptions{Getenv: func(name string) (string, bool) {
			return "secret", name == "MODEL_API_KEY"
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func request() contract.GenerateRequest {
	return contract.GenerateRequest{
		ModelProfile: "api",
		Input: contract.ModelRequest{
			Messages: []contract.Message{{Role: contract.RoleUser, Content: "hello"}},
		},
	}
}

func TestNormalizeArgumentsRejectsNonObject(t *testing.T) {
	if _, err := normalizeArguments(`[]`); err == nil || !strings.Contains(err.Error(), "object") {
		t.Fatalf("error=%v", err)
	}
	value, err := normalizeArguments(`{"ok":true}`)
	if err != nil || !json.Valid(value) {
		t.Fatalf("value=%s error=%v", value, err)
	}
}
