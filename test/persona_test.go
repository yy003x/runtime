package test

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"agent-arch/internal/persona"
)

func TestPersonaLoader(t *testing.T) {
	t.Parallel()

	dir := t.TempDir()
	if err := os.WriteFile(filepath.Join(dir, "assistant.yaml"), []byte(`
id: "assistant"
name: "Assistant"
system_prompt: |
  You are helpful.
style_rules:
  - "Be precise."
model_policy:
  provider: "openai"
  model: "gpt-test"
  temperature: 0.1
  max_output_tokens: 512
memory_policy:
  max_context_tokens: 1024
  keep_recent_turns: 4
  summary_enabled: true
  retrieval_enabled: false
response_policy:
  format: "text"
  verbosity: "low"
`), 0o644); err != nil {
		t.Fatalf("write persona: %v", err)
	}

	loaded, err := persona.NewLoader(dir).Load(context.Background(), "assistant")
	if err != nil {
		t.Fatalf("load persona: %v", err)
	}

	if loaded.ID != "assistant" {
		t.Fatalf("unexpected persona id: %s", loaded.ID)
	}
	if loaded.ModelPolicy.Provider != "openai" {
		t.Fatalf("unexpected provider: %s", loaded.ModelPolicy.Provider)
	}
	if persona.RenderSystem(loaded) == "" {
		t.Fatal("expected rendered system prompt")
	}
}
