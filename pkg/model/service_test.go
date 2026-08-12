package model

import (
	"context"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/provider"
)

type testDriver struct {
	attempts    int
	events      []contract.Event
	result      contract.ModelResult
	err         *contract.RuntimeError
	mutateInput bool
	identity    *DriverExecutionIdentity
	attempt     provider.Attempt
}

func (driver *testDriver) ExecutionIdentity() DriverExecutionIdentity {
	if driver.identity != nil {
		return *driver.identity
	}
	return DriverExecutionIdentity{
		Driver:                DriverOpenAI,
		Implementation:        "runtime.model.test-driver",
		ImplementationVersion: 1,
	}
}

func (driver *testDriver) Validate(Profile) error {
	return nil
}

func (driver *testDriver) Stream(
	_ context.Context,
	_ ResolvedModel,
	request contract.ModelRequest,
	sink contract.EventSink,
) (contract.ModelResult, provider.Attempt, *contract.RuntimeError) {
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

func TestServiceRedactsSecretsAndDoesNotRetryAfterOutput(t *testing.T) {
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
	data, _ := json.Marshal(struct {
		Events []contract.Event       `json:"events"`
		Error  *contract.RuntimeError `json:"error"`
	}{Events: events, Error: runtimeErr})
	if strings.Contains(string(data), "secret-value") {
		t.Fatalf("secret leaked: %s", data)
	}
	if driver.attempts != 1 {
		t.Fatalf("attempts=%d", driver.attempts)
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
		Driver: DriverOpenAI, Endpoint: "https://example.invalid/faux", Model: "fixture",
		Headers: map[string]string{"Authorization": "${MODEL_API_KEY}"},
		Timeout: "1m",
	}
	catalog, err := NewCatalog(map[string]Profile{"fixture": profile})
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(
		catalog,
		map[DriverName]Driver{DriverOpenAI: driver},
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
