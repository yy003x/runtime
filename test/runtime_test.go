package test

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-arch/internal/agent"
	"agent-arch/internal/config"
	"agent-arch/internal/llm"
	"agent-arch/internal/llm/anthropic"
	"agent-arch/internal/memory"
	"agent-arch/internal/persona"
	"agent-arch/internal/token"
)

type mockFactory struct {
	client llm.Client
}

func (m mockFactory) New(provider string, cfg config.ProviderConfig) (llm.Client, error) {
	return m.client, nil
}

type mockClient struct{}

func (mockClient) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	return llm.Response{OutputText: "assistant reply"}, nil
}

func TestRuntimeChatWithMockedClient(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	traceDir := filepath.Join(dir, "logs")
	if err := os.WriteFile(filepath.Join(dir, "default.yaml"), []byte(`
id: "default"
system_prompt: "You are helpful."
model_policy:
  provider: "openai"
  model: "gpt-test"
  temperature: 0.1
  max_output_tokens: 128
memory_policy:
  summary_enabled: true
  retrieval_enabled: true
response_policy:
  format: "text"
  verbosity: "medium"
`), 0o644); err != nil {
		t.Fatalf("write persona: %v", err)
	}

	cfg := config.Config{
		Agent: config.AgentConfig{
			DefaultPersona:  "default",
			DefaultProvider: "openai",
			DefaultModel:    "gpt-test",
		},
		Providers: map[string]config.ProviderConfig{
			"openai": {Enabled: true, TimeoutSeconds: 1},
		},
		Debug: config.DebugConfig{
			LLMTraceDir: traceDir,
		},
		Memory: config.MemoryConfig{
			MaxContextTokens:       1024,
			ResponseReservedTokens: 128,
			SafetyBufferTokens:     64,
			RecentTurnsReserved:    256,
			CompressionThreshold:   0.8,
			KeepRecentTurns:        4,
		},
	}

	manager := memory.NewManager(memory.NewInMemoryStore(), memory.StubRetriever{}, cfg.Memory, token.NewApproxCounter())
	service := agent.NewServiceWithDeps(cfg, persona.NewLoader(dir), mockFactory{client: mockClient{}}, manager)

	created, err := service.CreateAgent(context.Background(), agent.CreateAgentRequest{})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	resp, err := service.Chat(context.Background(), agent.ChatRequest{
		SessionID: created.SessionID,
		Message:   "hello",
	})
	if err != nil {
		t.Fatalf("chat: %v", err)
	}

	if resp.Message != "assistant reply" {
		t.Fatalf("unexpected response: %s", resp.Message)
	}

	mem, err := service.GetMemory(context.Background(), created.SessionID)
	if err != nil {
		t.Fatalf("get memory: %v", err)
	}

	if len(mem.Turns) != 2 {
		t.Fatalf("expected two stored turns, got %d", len(mem.Turns))
	}

	tracePath := filepath.Join(traceDir, created.SessionID, "turn_0001.json")
	raw, err := os.ReadFile(tracePath)
	if err != nil {
		t.Fatalf("read trace log: %v", err)
	}
	if !strings.Contains(string(raw), "\"request\":") || !strings.Contains(string(raw), "\"response\":") || !strings.Contains(string(raw), "\"trace_id\":") {
		t.Fatalf("unexpected trace log contents: %s", string(raw))
	}
}

