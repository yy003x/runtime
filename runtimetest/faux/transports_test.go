package faux

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
	transporthttp "github.com/yy003x/runtime/transport/http"
)

func TestCanonicalScenarioMatchesGoAndHTTP(t *testing.T) {
	set := loadFixtureSet(t)
	provider, err := NewProvider(set.Scripts)
	if err != nil {
		t.Fatal(err)
	}
	driver, err := NewDriver(provider)
	if err != nil {
		t.Fatal(err)
	}
	service := newCanonicalService(t, driver)
	request := canonicalRequest()

	var goEvents []contract.Event
	goResult, runtimeErr := service.GenerateStream(
		context.Background(),
		request,
		func(event contract.Event) error {
			goEvents = append(goEvents, event)
			return nil
		},
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}

	requestData, err := json.Marshal(request)
	if err != nil {
		t.Fatal(err)
	}
	httpRequest := httptest.NewRequest(
		"POST",
		"/v1/model/generate",
		bytes.NewReader(requestData),
	)
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	recorder := httptest.NewRecorder()
	transporthttp.NewHandler(service).ServeHTTP(recorder, httpRequest)
	if recorder.Code != 200 {
		t.Fatalf("status=%d body=%s", recorder.Code, recorder.Body.String())
	}
	httpEvents := decodeSSEEvents(t, recorder.Body.String())
	if !reflect.DeepEqual(httpEvents, goEvents) {
		t.Fatalf("HTTP events differ:\nHTTP=%#v\nGo=%#v", httpEvents, goEvents)
	}

	normalRequest := httptest.NewRequest(
		"POST",
		"/v1/model/generate",
		bytes.NewReader(requestData),
	)
	normalRequest.Header.Set("Content-Type", "application/json")
	normalRecorder := httptest.NewRecorder()
	transporthttp.NewHandler(service).ServeHTTP(normalRecorder, normalRequest)
	if normalRecorder.Code != 200 {
		t.Fatalf("normal status=%d body=%s", normalRecorder.Code, normalRecorder.Body.String())
	}
	var httpResult contract.ModelResult
	if err := json.Unmarshal(normalRecorder.Body.Bytes(), &httpResult); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(httpResult, goResult) {
		t.Fatalf("HTTP result=%#v Go result=%#v", httpResult, goResult)
	}
	if provider.Attempts("text") != 3 {
		t.Fatalf("attempts=%d, want one per Go/HTTP call", provider.Attempts("text"))
	}
}

func newCanonicalService(t *testing.T, driver model.Driver) *model.Service {
	t.Helper()
	profile := model.Profile{
		Driver:   model.DriverOpenAI,
		Endpoint: "https://example.invalid/v1/chat/completions",
		Model:    "fixture",
		Headers:  map[string]string{"Authorization": "${FAUX_MODEL_KEY}"},
		Timeout:  "1m",
	}
	catalog, err := model.NewCatalog(map[string]model.Profile{"fixture": profile})
	if err != nil {
		t.Fatal(err)
	}
	service, err := model.NewService(
		catalog,
		map[model.DriverName]model.Driver{model.DriverOpenAI: driver},
		model.ServiceOptions{Getenv: func(name string) (string, bool) {
			return "fixture-secret", name == "FAUX_MODEL_KEY"
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func canonicalRequest() contract.GenerateRequest {
	return contract.GenerateRequest{
		ModelProfile: "fixture",
		Input: contract.ModelRequest{
			Messages: []contract.Message{{Role: contract.RoleUser, Content: "hello"}},
			Trace:    contract.TraceContext{Labels: map[string]string{ScenarioLabel: "text"}},
		},
	}
}

func decodeSSEEvents(t *testing.T, data string) []contract.Event {
	t.Helper()
	var events []contract.Event
	for _, line := range strings.Split(data, "\n") {
		if !strings.HasPrefix(line, "data: ") {
			continue
		}
		var event contract.Event
		if err := json.Unmarshal([]byte(strings.TrimPrefix(line, "data: ")), &event); err != nil {
			t.Fatal(err)
		}
		events = append(events, event)
	}
	return events
}
