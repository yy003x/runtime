package main

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"
)

func TestAuditHTTPWritesRedactedNormalizedRecord(t *testing.T) {
	logsDir := t.TempDir()
	handler := auditHTTP(logsDir, http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		http.Error(writer, "response detail", http.StatusConflict)
	}))
	request := httptest.NewRequest(
		http.MethodPost,
		"/v1/runs/run_11111111111111111111111111111111:cancel?token=secret",
		strings.NewReader("private prompt"),
	)
	response := httptest.NewRecorder()
	handler.ServeHTTP(response, request)

	data, err := os.ReadFile(filepath.Join(
		logsDir, time.Now().Format("060102"), "audit.jsonl",
	))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "secret") ||
		strings.Contains(string(data), "private prompt") ||
		strings.Contains(string(data), "response detail") {
		t.Fatalf("audit retained request or response content: %s", data)
	}
	var record struct {
		Source     string            `json:"source"`
		Namespace  string            `json:"namespace"`
		Action     string            `json:"action"`
		Outcome    string            `json:"outcome"`
		Targets    map[string]string `json:"targets"`
		HTTPStatus int               `json:"http_status"`
	}
	if err := json.Unmarshal(data[:len(data)-1], &record); err != nil {
		t.Fatal(err)
	}
	if record.Source != "sn-server" || record.Namespace != "http" ||
		record.Action != "POST run.cancel" || record.Outcome != "failed" ||
		record.Targets["run_id"] != "run_11111111111111111111111111111111" ||
		record.HTTPStatus != http.StatusConflict {
		t.Fatalf("record=%#v", record)
	}
}

func TestAuditHTTPPreservesFlusher(t *testing.T) {
	logsDir := t.TempDir()
	handler := auditHTTP(logsDir, http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		flusher, ok := writer.(http.Flusher)
		if !ok {
			t.Fatal("audit middleware hid http.Flusher")
		}
		flusher.Flush()
	}))
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest(http.MethodGet, "/v1/runs", nil),
	)
	if response.Code != http.StatusOK || !response.Flushed {
		t.Fatalf("status=%d flushed=%t", response.Code, response.Flushed)
	}
}

func TestAuditHTTPNeverRetainsUnknownPathOrMethod(t *testing.T) {
	logsDir := t.TempDir()
	handler := auditHTTP(logsDir, http.NotFoundHandler())
	response := httptest.NewRecorder()
	handler.ServeHTTP(
		response,
		httptest.NewRequest("SECRET-METHOD", "/v1/private-secret", nil),
	)
	data, err := os.ReadFile(filepath.Join(
		logsDir, time.Now().Format("060102"), "audit.jsonl",
	))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "SECRET") ||
		strings.Contains(string(data), "private-secret") ||
		!strings.Contains(string(data), `"action":"OTHER unknown"`) {
		t.Fatalf("audit=%s", data)
	}
}

func TestAuditHTTPMarksPanicsFailedWithoutRecovering(t *testing.T) {
	logsDir := t.TempDir()
	handler := auditHTTP(logsDir, http.HandlerFunc(func(
		http.ResponseWriter,
		*http.Request,
	) {
		panic("private panic detail")
	}))
	func() {
		defer func() {
			if recover() == nil {
				t.Fatal("audit middleware swallowed panic")
			}
		}()
		handler.ServeHTTP(
			httptest.NewRecorder(),
			httptest.NewRequest(http.MethodGet, "/v1/runs", nil),
		)
	}()
	data, err := os.ReadFile(filepath.Join(
		logsDir, time.Now().Format("060102"), "audit.jsonl",
	))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "private panic detail") ||
		!strings.Contains(string(data), `"outcome":"failed"`) ||
		!strings.Contains(string(data), `"http_status":500`) {
		t.Fatalf("audit=%s", data)
	}
}
