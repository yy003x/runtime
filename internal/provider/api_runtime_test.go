package provider

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
	"sync"
	"testing"

	"agent-runtime/internal/capability"
	"agent-runtime/internal/mcp"
	nativeengine "agent-runtime/internal/provider/native"
)

func TestOpenAIAPIRuntimeExecutesToolAndLoadsSkillMemoryContext(t *testing.T) {
	root := t.TempDir()
	skillDir := filepath.Join(root, "skills", "review")
	if err := os.MkdirAll(skillDir, 0o755); err != nil {
		t.Fatal(err)
	}
	skill := "name: review\ndescription: review code\nkeywords: [review]\nprompt_template: 'SKILL_REVIEW {{input}}'\n"
	if err := os.WriteFile(filepath.Join(skillDir, "skill.yaml"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	memoryFile := filepath.Join(root, "memory.json")
	memory, err := capability.OpenMemory(memoryFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Write([]capability.MemoryItem{{ID: "local", Type: "fact", Content: "MEMORY_LOCAL_CONTEXT", Source: "test"}}); err != nil {
		t.Fatal(err)
	}

	var mu sync.Mutex
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		mu.Lock()
		defer mu.Unlock()
		calls++
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		messages, _ := body["messages"].([]any)
		if calls == 1 {
			encoded, _ := json.Marshal(messages)
			if !strings.Contains(string(encoded), "SKILL_REVIEW") || !strings.Contains(string(encoded), "MEMORY_LOCAL_CONTEXT") {
				t.Errorf("initial context=%s", encoded)
			}
			writeFixtureJSON(writer, map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{"content": "", "tool_calls": []any{map[string]any{
						"id": "call-1", "type": "function", "function": map[string]any{"name": "echo", "arguments": `{"value":"ok"}`},
					}}},
					"finish_reason": "tool_calls",
				}},
				"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 2},
			})
			return
		}
		encoded, _ := json.Marshal(messages)
		if !strings.Contains(string(encoded), `"role":"tool"`) || !strings.Contains(string(encoded), `\"value\":\"ok\"`) {
			t.Errorf("tool result was not returned: %s", encoded)
		}
		writeFixtureJSON(writer, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "openai done"}, "finish_reason": "stop"}},
			"usage":   map[string]any{"prompt_tokens": 12, "completion_tokens": 3},
		})
	}))
	defer server.Close()
	t.Setenv("PROVIDER_API_RUNTIME_KEY", "fixture-key")
	cfg := Config{ID: "openai-agent", Type: TypeAPI, API: &APIConfig{
		Protocol: "openai", BaseURL: server.URL, Model: "fixture", APIKeyEnv: "PROVIDER_API_RUNTIME_KEY",
		Runtime: &APIRuntimeConfig{
			Enabled: true, MaxRounds: 3, AutoRouteSkills: true, Skills: []string{"*"},
			Memory: &APIMemoryConfig{Enabled: true, TopK: 3},
		},
	}}
	snapshotFile := filepath.Join(root, "context-snapshot.json")
	prepared, err := (apiProvider{}).Prepare(context.Background(), cfg, Request{
		Prompt: "review this runtime", RunID: "run-openai", SnapshotFile: snapshotFile,
		SkillDir: filepath.Join(root, "skills"), MemoryFile: memoryFile, Allowed: []string{"echo"},
		HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (apiProvider{}).Execute(context.Background(), prepared, nopSink{})
	if err != nil || result.FinalText != "openai done" || result.State != "completed" || calls != 2 {
		t.Fatalf("result=%#v calls=%d err=%v", result, calls, err)
	}
	snapshot, err := os.ReadFile(snapshotFile)
	if err != nil || !strings.Contains(string(snapshot), "SKILL_REVIEW") || !strings.Contains(string(snapshot), "MEMORY_LOCAL_CONTEXT") {
		t.Fatalf("snapshot=%s err=%v", snapshot, err)
	}
}

func TestAPIRuntimeMemoryToolsRespectSeparatePermissions(t *testing.T) {
	root := t.TempDir()
	memoryFile := filepath.Join(root, "memory.json")
	candidateFile := filepath.Join(root, "candidates.json")
	memory, err := capability.OpenMemory(memoryFile)
	if err != nil {
		t.Fatal(err)
	}
	if err := memory.Write([]capability.MemoryItem{{ID: "old", Type: "fact", Content: "old fact", Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	runtime, err := buildAPIToolRuntime(context.Background(), Request{
		RunID: "memory-tools", MemoryFile: memoryFile, MemoryCandidateFile: candidateFile,
		SessionID: "session-memory-tools", TurnID: "turn-memory-tools",
		Allowed: []string{"memory.read", "memory.write", "memory.delete"},
	}, APIRuntimeConfig{Memory: &APIMemoryConfig{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer runtime.Close()
	if len(runtime.tools) != 3 || runtime.executor == nil {
		t.Fatalf("tools=%#v", runtime.tools)
	}
	if _, err := runtime.executor.Execute(context.Background(), nativeToolCall("memory_write", map[string]any{"id": "new", "content": "new fact"})); err != nil {
		t.Fatal(err)
	}
	if output, err := runtime.executor.Execute(context.Background(), nativeToolCall("memory_recall", map[string]any{"query": "new", "top_k": 2})); err != nil || len(output.([]capability.MemoryItem)) != 1 || output.([]capability.MemoryItem)[0].ID != "old" {
		t.Fatalf("durable recall unexpectedly included candidate: recall=%#v err=%v", output, err)
	}
	candidates, err := capability.OpenMemory(candidateFile)
	if err != nil || len(candidates.Recall("new", "", 5)) != 1 {
		t.Fatalf("candidate was not persisted: err=%v", err)
	}
	candidate := candidates.Recall("new", "", 5)[0]
	if candidate.SessionID != "session-memory-tools" || candidate.TurnID != "turn-memory-tools" || candidate.RunID != "memory-tools" {
		t.Fatalf("candidate provenance=%#v", candidate)
	}
	if _, err := runtime.executor.Execute(context.Background(), nativeToolCall("memory_forget", map[string]any{"ids": []any{"old"}})); err != nil {
		t.Fatal(err)
	}
	reopened, err := capability.OpenMemory(memoryFile)
	if err != nil || len(reopened.Recall("fact", "", 5)) != 0 {
		t.Fatalf("memory was not cleared: err=%v", err)
	}
	blocked, err := buildAPIToolRuntime(context.Background(), Request{
		MemoryFile: memoryFile, MemoryCandidateFile: candidateFile, Allowed: []string{"memory.write"}, Forbidden: []string{"memory_write"},
	}, APIRuntimeConfig{Memory: &APIMemoryConfig{Enabled: true}})
	if err != nil {
		t.Fatal(err)
	}
	defer blocked.Close()
	if len(blocked.tools) != 0 {
		t.Fatalf("forbidden tools=%#v", blocked.tools)
	}
}

func TestAgentRuntimeResultMapsControlStates(t *testing.T) {
	blocked := agentRuntimeResult(nativeSnapshot("blocked", "denied"), "/tmp/context.json", "context_file")
	if blocked.State != "blocked" || blocked.BlockedReason != "denied" {
		t.Fatalf("blocked=%#v", blocked)
	}
	cancelled := agentRuntimeResult(nativeSnapshot("cancelled", ""), "/tmp/context.json", "context_file")
	if cancelled.State != "cancelled" {
		t.Fatalf("cancelled=%#v", cancelled)
	}
	failed := agentRuntimeResult(nativeSnapshot("failed", "upstream"), "/tmp/context.json", "context_file")
	if failed.ExitCode != 1 {
		t.Fatalf("failed=%#v", failed)
	}
}

func TestAPIRuntimeAuthorizationAndNormalizationHelpers(t *testing.T) {
	if !agentActionAllowed("mcp__one__tool", "mcp.one", []string{"mcp"}, nil) {
		t.Fatal("global MCP permission was not accepted")
	}
	if agentActionAllowed("echo", "", []string{"*"}, []string{"*"}) {
		t.Fatal("forbidden wildcard did not win")
	}
	if !mcpServerPotentiallyAllowed("one", []string{"mcp__one__tool"}, nil) || mcpServerPotentiallyAllowed("one", []string{"mcp"}, []string{"mcp.one"}) {
		t.Fatal("MCP server authorization mismatch")
	}
	name := mcpToolName("one", strings.Repeat("bad/tool", 20))
	if len(name) != 64 || strings.Contains(name, "/") {
		t.Fatalf("normalized name=%q", name)
	}
	if values := anyStringSlice([]string{"a", "b"}); len(values) != 2 {
		t.Fatalf("values=%#v", values)
	}
	if values := anyStringSlice("invalid"); values != nil {
		t.Fatalf("values=%#v", values)
	}
	if truncateText("short", 10) != "short" || truncateText("abcdef", 3) != "abc…" {
		t.Fatal("text truncation mismatch")
	}
}

func TestAnthropicAPIRuntimeExecutesMemoryTool(t *testing.T) {
	root := t.TempDir()
	memoryFile := filepath.Join(root, "memory.json")
	candidateFile := filepath.Join(root, "candidates.json")
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		if request.URL.Path != "/v1/messages" {
			t.Errorf("path=%s", request.URL.Path)
		}
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if calls == 1 {
			writeFixtureJSON(writer, map[string]any{
				"content": []any{map[string]any{
					"type": "tool_use", "id": "tool-1", "name": "memory_write",
					"input": map[string]any{"id": "saved", "type": "fact", "content": "anthropic memory"},
				}},
				"stop_reason": "tool_use", "usage": map[string]any{"input_tokens": 4, "output_tokens": 2},
			})
			return
		}
		encoded, _ := json.Marshal(body["messages"])
		if !strings.Contains(string(encoded), "tool_result") || !strings.Contains(string(encoded), "candidate") {
			t.Errorf("tool result was not returned: %s", encoded)
		}
		writeFixtureJSON(writer, map[string]any{
			"content":     []any{map[string]any{"type": "text", "text": "anthropic done"}},
			"stop_reason": "end_turn", "usage": map[string]any{"input_tokens": 6, "output_tokens": 3},
		})
	}))
	defer server.Close()
	t.Setenv("PROVIDER_API_RUNTIME_KEY", "fixture-key")
	cfg := Config{ID: "anthropic-agent", Type: TypeAPI, API: &APIConfig{
		Protocol: "anthropic", BaseURL: server.URL, Model: "fixture", APIKeyEnv: "PROVIDER_API_RUNTIME_KEY",
		Runtime: &APIRuntimeConfig{Enabled: true, MaxRounds: 3, Memory: &APIMemoryConfig{Enabled: true}},
	}}
	prepared, err := (apiProvider{}).Prepare(context.Background(), cfg, Request{
		Prompt: "save memory", RunID: "run-anthropic", SnapshotFile: filepath.Join(root, "context-snapshot.json"),
		MemoryFile: memoryFile, MemoryCandidateFile: candidateFile, Allowed: []string{"memory.write"}, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (apiProvider{}).Execute(context.Background(), prepared, nopSink{})
	if err != nil || result.FinalText != "anthropic done" || calls != 2 {
		t.Fatalf("result=%#v calls=%d err=%v", result, calls, err)
	}
	memory, err := capability.OpenMemory(candidateFile)
	if err != nil || len(memory.Recall("anthropic", "fact", 5)) != 1 {
		t.Fatalf("memory candidate recall err=%v", err)
	}
}

func TestOpenAIAPIRuntimeCallsMCPTool(t *testing.T) {
	root := t.TempDir()
	calls := 0
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		calls++
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		if calls == 1 {
			encoded, _ := json.Marshal(body["tools"])
			if !strings.Contains(string(encoded), "mcp__fixture__echo") {
				t.Errorf("MCP tool missing: %s", encoded)
			}
			writeFixtureJSON(writer, map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{"content": "", "tool_calls": []any{map[string]any{
						"id": "mcp-1", "type": "function", "function": map[string]any{"name": "mcp__fixture__echo", "arguments": `{"text":"via mcp"}`},
					}}},
					"finish_reason": "tool_calls",
				}},
			})
			return
		}
		encoded, _ := json.Marshal(body["messages"])
		if !strings.Contains(string(encoded), "via mcp") {
			t.Errorf("MCP result missing: %s", encoded)
		}
		writeFixtureJSON(writer, map[string]any{
			"choices": []any{map[string]any{"message": map[string]any{"content": "mcp done"}, "finish_reason": "stop"}},
		})
	}))
	defer server.Close()
	t.Setenv("PROVIDER_API_RUNTIME_KEY", "fixture-key")
	cfg := Config{ID: "mcp-agent", Type: TypeAPI, API: &APIConfig{
		Protocol: "openai", BaseURL: server.URL, Model: "fixture", APIKeyEnv: "PROVIDER_API_RUNTIME_KEY",
		Runtime: &APIRuntimeConfig{Enabled: true, MaxRounds: 3, MCPServers: []MCPServerConfig{{
			Name: "fixture", Transport: "stdio", Command: os.Args[0], Args: []string{"-test.run=TestProviderMCPHelperProcess"},
			Env: map[string]string{"GO_WANT_PROVIDER_MCP_HELPER": "1"},
		}}},
	}}
	prepared, err := (apiProvider{}).Prepare(context.Background(), cfg, Request{
		Prompt: "use MCP", RunID: "run-mcp", SnapshotFile: filepath.Join(root, "context-snapshot.json"),
		CWD: root, Allowed: []string{"mcp.fixture"}, HTTPClient: server.Client(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := (apiProvider{}).Execute(context.Background(), prepared, nopSink{})
	if err != nil || result.FinalText != "mcp done" || calls != 2 {
		t.Fatalf("result=%#v calls=%d err=%v", result, calls, err)
	}
}

func TestProviderMCPHelperProcess(t *testing.T) {
	if os.Getenv("GO_WANT_PROVIDER_MCP_HELPER") != "1" {
		return
	}
	os.Exit(runProviderMCPHelper())
}

func runProviderMCPHelper() int {
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
				"protocolVersion": mcp.ProtocolVersion, "capabilities": map[string]any{"tools": map[string]any{}},
				"serverInfo": map[string]any{"name": "fixture", "version": "1"},
			}
		case "tools/list":
			response["result"] = map[string]any{"tools": []any{map[string]any{
				"name": "echo", "description": "MCP echo", "inputSchema": map[string]any{"type": "object"},
			}}}
		case "tools/call":
			arguments, _ := request.Params["arguments"].(map[string]any)
			response["result"] = map[string]any{
				"content": []any{map[string]any{"type": "text", "text": fmt.Sprint(arguments["text"])}}, "isError": false,
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

func writeFixtureJSON(writer http.ResponseWriter, value any) {
	writer.Header().Set("Content-Type", "application/json")
	_ = json.NewEncoder(writer).Encode(value)
}

func nativeToolCall(name string, arguments map[string]any) nativeengine.ToolCall {
	return nativeengine.ToolCall{ID: "test-call", Name: name, Arguments: arguments}
}

func nativeSnapshot(state, lastError string) nativeengine.Snapshot {
	return nativeengine.Snapshot{State: nativeengine.State(state), LastError: lastError}
}
