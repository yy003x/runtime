package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/provider"
)

type originTestDriver struct{}

func (originTestDriver) ExecutionIdentity() model.DriverExecutionIdentity {
	return model.DriverExecutionIdentity{
		Driver: model.DriverOpenAI, Implementation: "http.origin-test",
		ImplementationVersion: 1,
	}
}

func (originTestDriver) Validate(model.Profile) error { return nil }

func (originTestDriver) Stream(
	_ context.Context,
	_ model.ResolvedModel,
	_ contract.ModelRequest,
	sink contract.EventSink,
) (contract.ModelResult, provider.Attempt, *contract.RuntimeError) {
	if err := sink(contract.Event{Sequence: 1, Type: contract.EventModelStarted}); err != nil {
		return contract.ModelResult{}, provider.Attempt{}, &contract.RuntimeError{
			Code: contract.ErrorCancelled, Phase: contract.PhaseConsumer,
			Message: err.Error(),
		}
	}
	return contract.ModelResult{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "done"},
			FinishReason: contract.FinishStop,
		}, provider.Attempt{
			Started: true,
			Request: provider.Request{Method: "POST", Body: json.RawMessage(`{}`)},
		}, nil
}

func TestModelHTTPRouteMarksProviderAttemptOrigin(t *testing.T) {
	for _, accept := range []string{"application/json", "text/event-stream"} {
		t.Run(accept, func(t *testing.T) {
			catalog, err := model.NewCatalog(map[string]model.Profile{
				"api": {
					Driver:   model.DriverOpenAI,
					Endpoint: "https://example.invalid/v1/chat/completions",
					Model:    "fixture", Timeout: "1m",
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			var observed []model.Attempt
			models, err := model.NewService(
				catalog,
				map[model.DriverName]model.Driver{model.DriverOpenAI: originTestDriver{}},
				model.ServiceOptions{AttemptObserver: func(attempt model.Attempt) {
					observed = append(observed, attempt)
				}},
			)
			if err != nil {
				t.Fatal(err)
			}
			payload, err := json.Marshal(contract.GenerateRequest{
				ModelProfile: "api",
				Input: contract.ModelRequest{Messages: []contract.Message{{
					Role: contract.RoleUser, Content: "hello",
				}}},
			})
			if err != nil {
				t.Fatal(err)
			}
			request := httptest.NewRequest(
				http.MethodPost, "/v1/model/generate", bytes.NewReader(payload),
			)
			request.Header.Set("Content-Type", "application/json")
			request.Header.Set("Accept", accept)
			writer := httptest.NewRecorder()
			NewHandler(models).ServeHTTP(writer, request)
			if writer.Code != http.StatusOK {
				t.Fatalf("status=%d body=%s", writer.Code, writer.Body.Bytes())
			}
			if len(observed) != 1 ||
				observed[0].Origin.Namespace != model.AttemptNamespaceRequest ||
				observed[0].Origin.Source != "POST /v1/model/generate" {
				t.Fatalf("observed=%#v", observed)
			}
		})
	}
}
