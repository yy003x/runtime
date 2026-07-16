package provider

import (
	"context"
	"strings"
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

func TestCommandAndAPIProvidersExecuteThroughSink(t *testing.T) {
	sink := &recordingSink{}
	cliConfig := Config{ID: "shell", Type: TypeCLI, CLI: &CLIConfig{
		Executor: ExecutorCommand, Command: CommandConfig{Binary: "/bin/sh"}, Runtime: CLIRuntime{PromptDelivery: "none"},
	}}
	cliPrepared := PreparedRequest{
		Config: cliConfig, CLI: &CLIRequest{Argv: []string{"/bin/sh", "-c", "printf stdout; printf stderr >&2"}},
		Request: Request{CWD: t.TempDir(), Environment: map[string]string{"TEST_VALUE": "one"}},
	}
	result, err := (cliProvider{}).Execute(context.Background(), cliPrepared, sink)
	if err != nil || result.FinalText != "stdout" || !strings.Contains(sink.stdout, "stdout") || !strings.Contains(sink.stderr, "stderr") {
		t.Fatalf("result=%#v sink=%#v err=%v", result, sink, err)
	}
	if len(sink.statuses) == 0 || len(sink.events) == 0 {
		t.Fatalf("sink=%#v", sink)
	}

	apiConfig := Config{ID: "api", Type: TypeAPI, API: &APIConfig{
		Protocol: "openai", BaseURL: "https://example.test/v1", Model: "mock-model", APIKeyEnv: "UNUSED", Mock: true,
	}}
	prepared, err := (apiProvider{}).Prepare(context.Background(), apiConfig, Request{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	apiResult, err := (apiProvider{}).Execute(context.Background(), prepared, sink)
	if err != nil || apiResult.FinalText != "[mock openai:mock-model] 5 chars" {
		t.Fatalf("result=%#v err=%v", apiResult, err)
	}
}

type recordingSink struct {
	stdout   string
	stderr   string
	events   []Event
	statuses []StatusPatch
}

func (sink *recordingSink) Stdout(value []byte) error {
	sink.stdout += string(value)
	return nil
}

func (sink *recordingSink) Stderr(value []byte) error {
	sink.stderr += string(value)
	return nil
}

func (sink *recordingSink) Event(event Event) error {
	sink.events = append(sink.events, event)
	return nil
}

func (sink *recordingSink) StatusPatch(status StatusPatch) error {
	sink.statuses = append(sink.statuses, status)
	return nil
}
