package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yy003x/sn-code/pkg/contract"
	"github.com/yy003x/sn-code/pkg/provider"
)

type testDriver struct {
	attempts    int
	events      []contract.Event
	result      contract.ModelResult
	err         *contract.RuntimeError
	mutateInput bool
	identity    *DriverExecutionIdentity
	attempt     provider.Attempt
	gotStream   *bool
}

func (driver *testDriver) ExecutionIdentity() DriverExecutionIdentity {
	if driver.identity != nil {
		return *driver.identity
	}
	return DriverExecutionIdentity{
		Driver:                DriverOpenAIChatCompletions,
		Implementation:        "runtime.model.test-driver",
		ImplementationVersion: 1,
	}
}

func (driver *testDriver) Validate(Profile) error {
	return nil
}

func (driver *testDriver) Stream(
	_ context.Context,
	resolved ResolvedModel,
	request contract.ModelRequest,
	sink contract.EventSink,
) (contract.ModelResult, provider.Attempt, *contract.RuntimeError) {
	value := resolved.Parameters.StreamingEnabled()
	driver.gotStream = &value
	driver.attempts++
	if driver.mutateInput {
		request.Messages[0].Content = "mutated"
		request.Trace.Labels["driver"] = "mutated"
	}
	for _, event := range driver.events {
		if err := sink(event); err != nil {
			return contract.ModelResult{}, driver.attempt, &contract.RuntimeError{
				Code: contract.ErrorCancelled, Phase: contract.PhaseConsumer,
				Message: err.Error(),
			}
		}
	}
	return driver.result, driver.attempt, driver.err
}

