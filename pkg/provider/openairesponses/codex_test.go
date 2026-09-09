package openairesponses

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
)

type codexTransport func(*http.Request) (*http.Response, error)

func (fn codexTransport) RoundTrip(request *http.Request) (*http.Response, error) { return fn(request) }

func codexService(t *testing.T, transport http.RoundTripper, resolver model.CredentialResolver, observe model.CallObserver) *model.Service {
	t.Helper()
	catalog, err := model.NewCatalog(map[string]model.Profile{"api": {
		Driver: model.DriverOpenAICodexResponses, Endpoint: model.OpenAICodexEndpoint, Model: "fixture", Timeout: "1m",
	}})
	if err != nil {
		t.Fatal(err)
	}
	service, err := model.NewService(catalog, map[model.DriverName]model.Driver{model.DriverOpenAICodexResponses: NewCodex(&http.Client{Transport: transport})}, model.ServiceOptions{Credentials: resolver, CallObserver: observe})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestCodexSSEWithoutReliableContentType(t *testing.T) {
	for _, contentType := range []string{"", "text/event-stream; charset=utf-8", "application/json", "text/plain"} {
		for _, terminal := range []string{"response.completed", "response.done"} {
			t.Run(contentType+"/"+terminal, func(t *testing.T) {
				var observation model.CallObservation
				calls := 0
				service := codexService(t, codexTransport(func(*http.Request) (*http.Response, error) {
					calls++
					headers := make(http.Header)
					if contentType != "" {
						headers.Set("Content-Type", contentType)
					}
					body := strings.ReplaceAll(sseResponse(textResponse), "response.completed", terminal)
					return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
				}), func(context.Context, model.DriverName, string) (model.Credentials, error) {
					return model.Credentials{AccessToken: "fixture-token", AccountID: "fixture-account"}, nil
				}, func(value model.CallObservation) { observation = value })
				var events []contract.Event
				result, err := service.GenerateStream(context.Background(), input(), func(event contract.Event) error {
					events = append(events, event)
					return nil
				})
				if err != nil || result.Message.Content != "Hello" || result.Provider.Name != "openai-codex-responses" || len(events) != 3 {
					t.Fatalf("result=%+v err=%v events=%d", result, err, len(events))
				}
				if calls != 1 || len(observation.Attempts) != 1 || observation.Attempts[0].Wire.Response == nil {
					t.Fatalf("calls=%d observation=%+v", calls, observation)
				}
				response := observation.Attempts[0].Wire.Response
				if response.Status != http.StatusOK || response.Headers["Content-Type"] != contentType {
					t.Fatalf("wire response metadata changed: %+v", response)
				}
				if _, present := response.Headers["Content-Type"]; present != (contentType != "") {
					t.Fatalf("wire Content-Type presence changed: %v", response.Headers)
				}
				wantTypes := []string{"response.output_text.delta", "response.output_text.delta", "response.output_text.done", terminal}
				if len(response.Data) != len(wantTypes) {
					t.Fatalf("raw frames=%d want=%d", len(response.Data), len(wantTypes))
				}
				for index, data := range response.Data {
					var frame struct {
						Type string `json:"type"`
					}
					if err := json.Unmarshal(data, &frame); err != nil || frame.Type != wantTypes[index] {
						t.Fatalf("raw frame %d=%s err=%v", index, data, err)
					}
				}
			})
		}
	}
}

func TestCodexCompletedItemsWithEmptyTerminalOutput(t *testing.T) {
	for _, contentType := range []string{"", "text/event-stream"} {
		for _, terminal := range []string{"response.completed", "response.done"} {
			t.Run(contentType+"/"+terminal, func(t *testing.T) {
				const completedItem = `{"type":"response.output_item.done","output_index":0,"item":{"id":"msg-fixture","type":"message","status":"completed","role":"assistant","content":[{"type":"output_text","text":"OK"}]}}`
				body := "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"output_index\":0,\"delta\":\"OK\"}\n\n" +
					"event: response.output_item.done\ndata: " + completedItem + "\n\n" +
					"event: " + terminal + "\ndata: {\"type\":\"" + terminal + "\",\"response\":{\"id\":\"resp-fixture\",\"status\":\"completed\",\"output\":[],\"usage\":{\"input_tokens\":21,\"output_tokens\":5,\"total_tokens\":26}}}\n\n"
				var observation model.CallObservation
				service := codexService(t, codexTransport(func(*http.Request) (*http.Response, error) {
					headers := make(http.Header)
					if contentType != "" {
						headers.Set("Content-Type", contentType)
					}
					return &http.Response{StatusCode: http.StatusOK, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
				}), func(context.Context, model.DriverName, string) (model.Credentials, error) {
					return model.Credentials{AccessToken: "fixture-token", AccountID: "fixture-account"}, nil
				}, func(value model.CallObservation) { observation = value })
				var events []contract.Event
				result, err := service.GenerateStream(context.Background(), input(), func(event contract.Event) error {
					events = append(events, event)
					return nil
				})
				if err != nil || result.Message.Content != "OK" || len(events) != 3 || result.Usage.TotalTokens == nil || *result.Usage.TotalTokens != 26 {
					t.Fatalf("result=%+v err=%v events=%d", result, err, len(events))
				}
				if len(observation.Attempts) != 1 || observation.Attempts[0].Wire.Response == nil || len(observation.Attempts[0].Wire.Response.Data) != 3 {
					t.Fatalf("raw frames changed: %+v", observation)
				}
				var frame struct {
					Response responseEnvelope `json:"response"`
				}
				if err := json.Unmarshal(observation.Attempts[0].Wire.Response.Data[2], &frame); err != nil || len(frame.Response.Output) != 0 {
					t.Fatal("raw terminal output must remain empty")
				}
			})
		}
	}
}

func TestCodexCompletedItemAssemblyValidation(t *testing.T) {
	const hello = `{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"Hello"}]}`
	const world = `{"type":"message","role":"assistant","status":"completed","content":[{"type":"output_text","text":"World"}]}`
	const tool = `{"type":"function_call","status":"completed","call_id":"call-fixture","name":"weather","arguments":"{\"value\":9007199254740993}"}`
	const terminal = "data: {\"type\":\"response.completed\",\"response\":{\"status\":\"completed\",\"output\":[]}}\n\n"
	item := func(index int, value string) string {
		return fmt.Sprintf("data: {\"type\":\"response.output_item.done\",\"output_index\":%d,\"item\":%s}\n\n", index, value)
	}
	for _, test := range []struct {
		name, body, text string
		tools            int
		finish           contract.FinishReason
		code             contract.ErrorCode
	}{
		{name: "ordered_by_index", body: item(1, world) + item(0, hello) + terminal, text: "HelloWorld", finish: contract.FinishStop},
		{name: "tool", body: item(0, tool) + terminal, tools: 1, finish: contract.FinishToolCall},
		{name: "text_and_tool", body: item(1, tool) + item(0, hello) + terminal, text: "Hello", tools: 1, finish: contract.FinishToolCall},
		{name: "terminal_output_authoritative", body: item(0, world) + sseResponse(textResponse), text: "Hello", finish: contract.FinishStop},
		{name: "duplicate_index", body: item(0, hello) + item(0, hello) + terminal, code: contract.ErrorProtocol},
		{name: "conflicting_index", body: item(0, hello) + item(0, world) + terminal, code: contract.ErrorProtocol},
		{name: "negative_index", body: item(-1, hello) + terminal, code: contract.ErrorProtocol},
		{name: "missing_index", body: "data: {\"type\":\"response.output_item.done\",\"item\":" + hello + "}\n\n" + terminal, code: contract.ErrorProtocol},
		{name: "missing_item", body: "data: {\"type\":\"response.output_item.done\",\"output_index\":0}\n\n" + terminal, code: contract.ErrorProtocol},
		{name: "index_gap", body: item(1, hello) + terminal, code: contract.ErrorProtocol},
		{name: "huge_index", body: item(2147483647, hello) + terminal, code: contract.ErrorProtocol},
		{name: "empty_item", body: item(0, `{}`) + terminal, code: contract.ErrorProtocol},
		{name: "unfinished_message", body: item(0, strings.Replace(hello, "completed", "in_progress", 1)) + terminal, code: contract.ErrorProtocol},
		{name: "unfinished_tool", body: item(0, strings.Replace(tool, "completed", "in_progress", 1)) + terminal, code: contract.ErrorProtocol},
		{name: "items_without_terminal", body: item(0, hello), code: contract.ErrorProtocol},
		{name: "done_sentinel", body: item(0, hello) + "data: [DONE]\n\n", code: contract.ErrorProtocol},
		{name: "delta_not_snapshot", body: "data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n" + terminal, code: contract.ErrorProtocol},
		{name: "empty_output", body: terminal, code: contract.ErrorProtocol},
		{name: "failed_terminal", body: item(0, hello) + "data: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"fixture failed\"}}}\n\n", code: contract.ErrorProviderUnavailable},
		{name: "incomplete_terminal", body: item(0, hello) + "data: {\"type\":\"response.incomplete\",\"response\":{\"status\":\"incomplete\",\"output\":[],\"incomplete_details\":{\"reason\":\"max_output_tokens\"}}}\n\n", text: "Hello", finish: contract.FinishLength},
	} {
		t.Run(test.name, func(t *testing.T) {
			envelope, err := decodeResponseStream(strings.NewReader(test.body), true)
			var result contract.ModelResult
			if err == nil {
				result, err = envelope.result(model.ResolvedModel{Model: "fixture"}, nil)
			}
			if test.code != "" {
				if err == nil || err.Code != test.code {
					t.Fatalf("err=%v want code=%s", err, test.code)
				}
				return
			}
			if err != nil || result.Message.Content != test.text || len(result.Message.ToolCalls) != test.tools || result.FinishReason != test.finish {
				t.Fatalf("result=%+v err=%v", result, err)
			}
			if test.tools > 0 && string(result.Message.ToolCalls[0].Arguments) != `{"value":9007199254740993}` {
				t.Fatalf("tool arguments changed: %s", result.Message.ToolCalls[0].Arguments)
			}
		})
	}
	// The API-key Responses driver still requires output in its terminal Response.
	envelope, err := decodeResponseStream(strings.NewReader(item(0, hello)+terminal), false)
	if err == nil {
		_, err = envelope.result(model.ResolvedModel{Model: "fixture"}, nil)
	}
	if err == nil || err.Code != contract.ErrorProtocol {
		t.Fatalf("regular Responses behavior changed: %v", err)
	}
}

func TestCodexHTTPErrorWithoutSSEContentType(t *testing.T) {
	for _, contentType := range []string{"", "application/json"} {
		for _, test := range []struct {
			status int
			code   contract.ErrorCode
		}{
			{400, contract.ErrorProtocol},
			{401, contract.ErrorAuthenticationFailed},
			{403, contract.ErrorPermissionDenied},
			{429, contract.ErrorRateLimited},
			{503, contract.ErrorProviderUnavailable},
		} {
			t.Run(fmt.Sprintf("%s/%d", contentType, test.status), func(t *testing.T) {
				const body = `{"error":{"message":"fixture failure"}}`
				var observation model.CallObservation
				calls := 0
				service := codexService(t, codexTransport(func(*http.Request) (*http.Response, error) {
					calls++
					headers := make(http.Header)
					if contentType != "" {
						headers.Set("Content-Type", contentType)
					}
					return &http.Response{StatusCode: test.status, Header: headers, Body: io.NopCloser(strings.NewReader(body))}, nil
				}), func(context.Context, model.DriverName, string) (model.Credentials, error) {
					return model.Credentials{AccessToken: "fixture-token", AccountID: "fixture-account"}, nil
				}, func(value model.CallObservation) { observation = value })
				_, err := service.Generate(context.Background(), input())
				if err == nil || err.Code != test.code || err.Provider != "openai-codex-responses" {
					t.Fatalf("err=%v want code=%s", err, test.code)
				}
				if calls != 1 || len(observation.Attempts) != 1 || observation.Attempts[0].Wire.Response == nil {
					t.Fatalf("calls=%d observation=%+v", calls, observation)
				}
				response := observation.Attempts[0].Wire.Response
				if response.Status != test.status || len(response.Data) != 1 || string(response.Data[0]) != body {
					t.Fatalf("HTTP error JSON not preserved: %+v", response)
				}
			})
		}
	}
}

func TestCodexMissingContentTypeStillRequiresValidTerminalSSE(t *testing.T) {
	for _, test := range []struct {
		name   string
		body   string
		code   contract.ErrorCode
		frames int
	}{
		{"empty", "", contract.ErrorProtocol, 0},
		{"html", "<html>upstream failure</html>", contract.ErrorProtocol, 0},
		{"json", textResponse, contract.ErrorProtocol, 0},
		{"truncated", "event: response.output_text.delta\ndata: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n", contract.ErrorProtocol, 1},
		{"done_sentinel", "data: [DONE]\n\n", contract.ErrorProtocol, 0},
		{"invalid_json", "event: response.completed\ndata: invalid\n\n", contract.ErrorProtocol, 0},
		{"missing_response", "data: {\"type\":\"response.completed\"}\n\n", contract.ErrorProtocol, 1},
		{"sse_error", "event: error\ndata: {\"type\":\"error\",\"code\":\"rate_limit_exceeded\",\"message\":\"fixture failure\"}\n\n", contract.ErrorRateLimited, 1},
		{"response_failed", "event: response.failed\ndata: {\"type\":\"response.failed\",\"response\":{\"status\":\"failed\",\"error\":{\"code\":\"server_error\",\"message\":\"fixture failure\"}}}\n\n", contract.ErrorProviderUnavailable, 1},
	} {
		t.Run(test.name, func(t *testing.T) {
			var observation model.CallObservation
			service := codexService(t, codexTransport(func(*http.Request) (*http.Response, error) {
				return &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(test.body))}, nil
			}), func(context.Context, model.DriverName, string) (model.Credentials, error) {
				return model.Credentials{AccessToken: "fixture-token", AccountID: "fixture-account"}, nil
			}, func(value model.CallObservation) { observation = value })
			var events []contract.Event
			result, err := service.GenerateStream(context.Background(), input(), func(event contract.Event) error {
				events = append(events, event)
				return nil
			})
			if err == nil || err.Code != test.code || err.Provider != "openai-codex-responses" || len(events) != 0 || result.Message.Content != "" {
				t.Fatalf("err=%v result=%+v events=%v", err, result, events)
			}
			if len(observation.Attempts) != 1 || observation.Attempts[0].Wire.Response == nil || len(observation.Attempts[0].Wire.Response.Data) != test.frames {
				t.Fatalf("partial raw frames not preserved: %+v", observation)
			}
		})
	}
}

