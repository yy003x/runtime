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
		Command: CommandConfig{Binary: "codex", Args: []string{"-c", "sandbox_mode=read-only", "-c", "model_reasoning_effort=low"}, Model: "base"},
		Runtime: CLIRuntime{PromptDelivery: "stdin", OverridePolicy: OverridePolicy{Allow: []string{"model", "sandbox_mode", "reasoning_effort", "images"}}},
	}}
	prepared, err := Prepare(cfg, "hello", map[string]any{
		"model": "next", "sandbox_mode": "danger-full-access", "reasoning_effort": "high", "images": []string{"a.png"},
	})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	want := []string{"codex", "-c", "sandbox_mode=danger-full-access", "-c", "model_reasoning_effort=high", "--image", "a.png", "--model", "next", "exec"}
	if !reflect.DeepEqual(prepared.CLI.Argv, want) {
		t.Fatalf("argv=%#v, want %#v", prepared.CLI.Argv, want)
	}
	if prepared.CLI.Stdin != "hello" {
		t.Fatalf("stdin=%q", prepared.CLI.Stdin)
	}
}

func TestPrepareCLIPlacesRawArgsAfterDerivedManagedArgs(t *testing.T) {
	cfg := Config{ID: "cx", Type: TypeCLI, CLI: &CLIConfig{
		Driver: "codex", Executor: ExecutorCommand,
		Command: CommandConfig{Binary: "codex", Args: []string{"--search"}, Model: "configured"},
		Runtime: CLIRuntime{PromptDelivery: "stdin"},
	}}
	prepared, err := prepare(cfg, "hello", nil, []string{"--skip-git-repo-check"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "--search", "--model", "configured", "exec", "--skip-git-repo-check"}
	if !reflect.DeepEqual(prepared.CLI.Argv, want) {
		t.Fatalf("argv=%#v want=%#v", prepared.CLI.Argv, want)
	}
}

func TestPrepareCLIPlacesArgumentPromptAfterProviderArgs(t *testing.T) {
	cfg := Config{ID: "cc", Type: TypeCLI, CLI: &CLIConfig{
		Driver: "claude", Executor: ExecutorCommand,
		Command: CommandConfig{Binary: "claude"},
		Runtime: CLIRuntime{PromptDelivery: "arg"},
	}}
	prepared, err := prepare(cfg, "hello", nil, []string{"--permission-mode", "dontAsk"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"claude", "-p", "--permission-mode", "dontAsk", "hello"}
	if !reflect.DeepEqual(prepared.CLI.Argv, want) {
		t.Fatalf("argv=%#v want=%#v", prepared.CLI.Argv, want)
	}
}

func TestPrepareClaudeTypedOverrides(t *testing.T) {
	cfg := Config{ID: "cc", Type: TypeCLI, CLI: &CLIConfig{
		Driver: "claude", Executor: ExecutorCommand,
		Command: CommandConfig{Binary: "claude", Args: []string{"--permission-mode", "default"}, Model: "opus"},
		Runtime: CLIRuntime{PromptDelivery: "arg", PromptArgs: []string{"--prompt", "{prompt}"}},
	}}
	prepared, err := Prepare(cfg, "hello", map[string]any{"effort": "high", "permission_mode": "bypassPermissions", "disallowed_tools": []string{"Bash(sudo:*)"}})
	if err != nil {
		t.Fatalf("Prepare: %v", err)
	}
	joined := strings.Join(prepared.CLI.Argv, " ")
	for _, want := range []string{"--effort high", "--permission-mode bypassPermissions", "--disallowed-tools Bash(sudo:*)", "--model opus", "-p --prompt hello"} {
		if !strings.Contains(joined, want) {
			t.Fatalf("argv=%q missing %q", joined, want)
		}
	}
}

func TestPrepareInteractiveCLIExcludesDerivedManagedArgsAndKeepsRawArgs(t *testing.T) {
	t.Setenv("SN_TEST_HOME", "/tmp/sn-test-home")
	cfg := Config{ID: "cx", Type: TypeCLI, CLI: &CLIConfig{
		Driver: "codex", Executor: ExecutorCommand,
		Command: CommandConfig{Binary: "codex", Args: []string{"--search", "-c", "model_instructions_file=${SN_TEST_HOME}/AGENTS.md"}, Model: "configured"},
		Runtime: CLIRuntime{PromptDelivery: "stdin"},
	}}
	prepared, err := PrepareInteractiveCLI(cfg, []string{"--help", "${SN_TEST_HOME}/raw"})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex", "--search", "-c", "model_instructions_file=/tmp/sn-test-home/AGENTS.md", "--model", "configured", "--help", "${SN_TEST_HOME}/raw"}
	if !reflect.DeepEqual(prepared.Argv, want) {
		t.Fatalf("argv=%#v want=%#v", prepared.Argv, want)
	}
}

func TestPrepareCLIExpandsConfiguredArgs(t *testing.T) {
	t.Setenv("SN_TEST_HOME", "/tmp/sn-test-home")
	cfg := Config{ID: "cx", Type: TypeCLI, CLI: &CLIConfig{
		Driver: "codex", Executor: ExecutorCommand,
		Command: CommandConfig{Binary: "${SN_TEST_HOME}/codex", Args: []string{"-c", "model_instructions_file=${SN_TEST_HOME}/AGENTS.md"}, Model: ""},
		Runtime: CLIRuntime{PromptDelivery: "stdin"},
	}}
	prepared, err := Prepare(cfg, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"/tmp/sn-test-home/codex", "-c", "model_instructions_file=/tmp/sn-test-home/AGENTS.md", "exec"}
	if !reflect.DeepEqual(prepared.CLI.Argv, want) {
		t.Fatalf("argv=%#v want=%#v", prepared.CLI.Argv, want)
	}
}

func TestPrepareCLIDoesNotDuplicateConfiguredManagedMode(t *testing.T) {
	tests := []struct {
		name   string
		driver string
		binary string
		args   []string
		want   []string
	}{
		{name: "codex exec", driver: "codex", binary: "codex", args: []string{"exec"}, want: []string{"codex", "exec"}},
		{name: "claude short print", driver: "claude", binary: "claude", args: []string{"-p"}, want: []string{"claude", "-p"}},
		{name: "claude long print", driver: "claude", binary: "claude", args: []string{"--print"}, want: []string{"claude", "--print"}},
		{name: "generic", driver: "generic", binary: "custom-cli", args: []string{"run"}, want: []string{"custom-cli", "run"}},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			cfg := Config{ID: "test", Type: TypeCLI, CLI: &CLIConfig{
				Driver: test.driver, Executor: ExecutorCommand,
				Command: CommandConfig{Binary: test.binary, Args: test.args, Model: ""},
				Runtime: CLIRuntime{PromptDelivery: "stdin"},
			}}
			prepared, err := Prepare(cfg, "hello", nil)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(prepared.CLI.Argv, test.want) {
				t.Fatalf("argv=%#v want=%#v", prepared.CLI.Argv, test.want)
			}
		})
	}
}

