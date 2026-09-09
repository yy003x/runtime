package model

import (
	"context"
	"encoding/json"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/provider"
)

func TestRequestAttemptObserverIsolatedFromServiceObserver(t *testing.T) {
	driver := &testDriver{
		err: &contract.RuntimeError{
			Code: contract.ErrorAuthenticationFailed, Phase: contract.PhaseProvider,
			Message: "fixture-secret rejected",
		},
		attempt: provider.Attempt{
			Started: true,
			Request: provider.Request{
				Headers: map[string]string{"Authorization": "Bearer fixture-secret"},
				Body:    json.RawMessage(`{"input":"fixture-secret"}`),
			},
			Response: &provider.Response{Status: 401, Data: []json.RawMessage{json.RawMessage(`{"error":"fixture-secret"}`)}},
		},
	}
	serviceCalls, requestCalls := 0, 0
	service := newTestServiceWithObserver(t, driver, "fixture-secret", func(attempt Attempt) {
		serviceCalls++
		attempt.Wire.Request.Headers["Authorization"] = "mutated"
		attempt.Wire.Request.Body[0] = '!'
		attempt.Wire.Response.Data[0][0] = '!'
		attempt.Error.Message = "mutated"
		panic("fixture service observer failed")
	})
	ctx := WithAttemptObserver(context.Background(), func(attempt Attempt) {
		requestCalls++
		if attempt.Wire.Request.Headers["Authorization"] != "Bearer [REDACTED]" ||
			string(attempt.Wire.Request.Body) != `{"input":"[REDACTED]"}` ||
			string(attempt.Wire.Response.Data[0]) != `{"error":"[REDACTED]"}` ||
			attempt.Error.Message != "[REDACTED] rejected" {
			t.Errorf("request observer received unsanitized or mutated evidence: %#v", attempt)
		}
		attempt.Error.Message = "request observer mutation"
		panic("fixture request observer failed")
	})
	_, runtimeErr := service.Generate(ctx, testRequest())
	if runtimeErr == nil || runtimeErr.Message != "[REDACTED] rejected" || serviceCalls != 1 || requestCalls != 1 {
		t.Fatalf("error=%v service=%d request=%d", runtimeErr, serviceCalls, requestCalls)
	}
	_, _ = service.Generate(context.Background(), testRequest())
	if serviceCalls != 2 || requestCalls != 1 || string(driver.attempt.Request.Body) != `{"input":"fixture-secret"}` {
		t.Fatal("observer state escaped its request scope")
	}
}

func TestRequestAttemptObserverWithoutServiceObserverAndPreflight(t *testing.T) {
	driver := &testDriver{
		events:  []contract.Event{{Sequence: 1, Type: contract.EventModelStarted}},
		result:  contract.ModelResult{Message: contract.Message{Role: contract.RoleAssistant, Content: "OK"}, FinishReason: contract.FinishStop},
		attempt: provider.Attempt{Started: true, Request: provider.Request{Body: json.RawMessage(`{}`)}},
	}
	service := newTestService(t, driver, "fixture-secret")
	calls := 0
	parent := WithAttemptObserver(nil, func(Attempt) { t.Error("replaced parent observer was called") })
	ctx := WithAttemptObserver(parent, func(Attempt) { calls++ })
	result, err := service.Generate(ctx, testRequest())
	if err != nil || calls != 1 || result.Message.Content != "OK" {
		t.Fatalf("result=%#v error=%v calls=%d", result, err, calls)
	}
	_, err = service.Generate(ctx, contract.GenerateRequest{})
	if err == nil || calls != 1 {
		t.Fatalf("preflight error=%v calls=%d", err, calls)
	}
	_, err = service.Generate(WithAttemptObserver(ctx, nil), testRequest())
	if err != nil || calls != 1 {
		t.Fatalf("nil observer error=%v calls=%d", err, calls)
	}
	if attemptObserver(nil) != nil {
		t.Fatal("nil context has an observer")
	}
}

// The ordinary testDriver counts attempts and is intentionally not concurrent.
// This fixture has no mutable driver state, like the production wire adapters.
type concurrentAttemptDriver struct{ testDriver }