func TestCodexSingleCallRefreshAndWireContract(t *testing.T) {
	for _, test := range []string{"success", "401_refresh", "401_twice", "account_switch", "redirect", "truncated", "done"} {
		t.Run(test, func(t *testing.T) {
			calls, resolves := 0, 0
			var rejectedTokens []string
			var observation model.CallObservation
			service := codexService(t, codexTransport(func(request *http.Request) (*http.Response, error) {
				calls++
				if request.URL.String() != model.OpenAICodexEndpoint || request.Header.Get("Chatgpt-Account-Id") != "fixture-account" || request.Header.Get("Originator") != "sn-cli" || request.Header.Get("Accept") != "text/event-stream" {
					t.Fatal("incorrect Codex destination or headers")
				}
				wantToken := "fixture-token"
				if calls > 1 {
					wantToken = "rotated-token"
				}
				if request.Header.Get("Authorization") != "Bearer "+wantToken {
					t.Fatal("wrong access token")
				}
				var body map[string]any
				if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
					t.Fatal(err)
				}
				if body["store"] != false || body["stream"] != true || body["instructions"] != "You are a helpful assistant." || body["input"] == nil {
					t.Fatalf("body=%v", body)
				}
				for _, key := range []string{"max_output_tokens", "max_tokens", "top_p", "previous_response_id"} {
					if _, found := body[key]; found {
						t.Fatal("unsupported wire option")
					}
				}
				response := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: io.NopCloser(strings.NewReader(sseResponse(textResponse)))}
				if test == "401_twice" || calls == 1 && (test == "401_refresh" || test == "account_switch") {
					response.StatusCode = 401
					response.Body = io.NopCloser(strings.NewReader(fmt.Sprintf(`{"error":{"message":%q}}`, wantToken+" fixture-token fixture-account")))
				}
				if test == "redirect" {
					response.StatusCode = 302
					response.Header.Set("Location", "https://untrusted.invalid/")
				}
				if test == "truncated" {
					response.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.output_text.delta\",\"delta\":\"partial\"}\n\n"))
				}
				if test == "done" {
					response.Body = io.NopCloser(strings.NewReader("data: {\"type\":\"response.done\",\"response\":" + textResponse + "}\n\n"))
				}
				return response, nil
			}), func(ctx context.Context, driver model.DriverName, rejected string) (model.Credentials, error) {
				resolves++
				rejectedTokens = append(rejectedTokens, rejected)
				credential := model.Credentials{AccessToken: "fixture-token", AccountID: "fixture-account"}
				if resolves > 1 {
					credential.AccessToken = "rotated-token"
					if test == "account_switch" {
						credential.AccountID = "another-account"
					}
				}
				return credential, nil
			}, func(value model.CallObservation) { observation = value })
			request := input()
			request.Input.System = ""
			var events []contract.Event
			result, err := model.NewResilientModel(service, model.DefaultRetryPolicy()).GenerateStream(context.Background(), request, func(event contract.Event) error { events = append(events, event); return nil })
			good := test == "success" || test == "401_refresh" || test == "done"
			if good && (err != nil || result.Message.Content != "Hello" || result.Provider.Name != "openai-codex-responses" || len(events) != 3) {
				t.Fatalf("result=%+v err=%v events=%d", result, err, len(events))
			}
			if !good && (err == nil || len(events) != 0) {
				t.Fatalf("err=%v events=%d", err, len(events))
			}
			wantCalls := 1
			if test == "401_refresh" || test == "401_twice" {
				wantCalls = 2
			}
			if calls != wantCalls || len(observation.Attempts) != calls {
				t.Fatalf("calls=%d attempts=%d", calls, len(observation.Attempts))
			}
			if resolves > 1 && (len(rejectedTokens) != 2 || rejectedTokens[1] != "fixture-token") {
				t.Fatalf("conditional refresh not requested")
			}
			data, _ := json.Marshal(observation)
			for _, secret := range []string{"fixture-token", "rotated-token", "fixture-account"} {
				if strings.Contains(string(data), secret) {
					t.Fatal("credential leaked in call observation")
				}
			}
		})
	}
}