func TestServiceRequestStreamOverridesProfile(t *testing.T) {
	driver := &testDriver{
		events: []contract.Event{{Sequence: 1, Type: contract.EventModelStarted}},
		result: contract.ModelResult{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "ok"},
			FinishReason: contract.FinishStop,
		},
	}
	falseValue := false
	profile := Profile{
		Driver: DriverOpenAIChatCompletions, Endpoint: "https://example.invalid/faux", Model: "fixture",
		Headers:    map[string]string{"Authorization": "${MODEL_API_KEY}"},
		Timeout:    "1m",
		Parameters: Parameters{Stream: &falseValue},
	}
	catalog, err := NewCatalog(map[string]Profile{"fixture": profile})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(
		catalog,
		map[DriverName]Driver{DriverOpenAIChatCompletions: driver},
		ServiceOptions{Getenv: func(name string) (string, bool) {
			return "secret", name == "MODEL_API_KEY"
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	request := testRequest()
	if _, runtimeErr := service.Generate(context.Background(), request); runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if driver.gotStream == nil || *driver.gotStream {
		t.Fatalf("profile stream=false leaked as %v", driver.gotStream)
	}
	trueValue := true
	request.Input.Options.Stream = &trueValue
	if _, runtimeErr := service.Generate(context.Background(), request); runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if driver.gotStream == nil || !*driver.gotStream {
		t.Fatalf("request stream=true was not applied: %v", driver.gotStream)
	}
}

func TestServiceUsesOneAttemptAndCompletesStream(t *testing.T) {
	driver := &testDriver{
		events: []contract.Event{
			{Sequence: 1, Type: contract.EventModelStarted},
			{
				Sequence: 2, Type: contract.EventContentDelta,
				Model: &contract.ModelEvent{Text: "hello"},
			},
		},
		result: contract.ModelResult{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "hello"},
			FinishReason: contract.FinishStop,
		},
	}
	service := newTestService(t, driver, "secret")
	var events []contract.Event
	result, runtimeErr := service.GenerateStream(
		context.Background(),
		testRequest(),
		func(event contract.Event) error {
			events = append(events, event)
			return nil
		},
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if result.Message.Content != "hello" || driver.attempts != 1 {
		t.Fatalf("result=%#v attempts=%d", result, driver.attempts)
	}
	if len(events) != 3 || events[2].Type != contract.EventModelCompleted ||
		events[2].Model == nil || events[2].Model.Result == nil {
		t.Fatalf("events=%#v", events)
	}
}

func TestServiceIsolatesRequestAndSinkMutations(t *testing.T) {
	driver := &testDriver{
		mutateInput: true,
		events: []contract.Event{
			{Sequence: 1, Type: contract.EventModelStarted},
		},
		result: contract.ModelResult{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "hello"},
			FinishReason: contract.FinishStop,
		},
	}
	service := newTestService(t, driver, "secret")
	request := testRequest()
	request.Input.Trace.Labels = map[string]string{"caller": "stable"}
	result, runtimeErr := service.GenerateStream(
		context.Background(),
		request,
		func(event contract.Event) error {
			if event.Type == contract.EventModelCompleted {
				event.Model.Result.Message.Content = "sink-mutated"
			}
			return nil
		},
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if request.Input.Messages[0].Content != "hello" ||
		request.Input.Trace.Labels["driver"] != "" {
		t.Fatalf("request was mutated: %#v", request.Input)
	}
	if result.Message.Content != "hello" {
		t.Fatalf("result was mutated by sink: %#v", result)
	}
}

// 消费者面（事件流与返回错误）原样透传：resolved secret 只存在于 HTTP 请求
// 层；诊断面的脱敏由 attempt/call observer 测试覆盖。
func TestServiceStreamsSecretTextVerbatimAndDoesNotRetryAfterOutput(t *testing.T) {
	driver := &testDriver{
		events: []contract.Event{
			{Sequence: 1, Type: contract.EventModelStarted},
			{
				Sequence: 2, Type: contract.EventContentDelta,
				Model: &contract.ModelEvent{Text: "partial secret-value"},
			},
		},
		err: &contract.RuntimeError{
			Code: contract.ErrorProviderUnavailable, Phase: contract.PhaseProvider,
			Message: "secret-value connection closed", Retryable: true,
		},
	}
	service := newTestService(t, driver, "secret-value")
	var events []contract.Event
	_, runtimeErr := service.GenerateStream(
		context.Background(),
		testRequest(),
		func(event contract.Event) error {
			events = append(events, event)
			return nil
		},
	)
	if runtimeErr == nil || runtimeErr.Code != contract.ErrorProviderUnavailable {
		t.Fatalf("error=%#v", runtimeErr)
	}
	if runtimeErr.Message != "secret-value connection closed" {
		t.Fatalf("consumer error was rewritten: %#v", runtimeErr)
	}
	if len(events) != 2 || events[1].Model == nil ||
		events[1].Model.Text != "partial secret-value" {
		t.Fatalf("consumer events were rewritten: %#v", events)
	}
	if driver.attempts != 1 {
		t.Fatalf("attempts=%d", driver.attempts)
	}
}

// 单字符 secret 是常见 JSON key（"text"、"type"）的子串；脱敏只允许替换
// 字符串值，改写 key 会破坏结构并让下游 unmarshal 丢弃字段（回归：此前对
// 整段 JSON 文本做字节替换会命中此缺陷）。
func TestRedactionKeepsJSONKeysIntactForShortSecrets(t *testing.T) {
	driver := &testDriver{
		events: []contract.Event{
			{Sequence: 1, Type: contract.EventModelStarted},
			{
				Sequence: 2, Type: contract.EventContentDelta,
				Model: &contract.ModelEvent{Text: "x marks"},
			},
		},
		err: &contract.RuntimeError{
			Code: contract.ErrorProviderUnavailable, Phase: contract.PhaseProvider,
			Message: "x failed", Retryable: true,
		},
		attempt: provider.Attempt{
			Started: true,
			Request: provider.Request{
				Method: "POST", URL: "https://example.invalid",
				Headers: map[string]string{"Authorization": "Bearer x"},
				Body: json.RawMessage(
					`{"text":"x","tool_calls":[{"type":"x"}],"big":9007199254740993}`,
				),
			},
			Response: &provider.Response{
				Status: 503,
				Data:   []json.RawMessage{json.RawMessage(`{"error":"x failed"}`)},
			},
		},
	}
	var events []contract.Event
	var observed []Attempt
	service := newTestServiceWithObserver(t, driver, "x", func(attempt Attempt) {
		observed = append(observed, attempt)
	})
	_, runtimeErr := service.GenerateStream(
		context.Background(),
		testRequest(),
		func(event contract.Event) error {
			events = append(events, event)
			return nil
		},
	)
	if runtimeErr == nil || runtimeErr.Code != contract.ErrorProviderUnavailable {
		t.Fatalf("error=%#v", runtimeErr)
	}
	// 消费者面原样：含 secret 的 delta 与错误文本不被替换。
	if len(events) != 2 || events[1].Model.Text != "x marks" ||
		runtimeErr.Message != "x failed" {
		t.Fatalf("consumer stream was rewritten: %#v %v", events, runtimeErr)
	}
	if len(observed) != 1 {
		t.Fatalf("observed=%d", len(observed))
	}
	safe := observed[0]
	if safe.Wire.Request.Headers["Authorization"] != "Bearer [REDACTED]" {
		t.Fatalf("header not redacted: %#v", safe.Wire.Request.Headers)
	}
	if safe.Error == nil || safe.Error.Message != "[REDACTED] failed" {
		t.Fatalf("attempt error not redacted: %#v", safe.Error)
	}
	var body map[string]any
	if err := json.Unmarshal(safe.Wire.Request.Body, &body); err != nil {
		t.Fatalf("redacted body is not valid JSON: %s", safe.Wire.Request.Body)
	}
	if body["text"] != "[REDACTED]" {
		t.Fatalf(`key "text" was rewritten or value not redacted: %s`, safe.Wire.Request.Body)
	}
	calls, _ := body["tool_calls"].([]any)
	if len(calls) != 1 {
		t.Fatalf("tool_calls lost: %s", safe.Wire.Request.Body)
	}
	first, _ := calls[0].(map[string]any)
	if first["type"] != "[REDACTED]" {
		t.Fatalf(`nested key "type" was rewritten: %s`, safe.Wire.Request.Body)
	}
	// 大整数经 JSON 树往返必须保持字面量精度（UseNumber 路径）。
	if !strings.Contains(string(safe.Wire.Request.Body), "9007199254740993") {
		t.Fatalf("number literal lost precision: %s", safe.Wire.Request.Body)
	}
}

func TestServiceStopsOnSinkFailure(t *testing.T) {
	driver := &testDriver{
		events: []contract.Event{{Sequence: 1, Type: contract.EventModelStarted}},
		result: contract.ModelResult{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "hello"},
			FinishReason: contract.FinishStop,
		},
	}
	service := newTestService(t, driver, "secret")
	_, runtimeErr := service.GenerateStream(
		context.Background(),
		testRequest(),
		func(contract.Event) error { return errors.New("stop") },
	)
	if runtimeErr == nil || runtimeErr.Code != contract.ErrorCancelled ||
		runtimeErr.Phase != contract.PhaseConsumer {
		t.Fatalf("error=%#v", runtimeErr)
	}
}

func TestServiceObservesOnlyStartedAttemptsWithTrustedOriginAndRedaction(t *testing.T) {
	driver := &testDriver{
		events: []contract.Event{{Sequence: 1, Type: contract.EventModelStarted}},
		err: &contract.RuntimeError{
			Code: contract.ErrorProviderUnavailable, Phase: contract.PhaseProvider,
			Message: "secret-value failed", Retryable: true,
		},
		attempt: provider.Attempt{
			Started: true,
			Request: provider.Request{
				Method: "POST", URL: "https://example.invalid",
				Headers: map[string]string{"Authorization": "Bearer secret-value"},
				Body:    json.RawMessage(`{"prompt":"secret-value"}`),
			},
			Response: &provider.Response{
				Status: 503,
				Data: []json.RawMessage{
					json.RawMessage(`{"message":"secret-value failed"}`),
				},
			},
		},
	}
	var observed []Attempt
	service := newTestServiceWithObserver(t, driver, "secret-value", func(attempt Attempt) {
		observed = append(observed, attempt)
	})
	ctx := WithAttemptOrigin(context.Background(), AttemptOrigin{
		Namespace: AttemptNamespaceSession, Source: "session session_fixture",
	})
	_, runtimeErr := service.Generate(ctx, testRequest())
	if runtimeErr == nil || len(observed) != 1 {
		t.Fatalf("error=%#v observed=%#v", runtimeErr, observed)
	}
	got := observed[0]
	if got.Origin.Namespace != AttemptNamespaceSession ||
		got.Origin.Source != "session session_fixture" || got.ProfileID != "fixture" {
		t.Fatalf("attempt=%#v", got)
	}
	data, err := json.Marshal(got)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret-value") ||
		!strings.Contains(string(data), "[REDACTED]") {
		t.Fatalf("attempt was not redacted: %s", data)
	}

	driver.attempt.Started = false
	observed = nil
	_, _ = service.Generate(context.Background(), testRequest())
	if len(observed) != 0 {
		t.Fatalf("pre-network failure was observed: %#v", observed)
	}
}

func TestServiceIgnoresAttemptObserverPanic(t *testing.T) {
	driver := &testDriver{
		events: []contract.Event{{Sequence: 1, Type: contract.EventModelStarted}},
		result: contract.ModelResult{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "hello"},
			FinishReason: contract.FinishStop,
		},
		attempt: provider.Attempt{
			Started: true,
			Request: provider.Request{Method: "POST", Body: json.RawMessage(`{}`)},
		},
	}
	service := newTestServiceWithObserver(t, driver, "secret", func(Attempt) {
		panic("diagnostic writer failed")
	})
	result, runtimeErr := service.Generate(context.Background(), testRequest())
	if runtimeErr != nil || result.Message.Content != "hello" {
		t.Fatalf("result=%#v error=%#v", result, runtimeErr)
	}
}

