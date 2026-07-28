package cli

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	runtimecommand "github.com/yy003x/runtime/command"
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

func TestAPIProfileStreamOptionValueDoesNotSelectStreamMode(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		request *http.Request,
	) {
		var body struct {
			Messages []struct {
				Role    string `json:"role"`
				Content string `json:"content"`
			} `json:"messages"`
		}
		if err := json.NewDecoder(request.Body).Decode(&body); err != nil {
			t.Error(err)
		}
		if len(body.Messages) < 2 || body.Messages[0].Content != "--stream" {
			t.Errorf("body=%#v", body)
		}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
		  "id":"req-system-value",
		  "model":"fixture",
		  "choices":[{"message":{"content":"not streamed"},"finish_reason":"stop"}]
		}`))
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
	if err := runVNextProfileNamespace(
		paths,
		[]string{"api-cx", "--system", "--stream", "hello"},
		output,
	); err != nil {
		t.Fatal(err)
	}
	if output.streamMode || output.streamStarted ||
		stdout.String() != "not streamed\n" {
		t.Fatalf(
			"streamMode=%t streamStarted=%t output=%q",
			output.streamMode, output.streamStarted, stdout.String(),
		)
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

func TestParseCommandProfileInputAppliesGenericEffort(t *testing.T) {
	for _, testCase := range []struct {
		name           string
		profile        runtimecommand.Profile
		args           []string
		wantPrompt     string
		wantNativeArgs []string
		wantSuffix     []string
	}{
		{
			name: "codex_automatic",
			profile: runtimecommand.Profile{
				Args:           []string{"fixed"},
				PromptDelivery: runtimecommand.PromptArgv,
				EffortAdapter:  runtimecommand.EffortAdapterCodexConfig,
			},
			args:       []string{"--effort", "high", "reply ok"},
			wantPrompt: "reply ok",
			wantSuffix: []string{"fixed", "-c", "model_reasoning_effort=high"},
		},
		{
			name: "claude_manual",
			profile: runtimecommand.Profile{
				Args:           []string{"fixed"},
				PromptDelivery: runtimecommand.PromptManual,
				EffortAdapter:  runtimecommand.EffortAdapterClaudeFlag,
			},
			args:           []string{"--effort=max", "--", "--print", "reply ok"},
			wantNativeArgs: []string{"--print", "reply ok"},
			wantSuffix:     []string{"fixed", "--effort", "max"},
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			profile, prompt, nativeArgs, err := parseCommandProfileInput(
				testCase.profile,
				testCase.args,
			)
			if err != nil {
				t.Fatal(err)
			}
			if prompt != testCase.wantPrompt ||
				!reflect.DeepEqual(nativeArgs, testCase.wantNativeArgs) ||
				!reflect.DeepEqual(profile.Args, testCase.wantSuffix) {
				t.Fatalf(
					"profile.args=%q prompt=%q native=%q",
					profile.Args,
					prompt,
					nativeArgs,
				)
			}
		})
	}
}

func TestParseCommandProfileInputRejectsUnsupportedOrInvalidEffort(t *testing.T) {
	profile := runtimecommand.Profile{
		PromptDelivery: runtimecommand.PromptArgv,
	}
	for _, args := range [][]string{
		{"--effort", "high", "reply ok"},
		{"--effort", "extreme", "reply ok"},
		{"--effort", "high", "--effort", "max", "reply ok"},
	} {
		if _, _, _, err := parseCommandProfileInput(profile, args); err == nil {
			t.Fatalf("args=%q returned nil error", args)
		}
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
	if err := os.MkdirAll(paths.CommandDir, 0o700); err != nil {
		t.Fatal(err)
	}
	return paths
}

func writeVNextCommand(t *testing.T, dir, id string) {
	t.Helper()
	if err := os.WriteFile(
		filepath.Join(dir, id+".json"),
		[]byte(`{"type":"cli","binary":"codex","transport":"tty","prompt_delivery":"manual"}`),
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
