package openairesponses

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	httpx "github.com/yy003x/runtime/internal/infrastructure/providerhttp"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
)

const textResponse = `{"id":"resp-fixture","model":"wire-model","status":"completed","output":[{"type":"reasoning","id":"rs-fixture"},{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello"}]}],"usage":{"input_tokens":10,"output_tokens":0,"total_tokens":10,"input_tokens_details":{"cached_tokens":0},"output_tokens_details":{"reasoning_tokens":0}}}`
const toolResponse = `{"id":"resp-fixture","status":"completed","output":[{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello"}]},{"id":"fc-item-not-call-id","type":"function_call","status":"completed","call_id":"call-fixture","name":"weather","arguments":"{\"city\":\"上海\"}"}]}`

func sseResponse(body string) string {
	return "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"Hel\"}\n\n" +
		"data: {\"type\":\"response.output_text.delta\",\"delta\":\"lo\"}\n\n" +
		"data: {\"type\":\"response.output_text.done\",\"text\":\"Hello\"}\n\n" +
		"event: response.completed\ndata: {\"type\":\"response.completed\",\ndata: \"response\":" + body + "}\n\n"
}

func newService(t *testing.T, server *httptest.Server, stream *bool, observer model.AttemptObserver) *model.Service {
	t.Helper()
	max := int64(100)
	temperature, topP := 0.5, 0.8
	catalog, err := model.NewCatalog(map[string]model.Profile{"api": {
		Driver: model.DriverOpenAIResponses, BaseURL: server.URL + "/v1", Model: "fixture", Timeout: "1m",
		Headers:    map[string]string{"Authorization": "${MODEL_API_KEY}"},
		Parameters: model.Parameters{MaxTokens: &max, Temperature: &temperature, TopP: &topP, Stream: stream},
	}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := model.NewService(catalog, map[model.DriverName]model.Driver{model.DriverOpenAIResponses: New(server.Client())}, model.ServiceOptions{
		Getenv: func(string) (string, bool) { return "fixture-secret", true }, AttemptObserver: observer,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func input() contract.GenerateRequest {
	return contract.GenerateRequest{ModelProfile: "api", Input: contract.ModelRequest{
		System: "Be concise", Messages: []contract.Message{{Role: contract.RoleUser, Content: "Hello"}},
		Tools: []contract.ToolSpec{{Name: "weather", InputSchema: json.RawMessage(`{"type":"object","properties":{"city":{"type":"string"}}}`)}},
	}}
}

func TestDriverIdentityAndValidation(t *testing.T) {
	driver := New(nil)
	identity := driver.ExecutionIdentity()
	if err := identity.Validate(); err != nil {
		t.Fatal(err)
	}
	if identity.Driver != model.DriverOpenAIResponses || identity.ImplementationVersion != 1 || identity.Implementation != "runtime.provider.openai-responses" {
		t.Fatalf("identity=%#v", identity)
	}
	if err := driver.Validate(model.Profile{Driver: model.DriverOpenAI}); err == nil {
		t.Fatal("wrong driver accepted")
	}
}

func TestResponsesJSONAndSSEModes(t *testing.T) {
	for _, configured := range []*bool{nil, boolPtr(true), boolPtr(false)} {
		for _, sse := range []bool{false, true} {
			for _, tools := range []bool{false, true} {
				t.Run(fmt.Sprintf("stream=%v/sse=%t/tools=%t", configured, sse, tools), func(t *testing.T) {
					body := textResponse
					if tools {
						body = toolResponse
					}
					wireBody := body
					if sse {
						wireBody = sseResponse(body)
					}
					calls := atomic.Int32{}
					server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
						calls.Add(1)
						if r.Method != "POST" || r.URL.Path != "/v1/responses" || r.Header.Get("Authorization") != "Bearer fixture-secret" {
							t.Errorf("invalid HTTP request: %s %s", r.Method, r.URL)
						}
						var request map[string]any
						if err := json.NewDecoder(r.Body).Decode(&request); err != nil {
							t.Error(err)
						}
						stream := configured == nil || *configured
						accept := "application/json"
						if stream {
							accept = "text/event-stream, application/json"
						}
						if request["stream"] != stream || request["store"] != false || request["model"] != "fixture" || request["instructions"] != "Be concise" || request["max_output_tokens"] != float64(17) || request["temperature"] != 0.5 || request["top_p"] != 0.8 || r.Header.Get("Accept") != accept {
							t.Errorf("request=%#v accept=%s", request, r.Header.Get("Accept"))
						}
						for _, forbidden := range []string{"messages", "max_tokens", "max_completion_tokens", "stream_options", "stop", "previous_response_id"} {
							if _, ok := request[forbidden]; ok {
								t.Errorf("unexpected wire field %s", forbidden)
							}
						}
						tool := request["tools"].([]any)[0].(map[string]any)
						if tool["type"] != "function" || tool["name"] != "weather" || tool["strict"] != false || tool["parameters"] == nil || tool["function"] != nil {
							t.Errorf("tool=%#v", tool)
						}
						w.Header().Set("Content-Type", "application/json")
						if sse {
							w.Header().Set("Content-Type", "text/event-stream; charset=utf-8")
						}
						fmt.Fprint(w, wireBody)
					}))
					defer server.Close()
					var observed []model.Attempt
					service := newService(t, server, configured, func(attempt model.Attempt) { observed = append(observed, attempt) })
					request := input()
					max := int64(17)
					request.Input.Options.MaxOutputTokens = &max
					var events []contract.Event
					result, err := service.GenerateStream(context.Background(), request, func(e contract.Event) error { events = append(events, e); return nil })
					if err != nil {
						t.Fatal(err)
					}
					if calls.Load() != 1 || len(observed) != 1 || result.Message.Content != "Hello" {
						t.Fatalf("calls=%d observed=%d result=%#v", calls.Load(), len(observed), result)
					}
					wantEvents, wantFrames := 3, 1
					if tools {
						wantEvents = 5
						if result.FinishReason != contract.FinishToolCall || len(result.Message.ToolCalls) != 1 || result.Message.ToolCalls[0].ID != "call-fixture" || string(result.Message.ToolCalls[0].Arguments) != `{"city":"上海"}` {
							t.Fatalf("result=%#v", result)
						}
					} else if result.Usage.OutputTokens == nil || *result.Usage.OutputTokens != 0 || result.Usage.CacheReadTokens == nil || *result.Usage.CacheReadTokens != 0 || result.Usage.ReasoningTokens == nil || *result.Usage.ReasoningTokens != 0 || result.Usage.Completeness != contract.UsageComplete {
						t.Fatalf("usage=%#v", result.Usage)
					}
					if sse {
						wantFrames = 4
					}
					if len(events) != wantEvents || events[0].Type != contract.EventModelStarted || events[len(events)-1].Type != contract.EventModelCompleted {
						t.Fatalf("events=%#v", events)
					}
					for i, event := range events {
						if event.Sequence != uint64(i+1) {
							t.Fatalf("sequence=%d", event.Sequence)
						}
					}
					wire := observed[0].Wire
					if len(wire.Response.Data) != wantFrames || wire.Request.Headers["Authorization"] != "Bearer ${MODEL_API_KEY}" || wire.Request.URL != server.URL+"/v1/responses" {
						t.Fatalf("wire=%#v", wire)
					}
					snapshot, snapErr := service.ExecutionSnapshot("api")
					if snapErr != nil || snapshot.DriverIdentity.Driver != model.DriverOpenAIResponses {
						t.Fatalf("snapshot=%#v err=%v", snapshot, snapErr)
					}
				})
			}
		}
	}
}

func TestResponsesRejectUnsupportedInputBeforeHTTP(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewTLSServer(http.HandlerFunc(func(http.ResponseWriter, *http.Request) { calls.Add(1) }))
	defer server.Close()
	observed := 0
	service := newService(t, server, nil, func(model.Attempt) { observed++ })
	for _, validator := range []model.ManagedExecutionValidator{service, model.NewResilientModel(service, model.DefaultRetryPolicy())} {
		if err := validator.ValidateManagedExecution("api"); err == nil || !strings.Contains(err.Error(), "single calls only") {
			t.Fatalf("managed gate=%v", err)
		}
		if err := validator.ValidateManagedExecution("missing"); err == nil {
			t.Fatal("unknown profile accepted")
		}
	}
	for _, message := range []contract.Message{{Role: contract.RoleAssistant, Content: "history"}, {Role: contract.RoleTool, ToolCallID: "call-id", Content: "result"}} {
		request := input()
		request.Input.Messages = append(request.Input.Messages, message)
		_, err := service.Generate(context.Background(), request)
		if err == nil || err.Code != contract.ErrorInvalidRequest || err.Phase != contract.PhaseRequest {
			t.Fatalf("err=%v", err)
		}
	}
	request := input()
	request.Input.Options.StopSequences = []string{"END"}
	_, err := service.Generate(context.Background(), request)
	if err == nil || err.Code != contract.ErrorInvalidRequest || calls.Load() != 0 || observed != 0 {
		t.Fatalf("err=%v calls=%d observed=%d", err, calls.Load(), observed)
	}
}

func TestResponsesTerminalValidation(t *testing.T) {
	for name, test := range map[string]struct {
		body   string
		sse    bool
		code   contract.ErrorCode
		finish contract.FinishReason
	}{
		"truncated":        {body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"Hello\"}\n\n", sse: true, code: contract.ErrorProtocol},
		"done_only":        {body: "data: [DONE]\n\n", sse: true, code: contract.ErrorProtocol},
		"invalid_json":     {body: "data: {broken}\n\n", sse: true, code: contract.ErrorProtocol},
		"missing_response": {body: "data: {\"type\":\"response.completed\"}\n\n", sse: true, code: contract.ErrorProtocol},
		"wrong_status":     {body: "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"in_progress\"}}\n\n", sse: true, code: contract.ErrorProtocol},
		"wrong_event_name": {body: "event: response.failed\ndata: {\"type\":\"response.completed\"}\n\n", sse: true, code: contract.ErrorProtocol},
		"sse_error":        {body: "data: {\"type\":\"error\",\"code\":\"server_error\",\"message\":\"failed\"}\n\n", sse: true, code: contract.ErrorProviderUnavailable},
		"failed":           {body: `{"status":"failed","error":{"code":"rate_limit_exceeded","message":"slow down"}}`, code: contract.ErrorRateLimited},
		"empty":            {body: `{"status":"completed","output":[]}`, code: contract.ErrorProtocol},
		"unfinished":       {body: strings.Replace(textResponse, `"status":"completed"`, `"status":"in_progress"`, 1), code: contract.ErrorProtocol},
		"null_arguments":   {body: strings.Replace(toolResponse, `"arguments":"{\"city\":\"上海\"}"`, `"arguments":"null"`, 1), code: contract.ErrorProtocol},
		"missing_call_id":  {body: strings.Replace(toolResponse, `"call_id":"call-fixture",`, "", 1), code: contract.ErrorProtocol},
		"builtin_output":   {body: `{"status":"completed","output":[{"type":"web_search_call"}]}`, code: contract.ErrorProtocol},
		"negative_usage":   {body: strings.Replace(textResponse, `"output_tokens":0`, `"output_tokens":-1`, 1), code: contract.ErrorProtocol},
		"incomplete_text":  {body: strings.Replace(textResponse, `"status":"completed","output"`, `"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output"`, 1), finish: contract.FinishLength},
		"incomplete_tool":  {body: strings.Replace(toolResponse, `"status":"completed","output"`, `"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output"`, 1), code: contract.ErrorProtocol},
		"refusal":          {body: `{"status":"completed","output":[{"type":"message","status":"completed","role":"assistant","content":[{"type":"refusal","refusal":"Cannot help"}]}]}`, finish: contract.FinishContentFilter},
	} {
		t.Run(name, func(t *testing.T) {
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
				w.Header().Set("Content-Type", "application/json")
				if test.sse {
					w.Header().Set("Content-Type", "text/event-stream")
				}
				fmt.Fprint(w, test.body)
			}))
			defer server.Close()
			service := newService(t, server, nil, nil)
			result, err := service.Generate(context.Background(), input())
			if test.code != "" {
				if err == nil || err.Code != test.code {
					t.Fatalf("error=%v want=%s", err, test.code)
				}
			} else if err != nil || result.FinishReason != test.finish {
				t.Fatalf("result=%#v err=%v", result, err)
			}
		})
	}
}

