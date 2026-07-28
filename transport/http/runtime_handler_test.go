package http

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/agent"
	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/profile"
	runtime "github.com/yy003x/runtime/run"
	"github.com/yy003x/runtime/session"
	sqlitestore "github.com/yy003x/runtime/store/sqlite"
)

func TestRuntimeHandlerSessionAndAgentShareServices(t *testing.T) {
	handler, services := newRuntimeHandlerTest(t)
	created := performJSON(
		t, handler, http.MethodPost, "/v1/sessions", `{}`,
	)
	if created.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", created.Code, created.Body.String())
	}
	var sessionValue session.Session
	if err := json.Unmarshal(created.Body.Bytes(), &sessionValue); err != nil {
		t.Fatal(err)
	}
	turn := performJSON(
		t, handler, http.MethodPost,
		"/v1/sessions/"+sessionValue.ID+"/turns",
		`{"profile_id":"api","input":"hello"}`,
	)
	if turn.Code != http.StatusOK ||
		!strings.Contains(turn.Body.String(), `"state":"completed"`) {
		t.Fatalf("turn status=%d body=%s", turn.Code, turn.Body.String())
	}
	agentResponse := performJSON(
		t, handler, http.MethodPost, "/v1/agent/run",
		`{"profile_id":"api","input":"finish"}`,
	)
	if agentResponse.Code != http.StatusOK {
		t.Fatalf(
			"agent status=%d body=%s",
			agentResponse.Code, agentResponse.Body.String(),
		)
	}
	var record runtime.Record
	if err := json.Unmarshal(agentResponse.Body.Bytes(), &record); err != nil {
		t.Fatal(err)
	}
	if record.State != runtime.StateCompleted || record.SettledSequence == 0 {
		t.Fatalf("record=%#v", record)
	}
	streamRequest := httptest.NewRequest(
		http.MethodGet, "/v1/runs/"+record.ID+"/events", nil,
	)
	streamRequest.Header.Set("Accept", "text/event-stream")
	streamRequest.Header.Set("Last-Event-ID", "1")
	stream := httptest.NewRecorder()
	handler.ServeHTTP(stream, streamRequest)
	if stream.Code != http.StatusOK ||
		!strings.Contains(stream.Body.String(), "event: run.settled") {
		t.Fatalf("stream status=%d body=%s", stream.Code, stream.Body.String())
	}
	values, err := services.List(context.Background(), runtime.ListFilter{})
	if err != nil || len(values) != 1 {
		t.Fatalf("runs=%#v error=%v", values, err)
	}
}

func TestRuntimeHandlerRejectsCommandProfileForAgentBeforeRunCreation(t *testing.T) {
	handler, services := newRuntimeHandlerTest(t)
	response := performJSON(
		t, handler, http.MethodPost, "/v1/agent/run",
		`{"profile_id":"cli","input":"must fail"}`,
	)
	if response.Code != http.StatusBadRequest ||
		!strings.Contains(response.Body.String(), "requires an API model profile") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	values, err := services.List(context.Background(), runtime.ListFilter{})
	if err != nil || len(values) != 0 {
		t.Fatalf("invalid Agent mutated Run Store: runs=%#v error=%v", values, err)
	}
}

func TestRuntimeHandlerRejectsUnknownRunFields(t *testing.T) {
	handler, _ := newRuntimeHandlerTest(t)
	response := performJSON(
		t, handler, http.MethodPost, "/v1/runs",
		`{"kind":"agent","profile_id":"api","input":"x","command":"/bin/sh"}`,
	)
	if response.Code != http.StatusBadRequest {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func newRuntimeHandlerTest(
	t *testing.T,
) (*RuntimeHandler, *runtime.Service) {
	t.Helper()
	commandCatalog, err := runtimecommand.NewCatalog(
		map[string]runtimecommand.Profile{
			"cli": {
				Binary: "/bin/true", Transport: runtimecommand.TransportTTY,
				PromptDelivery: runtimecommand.PromptManual,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	modelCatalog, err := model.NewCatalog(map[string]model.Profile{
		"api": {
			Driver:   model.DriverOpenAICompatible,
			Endpoint: "https://example.invalid/v1/chat/completions",
			Model:    "fixture",
			Auth: model.Auth{
				Header: "Authorization", Scheme: "Bearer", FromEnv: "KEY",
			},
			Timeout: "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewCatalog(commandCatalog, modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	generator := stubGenerator{result: contract.ModelResult{
		Message: contract.Message{
			Role: contract.RoleAssistant, Content: "done",
		},
		FinishReason: contract.FinishStop,
	}}
	root := t.TempDir()
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
		Commands: runtimecommand.NewRunner(),
	})
	if err != nil {
		t.Fatal(err)
	}
	runStore, err := sqlitestore.Open(
		filepath.Join(root, "state", "runtime.db"), sqlitestore.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := agent.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: runStore,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Profiles: profiles, Model: generator, Tools: tools,
				Store: runStore, Sessions: sessions,
			},
			runtime.KindSession: &runtime.SessionExecutor{
				Profiles: profiles, Sessions: sessions,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runs.Close() })
	handler, err := NewRuntimeHandler(RuntimeServices{
		Model: NewHandler(generator), Sessions: sessions, Runs: runs,
		AgentBudget: agent.DefaultBudget(), SettledRetention: 7 * 24 * time.Hour,
	})
	if err != nil {
		t.Fatal(err)
	}
	return handler, runs
}

func performJSON(
	t *testing.T,
	handler http.Handler,
	method, path, body string,
) *httptest.ResponseRecorder {
	t.Helper()
	request := httptest.NewRequest(method, path, bytes.NewBufferString(body))
	request.Header.Set("Content-Type", "application/json")
	recorder := httptest.NewRecorder()
	handler.ServeHTTP(recorder, request)
	return recorder
}
