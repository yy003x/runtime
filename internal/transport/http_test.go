package transport

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
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
	body, _ := json.Marshal(RunRequest{RunID: "task-20260714-000000-httpnative", RunType: agentrun.RunTask, Profile: "native-test", Prompt: "hello"})
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, httptest.NewRequest(http.MethodPost, "/v1/runs", bytes.NewReader(body)))
	if response.Code != http.StatusCreated {
		t.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}

	resultResponse := httptest.NewRecorder()
	handler.ServeHTTP(resultResponse, httptest.NewRequest(http.MethodGet, "/v1/runs/task/task-20260714-000000-httpnative/result", nil))
	if resultResponse.Code != http.StatusOK || !bytes.Contains(resultResponse.Body.Bytes(), []byte("http native ok")) {
		t.Fatalf("status=%d body=%s", resultResponse.Code, resultResponse.Body.String())
	}
}
