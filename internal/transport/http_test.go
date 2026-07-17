package transport

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-runtime/internal/agentrun"
)

func TestHTTPRunUsesAgentRunArtifacts(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := `{"type":"native","native":{"system_prompt":"test","max_rounds":1,"mock":{"responses":["http native ok"],"done_after":1}}}`
	if err := os.WriteFile(filepath.Join(root, "configs", "native-test.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(agentrun.New(root))
	body, _ := json.Marshal(RunRequest{
		RunID: "task-20260714-000000-httpnative", RunType: agentrun.RunTask, Profile: "native-test", Prompt: "hello",
		AllowedActions: []string{"echo"}, ForbiddenActions: []string{"shell"},
	})
	response := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(body))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(response, request)
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	requestData, err := os.ReadFile(filepath.Join(root, "runs", "task", "2026-07-14", "task-20260714-000000-httpnative", "request.json"))
	if err != nil {
		t.Fatal(err)
	}
	var persisted agentrun.Request
	if err := json.Unmarshal(requestData, &persisted); err != nil {
		t.Fatal(err)
	}
	if len(persisted.AllowedActions) != 1 || persisted.AllowedActions[0] != "echo" || len(persisted.ForbiddenActions) != 1 {
		t.Fatalf("persisted request=%#v", persisted)
	}

	resultResponse := httptest.NewRecorder()
	handler.ServeHTTP(resultResponse, httptest.NewRequest(http.MethodGet, "/v1/runs/task/task-20260714-000000-httpnative/result", nil))
	if resultResponse.Code != http.StatusOK || !bytes.Contains(resultResponse.Body.Bytes(), []byte("http native ok")) {
		t.Fatalf("status=%d body=%s", resultResponse.Code, resultResponse.Body.String())
	}
	for _, resource := range []string{"status", "logs"} {
		readResponse := httptest.NewRecorder()
		handler.ServeHTTP(readResponse, httptest.NewRequest(http.MethodGet, "/v1/runs/task/task-20260714-000000-httpnative/"+resource, nil))
		if readResponse.Code != http.StatusOK {
			t.Fatalf("resource=%s status=%d body=%s", resource, readResponse.Code, readResponse.Body.String())
		}
	}
	cancelResponse := httptest.NewRecorder()
	handler.ServeHTTP(cancelResponse, httptest.NewRequest(http.MethodPost, "/v1/runs/task/task-20260714-000000-httpnative/cancel", nil))
	if cancelResponse.Code != http.StatusOK {
		t.Fatalf("cancel status=%d body=%s", cancelResponse.Code, cancelResponse.Body.String())
	}
	for _, request := range []*http.Request{
		httptest.NewRequest(http.MethodGet, "/v1/runs/", nil),
		httptest.NewRequest(http.MethodGet, "/v1/runs/task/task-20260714-000000-httpnative/unknown", nil),
		httptest.NewRequest(http.MethodPut, "/v1/runs/task/task-20260714-000000-httpnative/status", nil),
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusNotFound && response.Code != http.StatusMethodNotAllowed {
			t.Fatalf("path=%s status=%d", request.URL.Path, response.Code)
		}
	}
}

func TestHTTPSessionHistoryAPIUsesRuntimeStore(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := `{"type":"native","native":{"system_prompt":"test","max_rounds":1,"mock":{"responses":["session turn ok"],"done_after":1}}}`
	if err := os.WriteFile(filepath.Join(root, "configs", "native-test.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	handler := NewHTTPHandler(agentrun.New(root))
	createBody := bytes.NewBufferString(`{"session_id":"session-20260717-130000-http","project_id":"project","title":"HTTP session","runtime":"api","profile":"native-test","tags":["workbench"]}`)
	create := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/sessions", createBody)
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(create, request)
	if create.Code != http.StatusCreated {
		t.Fatalf("create status=%d body=%s", create.Code, create.Body.String())
	}
	if !strings.Contains(create.Body.String(), `"runtime":"api"`) || !strings.Contains(create.Body.String(), `"profile":"native-test"`) || !strings.Contains(create.Body.String(), `"workbench"`) {
		t.Fatalf("create body=%s", create.Body.String())
	}
	turnBody := bytes.NewBufferString(`{"run_id":"turn-20260717-130001-http","profile":"native-test","prompt":"hello"}`)
	turn := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/sessions/session-20260717-130000-http/turns", turnBody)
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(turn, request)
	if turn.Code != http.StatusCreated {
		t.Fatalf("turn status=%d body=%s", turn.Code, turn.Body.String())
	}
	for _, path := range []string{
		"/v1/sessions?tag=workbench", "/v1/sessions/session-20260717-130000-http",
		"/v1/sessions/session-20260717-130000-http/messages?after_seq=0",
		"/v1/sessions/session-20260717-130000-http/events?after_seq=0",
	} {
		response := httptest.NewRecorder()
		handler.ServeHTTP(response, httptest.NewRequest(http.MethodGet, path, nil))
		if response.Code != http.StatusOK {
			t.Fatalf("path=%s status=%d body=%s", path, response.Code, response.Body.String())
		}
	}
	messages := httptest.NewRecorder()
	handler.ServeHTTP(messages, httptest.NewRequest(http.MethodGet, "/v1/sessions/session-20260717-130000-http/messages", nil))
	if !strings.Contains(messages.Body.String(), "hello") || !strings.Contains(messages.Body.String(), "session turn ok") {
		t.Fatalf("messages=%s", messages.Body.String())
	}
}

func TestHTTPHandlerRequiresConfiguredBearerToken(t *testing.T) {
	handler := NewHTTPHandlerWithOptions(agentrun.New(t.TempDir()), HTTPOptions{BearerToken: "test-token"})

	unauthorized := httptest.NewRecorder()
	handler.ServeHTTP(unauthorized, httptest.NewRequest(http.MethodGet, "/v1/runs/task/example/status", nil))
	if unauthorized.Code != http.StatusUnauthorized {
		t.Fatalf("status=%d body=%s", unauthorized.Code, unauthorized.Body.String())
	}

	authorizedRequest := httptest.NewRequest(http.MethodGet, "/v1/runs/task/example/status", nil)
	authorizedRequest.Header.Set("Authorization", "Bearer test-token")
	authorized := httptest.NewRecorder()
	handler.ServeHTTP(authorized, authorizedRequest)
	if authorized.Code == http.StatusUnauthorized {
		t.Fatalf("configured token was rejected: %s", authorized.Body.String())
	}

	health := httptest.NewRecorder()
	handler.ServeHTTP(health, httptest.NewRequest(http.MethodGet, "/healthz", nil))
	if health.Code != http.StatusOK {
		t.Fatalf("health status=%d body=%s", health.Code, health.Body.String())
	}
}

func TestHTTPHandlerRejectsOversizedAndInvalidJSONRequests(t *testing.T) {
	handler := NewHTTPHandlerWithOptions(agentrun.New(t.TempDir()), HTTPOptions{MaxBodyBytes: 64})

	oversized := httptest.NewRecorder()
	request := httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"profile":"x","prompt":"`+strings.Repeat("a", 128)+`"}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(oversized, request)
	if oversized.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("oversized status=%d body=%s", oversized.Code, oversized.Body.String())
	}

	unknown := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"profile":"x","prompt":"p","unexpected":true}`))
	request.Header.Set("Content-Type", "application/json")
	handler.ServeHTTP(unknown, request)
	if unknown.Code != http.StatusBadRequest {
		t.Fatalf("unknown status=%d body=%s", unknown.Code, unknown.Body.String())
	}

	wrongType := httptest.NewRecorder()
	request = httptest.NewRequest(http.MethodPost, "/v1/runs", strings.NewReader(`{"profile":"x","prompt":"p"}`))
	request.Header.Set("Content-Type", "text/plain")
	handler.ServeHTTP(wrongType, request)
	if wrongType.Code != http.StatusUnsupportedMediaType {
		t.Fatalf("content-type status=%d body=%s", wrongType.Code, wrongType.Body.String())
	}
}

func TestHTTPHandlerRejectsUnsafeFileInputs(t *testing.T) {
	handler := NewHTTPHandler(agentrun.New(t.TempDir()))
	tests := []RunRequest{
		{Profile: "x", Prompt: "p", CWD: "relative/path"},
		{Profile: "x", PromptFile: "/etc/passwd", CWD: t.TempDir()},
		{Profile: "x", PromptFile: "../secret", CWD: t.TempDir()},
		{Profile: "x", Prompt: "p", DeadlineSeconds: -1},
		{Profile: "x", Prompt: "p", AllowedActions: []string{"echo", "echo"}},
		{Profile: "x", Prompt: "p", ForbiddenActions: []string{" bad"}},
	}
	for _, input := range tests {
		body, _ := json.Marshal(input)
		response := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(body))
		request.Header.Set("Content-Type", "application/json")
		handler.ServeHTTP(response, request)
		if response.Code != http.StatusBadRequest {
			t.Fatalf("input=%#v status=%d body=%s", input, response.Code, response.Body.String())
		}
	}
}

func TestValidateRunRequestRejectsPromptFileSymlinkEscape(t *testing.T) {
	cwd := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(cwd, "prompt.md")); err != nil {
		t.Skipf("symlink unavailable: %v", err)
	}
	if err := validateRunRequest(RunRequest{CWD: cwd, PromptFile: "prompt.md"}); err == nil {
		t.Fatal("prompt_file symlink outside cwd was accepted")
	}
}
