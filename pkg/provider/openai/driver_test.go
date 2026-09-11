package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
)

func TestDriverExecutionIdentity(t *testing.T) {
	identity := New(nil).ExecutionIdentity()
	if identity.Driver != model.DriverOpenAIChatCompletions ||
		identity.Implementation != "runtime.provider.openai-chat-completions" ||
		identity.ImplementationVersion != 1 {
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
			t.Errorf("unexpected max_tokens wire field was sent")
		}
		if body["top_p"] != float64(0.9) {
			t.Errorf("top_p=%v", body["top_p"])
		}
		stop, ok := body["stop"].([]any)
		if !ok || len(stop) != 1 || stop[0] != "END" {
			t.Errorf("stop=%v", body["stop"])
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(writer, `data: {"id":"req-1","model":"wire-model","choices":[{"delta":{"content":"hello "}}]}`)
		fmt.Fprintln(writer, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"id":"call-1","function":{"name":"weather","arguments":"{\"city\":"}}]}}]}`)
		fmt.Fprintln(writer, `data: {"choices":[{"delta":{"tool_calls":[{"index":0,"function":{"arguments":"\"Shanghai\"}"}}]},"finish_reason":"tool_calls"}]}`)
		fmt.Fprintln(writer, `data: {"choices":[],"usage":{"prompt_tokens":10,"completion_tokens":4,"total_tokens":14}}`)
		fmt.Fprintln(writer, "data: [DONE]")
	}))
	defer server.Close()

	var observed []model.Attempt
	service := newService(t, server.URL, server.Client(), func(attempt model.Attempt) {
		observed = append(observed, attempt)
	})
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
	if len(observed) != 1 {
		t.Fatalf("observed=%#v", observed)
	}
	attempt := observed[0]
	if attempt.ProfileID != "api" || attempt.Wire.Request.Method != http.MethodPost ||
		attempt.Wire.Request.URL != server.URL+"/v1/chat/completions" ||
		attempt.Wire.Request.Headers["Authorization"] != "Bearer ${MODEL_API_KEY}" ||
		attempt.Wire.Response == nil || attempt.Wire.Response.Status != http.StatusOK ||
		len(attempt.Wire.Response.Data) != 4 || attempt.Error != nil {
		t.Fatalf("attempt=%#v", attempt)
	}
	data, err := json.Marshal(attempt)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "Bearer secret") {
		t.Fatalf("attempt leaked API key: %s", data)
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
	service := newService(t, server.URL, server.Client())
	_, runtimeErr := service.Generate(context.Background(), request())
	if runtimeErr == nil || runtimeErr.Code != contract.ErrorRateLimited ||
		runtimeErr.RetryAfterMS != 2000 || attempts.Load() != 1 {
		t.Fatalf("attempts=%d error=%#v", attempts.Load(), runtimeErr)
	}
}

func TestDriverDoesNotFollowRedirect(t *testing.T) {
	var attempts atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		attempts.Add(1)
		if request.URL.Path == "/v1/chat/completions" {
			http.Redirect(writer, request, "/redirected", http.StatusTemporaryRedirect)
			return
		}
		t.Errorf("driver followed redirect to %s", request.URL.Path)
		http.Error(writer, "unexpected redirect", http.StatusInternalServerError)
	}))
	defer server.Close()
	service := newService(t, server.URL, server.Client())
	_, runtimeErr := service.Generate(context.Background(), request())
	if runtimeErr == nil || runtimeErr.Code != contract.ErrorProtocol ||
		attempts.Load() != 1 {
		t.Fatalf("attempts=%d error=%#v", attempts.Load(), runtimeErr)
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
	payload, err := encodeRequest(model.ResolvedModel{Model: "fixture"}, contract.ModelRequest{
		Messages: []contract.Message{{
			Role: contract.RoleUser,
			ContentParts: []contract.ContentPart{
				{Type: contract.ContentPartText, Text: "what is this"},
				{Type: contract.ContentPartImageURL, ImageURL: &contract.ImageURL{URL: encoded}},
			},
		}},
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
	if body.Messages[0].Content[0]["type"] != "text" || body.Messages[0].Content[0]["text"] != "what is this" {
		t.Fatalf("text part=%v", body.Messages[0].Content[0])
	}
	image, _ := body.Messages[0].Content[1]["image_url"].(map[string]any)
	if body.Messages[0].Content[1]["type"] != "image_url" || image["url"] != encoded {
		t.Fatalf("image part=%v", body.Messages[0].Content[1])
	}
}

func TestEncodeRequestOmitsUnsetCommonOptions(t *testing.T) {
	maxTokens := int64(64)
	payload, err := encodeRequest(model.ResolvedModel{Model: "fixture"}, contract.ModelRequest{
		Messages: []contract.Message{{Role: contract.RoleUser, Content: "hello"}},
		Options:  contract.GenerateOptions{MaxOutputTokens: &maxTokens},
	})
	if err != nil {
		t.Fatal(err)
	}
	for _, field := range [][]byte{
		[]byte(`"temperature"`), []byte(`"top_p"`), []byte(`"stop"`),
	} {
		if bytes.Contains(payload, field) {
			t.Fatalf("unset field %s was sent: %s", field, payload)
		}
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
			payload, err := encodeRequest(model.ResolvedModel{
				Model: "fixture", Parameters: model.Parameters{Stream: stream},
			}, request().Input)
			if err != nil {
				t.Fatal(err)
			}
			var body map[string]json.RawMessage
			if err := json.Unmarshal(payload, &body); err != nil {
				t.Fatal(err)
			}
			want := name != "false"
			if string(body["stream"]) != fmt.Sprint(want) {
				t.Fatalf("stream=%s want=%t", body["stream"], want)
			}
			if want {
				if string(body["stream_options"]) != `{"include_usage":true}` {
					t.Fatalf("stream_options=%s", body["stream_options"])
				}
			} else if _, exists := body["stream_options"]; exists {
				t.Fatal("non-stream request must not include stream_options")
			}
		})
	}
}

func newService(
	t *testing.T,
	baseURL string,
	client *http.Client,
	observers ...model.AttemptObserver,
) *model.Service {
	t.Helper()
	maxTokens := int64(1024)
	topP := 0.9
	catalog, err := model.NewCatalog(map[string]model.Profile{
		"api": {
			Driver: model.DriverOpenAIChatCompletions, BaseURL: baseURL + "/v1", Model: "fixture",
			Headers: map[string]string{"Authorization": "${MODEL_API_KEY}"},
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
	var observer model.AttemptObserver
	if len(observers) > 0 {
		observer = observers[0]
	}
	service, err := model.NewService(
		catalog,
		map[model.DriverName]model.Driver{model.DriverOpenAIChatCompletions: New(client)},
		model.ServiceOptions{Getenv: func(name string) (string, bool) {
			return "secret", name == "MODEL_API_KEY"
		}, AttemptObserver: observer},
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
