package runtimebootstrap

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yy003x/runtime/internal/agentrun"
	"github.com/yy003x/runtime/runtimeapi"
)

func TestMCPEnvironmentUsesAllowlistPassthroughAndExplicitValues(t *testing.T) {
	t.Setenv("TEST_MCP_PASSTHROUGH", "passthrough-value")
	t.Setenv("TEST_MCP_REFERENCE", "resolved-value")
	t.Setenv("TEST_MCP_NOT_ALLOWED", "must-not-leak")
	environment, err := mcpEnvironment(agentrun.MCPServerSettings{
		EnvPassthrough: []string{"TEST_MCP_PASSTHROUGH"},
		Env: map[string]string{
			"EXPLICIT": "${TEST_MCP_REFERENCE}",
			"LITERAL":  "literal-value",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(environment, "\n")
	for _, expected := range []string{
		"TEST_MCP_PASSTHROUGH=passthrough-value",
		"EXPLICIT=resolved-value",
		"LITERAL=literal-value",
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("environment missing %q:\n%s", expected, joined)
		}
	}
	if strings.Contains(joined, "TEST_MCP_NOT_ALLOWED=") {
		t.Fatalf("non-allowlisted environment leaked:\n%s", joined)
	}
}

func TestMCPEnvironmentRejectsMissingReference(t *testing.T) {
	const name = "TEST_MCP_MISSING_REFERENCE"
	t.Setenv(name, "")
	if err := os.Unsetenv(name); err != nil {
		t.Fatal(err)
	}
	if _, err := mcpEnvironment(agentrun.MCPServerSettings{
		Env: map[string]string{"TOKEN": "${" + name + "}"},
	}); err == nil {
		t.Fatal("missing environment reference was accepted")
	}
}

func TestNewRegistersConfiguredMCPServer(t *testing.T) {
	var providerCalls atomic.Int32
	providerServer := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatal(err)
		}
		if providerCalls.Add(1) == 1 {
			rawTools, _ := json.Marshal(body["tools"])
			if !strings.Contains(string(rawTools), `"echo"`) {
				t.Fatalf("configured MCP tool missing: %s", rawTools)
			}
			_ = json.NewEncoder(writer).Encode(map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{"tool_calls": []any{map[string]any{
						"id": "call-1", "type": "function",
						"function": map[string]any{"name": "echo", "arguments": `{"text":"hello"}`},
					}}},
					"finish_reason": "tool_calls",
				}},
			})
			return
		}
		rawMessages, _ := json.Marshal(body["messages"])
		if !strings.Contains(string(rawMessages), "hello") {
			t.Fatalf("MCP result missing from follow-up: %s", rawMessages)
		}
		_ = json.NewEncoder(writer).Encode(map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": "done"}, "finish_reason": "stop",
			}},
		})
	}))
	defer providerServer.Close()

	home := t.TempDir()
	configDir := filepath.Join(home, "configs")
	if err := os.MkdirAll(configDir, 0o755); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_BOOTSTRAP_API_KEY", "secret")
	profile, _ := json.Marshal(map[string]any{
		"protocol": "openai", "base_url": providerServer.URL, "model": "test",
		"api_key": "${TEST_BOOTSTRAP_API_KEY}",
	})
	if err := os.WriteFile(filepath.Join(configDir, "test.json"), profile, 0o600); err != nil {
		t.Fatal(err)
	}
	settings := fmt.Sprintf(`llm:
  mcp_servers:
    - name: fixture
      command: %q
      args: ["-test.run=TestRuntimeBootstrapMCPHelperProcess"]
      env:
        GO_WANT_RUNTIME_MCP_HELPER: "1"
      timeout_seconds: 2
`, os.Args[0])
	if err := os.WriteFile(filepath.Join(configDir, "runtime.yaml"), []byte(settings), 0o600); err != nil {
		t.Fatal(err)
	}
	runtime, err := New(agentrun.New(home))
	if err != nil {
		t.Fatal(err)
	}
	response, err := runtime.Generate(context.Background(), runtimeapi.Request{
		Profile:  "test",
		Prompt:   "use echo",
		ToolMode: runtimeapi.ToolModeRuntimeExecute,
		Tools:    runtimeapi.ToolSelection{MCP: []string{"fixture"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Done || response.Rounds != 2 || response.Message.Content != "done" || providerCalls.Load() != 2 {
		t.Fatalf("response=%#v provider calls=%d", response, providerCalls.Load())
	}
}

func TestRuntimeBootstrapMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_RUNTIME_MCP_HELPER") != "1" {
		return
	}
	os.Exit(runRuntimeBootstrapMCPHelper())
}

func runRuntimeBootstrapMCPHelper() int {
	scanner := bufio.NewScanner(os.Stdin)
	encoder := json.NewEncoder(os.Stdout)
	for scanner.Scan() {
		var request struct {
			ID     json.RawMessage `json:"id"`
			Method string          `json:"method"`
			Params map[string]any  `json:"params"`
		}
		if json.Unmarshal(scanner.Bytes(), &request) != nil {
			return 2
		}
		if len(request.ID) == 0 {
			continue
		}
		response := map[string]any{"jsonrpc": "2.0", "id": request.ID}
		switch request.Method {
		case "initialize":
			response["result"] = map[string]any{
				"protocolVersion": "2025-11-25",
				"capabilities":    map[string]any{"tools": map[string]any{}},
				"serverInfo":      map[string]any{"name": "fixture", "version": "1"},
			}
		case "tools/list":
			response["result"] = map[string]any{"tools": []any{map[string]any{
				"name": "echo", "description": "echo",
				"inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			arguments, _ := request.Params["arguments"].(map[string]any)
			response["result"] = map[string]any{
				"content": []any{map[string]any{
					"type": "text", "text": fmt.Sprint(arguments["text"]),
				}},
				"isError": false,
			}
		default:
			response["error"] = map[string]any{"code": -32601, "message": "method not found"}
		}
		if encoder.Encode(response) != nil {
			return 3
		}
	}
	return 0
}
