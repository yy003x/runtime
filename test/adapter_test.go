package test

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"testing"

	"agent-arch/internal/agent"
	"agent-arch/internal/config"
	"agent-arch/internal/llm"
	"agent-arch/internal/llm/anthropic"
	"agent-arch/internal/memory"
	"agent-arch/internal/persona"
	"agent-arch/internal/token"
)

type stubFactory struct {
	providers []string
}

func (s *stubFactory) New(provider string, cfg config.ProviderConfig) (llm.Client, error) {
	s.providers = append(s.providers, provider)
	return stubClient{}, nil
}

type stubClient struct{}

func (stubClient) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	return llm.Response{OutputText: "ok"}, nil
}

func TestProviderSelectionFromPersona(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "writer.yaml"), []byte(`
id: "writer"
system_prompt: "system"
model_policy:
  provider: "anthropic"
  model: "claude-test"
  max_output_tokens: 256
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
			DefaultPersona:  "writer",
			DefaultProvider: "openai",
			DefaultModel:    "fallback-model",
		},
		Providers: map[string]config.ProviderConfig{
			"openai":    {Enabled: true, TimeoutSeconds: 1},
			"anthropic": {Enabled: true, TimeoutSeconds: 1},
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
	factory := &stubFactory{}
	service := agent.NewServiceWithDeps(cfg, persona.NewLoader(dir), factory, manager)

	resp, err := service.CreateAgent(context.Background(), agent.CreateAgentRequest{PersonaID: "writer"})
	if err != nil {
		t.Fatalf("create agent: %v", err)
	}

	if resp.Provider != "anthropic" {
		t.Fatalf("expected anthropic provider, got %s", resp.Provider)
	}
	if resp.Model != "claude-test" {
		t.Fatalf("expected persona model, got %s", resp.Model)
	}
}

func TestAnthropicClientRetriesOn529(t *testing.T) {
	t.Parallel()

	attempts := 0
	client := anthropic.NewClient("https://anthropic.test", "token-123", &http.Client{
		Transport: roundTripperFunc(func(req *http.Request) (*http.Response, error) {
			attempts++

			status := http.StatusOK
			body := map[string]any{
				"content": []map[string]any{
					{"type": "text", "text": "ok after retry"},
				},
				"usage": map[string]any{
					"input_tokens":  10,
					"output_tokens": 10,
				},
			}

			if attempts < 5 {
				status = 529
				body = map[string]any{
					"type": "error",
					"error": map[string]any{
						"type":    "overloaded_error",
						"message": "overloaded_error (529)",
					},
				}
			}

			payload, err := json.Marshal(body)
			if err != nil {
				return nil, err
			}

			return &http.Response{
				StatusCode: status,
				Header:     make(http.Header),
				Body:       io.NopCloser(bytes.NewReader(payload)),
				Request:    req,
			}, nil
		}),
	})

	resp, err := client.Generate(context.Background(), llm.Request{
		Model:           "MiniMax-M2.7",
		System:          "system",
		Messages:        []llm.Message{{Role: "user", Content: "hello"}},
		MaxOutputTokens: 128,
	})
	if err != nil {
		t.Fatalf("generate: %v", err)
	}
	if resp.OutputText != "ok after retry" {
		t.Fatalf("unexpected output: %q", resp.OutputText)
	}
	if attempts != 5 {
		t.Fatalf("expected 5 attempts, got %d", attempts)
	}
}

type roundTripperFunc func(req *http.Request) (*http.Response, error)

func (f roundTripperFunc) RoundTrip(req *http.Request) (*http.Response, error) {
	return f(req)
}