func TestResponsesHTTPErrorAndRedirectAreSingleAttempt(t *testing.T) {
	for _, status := range []int{307, 401, 403, 429, 503} {
		t.Run(fmt.Sprint(status), func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				calls.Add(1)
				w.Header().Set("Content-Type", "application/json")
				w.Header().Set("Location", "/redirected")
				w.Header().Set("Retry-After", "2")
				w.WriteHeader(status)
				fmt.Fprint(w, `{"error":{"message":"fixture-secret failed"}}`)
			}))
			defer server.Close()
			service := newService(t, server, nil, nil)
			_, err := service.Generate(context.Background(), input())
			if err == nil || err.HTTPStatus != status || calls.Load() != 1 || strings.Contains(err.Message, "fixture-secret") {
				t.Fatalf("calls=%d err=%v", calls.Load(), err)
			}
			if status == 429 && (err.Code != contract.ErrorRateLimited || err.RetryAfterMS != 2000) {
				t.Fatal(err)
			}
		})
	}
}

func TestResponsesStreamLimitCancellationAndConsumerFailure(t *testing.T) {
	if _, err := decodeStream(strings.NewReader(":" + strings.Repeat("x", int(httpx.MaxResponseBytes)))); err == nil {
		t.Fatal("oversized stream accepted")
	}
	server := httptest.NewTLSServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Header.Get("X-Wait") == "true" {
			<-r.Context().Done()
			return
		}
		w.Header().Set("Content-Type", "application/json")
		fmt.Fprint(w, textResponse)
	}))
	defer server.Close()
	service := newService(t, server, nil, nil)
	_, err := service.GenerateStream(context.Background(), input(), func(contract.Event) error { return errors.New("sink failed") })
	if err == nil || err.Phase != contract.PhaseConsumer || err.Code != contract.ErrorCancelled {
		t.Fatalf("err=%v", err)
	}
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err = service.Generate(ctx, input())
	if err == nil || err.Code != contract.ErrorCancelled {
		t.Fatalf("err=%v", err)
	}
	ctx, cancel = context.WithTimeout(context.Background(), time.Nanosecond)
	defer cancel()
	<-ctx.Done()
	_, err = service.Generate(ctx, input())
	if err == nil || err.Code != contract.ErrorTimeout {
		t.Fatalf("err=%v", err)
	}
}

func boolPtr(value bool) *bool { return &value }