func TestAnthropicFiveTurnConversationKeepsFirstTurnContext(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "default.yaml"), []byte(`
id: "default"
system_prompt: "You are helpful."
model_policy:
  provider: "anthropic"
  model: "claude-test"
  temperature: 0.1
  max_output_tokens: 128
memory_policy:
  summary_enabled: true
  retrieval_enabled: true
response_policy:
  format: "text"
  verbosity: "medium"
`), 0o644); err != nil {
		t.Fatalf("write persona: %v", err)
	}

	round := 0
	factory := anthropicFactory{
		t: &roundTripperState{
			t:     t,
			round: &round,
		},
	}

	cfg := config.Config{
		Agent: config.AgentConfig{
			DefaultPersona:  "default",
			DefaultProvider: "anthropic",
			DefaultModel:    "claude-test",
		},
		Providers: map[string]config.ProviderConfig{
			"anthropic": {
				Enabled:        true,
				BaseURL:        "https://anthropic.test",
				APIKey:         "token-123",
				Model:          "claude-test",
				TimeoutSeconds: 1,
			},
		},
		Memory: config.MemoryConfig{
			MaxContextTokens:       131072,
			ResponseReservedTokens: 256,
			SafetyBufferTokens:     128,
			RecentTurnsReserved:    64,
			CompressionThreshold:   0.2,
			KeepRecentTurns:        2,
		},
	}

	manager := memory.NewManager(memory.NewInMemoryStore(), memory.StubRetriever{}, cfg.Memory, token.NewApproxCounter())
	service := agent.NewServiceWithDeps(cfg, persona.NewLoader(dir), factory, manager)

	created, err := service.CreateAgent(context.Background(), agent.CreateAgentRequest{
		PersonaID: "default",
		Provider:  "anthropic",
	})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	messages := []string{
		"第1轮：请记住，我最喜欢的编程语言是 Go。",
		"第2轮：我们聊聊 HTTP API 设计。",
		"第3轮：再聊聊上下文裁剪策略。",
		"第4轮：总结一下前面的实现约束。",
		"第5轮：回答第1轮的问题，我最喜欢的编程语言是什么？",
	}

	var last agent.ChatResponse
	for _, message := range messages {
		last, err = service.Chat(context.Background(), agent.ChatRequest{
			SessionID: created.SessionID,
			Message:   message,
		})
		if err != nil {
			t.Fatalf("chat: %v", err)
		}
	}

	if !strings.Contains(last.Message, "Go") {
		t.Fatalf("expected fifth round answer to reference first turn, got %q", last.Message)
	}
}

type anthropicFactory struct {
	t *roundTripperState
}

func (f anthropicFactory) New(provider string, cfg config.ProviderConfig) (llm.Client, error) {
	client := &http.Client{
		Transport: f.t,
	}
	return anthropic.NewClient("https://anthropic.test", cfg.APIKey, client), nil
}

type roundTripperState struct {
	t     *testing.T
	round *int
}

func (r *roundTripperState) RoundTrip(req *http.Request) (*http.Response, error) {
	r.t.Helper()

	if req.URL.Path != "/v1/messages" {
		r.t.Fatalf("unexpected path: %s", req.URL.Path)
	}
	if got := req.Header.Get("x-api-key"); got != "token-123" {
		r.t.Fatalf("unexpected x-api-key header: %q", got)
	}
	if got := req.Header.Get("Authorization"); got != "Bearer token-123" {
		r.t.Fatalf("unexpected authorization header: %q", got)
	}

	var body struct {
		System   string `json:"system"`
		Messages []struct {
			Role    string `json:"role"`
			Content []struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"content"`
		} `json:"messages"`
	}
	if err := json.NewDecoder(req.Body).Decode(&body); err != nil {
		r.t.Fatalf("decode request: %v", err)
	}

	*r.round = *r.round + 1
	reply := "round reply"
	if *r.round == 5 {
		if !strings.Contains(body.System, "Go") && !hasMessageText(body.Messages, "Go") {
			r.t.Fatalf("fifth round lost first-turn context; system=%q", body.System)
		}
		reply = "第1轮提到你最喜欢的编程语言是 Go。"
	}

	payload, err := json.Marshal(map[string]any{
		"content": []map[string]any{
			{"type": "text", "text": reply},
		},
		"usage": map[string]any{
			"input_tokens":  10,
			"output_tokens": 10,
		},
	})
	if err != nil {
		r.t.Fatalf("marshal response: %v", err)
	}

	return &http.Response{
		StatusCode: http.StatusOK,
		Header:     make(http.Header),
		Body:       io.NopCloser(strings.NewReader(string(payload))),
		Request:    req,
	}, nil
}

func hasMessageText(messages []struct {
	Role    string `json:"role"`
	Content []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	} `json:"content"`
}, needle string) bool {
	for _, message := range messages {
		for _, part := range message.Content {
			if strings.Contains(part.Text, needle) {
				return true
			}
		}
	}
	return false
}
