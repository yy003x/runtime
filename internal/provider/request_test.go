package provider

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"reflect"
	"strings"
	"testing"
)

func TestPrepareCodexTypedOverridesReplaceDefaults(t *testing.T) {
	cfg := Config{ID: "cx", Type: TypeCLI, CLI: &CLIConfig{
		Driver: "codex", Executor: ExecutorCommand,
		Command: CommandConfig{Binary: "codex", Args: []string{"-c", "sandbox_mode=read-only", "-c", "model_reasoning_effort=low", "exec"}, Model: "base"},
		Runtime: CLIRuntime{PromptDelivery: "stdin", ResultContract: "required", OverridePolicy: OverridePolicy{Allow: []string{"model", "sandbox_mode", "reasoning_effort", "images"}}},
	}}
	prepared, err := Prepare(cfg, "hello", map[string]any{
		"model": "next", "sandbox_mode": "danger-full-access", "reasoning_effort": "high", "images": []string{"a.png"},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := []string{"codex", "-c", "sandbox_mode=danger-full-access", "-c", "model_reasoning_effort=high", "exec", "--image", "a.png", "--model", "next"}
	if !reflect.DeepEqual(prepared.CLI.Argv, want) {
		t.Fatalf("argv=%#v, want %#v", prepared.CLI.Argv, want)
	}
	if prepared.CLI.Stdin != "hello" {
		t.Fatalf("stdin=%q", prepared.CLI.Stdin)
	}
}

func TestPrepareClaudeTypedOverrides(t *testing.T) {
	cfg := Config{ID: "cc", Type: TypeCLI, CLI: &CLIConfig{
		Driver: "claude", Executor: ExecutorCommand,
		Command: CommandConfig{Binary: "claude", Args: []string{"-p", "--permission-mode", "default"}, Model: "opus"},
		Runtime: CLIRuntime{PromptDelivery: "arg", PromptArgs: []string{"--prompt", "{prompt}"}, ResultContract: "required"},
	}}
	prepared, err := Prepare(cfg, "hello", map[string]any{"effort": "high", "permission_mode": "bypassPermissions", "disallowed_tools": []string{"Bash(sudo:*)"}})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	joined := strings.Join(prepared.CLI.Argv, " ")
	for _, want := range []string{"--effort high", "--permission-mode bypassPermissions", "--disallowed-tools Bash(sudo:*)", "--model opus", "--prompt hello"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv=%q missing %q", joined, want)
		}
	}
}

func TestPrepareRejectsLockedOverride(t *testing.T) {
	cfg := Config{ID: "cx", Type: TypeCLI, CLI: &CLIConfig{
		Driver: "codex", Executor: ExecutorCommand,
		Command: CommandConfig{Binary: "codex", Model: "base"},
		Runtime: CLIRuntime{PromptDelivery: "stdin", OverridePolicy: OverridePolicy{Allow: []string{"model"}, Locked: []string{"model"}}},
	}}
	if _, err := Prepare(cfg, "hello", map[string]any{"model": "next"}); err == nil {
		t.Fatal("Prepare returned nil error")
	}
}

func TestExecuteOpenAIAndAnthropicAPI(t *testing.T) {
	for _, protocol := range []string{"openai", "anthropic"} {
		t.Run(protocol, func(t *testing.T) {
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Fatalf("method=%s", r.Method)
				}
				body, _ := io.ReadAll(r.Body)
				var payload map[string]any
				if err := json.Unmarshal(body, &payload); err != nil {
					t.Fatal(err)
				}
				if payload["model"] != "test-model" {
					t.Fatalf("payload=%v", payload)
				}
				w.Header().Set("Content-Type", "application/json")
				if protocol == "anthropic" {
					if r.Header.Get("x-api-key") != "secret" {
						t.Fatalf("x-api-key=%q", r.Header.Get("x-api-key"))
					}
					_, _ = io.WriteString(w, `{"content":[{"type":"text","text":"anthropic ok"}]}`)
				} else {
					if r.Header.Get("Authorization") != "Bearer secret" {
						t.Fatalf("authorization=%q", r.Header.Get("Authorization"))
					}
					_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"openai ok"}}]}`)
				}
			}))
			defer server.Close()
			t.Setenv("TEST_PROVIDER_KEY", "secret")
			cfg := Config{ID: protocol, Type: TypeAPI, API: &APIConfig{Protocol: protocol, BaseURL: server.URL + "/v1", Model: "test-model", APIKeyEnv: "TEST_PROVIDER_KEY"}}
			prepared, err := Prepare(cfg, "hello", nil)
			if err != nil {
				t.Fatalf("Prepare: %v", err)
			}
			result, err := ExecuteAPI(context.Background(), server.Client(), cfg, *prepared.API)
			if err != nil {
				t.Fatalf("ExecuteAPI: %v", err)
			}
			if result.FinalText != protocol+" ok" {
				t.Fatalf("final_text=%q", result.FinalText)
			}
		})
	}
}

func TestExecuteCLIForwardsEnvAndStdin(t *testing.T) {
	t.Setenv("PARENT_VALUE", "parent")
	cfg := Config{ID: "shell", Type: TypeCLI, CLI: &CLIConfig{Command: CommandConfig{
		Binary: "/bin/sh", Args: []string{"-c", "printf '%s:' \"$STATIC_VALUE\"; cat"}, Model: "", Env: map[string]string{"STATIC_VALUE": "${PARENT_VALUE}"},
	}}}
	request := CLIRequest{ProfileID: "shell", Argv: append([]string{cfg.CLI.Command.Binary}, cfg.CLI.Command.Args...), Stdin: "input"}
	result, err := ExecuteCLI(context.Background(), cfg, request, "", nil)
	if err != nil {
		t.Fatalf("ExecuteCLI: %v", err)
	}
	if result.FinalText != "parent:input" {
		t.Fatalf("final_text=%q", result.FinalText)
	}
}
