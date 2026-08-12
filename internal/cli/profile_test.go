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
	runtimeprofile "github.com/yy003x/runtime/profile"
)

func TestProfileUsageDocumentsSingleOptionalInput(t *testing.T) {
	err := runVNextProfileNamespace(
		layout.Paths{}, nil, newCLIOutput(false, &strings.Builder{}, &strings.Builder{}),
	)
	if err == nil || err.Error() !=
		"usage: profile list|show|check" {
		t.Fatalf("error=%v", err)
	}
}

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

func TestProfileManagementDoesNotLoadRuntimeConfig(t *testing.T) {
	paths := prepareVNextHome(t)
	writeVNextCommand(t, paths.ConfigDir, "cx")
	if err := os.WriteFile(
		paths.RuntimeConfigFile, []byte(`{"agent":{"max_rounds":0}}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	var stdout strings.Builder
	if err := runVNextProfileNamespace(
		paths, []string{"list"}, newCLIOutput(false, &stdout, os.Stderr),
	); err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(stdout.String(), "cx") {
		t.Fatalf("output=%q", stdout.String())
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
		if err := runTestReq(
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
		if err := runTestReq(
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
		if err := runTestReq(
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
		vNextTestModelProfile("openai"),
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
	err := runTestReq(
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

func TestParseDirectModelInputRejectsModelOverride(t *testing.T) {
	openAIProfile := vNextTestModelProfile("openai")
	if _, _, err := parseDirectModelInput(
		"api", openAIProfile, []string{"--model", "other", "hello"},
	); err == nil {
		t.Fatal("unsupported --model override was accepted")
	}
	request, stream, err := parseDirectModelInput(
		"api",
		openAIProfile,
		[]string{
			"--stream", "--temperature", "0.2",
			"--max-tokens", "128", "hello",
		},
	)
	if err != nil || !stream || request.Input.Options.Temperature == nil ||
		request.Input.Options.MaxOutputTokens == nil ||
		*request.Input.Options.MaxOutputTokens != 128 ||
		len(request.Input.Messages) != 1 {
		t.Fatalf("request=%#v stream=%v error=%v", request, stream, err)
	}
	anthropicProfile := vNextTestModelProfile("anthropic")
	if _, _, err := parseDirectModelInput(
		"api", anthropicProfile, []string{"--max-tokens", "128", "hello"},
	); err != nil {
		t.Fatalf("Anthropic max_tokens rejected: %v", err)
	}
}

func TestParseDirectModelInputEnforcesStrictOptionAndInputGrammar(t *testing.T) {
	profile := vNextTestModelProfile("openai")
	request, stream, err := parseDirectModelInput(
		"api",
		profile,
		[]string{
			"--stream", "--system", "system",
			"--temperature", "0.5",
			"--max-tokens", "128",
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
			"--max-tokens", "1",
			"--max-tokens", "2", "input",
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
	profile := vNextTestModelProfile("openai")
	for _, value := range []string{"NaN", "+Inf", "-Inf"} {
		if _, _, err := parseDirectModelInput(
			"api", profile,
			[]string{"--temperature", value, "input"},
		); err == nil {
			t.Fatalf("temperature %q was accepted", value)
		}
	}
}

func TestReadModelRequestRejectsTrailingOversizedAndInvalidText(t *testing.T) {
	root := t.TempDir()
	valid := `{"messages":[{"role":"user","content":"hello"}]}`
	tests := []struct {
		name    string
		content []byte
	}{
		{
			name:    "multiple_values",
			content: []byte(valid + ` {"messages":[{"role":"user","content":"again"}]}`),
		},
		{
			name:    "trailing_garbage",
			content: []byte(valid + ` trailing`),
		},
		{
			name:    "oversized",
			content: []byte(`{"messages":[{"role":"user","content":"` + strings.Repeat("x", 1<<20) + `"}]}`),
		},
		{
			name:    "nul_text",
			content: []byte(`{"messages":[{"role":"user","content":"bad\u0000text"}]}`),
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			path := filepath.Join(root, test.name+".json")
			if err := os.WriteFile(path, test.content, 0o600); err != nil {
				t.Fatal(err)
			}
			if _, err := readModelRequest(path); err == nil {
				t.Fatal("invalid request file was accepted")
			}
		})
	}
}

func TestReadModelRequestClassifiesDocumentsButNotFileIO(t *testing.T) {
	root := t.TempDir()
	for _, testCase := range []struct {
		name    string
		content string
	}{
		{name: "malformed", content: `{"messages":`},
		{
			name: "duplicate",
			content: `{
				"messages":[{"role":"user","content":"one"}],
				"messages":[{"role":"user","content":"two"}]
			}`,
		},
		{
			name: "unknown",
			content: `{
				"messages":[{"role":"user","content":"one"}],
				"unknown":true
			}`,
		},
		{
			name:    "trailing",
			content: `{"messages":[{"role":"user","content":"one"}]} {}`,
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			path := filepath.Join(root, testCase.name+".json")
			if err := os.WriteFile(path, []byte(testCase.content), 0o600); err != nil {
				t.Fatal(err)
			}
			_, err := readModelRequest(path)
			var validationErr *cliValidationError
			if err == nil || !errors.As(err, &validationErr) {
				t.Fatalf("error=%v, want CLI validation", err)
			}
			assertMachineErrorCode(t, err, contract.ErrorInvalidRequest)
		})
	}

	_, err := readModelRequest(filepath.Join(root, "missing.json"))
	var validationErr *cliValidationError
	if err == nil || errors.As(err, &validationErr) ||
		!errors.Is(err, os.ErrNotExist) {
		t.Fatalf("missing file error=%v, want internal file I/O", err)
	}
	assertMachineErrorCode(t, err, contract.ErrorInternal)
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
	invocation, err := buildCommandProfileInvocation(
		runtimecommand.Profile{
			Command: commandPath,
			Args:    []string{"--sandbox", "read-only"},
			Model:   "old", Effort: runtimecommand.EffortLow,
			Prompt: "base.txt",
		},
		[]string{
			"--model", "new", "--effort=high",
			"--prompt", "typed.txt",
			"--cwd", "work", "position",
		},
		"stdin", root, []string{"PATH=" + root}, runtimecommand.ModeExec,
	)
	if err != nil {
		t.Fatal(err)
	}
	if invocation.CWD != work {
		t.Fatalf("cwd=%q", invocation.CWD)
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

func TestParseCommandProfileOptionsRejectsExecAndSupportsTerminator(t *testing.T) {
	options, err := parseCommandProfileOptions(
		[]string{"--", "-leading"},
	)
	if err != nil || options.positional == nil || *options.positional != "-leading" {
		t.Fatalf("options=%#v error=%v", options, err)
	}
	for _, args := range [][]string{
		{"--unknown"},
		{"--exec"},
		{"--exec=false"},
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

func TestParseCommandProfileOptionsRecognizesResume(t *testing.T) {
	withID, err := parseCommandProfileOptions([]string{"resume", "session-1"})
	if err != nil || withID.resume == nil || *withID.resume != "session-1" {
		t.Fatalf("resume <id>: options=%#v err=%v", withID, err)
	}
	bare, err := parseCommandProfileOptions([]string{"resume"})
	if err != nil || bare.resume == nil || *bare.resume != "" {
		t.Fatalf("bare resume: options=%#v err=%v", bare, err)
	}
	// resume 之后允许 typed option（--model 等）。
	withModel, err := parseCommandProfileOptions(
		[]string{"resume", "session-1", "--model", "gpt-5"},
	)
	if err != nil || withModel.resume == nil || *withModel.resume != "session-1" ||
		withModel.model == nil || *withModel.model != "gpt-5" {
		t.Fatalf("resume + --model: options=%#v err=%v", withModel, err)
	}
	// resume 后紧跟 typed option（--model）→ 不当作 id 消费，resume 为 bare。
	bareThenModel, err := parseCommandProfileOptions(
		[]string{"resume", "--model", "x"},
	)
	if err != nil || bareThenModel.resume == nil || *bareThenModel.resume != "" ||
		bareThenModel.model == nil {
		t.Fatalf("bare resume + --model: options=%#v err=%v", bareThenModel, err)
	}
	for _, args := range [][]string{
		{"resume", "a", "positional"}, // resume + id + bare positional
		{"input", "resume"},           // positional 后再 resume
	} {
		if _, err := parseCommandProfileOptions(args); err == nil {
			t.Fatalf("args=%q returned nil error", args)
		}
	}
}

func TestBuildCommandProfileInvocationResumeTranslates(t *testing.T) {
	root := t.TempDir()
	for _, test := range []struct {
		name     string
		command  string
		argv     []string
		fragment string
	}{
		{name: "claude", command: "claude", argv: []string{"resume", "s-1"}, fragment: "--resume\x00s-1"},
		{name: "codex", command: "codex", argv: []string{"resume", "s-1"}, fragment: "resume\x00s-1"},
	} {
		t.Run(test.name, func(t *testing.T) {
			commandPath := filepath.Join(root, test.command)
			if err := os.WriteFile(
				commandPath, []byte("#!/bin/sh\nexit 0\n"), 0o700,
			); err != nil {
				t.Fatal(err)
			}
			invocation, err := buildCommandProfileInvocation(
				runtimecommand.Profile{Command: commandPath},
				test.argv, "", root, []string{"PATH=" + root},
				runtimecommand.ModeInteractive,
			)
			if err != nil {
				t.Fatalf("error=%v", err)
			}
			joined := strings.Join(invocation.Argv, "\x00")
			if !strings.Contains(joined, test.fragment) {
				t.Fatalf("argv=%q missing fragment=%q", invocation.Argv, test.fragment)
			}
		})
	}
	// resume 在 exec 模式下被拒。
	commandPath := filepath.Join(root, "claude")
	if err := os.WriteFile(
		commandPath, []byte("#!/bin/sh\nexit 0\n"), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	_, err := buildCommandProfileInvocation(
		runtimecommand.Profile{Command: commandPath},
		[]string{"resume", "s-1", "--prompt", "x"}, "", root,
		[]string{"PATH=" + root}, runtimecommand.ModeExec,
	)
	if err == nil || !strings.Contains(err.Error(), "interactive") {
		t.Fatalf("expected interactive-only error, got %v", err)
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
	_, err := buildCommandProfileInvocation(
		runtimecommand.Profile{Command: commandPath},
		nil, "", root, []string{"PATH=" + root}, runtimecommand.ModeExec,
	)
	var runtimeErr *contract.RuntimeError
	if err == nil || !strings.Contains(err.Error(), "prompt is required") ||
		!errors.As(err, &runtimeErr) ||
		runtimeErr.Code != contract.ErrorInvalidRequest ||
		runtimeErr.Phase != contract.PhaseProfile {
		t.Fatalf("error=%v", err)
	}
	if _, err := buildCommandProfileInvocation(
		runtimecommand.Profile{Command: commandPath},
		nil, "", root, []string{"PATH=" + root}, runtimecommand.ModeInteractive,
	); err != nil {
		t.Fatalf("error=%v", err)
	}
}

func TestDirectAPIProfileRejectsUnsupportedEffort(t *testing.T) {
	_, _, err := parseDirectModelInput(
		"api-cx",
		vNextTestModelProfile("openai"),
		[]string{"--effort", "high", "reply ok"},
	)
	if err == nil ||
		!strings.Contains(
			err.Error(),
			`--effort is not supported for API profile "api-cx"`,
		) {
		t.Fatalf("error=%v", err)
	}
}

func vNextTestModelProfile(driver string) model.Profile {
	return model.Profile{Driver: model.DriverName(driver)}
}

func runTestReq(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	return runProfileExecutionNamespace(
		paths, args, runtimeprofile.KindModel, "", "req", output,
	)
}

func prepareVNextHome(t *testing.T) layout.Paths {
	t.Helper()
	paths, err := layout.FromHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	t.Setenv("SN_CLI_HOME", paths.Home)
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	// CLI 单元 fixture 不验证配置型网络工具；这里使用内置工具保持执行组合隔离，
	// source Runtime 默认值仍启用已安装的 web_search 和 web_fetch manifest。
	if err := os.WriteFile(
		paths.RuntimeConfigFile,
		[]byte(`{"agent":{"tools":["read_file","list_directory"]}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	return paths
}

func writeVNextCommand(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(dir, id+".json"),
		[]byte(`{"type":"cli","command":"codex","model":"fixture"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
}

func writeVNextModel(t *testing.T, dir, id, endpoint string) {
	t.Helper()
	value := map[string]any{
		"type":   "api",
		"driver": "openai", "endpoint": endpoint, "model": "fixture",
		"headers":    map[string]any{"Authorization": "${MODEL_API_KEY}"},
		"parameters": map[string]any{"max_tokens": 1024},
		"timeout":    "1m",
	}
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(dir, fmt.Sprintf("%s.json", id)), data, 0o600); err != nil {
		t.Fatal(err)
	}
}
