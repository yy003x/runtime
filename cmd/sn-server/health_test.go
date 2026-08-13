package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
)

func TestHealthProbesAreGenericWithBearer(t *testing.T) {
	readiness := &readinessState{}
	readiness.Set(true)
	handler := newServerHandler(
		readiness,
		"private-token",
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("health probe reached Runtime handler")
		}),
	)
	for _, test := range []struct {
		path   string
		status int
		body   string
	}{
		{path: "/healthz", status: http.StatusOK, body: `{"status":"ok"}`},
		{path: "/readyz", status: http.StatusOK, body: `{"status":"ready"}`},
	} {
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodGet, test.path, nil)
		request.Header.Set("Authorization", "Bearer private-token")
		handler.ServeHTTP(response, request)
		if response.Code != test.status ||
			strings.TrimSpace(response.Body.String()) != test.body {
			t.Fatalf(
				"path=%s status=%d body=%s",
				test.path, response.Code, response.Body.String(),
			)
		}
		if strings.Contains(response.Body.String(), "private-token") {
			t.Fatalf("probe leaked bearer token: %s", response.Body.String())
		}
	}
}

func TestConfiguredBearerProtectsHealthProbes(t *testing.T) {
	readiness := &readinessState{}
	readiness.Set(true)
	handler := newServerHandler(
		readiness, "secret", http.NotFoundHandler(),
	)
	for _, path := range []string{"/healthz", "/readyz"} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(
			response, httptest.NewRequest(http.MethodGet, path, nil),
		)
		if response.Code != http.StatusUnauthorized ||
			strings.Contains(response.Body.String(), "ready") {
			t.Fatalf(
				"path=%s status=%d body=%s",
				path, response.Code, response.Body.String(),
			)
		}
	}
}

func TestReadyProbeReflectsExecutionPlane(t *testing.T) {
	readiness := &readinessState{}
	handler := healthProbes(readiness, http.NotFoundHandler())
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response, httptest.NewRequest(http.MethodGet, "/readyz", nil),
	)
	if response.Code != http.StatusServiceUnavailable ||
		strings.TrimSpace(response.Body.String()) != `{"status":"not_ready"}` {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	readiness.Set(true)
	response = httptest.NewRecorder()
	handler.ServeHTTP(
		response, httptest.NewRequest(http.MethodGet, "/readyz", nil),
	)
	if response.Code != http.StatusOK ||
		strings.TrimSpace(response.Body.String()) != `{"status":"ready"}` {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}

func TestHealthProbesRejectMethods(t *testing.T) {
	response := httptest.NewRecorder()
	healthProbes(&readinessState{}, http.NotFoundHandler()).ServeHTTP(
		response, httptest.NewRequest(http.MethodPost, "/healthz", nil),
	)
	if response.Code != http.StatusMethodNotAllowed ||
		response.Header().Get("Allow") != http.MethodGet {
		t.Fatalf("status=%d headers=%v", response.Code, response.Header())
	}
}

func TestUnreadyServerRejectsOnlyNewRunSubmissions(t *testing.T) {
	readiness := &readinessState{}
	forwarded := 0
	handler := newServerHandler(
		readiness,
		"secret",
		http.HandlerFunc(func(writer http.ResponseWriter, _ *http.Request) {
			forwarded++
			writer.WriteHeader(http.StatusNoContent)
		}),
	)

	submit := httptest.NewRequest(http.MethodPost, "/v1/runs", nil)
	submit.Header.Set("Authorization", "Bearer secret")
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, submit)
	if response.Code != http.StatusServiceUnavailable || forwarded != 0 {
		t.Fatalf("status=%d forwarded=%d", response.Code, forwarded)
	}
	var payload struct {
		Error contract.RuntimeError `json:"error"`
	}
	if err := json.Unmarshal(response.Body.Bytes(), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Phase != contract.PhaseRun ||
		!payload.Error.Retryable ||
		payload.Error.HTTPStatus != http.StatusServiceUnavailable {
		t.Fatalf("error=%#v", payload.Error)
	}

	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/runs", nil),
		httptest.NewRequest(
			http.MethodPost,
			"/v1/runs/run_11111111111111111111111111111111:cancel",
			nil,
		),
	} {
		request.Header.Set("Authorization", "Bearer secret")
		response = httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNoContent {
			t.Fatalf("%s %s status=%d", request.Method, request.URL.Path, response.Code)
		}
	}
	if forwarded != 2 {
		t.Fatalf("forwarded=%d", forwarded)
	}

	readiness.Set(true)
	submit = httptest.NewRequest(http.MethodPost, "/v1/runs/", nil)
	submit.Header.Set("Authorization", "Bearer secret")
	response = httptest.NewRecorder()
	handler.ServeHTTP(response, submit)
	if response.Code != http.StatusNoContent || forwarded != 3 {
		t.Fatalf("status=%d forwarded=%d", response.Code, forwarded)
	}
}

func TestBearerAuthenticationPrecedesRunReadinessGate(t *testing.T) {
	handler := newServerHandler(
		&readinessState{},
		"secret",
		http.HandlerFunc(func(http.ResponseWriter, *http.Request) {
			t.Fatal("unauthenticated request reached Runtime handler")
		}),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodPost, "/v1/runs", nil),
	)
	if response.Code != http.StatusUnauthorized ||
		strings.Contains(response.Body.String(), "unavailable") {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
}