func (*concurrentAttemptDriver) Stream(
	_ context.Context, _ ResolvedModel, request contract.ModelRequest, sink contract.EventSink,
) (contract.ModelResult, provider.Attempt, *contract.RuntimeError) {
	_ = sink(contract.Event{Sequence: 1, Type: contract.EventModelStarted})
	body, _ := json.Marshal(request)
	return contract.ModelResult{
		Message: contract.Message{Role: contract.RoleAssistant, Content: "OK"}, FinishReason: contract.FinishStop,
	}, provider.Attempt{Started: true, Request: provider.Request{Body: body}}, nil
}

func TestRequestAttemptObserversDoNotCrossConcurrentCalls(t *testing.T) {
	var globalCalls atomic.Int64
	service := newTestServiceWithObserver(t, &concurrentAttemptDriver{}, "fixture-secret", func(Attempt) { globalCalls.Add(1) })
	var workers sync.WaitGroup
	for i := 0; i < 16; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			id := fmt.Sprintf("request-%d-unique", i)
			request := testRequest()
			request.Input.Messages[0].Content = id
			calls := 0
			ctx := WithAttemptObserver(context.Background(), func(attempt Attempt) {
				calls++
				if !strings.Contains(string(attempt.Wire.Request.Body), id) {
					t.Errorf("request %s received another call's evidence: %s", id, attempt.Wire.Request.Body)
				}
			})
			if _, err := service.Generate(ctx, request); err != nil || calls != 1 {
				t.Errorf("request %s: error=%v calls=%d", id, err, calls)
			}
		}()
	}
	workers.Wait()
	if globalCalls.Load() != 16 {
		t.Fatalf("service observer calls=%d", globalCalls.Load())
	}
}

func TestDirectCallObserverIsRedactedIsolatedAndBestEffort(t *testing.T) {
	driver := &concurrentAttemptDriver{}
	service := newTestService(t, driver, "fixture-secret")
	calls := 0
	service.callObserver = func(call CallObservation) {
		calls++
		data, err := json.Marshal(call)
		if err != nil || strings.Contains(string(data), "fixture-secret") {
			t.Errorf("call observation not redacted: %s error=%v", data, err)
		}
		call.Attempts[0].Wire.Request.Body[0] = '!'
		panic("fixture call observer failed")
	}
	request := testRequest()
	request.Input.Messages[0].Content = "hello fixture-secret"
	attemptCalls := 0
	ctx := WithAttemptObserver(context.Background(), func(attempt Attempt) {
		attemptCalls++
		if !json.Valid(attempt.Wire.Request.Body) || strings.Contains(string(attempt.Wire.Request.Body), "fixture-secret") {
			t.Error("call observer mutated the request observer's evidence")
		}
	})
	result, err := service.Generate(ctx, request)
	if err != nil || result.Message.Content != "OK" || calls != 1 || attemptCalls != 1 {
		t.Fatalf("result=%#v error=%v calls=%d attempts=%d", result, err, calls, attemptCalls)
	}
	if _, err := service.Generate(ctx, contract.GenerateRequest{}); err == nil || calls != 1 {
		t.Fatalf("preflight error=%v calls=%d", err, calls)
	}
}

type observedRetryDriver struct {
	testDriver
	failures int
}

func (driver *observedRetryDriver) Stream(
	_ context.Context, _ ResolvedModel, request contract.ModelRequest, sink contract.EventSink,
) (contract.ModelResult, provider.Attempt, *contract.RuntimeError) {
	driver.attempts++
	_ = sink(contract.Event{Sequence: 1, Type: contract.EventModelStarted})
	body, _ := json.Marshal(request)
	wire := provider.Attempt{
		Started: true, Request: provider.Request{Body: body},
		Response: &provider.Response{Status: 200, Data: []json.RawMessage{json.RawMessage(`{"text":"OK"}`)}},
	}
	if driver.attempts <= driver.failures {
		wire.Response.Status = 503
		wire.Response.Data = []json.RawMessage{json.RawMessage(`{"message":"fixture-secret failed"}`)}
		return contract.ModelResult{}, wire, retryableProviderError("fixture-secret failed")
	}
	return contract.ModelResult{
		Message: contract.Message{Role: contract.RoleAssistant, Content: "OK"}, FinishReason: contract.FinishStop,
	}, wire, nil
}