func TestPrepareInteractiveCLIWithEmptyOptionsEqualsBinary(t *testing.T) {
	cfg := Config{ID: "cx", Type: TypeCLI, CLI: &CLIConfig{
		Driver: "codex", Executor: ExecutorCommand,
		Command: CommandConfig{Binary: "codex", Args: []string{}, Model: ""},
	}}
	prepared, err := PrepareInteractiveCLI(cfg, nil)
	if err != nil {
		t.Fatal(err)
	}
	want := []string{"codex"}
	if !reflect.DeepEqual(prepared.Argv, want) {
		t.Fatalf("argv=%#v want=%#v", prepared.Argv, want)
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
			basePath := "/compatible-mode"
			wantPath := "/compatible-mode/v1/chat/completions"
			if protocol == "anthropic" {
				basePath = "/apps/anthropic"
				wantPath = "/apps/anthropic/v1/messages"
			}
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if r.Method != http.MethodPost {
					t.Fatalf("method=%s", r.Method)
				}
				if r.URL.Path != wantPath {
					t.Fatalf("path=%q want=%q", r.URL.Path, wantPath)
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
					if r.Header.Get("anthropic-version") != "2023-06-01" {
						t.Fatalf("anthropic-version=%q", r.Header.Get("anthropic-version"))
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
			cfg := Config{ID: protocol, Type: TypeAPI, API: &APIConfig{Protocol: protocol, BaseURL: server.URL + basePath, Model: "test-model", APIKey: "${TEST_PROVIDER_KEY}"}}
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

func TestDefaultAPIAuthUsesBearerForOpenRouterAnthropic(t *testing.T) {
	tests := []struct {
		name     string
		protocol string
		baseURL  string
		header   string
		prefix   string
	}{
		{name: "openai", protocol: "openai", baseURL: "https://example.test/v1", header: "Authorization", prefix: "Bearer "},
		{name: "anthropic", protocol: "anthropic", baseURL: "https://api.anthropic.com/v1", header: "x-api-key"},
		{name: "openrouter", protocol: "anthropic", baseURL: "https://openrouter.ai/api/v1", header: "Authorization", prefix: "Bearer "},
		{name: "openrouter-subdomain", protocol: "anthropic", baseURL: "https://edge.openrouter.ai/api/v1", header: "Authorization", prefix: "Bearer "},
		{name: "lookalike", protocol: "anthropic", baseURL: "https://openrouter.ai.example.test/v1", header: "x-api-key"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got := defaultAPIAuth(test.protocol, test.baseURL)
			if got.Header != test.header || got.Prefix != test.prefix {
				t.Fatalf("auth=%#v, want header=%q prefix=%q", got, test.header, test.prefix)
			}
		})
	}
}

func TestExecuteOpenRouterAnthropicUsesBearer(t *testing.T) {
	t.Setenv("TEST_OPENROUTER_KEY", "secret")
	cfg := Config{ID: "api-cc", Type: TypeAPI, API: &APIConfig{
		Protocol: "anthropic", BaseURL: "https://openrouter.ai/api/v1", Model: "test-model", APIKey: "${TEST_OPENROUTER_KEY}",
	}}
	prepared, err := Prepare(cfg, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	client := &http.Client{Transport: roundTripFunc(func(request *http.Request) (*http.Response, error) {
		if request.URL.String() != "https://openrouter.ai/api/v1/messages" {
			t.Fatalf("url=%s", request.URL)
		}
		if request.Header.Get("Authorization") != "Bearer secret" || request.Header.Get("x-api-key") != "" {
			t.Fatalf("headers=%v", request.Header)
		}
		if request.Header.Get("anthropic-version") != "2023-06-01" {
			t.Fatalf("anthropic-version=%q", request.Header.Get("anthropic-version"))
		}
		return &http.Response{
			StatusCode: http.StatusOK,
			Header:     http.Header{"Content-Type": []string{"application/json"}},
			Body:       io.NopCloser(strings.NewReader(`{"content":[{"type":"text","text":"ok"}]}`)),
			Request:    request,
		}, nil
	})}
	result, err := ExecuteAPI(context.Background(), client, cfg, *prepared.API)
	if err != nil || result.FinalText != "ok" {
		t.Fatalf("result=%#v err=%v", result, err)
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

type roundTripFunc func(*http.Request) (*http.Response, error)

func (function roundTripFunc) RoundTrip(request *http.Request) (*http.Response, error) {
	return function(request)
}