func TestCodexRejectsUnsupportedInputBeforeAuthentication(t *testing.T) {
	service := codexService(t, codexTransport(func(*http.Request) (*http.Response, error) { t.Fatal("unexpected provider request"); return nil, nil }), func(context.Context, model.DriverName, string) (model.Credentials, error) {
		t.Fatal("unexpected credential resolution")
		return model.Credentials{}, nil
	}, nil)
	for _, kind := range []string{"max_tokens", "top_p", "stop", "history"} {
		request := input()
		switch kind {
		case "max_tokens":
			value := int64(1)
			request.Input.Options.MaxOutputTokens = &value
		case "top_p":
			value := 0.5
			request.Input.Options.TopP = &value
		case "stop":
			request.Input.Options.StopSequences = []string{"END"}
		case "history":
			request.Input.Messages = append(request.Input.Messages, contract.Message{Role: contract.RoleAssistant, Content: "history"})
		}
		if _, err := service.Generate(context.Background(), request); err == nil || err.Code != contract.ErrorInvalidRequest {
			t.Fatalf("%s err=%v", kind, err)
		}
	}
	if err := service.ValidateManagedExecution("api"); err == nil {
		t.Fatal("managed execution accepted")
	}
}

func TestCodexPreservesToolSchemaIntegerPrecision(t *testing.T) {
	request := input().Input
	request.Tools[0].InputSchema = json.RawMessage(`{"type":"object","properties":{"value":{"enum":[9007199254740993]}}}`)
	body, err := encodeCodexRequest(model.ResolvedModel{Driver: model.DriverOpenAICodexResponses, Model: "fixture"}, request)
	if err != nil || !strings.Contains(string(body), "9007199254740993") {
		t.Fatalf("tool schema changed: %s err=%v", body, err)
	}
}