func TestResilientCallObserverGroupsAttempts(t *testing.T) {
	for _, test := range []struct {
		name          string
		failures      int
		cancelBackoff bool
		consumerError bool
		wantAttempts  int
		wantError     contract.ErrorCode
	}{
		{"success", 0, false, false, 1, ""},
		{"retry_success", 1, false, false, 2, ""},
		{"exhausted", 3, false, false, 3, contract.ErrorProviderUnavailable},
		{"cancelled_backoff", 1, true, false, 1, contract.ErrorCancelled},
		{"consumer_failed", 0, false, true, 1, contract.ErrorCancelled},
	} {
		t.Run(test.name, func(t *testing.T) {
			driver := &observedRetryDriver{failures: test.failures}
			var perAttempt []Attempt
			service := newTestServiceWithObserver(t, driver, "fixture-secret", func(attempt Attempt) {
				perAttempt = append(perAttempt, attempt)
				attempt.Wire.Request.Body[0] = '!'
			})
			var calls []CallObservation
			service.callObserver = func(call CallObservation) {
				calls = append(calls, call)
				panic("fixture observer failure must not affect retry")
			}
			generator := NewResilientModel(service, DefaultRetryPolicy())
			generator.backoff = func(int, time.Duration) time.Duration { return 0 }
			ctx, cancel := context.WithCancel(context.Background())
			defer cancel()
			ctx = WithAttemptOrigin(ctx, AttemptOrigin{Namespace: AttemptNamespaceAgent, Source: "agent fixture-secret"})
			if test.cancelBackoff {
				generator.sleep = func(context.Context, time.Duration) bool { cancel(); return false }
			}
			var sink contract.EventSink
			if test.consumerError {
				sink = func(contract.Event) error { return fmt.Errorf("fixture consumer failed") }
			}
			request := testRequest()
			request.Input.Messages[0].Content = "hello fixture-secret"
			_, resultError := generator.GenerateStream(ctx, request, sink)
			if (resultError != nil) != (test.wantError != "") || (resultError != nil && resultError.Code != test.wantError) {
				t.Fatalf("error=%v want=%s", resultError, test.wantError)
			}
			if len(calls) != 1 || len(calls[0].Attempts) != test.wantAttempts || len(perAttempt) != test.wantAttempts {
				t.Fatalf("calls=%d attempts=%d", len(calls), len(perAttempt))
			}
			call := calls[0]
			if call.Origin.Namespace != AttemptNamespaceAgent || call.Origin.Source != "agent [REDACTED]" || call.ProfileID != "fixture" {
				t.Fatalf("origin=%#v profile=%s", call.Origin, call.ProfileID)
			}
			if (call.Error != nil) != (resultError != nil) || (call.Error != nil && call.Error.Code != resultError.Code) {
				t.Fatalf("observed error=%v result error=%v", call.Error, resultError)
			}
			for index, attempt := range call.Attempts {
				if !json.Valid(attempt.Wire.Request.Body) || (attempt.Error != nil) != (index < test.failures) {
					t.Fatalf("attempt %d mutated or has wrong error: %#v", index+1, attempt)
				}
			}
			data, err := json.Marshal(call)
			if err != nil || strings.Contains(string(data), "fixture-secret") {
				t.Fatalf("call not redacted: %s error=%v", data, err)
			}
			if _, err := generator.Generate(ctx, contract.GenerateRequest{}); err == nil || len(calls) != 1 {
				t.Fatalf("preflight error=%v calls=%d", err, len(calls))
			}
		})
	}
}

func TestCallObservationsDoNotCrossConcurrentCalls(t *testing.T) {
	service := newTestService(t, &concurrentAttemptDriver{}, "fixture-secret")
	var mutex sync.Mutex
	seen := make(map[string]bool)
	service.callObserver = func(call CallObservation) {
		if len(call.Attempts) != 1 {
			t.Errorf("attempt count=%d", len(call.Attempts))
			return
		}
		mutex.Lock()
		defer mutex.Unlock()
		key := string(call.Attempts[0].Wire.Request.Body)
		if seen[key] {
			t.Errorf("duplicated call observation: %s", key)
		}
		seen[key] = true
	}
	generator := NewResilientModel(service, DefaultRetryPolicy())
	var workers sync.WaitGroup
	for index := 0; index < 16; index++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			request := testRequest()
			request.Input.Messages[0].Content = fmt.Sprintf("call-%d", index)
			if _, err := generator.Generate(context.Background(), request); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	if len(seen) != 16 {
		t.Fatalf("calls=%d", len(seen))
	}
}