func TestServiceRejectsInvalidStartedPayload(t *testing.T) {
	driver := &testDriver{
		events: []contract.Event{{
			Sequence: 1, Type: contract.EventModelStarted,
			Model: &contract.ModelEvent{Text: "unexpected"},
		}},
	}
	service := newTestService(t, driver, "secret")
	_, runtimeErr := service.GenerateStream(
		context.Background(),
		testRequest(),
		func(contract.Event) error { return nil },
	)
	if runtimeErr == nil || runtimeErr.Code != contract.ErrorInvalidProviderResponse {
		t.Fatalf("error=%#v", runtimeErr)
	}
}

func TestServiceValidatesToolCallLifecycle(t *testing.T) {
	unknownDelta := &testDriver{
		events: []contract.Event{
			{Sequence: 1, Type: contract.EventModelStarted},
			{
				Sequence: 2, Type: contract.EventToolCallArgumentsDelta,
				Model: &contract.ModelEvent{ToolCallID: "missing", Text: `{"city":`},
			},
		},
	}
	service := newTestService(t, unknownDelta, "secret")
	_, runtimeErr := service.GenerateStream(
		context.Background(), testRequest(), func(contract.Event) error { return nil },
	)
	if runtimeErr == nil || runtimeErr.Code != contract.ErrorInvalidProviderResponse {
		t.Fatalf("unknown delta error=%#v", runtimeErr)
	}

	call := contract.ToolCall{
		ID: "call-1", Name: "weather", Arguments: json.RawMessage(`{}`),
	}
	duplicateDriver := &testDriver{
		events: []contract.Event{
			{Sequence: 1, Type: contract.EventModelStarted},
			{
				Sequence: 2, Type: contract.EventToolCallStarted,
				Model: &contract.ModelEvent{ToolCall: &call},
			},
			{
				Sequence: 3, Type: contract.EventToolCallStarted,
				Model: &contract.ModelEvent{ToolCall: &call},
			},
		},
	}
	service = newTestService(t, duplicateDriver, "secret")
	_, runtimeErr = service.GenerateStream(
		context.Background(), testRequest(), func(contract.Event) error { return nil },
	)
	if runtimeErr == nil || runtimeErr.Code != contract.ErrorInvalidProviderResponse {
		t.Fatalf("duplicate call error=%#v", runtimeErr)
	}

	resultCall := contract.ToolCall{
		ID: "call-1", Name: "weather",
		Arguments: json.RawMessage(`{"city":"Shanghai"}`),
	}
	validDriver := &testDriver{
		events: []contract.Event{
			{Sequence: 1, Type: contract.EventModelStarted},
			{
				Sequence: 2, Type: contract.EventToolCallStarted,
				Model: &contract.ModelEvent{ToolCall: &call},
			},
			{
				Sequence: 3, Type: contract.EventToolCallArgumentsDelta,
				Model: &contract.ModelEvent{ToolCallID: "call-1", Text: `{"city":"Shanghai"}`},
			},
		},
		result: contract.ModelResult{
			Message: contract.Message{
				Role: contract.RoleAssistant, ToolCalls: []contract.ToolCall{resultCall},
			},
			FinishReason: contract.FinishToolCall,
		},
	}
	service = newTestService(t, validDriver, "secret")
	if _, runtimeErr := service.GenerateStream(
		context.Background(), testRequest(), func(contract.Event) error { return nil },
	); runtimeErr != nil {
		t.Fatal(runtimeErr)
	}

	for name, current := range map[string]*testDriver{
		"started_missing_from_result": {
			events: []contract.Event{
				{Sequence: 1, Type: contract.EventModelStarted},
				{
					Sequence: 2, Type: contract.EventToolCallStarted,
					Model: &contract.ModelEvent{ToolCall: &call},
				},
			},
			result: contract.ModelResult{
				Message:      contract.Message{Role: contract.RoleAssistant, Content: "no tool"},
				FinishReason: contract.FinishStop,
			},
		},
		"result_without_start": {
			events: []contract.Event{{Sequence: 1, Type: contract.EventModelStarted}},
			result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, ToolCalls: []contract.ToolCall{resultCall},
				},
				FinishReason: contract.FinishToolCall,
			},
		},
		"result_changed_name": {
			events: []contract.Event{
				{Sequence: 1, Type: contract.EventModelStarted},
				{
					Sequence: 2, Type: contract.EventToolCallStarted,
					Model: &contract.ModelEvent{ToolCall: &call},
				},
			},
			result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant,
					ToolCalls: []contract.ToolCall{{
						ID: "call-1", Name: "time", Arguments: json.RawMessage(`{}`),
					}},
				},
				FinishReason: contract.FinishToolCall,
			},
		},
	} {
		t.Run(name, func(t *testing.T) {
			service := newTestService(t, current, "secret")
			_, runtimeErr := service.GenerateStream(
				context.Background(), testRequest(), func(contract.Event) error { return nil },
			)
			if runtimeErr == nil || runtimeErr.Code != contract.ErrorInvalidProviderResponse {
				t.Fatalf("error=%#v", runtimeErr)
			}
		})
	}
}

func newTestService(t *testing.T, driver Driver, secret string) *Service {
	return newTestServiceWithObserver(t, driver, secret, nil)
}

func newTestServiceWithObserver(
	t *testing.T,
	driver Driver,
	secret string,
	observer AttemptObserver,
) *Service {
	t.Helper()
	profile := Profile{
		Driver: DriverOpenAIChatCompletions, Endpoint: "https://example.invalid/faux", Model: "fixture",
		Headers: map[string]string{"Authorization": "${MODEL_API_KEY}"},
		Timeout: "1m",
	}
	catalog, err := NewCatalog(map[string]Profile{"fixture": profile})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(
		catalog,
		map[DriverName]Driver{DriverOpenAIChatCompletions: driver},
		ServiceOptions{Getenv: func(name string) (string, bool) {
			return secret, name == "MODEL_API_KEY"
		}, AttemptObserver: observer},
	)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func testRequest() contract.GenerateRequest {
	return contract.GenerateRequest{
		ModelProfile: "fixture",
		Input: contract.ModelRequest{
			Messages: []contract.Message{{Role: contract.RoleUser, Content: "hello"}},
		},
	}
}
