package toolmcp

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/internal/toolconfig"
)

func TestMCPExecutionNegotiatesSessionAndCallsRemoteTool(t *testing.T) {
	const secret = "secret-value-never-log"
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		step := calls.Add(1)
		if request.Header.Get("Authorization") != "Bearer "+secret {
			t.Errorf("missing resolved authorization")
		}
		if request.Method != http.MethodPost {
			t.Errorf("method = %s", request.Method)
		}
		body, _ := io.ReadAll(request.Body)
		switch step {
		case 1:
			if request.Header.Get("Mcp-Session-Id") != "" ||
				request.Header.Get("MCP-Protocol-Version") != "" {
				t.Errorf("initialize sent session headers")
			}
			assertRequestMethod(t, body, "initialize")
			if !strings.Contains(string(body),
				`"protocolVersion":"2025-06-18"`) {
				t.Errorf("initialize body = %s", body)
			}
			writer.Header().Set("Content-Type", "text/event-stream")
			writer.Header().Set("Mcp-Session-Id", "session-1")
			fmt.Fprint(writer, "event: message\n")
			fmt.Fprint(writer, `data: {"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{"name":"test","version":"1"}}}`+"\n\n")
		case 2:
			assertSessionHeaders(t, request)
			assertRequestMethod(t, body, "notifications/initialized")
			writer.WriteHeader(http.StatusOK)
		case 3:
			assertSessionHeaders(t, request)
			assertRequestMethod(t, body, "tools/call")
			if !strings.Contains(string(body), `"name":"remoteSearch"`) ||
				!strings.Contains(string(body), `"query":"Codex"`) {
				t.Errorf("call body = %s", body)
			}
			writer.Header().Set("Content-Type", "application/json")
			fmt.Fprint(writer, `{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"answer `+secret+`"}],"isError":false}}`)
		default:
			t.Errorf("unexpected request %d", step)
		}
	}))
	defer server.Close()

	bundle, err := Build([]toolconfig.Manifest{
		testManifest(server.URL),
	}, Options{LookupEnv: func(name string) (string, bool) {
		if name != "TOKEN" {
			t.Fatalf("lookup name = %s", name)
		}
		return secret, true
	}})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(bundle.Configuration), secret) ||
		!strings.Contains(string(bundle.Configuration), "${TOKEN}") {
		t.Fatalf("unsafe configuration: %s", bundle.Configuration)
	}
	result, err := bundle.Tools[0].Handler(context.Background(), agent.ToolRequest{
		Name: "web_search", Arguments: json.RawMessage(`{"query":"Codex"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	if result.IsError || result.Content != "answer [REDACTED]" {
		t.Fatalf("result = %#v", result)
	}
	if calls.Load() != 3 {
		t.Fatalf("calls = %d", calls.Load())
	}
}

func TestMCPRemoteAndTransportFailuresAreReadOnlyResults(t *testing.T) {
	const secret = "reflected-secret"
	tests := []struct {
		name      string
		handler   http.HandlerFunc
		wantCode  string
		wantCalls int32
		maxBytes  int64
	}{
		{
			name: "http status no retry",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				writer.WriteHeader(http.StatusServiceUnavailable)
			},
			wantCode: "http_error", wantCalls: 1,
		},
		{
			name: "oversized response",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				fmt.Fprint(writer, strings.Repeat("x", 2048))
			},
			wantCode: "response_too_large", wantCalls: 1, maxBytes: 1024,
		},
		{
			name: "json rpc error redacted",
			handler: func(writer http.ResponseWriter, _ *http.Request) {
				writer.Header().Set("Content-Type", "application/json")
				fmt.Fprint(writer, `{"jsonrpc":"2.0","id":1,"error":{"code":-32000,"message":"failed `+secret+`"}}`)
			},
			wantCode: "remote_error", wantCalls: 1,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			var calls atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(
				writer http.ResponseWriter,
				request *http.Request,
			) {
				calls.Add(1)
				test.handler(writer, request)
			}))
			defer server.Close()
			manifest := testManifest(server.URL)
			if test.maxBytes != 0 {
				manifest.Executor.MaxResponseBytes = test.maxBytes
			}
			bundle, err := Build([]toolconfig.Manifest{manifest}, Options{
				LookupEnv: func(string) (string, bool) { return secret, true },
			})
			if err != nil {
				t.Fatal(err)
			}
			result, err := bundle.Tools[0].Handler(
				context.Background(),
				agent.ToolRequest{Arguments: json.RawMessage(`{"query":"x"}`)},
			)
			if err != nil || !result.IsError ||
				!strings.Contains(result.Content, `"code":"`+test.wantCode+`"`) {
				t.Fatalf("result=%#v err=%v", result, err)
			}
			if strings.Contains(result.Content, secret) {
				t.Fatalf("secret leaked: %s", result.Content)
			}
			if calls.Load() != test.wantCalls {
				t.Fatalf("calls = %d", calls.Load())
			}
		})
	}
}

func TestMCPDoesNotFollowRedirects(t *testing.T) {
	var targetCalls atomic.Int32
	target := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		targetCalls.Add(1)
		writer.WriteHeader(http.StatusOK)
	}))
	defer target.Close()
	redirect := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		http.Redirect(writer, request, target.URL, http.StatusTemporaryRedirect)
	}))
	defer redirect.Close()
	bundle, err := Build([]toolconfig.Manifest{testManifest(redirect.URL)}, Options{
		LookupEnv: func(string) (string, bool) { return "secret", true },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := bundle.Tools[0].Handler(
		context.Background(),
		agent.ToolRequest{Arguments: json.RawMessage(`{"query":"x"}`)},
	)
	if err != nil || !result.IsError ||
		!strings.Contains(result.Content, `"code":"http_error"`) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if targetCalls.Load() != 0 {
		t.Fatalf("redirect target calls = %d", targetCalls.Load())
	}
}

func TestMCPTimeoutIsReadOnlyResult(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		time.Sleep(1500 * time.Millisecond)
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, `{}`)
	}))
	defer server.Close()
	manifest := testManifest(server.URL)
	manifest.Executor.Timeout = "1s"
	bundle, err := Build([]toolconfig.Manifest{manifest}, Options{
		LookupEnv: func(string) (string, bool) { return "secret", true },
	})
	if err != nil {
		t.Fatal(err)
	}
	started := time.Now()
	result, err := bundle.Tools[0].Handler(
		context.Background(),
		agent.ToolRequest{Arguments: json.RawMessage(`{"query":"x"}`)},
	)
	if err != nil || !result.IsError ||
		!strings.Contains(result.Content, `"code":"timeout"`) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if elapsed := time.Since(started); elapsed < 900*time.Millisecond ||
		elapsed > 3*time.Second {
		t.Fatalf("unexpected timeout duration: %s", elapsed)
	}
}

func TestMCPMissingEnvironmentIsToolErrorAndBuildDoesNotResolve(t *testing.T) {
	lookups := 0
	bundle, err := Build([]toolconfig.Manifest{testManifest("https://example.com/mcp")}, Options{
		LookupEnv: func(string) (string, bool) {
			lookups++
			return "", false
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if lookups != 0 {
		t.Fatal("Build resolved a secret")
	}
	result, err := bundle.Tools[0].Handler(
		context.Background(),
		agent.ToolRequest{Arguments: json.RawMessage(`{"query":"x"}`)},
	)
	if err != nil || !result.IsError ||
		!strings.Contains(result.Content, `"code":"configuration_error"`) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestMCPPreservesRemoteIsError(t *testing.T) {
	server := sequentialServer(t,
		`{"jsonrpc":"2.0","id":1,"result":{"protocolVersion":"2024-11-05","capabilities":{},"serverInfo":{}}}`,
		``,
		`{"jsonrpc":"2.0","id":2,"result":{"content":[{"type":"text","text":"not found"}],"isError":true}}`,
	)
	defer server.Close()
	bundle, err := Build([]toolconfig.Manifest{testManifest(server.URL)}, Options{
		LookupEnv: func(string) (string, bool) { return "secret", true },
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := bundle.Tools[0].Handler(
		context.Background(),
		agent.ToolRequest{Arguments: json.RawMessage(`{"query":"x"}`)},
	)
	if err != nil || !result.IsError || result.Content != "not found" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func sequentialServer(t *testing.T, responses ...string) *httptest.Server {
	t.Helper()
	var calls atomic.Int32
	return httptest.NewServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		index := int(calls.Add(1)) - 1
		if index >= len(responses) {
			t.Errorf("unexpected call %d", index+1)
			return
		}
		if responses[index] == "" {
			writer.WriteHeader(http.StatusOK)
			return
		}
		writer.Header().Set("Content-Type", "application/json")
		fmt.Fprint(writer, responses[index])
	}))
}

func testManifest(endpoint string) toolconfig.Manifest {
	return toolconfig.Manifest{
		SchemaVersion: toolconfig.SchemaVersion,
		Name:          "web_search",
		Effect:        toolconfig.EffectReadOnly,
		Description:   "search",
		InputSchema: json.RawMessage(
			`{"type":"object","properties":{"query":{"type":"string"}},"required":["query"],"additionalProperties":false}`,
		),
		Executor: toolconfig.Executor{
			Type: toolconfig.ExecutorMCP, Endpoint: endpoint,
			RemoteTool: "remoteSearch",
			Headers: map[string]string{
				"Authorization": "Bearer ${TOKEN}",
			},
			Timeout: "5s", MaxResponseBytes: 1 << 20,
		},
	}
}

func assertRequestMethod(t *testing.T, data []byte, expected string) {
	t.Helper()
	var request struct {
		Method string `json:"method"`
	}
	if err := json.Unmarshal(data, &request); err != nil {
		t.Fatal(err)
	}
	if request.Method != expected {
		t.Errorf("method = %q, want %q", request.Method, expected)
	}
}

func assertSessionHeaders(t *testing.T, request *http.Request) {
	t.Helper()
	if request.Header.Get("Mcp-Session-Id") != "session-1" ||
		request.Header.Get("MCP-Protocol-Version") != "2024-11-05" {
		t.Errorf("unexpected MCP headers: %v", request.Header)
	}
}
