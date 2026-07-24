package llmruntime

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/yy003x/runtime/runtimeapi"
)

func TestGenerateSchemaOnlyLoadsAssetsAndReturnsToolCall(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		writeProviderJSON(t, writer, map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{
					"tool_calls": []any{map[string]any{
						"id": "call-1", "type": "function",
						"function": map[string]any{"name": "lookup", "arguments": `{"id":"42"}`},
					}},
				},
				"finish_reason": "tool_calls",
			}},
			"usage": map[string]any{"prompt_tokens": 10, "completion_tokens": 3},
		})
	}))
	defer server.Close()

	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "prompt.md"), []byte("project policy"), 0o644); err != nil {
		t.Fatal(err)
	}
	skill := "name: review\nentry: instruction.md\n"
	if err := os.WriteFile(filepath.Join(root, "skill.yaml"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "instruction.md"), []byte("handle {{stage}} for {{input}}"), 0o644); err != nil {
		t.Fatal(err)
	}
	runtime := testRuntime(t, server.URL, map[string]string{"project": root})
	response, err := runtime.Generate(context.Background(), runtimeapi.Request{
		Profile: "test",
		Prompt:  "request one",
		Context: runtimeapi.ContextAssets{
			Prompts: []runtimeapi.AssetRef{{URI: "asset://project/prompt.md"}},
			Skills: []runtimeapi.SkillRef{{
				AssetRef: runtimeapi.AssetRef{URI: "asset://project/skill.yaml"},
				Name:     "review", Variables: map[string]any{"stage": "strict"},
			}},
			Memory: []runtimeapi.AssetRef{{Inline: `{"preference":"concise"}`}},
		},
		Tools: runtimeapi.ToolSelection{Inline: []runtimeapi.Tool{{
			Name: "lookup", Parameters: map[string]any{"type": "object"},
		}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Done || len(response.ToolCalls) != 1 || response.ToolCalls[0].Arguments["id"] != "42" {
		t.Fatalf("unexpected response: %#v", response)
	}
	messages := received["messages"].([]any)
	system := messages[0].(map[string]any)["content"].(string)
	for _, expected := range []string{"project policy", "handle strict for request one", `"preference":"concise"`} {
		if !strings.Contains(system, expected) {
			t.Fatalf("system context missing %q: %s", expected, system)
		}
	}
}

func TestGenerateRuntimeExecuteContinuesAfterToolResult(t *testing.T) {
	var calls atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if calls.Add(1) == 1 {
			writeProviderJSON(t, writer, map[string]any{
				"choices": []any{map[string]any{
					"message": map[string]any{"tool_calls": []any{map[string]any{
						"id": "call-1", "type": "function",
						"function": map[string]any{"name": "double", "arguments": `{"value":21}`},
					}}},
					"finish_reason": "tool_calls",
				}},
			})
			return
		}
		raw, _ := json.Marshal(body["messages"])
		if !strings.Contains(string(raw), `"42"`) {
			t.Fatalf("second request missing tool result: %s", raw)
		}
		writeProviderJSON(t, writer, map[string]any{
			"choices": []any{map[string]any{
				"message":       map[string]any{"content": "done"},
				"finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	runtime := testRuntime(t, server.URL, nil)
	if err := runtime.RegisterTool(runtimeapi.Tool{
		Name: "double", Parameters: map[string]any{"type": "object"},
	}, func(_ context.Context, arguments map[string]any) (any, error) {
		return int(arguments["value"].(float64)) * 2, nil
	}); err != nil {
		t.Fatal(err)
	}
	response, err := runtime.Generate(context.Background(), runtimeapi.Request{
		Profile: "test", Prompt: "calculate", ToolMode: runtimeapi.ToolModeRuntimeExecute,
		Tools: runtimeapi.ToolSelection{Registered: []string{"double"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !response.Done || response.Rounds != 2 || response.Message.Content != "done" {
		t.Fatalf("unexpected response: %#v", response)
	}
}

func TestGenerateUsesProfileMaxTokensAndRequestOverride(t *testing.T) {
	received := make(chan float64, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		var body map[string]any
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Errorf("decode request: %v", err)
		}
		value, _ := body["max_tokens"].(float64)
		received <- value
		writeProviderJSON(t, writer, map[string]any{
			"choices": []any{map[string]any{
				"message": map[string]any{"content": "done"}, "finish_reason": "stop",
			}},
		})
	}))
	defer server.Close()

	runtime := testRuntimeWithProfile(t, server.URL, nil, map[string]any{"max_tokens": 16384})
	if _, err := runtime.Generate(context.Background(), runtimeapi.Request{
		Profile: "test", Prompt: "profile default",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := runtime.Generate(context.Background(), runtimeapi.Request{
		Profile: "test", Prompt: "request override", MaxTokens: 2048,
	}); err != nil {
		t.Fatal(err)
	}
	if profileDefault, requestOverride := <-received, <-received; profileDefault != 16384 || requestOverride != 2048 {
		t.Fatalf("max_tokens profile=%v override=%v", profileDefault, requestOverride)
	}
}

func TestGenerateStreamEmitsOrderedEventsAndRecallsMemory(t *testing.T) {
	var received map[string]any
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		defer request.Body.Close()
		if err := json.NewDecoder(request.Body).Decode(&received); err != nil {
			t.Fatalf("decode request: %v", err)
		}
		if received["stream"] != true || request.Header.Get("Accept") != "text/event-stream" {
			t.Fatalf("stream request=%#v headers=%v", received, request.Header)
		}
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"hello \"}}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[{\"delta\":{\"content\":\"world\"},\"finish_reason\":\"stop\"}]}\n\n"))
		_, _ = writer.Write([]byte("data: {\"choices\":[],\"usage\":{\"prompt_tokens\":7,\"completion_tokens\":2}}\n\n"))
		_, _ = writer.Write([]byte("data: [DONE]\n\n"))
	}))
	defer server.Close()

	runtime := testRuntime(t, server.URL, nil)
	var recalledQuery runtimeapi.MemoryQuery
	if err := runtime.RegisterMemoryProvider("local", MemoryProviderFunc(func(
		_ context.Context,
		query runtimeapi.MemoryQuery,
	) ([]runtimeapi.MemoryItem, error) {
		recalledQuery = query
		return []runtimeapi.MemoryItem{
			{Content: "remember this", Source: `notes & "facts"`},
			{Content: "must be truncated"},
		}, nil
	})); err != nil {
		t.Fatal(err)
	}

	var events []runtimeapi.Event
	response, err := runtime.GenerateStream(context.Background(), runtimeapi.Request{
		Profile: "test",
		Prompt:  "current question",
		Context: runtimeapi.ContextAssets{Recall: []runtimeapi.MemoryQuery{{
			Provider: "local", TopK: 1,
		}}},
	}, func(event runtimeapi.Event) error {
		events = append(events, event)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	if recalledQuery.Query != "current question" || recalledQuery.TopK != 1 {
		t.Fatalf("recall query=%#v", recalledQuery)
	}
	if response.Message.Content != "hello world" || !response.Done ||
		response.Usage.InputTokens != 7 || response.Usage.OutputTokens != 2 {
		t.Fatalf("response=%#v", response)
	}
	messages := received["messages"].([]any)
	system := messages[0].(map[string]any)["content"].(string)
	if !strings.Contains(system, `<memory source="notes &amp; &quot;facts&quot;">`) ||
		!strings.Contains(system, "remember this") || strings.Contains(system, "must be truncated") {
		t.Fatalf("system memory=%q", system)
	}
	expectedTypes := []string{
		runtimeapi.EventRequestStarted,
		runtimeapi.EventContextCompiled,
		runtimeapi.EventProviderStarted,
		runtimeapi.EventOutputDelta,
		runtimeapi.EventOutputDelta,
		runtimeapi.EventResponseCompleted,
	}
	if len(events) != len(expectedTypes) {
		t.Fatalf("events=%#v", events)
	}
	for index, expected := range expectedTypes {
		if events[index].Sequence != int64(index+1) || events[index].Type != expected || events[index].Time.IsZero() {
			t.Fatalf("event[%d]=%#v", index, events[index])
		}
	}
	if events[3].Delta+events[4].Delta != "hello world" ||
		events[len(events)-1].Response == nil ||
		events[len(events)-1].Response.Message.Content != "hello world" {
		t.Fatalf("events=%#v", events)
	}
}

func TestAssetResolverRejectsSymlinkEscapeAndChecksDigest(t *testing.T) {
	root := t.TempDir()
	outside := filepath.Join(t.TempDir(), "secret.md")
	if err := os.WriteFile(outside, []byte("secret"), 0o644); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, filepath.Join(root, "escape.md")); err != nil {
		t.Fatal(err)
	}
	resolver, err := newAssetResolver(map[string]string{"root": root}, 0)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := resolver.read(runtimeapi.AssetRef{URI: "asset://root/escape.md"}); err == nil {
		t.Fatal("expected symlink escape rejection")
	}
	sum := sha256.Sum256([]byte("value"))
	if _, err := resolver.read(runtimeapi.AssetRef{
		Inline: "value", SHA256: hex.EncodeToString(sum[:]),
	}); err != nil {
		t.Fatalf("valid digest rejected: %v", err)
	}
	if _, err := resolver.read(runtimeapi.AssetRef{Inline: "value", SHA256: strings.Repeat("0", 64)}); err == nil {
		t.Fatal("expected digest mismatch")
	}
}

func TestAssetResolverCacheIsBoundedAndInvalidatesChangedFiles(t *testing.T) {
	root := t.TempDir()
	for name, content := range map[string]string{"a.md": "a", "b.md": "b", "c.md": "c"} {
		if err := os.WriteFile(filepath.Join(root, name), []byte(content), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	resolver, err := newAssetResolverWithCache(map[string]string{"root": root}, 0, 2)
	if err != nil {
		t.Fatal(err)
	}
	read := func(name string) string {
		t.Helper()
		value, err := resolver.read(runtimeapi.AssetRef{URI: "asset://root/" + name})
		if err != nil {
			t.Fatal(err)
		}
		return value.content
	}
	read("a.md")
	read("b.md")
	read("a.md")
	read("c.md")
	if len(resolver.cache) != 2 {
		t.Fatalf("cache entries=%d", len(resolver.cache))
	}
	resolvedRoot := resolver.roots["root"]
	if _, ok := resolver.cache[filepath.Join(resolvedRoot, "a.md")]; !ok {
		t.Fatalf("recent asset was evicted: %#v", resolver.cache)
	}
	if _, ok := resolver.cache[filepath.Join(resolvedRoot, "b.md")]; ok {
		t.Fatalf("least recently used asset remains cached: %#v", resolver.cache)
	}
	if err := os.WriteFile(filepath.Join(root, "a.md"), []byte("updated"), 0o644); err != nil {
		t.Fatal(err)
	}
	if value := read("a.md"); value != "updated" {
		t.Fatalf("stale cached value=%q", value)
	}
}

func testRuntime(t *testing.T, baseURL string, roots map[string]string) *Runtime {
	t.Helper()
	return testRuntimeWithProfile(t, baseURL, roots, nil)
}

func testRuntimeWithProfile(t *testing.T, baseURL string, roots map[string]string, fields map[string]any) *Runtime {
	t.Helper()
	profiles := t.TempDir()
	profile := map[string]any{
		"protocol": "openai", "base_url": baseURL,
		"model": "test-model", "api_key": "${TEST_RUNTIME_API_KEY}",
	}
	for name, value := range fields {
		profile[name] = value
	}
	data, _ := json.Marshal(profile)
	if err := os.WriteFile(filepath.Join(profiles, "test.json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	t.Setenv("TEST_RUNTIME_API_KEY", "secret")
	runtime, err := New(Options{ProfileDir: profiles, AssetRoots: roots})
	if err != nil {
		t.Fatal(err)
	}
	return runtime
}

func writeProviderJSON(t *testing.T, writer http.ResponseWriter, value any) {
	t.Helper()
	writer.Header().Set("Content-Type", "application/json")
	if err := json.NewEncoder(writer).Encode(value); err != nil {
		t.Fatalf("encode response: %v", err)
	}
}
