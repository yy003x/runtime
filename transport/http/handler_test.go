package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yy003x/runtime/contract"
)

type stubGenerator struct {
	events []contract.Event
	result contract.ModelResult
	err    *contract.RuntimeError
}

func (stub stubGenerator) Generate(
	context.Context,
	contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return stub.result, stub.err
}

func (stub stubGenerator) GenerateStream(
	_ context.Context,
	_ contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	for _, event := range stub.events {
		if err := sink(event); err != nil {
			return contract.ModelResult{}, &contract.RuntimeError{
				Code: contract.ErrorCancelled, Phase: contract.PhaseConsumer,
				Message: "sink stopped",
			}
		}
	}
	return stub.result, stub.err
}

func TestHandlerStrictJSONAndNormalResult(t *testing.T) {
	result := contract.ModelResult{
		Message:      contract.Message{Role: contract.RoleAssistant, Content: "ok"},
		FinishReason: contract.FinishStop,
	}
	handler := NewHandler(stubGenerator{result: result})

	unknown := httptest.NewRequest(
		"POST",
		"/v1/model/generate",
		strings.NewReader(`{
			"model_profile":"fixture",
			"input":{"messages":[{"role":"user","content":"hello"}]},
			"unknown":true
		}`),
	)
	unknown.Header.Set("Content-Type", "application/json")
	unknownRecorder := httptest.NewRecorder()
	handler.ServeHTTP(unknownRecorder, unknown)
	if unknownRecorder.Code != 400 {
		t.Fatalf("unknown status=%d body=%s", unknownRecorder.Code, unknownRecorder.Body.String())
	}

	request := httptest.NewRequest(
		"POST",
		"/v1/model/generate",
		bytes.NewReader(validRequestJSON(t)),
	)
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	var current contract.ModelResult
	if err := json.Unmarshal(recorder.Body.Bytes(), &current); err != nil {
		t.Fatal(err)
	}
	if current.Message.Content != "ok" {
		t.Fatalf("result=%#v", current)
	}
}

func TestHandlerStreamsTypedError(t *testing.T) {
	handler := NewHandler(stubGenerator{
		events: []contract.Event{{Sequence: 1, Type: contract.EventModelStarted}},
		err: &contract.RuntimeError{
			Code: contract.ErrorRateLimited, Phase: contract.PhaseProvider,
			Message: "provider returned 429", Retryable: true, HTTPStatus: 429,
		},
	})
	request := httptest.NewRequest(
		"POST",
		"/v1/model/generate",
		bytes.NewReader(validRequestJSON(t)),
	)
	request.Header.Set("Content-Type", "application/json")
	request.Header.Set("Accept", "text/event-stream")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	if recorder.Code != 200 {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	for _, expected := range []string{
		"event: model.started",
		"event: error",
		`"code":"rate_limited"`,
	} {
		if !strings.Contains(recorder.Body.String(), expected) {
			t.Fatalf("stream missing %q:\n%s", expected, recorder.Body.String())
		}
	}
}

func validRequestJSON(t *testing.T) []byte {
	t.Helper()
	data, err := json.Marshal(contract.GenerateRequest{
		ModelProfile: "fixture",
		Input: contract.ModelRequest{
			Messages: []contract.Message{{Role: contract.RoleUser, Content: "hello"}},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return data
}
