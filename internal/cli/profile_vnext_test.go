package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/model"
)

func TestVNextProfileManagementAggregatesCatalogs(t *testing.T) {
	paths := prepareVNextHome(t)
	writeVNextCommand(t, paths.ConfigDir, "cx")
	writeVNextModel(t, paths.ConfigDir, "api-cx", "https://example.invalid/v1/chat/completions")
	for _, args := range [][]string{
		{"list"}, {"show", "cx"}, {"show", "api-cx"}, {"check"}, {"check", "cx"},
	} {
		output := captureStdout(t, func() {
			if err := runVNextProfileNamespace(
				paths, args, newCLIOutput(true, os.Stdout, os.Stderr),
			); err != nil {
				t.Fatalf("args=%q error=%v", args, err)
			}
		})
		if !strings.Contains(output, `"ok":true`) {
			t.Fatalf("args=%q output=%s", args, output)
		}
	}
}

func TestVNextDirectModelReturnsCompletedWithoutRuntimeRecords(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.URL.Path != "/v1/chat/completions" {
			t.Errorf("path=%q", request.URL.Path)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
		  "id":"req-1",
		  "model":"fixture",
		  "choices":[{"message":{"content":"OK"},"finish_reason":"stop"}],
		  "usage":{"prompt_tokens":2,"completion_tokens":1,"total_tokens":3}
		}`))
	}))
	defer server.Close()

	paths := prepareVNextHome(t)
	writeVNextModel(t, paths.ConfigDir, "api-cx", server.URL+"/v1/chat/completions")
	t.Setenv("MODEL_API_KEY", "secret")
	originalTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	output := captureStdout(t, func() {
		if err := runVNextProfileNamespace(
			paths,
			[]string{"api-cx", "hello"},
			newCLIOutput(true, os.Stdout, os.Stderr),
		); err != nil {
			t.Fatal(err)
		}
	})
	if !strings.Contains(output, `"state":"completed"`) ||
		!strings.Contains(output, `"content":"OK"`) {
		t.Fatalf("output=%s", output)
	}
	for _, name := range []string{"sessions", "runs"} {
		entries, err := os.ReadDir(filepath.Join(paths.Home, name))
		if err == nil && len(entries) != 0 {
			t.Fatalf("%s entries=%v", name, entries)
		}
	}
}

func TestVNextDirectModelHumanOutputPrintsAssistantText(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
		  "id":"req-human",
		  "model":"fixture",
		  "choices":[{"message":{"content":"human answer"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()
	paths := prepareVNextHome(t)
	writeVNextModel(t, paths.ConfigDir, "api-cx", server.URL+"/v1/chat/completions")
	t.Setenv("MODEL_API_KEY", "secret")
	originalTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	output := captureStdout(t, func() {
		if err := runVNextProfileNamespace(
			paths,
			[]string{"api-cx", "hello"},
			newCLIOutput(false, os.Stdout, os.Stderr),
		); err != nil {
			t.Fatal(err)
		}
	})
	if output != "human answer\n" {
		t.Fatalf("output=%q", output)
	}
}

func TestVNextDirectModelStreamEndsWithOneCompactFinal(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "text/event-stream")
		fmt.Fprintln(writer, `data: {"id":"req-stream","model":"fixture","choices":[{"delta":{"content":"stream answer"},"finish_reason":"stop"}]}`)
		fmt.Fprintln(writer, "data: [DONE]")
	}))
	defer server.Close()
	paths := prepareVNextHome(t)
	writeVNextModel(t, paths.ConfigDir, "api-cx", server.URL+"/v1/chat/completions")
	t.Setenv("MODEL_API_KEY", "secret")
	originalTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	output := captureStdout(t, func() {
		if err := runVNextProfileNamespace(
			paths,
			[]string{"api-cx", "--stream", "hello"},
			newCLIOutput(false, os.Stdout, os.Stderr),
		); err != nil {
			t.Fatal(err)
		}
	})
	lines := strings.Split(strings.TrimSpace(output), "\n")
	if len(lines) < 2 {
		t.Fatalf("output=%q", output)
	}
	finalCount := 0
	for index, line := range lines {
		var value map[string]json.RawMessage
		if err := json.Unmarshal([]byte(line), &value); err != nil {
			t.Fatalf("line %d is not compact JSON: %v: %q", index, err, line)
		}
		if _, exists := value["state"]; exists {
			finalCount++
			if index != len(lines)-1 {
				t.Fatalf("final is not last: output=%q", output)
			}
		}
	}
	if finalCount != 1 {
		t.Fatalf("final_count=%d output=%q", finalCount, output)
	}
}

func TestAPIProfileFixedValueDoesNotConsumeStreamFlag(t *testing.T) {
	request, stream, err := parseDirectModelInput(
		"api-cx",
		vNextTestModelProfile("openai-compatible"),
		[]string{"--system", "--stream", "hello"},
	)
	if err == nil || !strings.Contains(err.Error(), "--system requires value") {
		t.Fatalf("request=%#v stream=%t error=%v", request, stream, err)
	}
	if stream {
		t.Fatal("value-position --stream selected stream mode")
	}
}

func TestAPIProfileStreamBeginsBeforeProviderFailure(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		http.Error(writer, "unavailable", http.StatusServiceUnavailable)
	}))
	defer server.Close()
	paths := prepareVNextHome(t)
	writeVNextModel(t, paths.ConfigDir, "api-cx", server.URL+"/v1/chat/completions")
	t.Setenv("MODEL_API_KEY", "secret")
	originalTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	var stdout strings.Builder
	output := newCLIOutput(false, &stdout, os.Stderr)
	err := runVNextProfileNamespace(
		paths, []string{"api-cx", "--stream", "hello"}, output,
	)
	if err == nil {
		t.Fatal("expected provider failure")
	}
	if !output.streamMode || output.streamStarted || stdout.Len() != 0 {
		t.Fatalf(
			"streamMode=%t streamStarted=%t output=%q error=%v",
			output.streamMode, output.streamStarted, stdout.String(), err,
		)
	}
}

func TestParseDirectModelInputRejectsLegacyModelOverride(t *testing.T) {
	openAIProfile := vNextTestModelProfile("openai-compatible")
	if _, _, err := parseDirectModelInput(
		"api", openAIProfile, []string{"--model", "other", "hello"},
	); err == nil {
		t.Fatal("legacy --model override was accepted")
	}
	request, stream, err := parseDirectModelInput(
		"api",
		openAIProfile,
		[]string{
			"--stream", "--temperature", "0.2",
			"--max-completion-tokens", "128", "hello",
		},
	)
	if err != nil || !stream || request.Input.Options.Temperature == nil ||
		request.Input.Options.MaxOutputTokens == nil ||
		*request.Input.Options.MaxOutputTokens != 128 ||
		len(request.Input.Messages) != 1 {
		t.Fatalf("request=%#v stream=%v error=%v", request, stream, err)
	}
	if _, _, err := parseDirectModelInput(
		"api", openAIProfile, []string{"--max-tokens", "128", "hello"},
	); err == nil {
		t.Fatal("Anthropic max_tokens was accepted for OpenAI")
	}
	anthropicProfile := vNextTestModelProfile("anthropic-compatible")
	if _, _, err := parseDirectModelInput(
		"api", anthropicProfile, []string{"--max-tokens", "128", "hello"},
	); err != nil {
		t.Fatalf("Anthropic max_tokens rejected: %v", err)
	}
}

func TestParseDirectModelInputEnforcesStrictOptionAndInputGrammar(t *testing.T) {
	profile := vNextTestModelProfile("openai-compatible")
	request, stream, err := parseDirectModelInput(
		"api",
		profile,
		[]string{
			"--stream", "--system", "system",
			"--temperature", "0.5",
			"--max-completion-tokens", "128",
			"--", "--leading-input",
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	if !stream || request.Input.System != "system" ||
		request.Input.Options.Temperature == nil ||
		*request.Input.Options.Temperature != 0.5 ||
		request.Input.Messages[0].Content != "--leading-input" {
		t.Fatalf("request=%#v stream=%t", request, stream)
	}

	for _, args := range [][]string{
		{"--stream", "--stream", "input"},
		{"--system", "one", "--system", "two", "input"},
		{"--temperature", "0.1", "--temperature", "0.2", "input"},
		{
			"--max-completion-tokens", "1",
			"--max-completion-tokens", "2", "input",
		},
		{"--request-file", "one", "--request-file", "two"},
		{"--system", "--stream", "input"},
		{"--temperature", "--stream", "input"},
		{"input", "--stream"},
		{"input", "extra"},
		{"input", "--", "extra"},
		{"--", "one", "two"},
	} {
		if _, _, err := parseDirectModelInput(
			"api", profile, args,
		); err == nil {
			t.Fatalf("args=%q returned nil error", args)
		}
	}
}

func TestParseDirectModelInputRejectsNonFiniteTemperature(t *testing.T) {
	profile := vNextTestModelProfile("openai-compatible")
	for _, value := range []string{"NaN", "+Inf", "-Inf"} {
		if _, _, err := parseDirectModelInput(
			"api", profile,
			[]string{"--temperature", value, "input"},
		); err == nil {
			t.Fatalf("temperature %q was accepted", value)
		}
	}
}

func TestBuildCommandProfileInvocationMergesPromptAndOverridesTypedFields(t *testing.T) {
	root := t.TempDir()
	commandPath := filepath.Join(root, "codex")
	if err := os.WriteFile(
		commandPath, []byte("#!/bin/sh\nexit 0\n"), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "base.txt"), []byte("base"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(root, "typed.txt"), []byte("typed"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	invocation, mode, err := buildCommandProfileInvocation(
		runtimecommand.Profile{
			Command: commandPath,
			Args:    []string{"--sandbox", "read-only"},
			Model:   "old", Effort: runtimecommand.EffortLow,
			Prompt: "base.txt", Exec: false,
		},
		[]string{
			"--model", "new", "--effort=high",
			"--prompt", "typed.txt", "--exec",
			"--cwd", "work", "position",
		},
		"stdin", root, []string{"PATH=" + root},
	)
	if err != nil {
		t.Fatal(err)
	}
	if mode != runtimecommand.ModeExec || invocation.CWD != work {
		t.Fatalf("mode=%s cwd=%q", mode, invocation.CWD)
	}
	wantPrompt := "base\ntyped\nstdin\nposition"
	if got := invocation.Argv[len(invocation.Argv)-1]; got != wantPrompt {
		t.Fatalf("prompt=%q want=%q argv=%q", got, wantPrompt, invocation.Argv)
	}
	joined := strings.Join(invocation.Argv, "\x00")
	for _, expected := range []string{
		"--model\x00new",
		"-c\x00model_reasoning_effort=high",
		"exec\x00--\x00" + wantPrompt,
	} {
		if !strings.Contains(joined, expected) {
			t.Fatalf("argv=%q missing=%q", invocation.Argv, expected)
		}
	}
}

func TestParseCommandProfileOptionsExecGrammarAndTerminator(t *testing.T) {
	options, err := parseCommandProfileOptions(
		[]string{"--exec", "false"},
	)
	if err != nil || options.exec == nil || !*options.exec ||
		options.positional == nil || *options.positional != "false" {
		t.Fatalf("options=%#v error=%v", options, err)
	}
	options, err = parseCommandProfileOptions(
		[]string{"--exec=false", "--", "-leading"},
	)
	if err != nil || options.exec == nil || *options.exec ||
		options.positional == nil || *options.positional != "-leading" {
		t.Fatalf("options=%#v error=%v", options, err)
	}
	for _, args := range [][]string{
		{"--interactive"},
		{"--exec=maybe"},
		{"--effort", "extreme"},
		{"--effort", "high", "--effort", "max"},
		{"--prompt", "--exec"},
		{"--prompt", "--unknown"},
		{"--model", "--effort", "high"},
		{"--cwd="},
		{"input", "--model", "late"},
		{"--", "one", "two"},
	} {
		if _, err := parseCommandProfileOptions(args); err == nil {
			t.Fatalf("args=%q returned nil error", args)
		}
	}
}

func TestBuildCommandProfileInvocationExecRequiresPrompt(t *testing.T) {
	root := t.TempDir()
	commandPath := filepath.Join(root, "codex")
	if err := os.WriteFile(
		commandPath, []byte("#!/bin/sh\nexit 0\n"), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	_, _, err := buildCommandProfileInvocation(
		runtimecommand.Profile{Command: commandPath},
		[]string{"--exec"}, "", root, []string{"PATH=" + root},
	)
	var runtimeErr *contract.RuntimeError
	if err == nil || !strings.Contains(err.Error(), "prompt is required") ||
		!errors.As(err, &runtimeErr) ||
		runtimeErr.Code != contract.ErrorInvalidRequest ||
		runtimeErr.Phase != contract.PhaseProfile {
		t.Fatalf("error=%v", err)
	}
	if _, mode, err := buildCommandProfileInvocation(
		runtimecommand.Profile{Command: commandPath},
		nil, "", root, []string{"PATH=" + root},
	); err != nil || mode != runtimecommand.ModeInteractive {
		t.Fatalf("mode=%s error=%v", mode, err)
	}
}

func TestDirectAPIProfileRecognizesButRejectsEffortWithoutAdapter(t *testing.T) {
	_, _, err := parseDirectModelInput(
		"api-cx",
		vNextTestModelProfile("openai-compatible"),
		[]string{"--effort", "high", "reply ok"},
	)
	if err == nil || !strings.Contains(err.Error(), "API effort adapter") {
		t.Fatalf("error=%v", err)
	}
}

func vNextTestModelProfile(driver string) model.Profile {
	return model.Profile{Driver: model.DriverName(driver)}
}

func prepareVNextHome(t *testing.T) layout.Paths {
	t.Helper()
	paths, err := layout.FromHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return paths
}

func writeVNextCommand(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(dir, id+".json"),
		[]byte(`{"type":"cli","command":"codex","model":"fixture","exec":false}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}

func writeVNextModel(t *testing.T, dir, id, endpoint string) {
	t.Helper()
	value := map[string]any{
		"type":   "api",
		"driver": "openai-compatible", "endpoint": endpoint, "model": "fixture",
		"auth": map[string]any{
			"header": "Authorization", "scheme": "Bearer", "from_env": "MODEL_API_KEY",
		},
		"defaults": map[string]any{"max_completion_tokens": 1024},
		"timeout":  "1m",
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s.json", id)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
