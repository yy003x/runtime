package provider

import (
	"context"
	"testing"
)

func TestSelectReturnsProviderForEveryConfiguredTransport(t *testing.T) {
	tests := []struct {
		name string
		cfg  Config
		kind string
	}{
		{
			name: "cli",
			cfg: Config{ID: "cli", Type: TypeCLI, CLI: &CLIConfig{
				Executor: ExecutorCommand,
				Command:  CommandConfig{Binary: "printf"},
				Runtime:  CLIRuntime{PromptDelivery: "stdin"},
			}},
			kind: TypeCLI,
		},
		{
			name: "api",
			cfg:  Config{ID: "api", Type: TypeAPI, API: &APIConfig{Protocol: "openai", BaseURL: "https://example.test", Model: "test", APIKeyEnv: "TEST_KEY", Mock: true}},
			kind: TypeAPI,
		},
		{
			name: "tmux",
			cfg: Config{ID: "tmux", Type: TypeCLI, CLI: &CLIConfig{
				Executor: ExecutorTmux,
				Command:  CommandConfig{Binary: "sh"},
				Runtime:  CLIRuntime{PromptDelivery: "paste"},
				Tmux:     &TmuxConfig{SessionName: "test"},
			}},
			kind: ExecutorTmux,
		},
	}

	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			selected, err := Select(test.cfg)
			if err != nil {
				t.Fatalf("Select: %v", err)
			}
			if selected.Kind() != test.kind {
				t.Fatalf("kind=%q want=%q", selected.Kind(), test.kind)
			}
			if _, err := selected.Prepare(context.Background(), test.cfg, Request{Prompt: "hello"}); err != nil {
				t.Fatalf("Prepare: %v", err)
			}
		})
	}
}

func TestSelectRejectsUnsupportedProviderType(t *testing.T) {
	if _, err := Select(Config{ID: "invalid", Type: "invalid"}); err == nil {
		t.Fatal("Select returned nil error")
	}
}
