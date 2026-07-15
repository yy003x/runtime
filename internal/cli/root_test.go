package cli

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"agent-runtime/internal/cli/config"
	"agent-runtime/internal/provider"
)

func TestRunProfilePrintsFinalText(t *testing.T) {
	root := t.TempDir()
	writeCLIProfile(t, root, "fake", `{
  "type": "api",
  "api": {
    "protocol": "openai",
    "base_url": "https://example.test/v1",
    "model": "mock-model",
    "api_key_env": "UNSET_TEST_KEY",
    "mock": true
  }
}`)

	stdout := captureStdout(t, func() {
		err := runProfile(&config.Config{Home: root}, []string{"fake", "hello"})
		if err != nil {
			t.Fatalf("runProfile returned error: %v", err)
		}
	})

	if strings.TrimSpace(stdout) != "[mock openai:mock-model] 5 chars" {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestProfileConfigExists(t *testing.T) {
	root := t.TempDir()
	writeCLIProfile(t, root, "fake", `{"type":"api","aliases":["mock-alias"],"api":{"protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key_env":"UNSET","mock":true},"presets":{"fake-fast":{"overrides":{"model":"fast"}}}}`)

	if !profileConfigExists(root, "fake") {
		t.Fatal("profileConfigExists(fake)=false, want true")
	}
	if profileConfigExists(root, "../fake") {
		t.Fatal("profileConfigExists accepted path traversal")
	}
	if profileConfigExists(root, "missing") {
		t.Fatal("profileConfigExists(missing)=true, want false")
	}
	if !profileConfigExists(root, "fake-fast") || !profileConfigExists(root, "mock-alias") {
		t.Fatal("profileConfigExists did not resolve preset/alias")
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "json.json"), []byte(`{"type":"api","api":{"protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key_env":"UNSET","mock":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !profileConfigExists(root, "json") {
		t.Fatal("profileConfigExists(json)=false, want true")
	}
}

func TestParseProfileInvocationSupportsPromptFileAndImages(t *testing.T) {
	got, err := parseProfileInvocation([]string{
		"codex", "--prompt-file", "prompt.md", "--image", "one.png", "--image", "two.png", "--session-id", "s1",
	})
	if err != nil {
		t.Fatalf("parseProfileInvocation returned error: %v", err)
	}
	if got.Profile != "codex" || got.PromptFile != "prompt.md" || got.SessionID != "s1" {
		t.Fatalf("invocation=%#v", got)
	}
	if len(got.Images) != 2 || got.Images[0] != "one.png" || got.Images[1] != "two.png" {
		t.Fatalf("images=%#v", got.Images)
	}
}

func TestParseProfileInvocationSeparatesRawCLIArgs(t *testing.T) {
	got, err := parseProfileInvocation([]string{"codex", "review", "repo", "--", "--skip-git-repo-check"})
	if err != nil {
		t.Fatal(err)
	}
	if got.Prompt != "review repo" || !reflect.DeepEqual(got.RawCLIArgs, []string{"--skip-git-repo-check"}) {
		t.Fatalf("invocation=%#v", got)
	}
}

func TestParseProfileInvocationSupportsInlinePrompt(t *testing.T) {
	got, err := parseProfileInvocation([]string{"codex", "review", "this", "repo"})
	if err != nil {
		t.Fatalf("parseProfileInvocation returned error: %v", err)
	}
	if got.Prompt != "review this repo" {
		t.Fatalf("prompt=%q", got.Prompt)
	}
}

func TestParseProfileInvocationRejectsUnknownOptionsBeforeSeparator(t *testing.T) {
	_, err := parseProfileInvocation([]string{"codex", "prompt", "--not-a-cli-flag"})
	if err == nil || !strings.Contains(err.Error(), "put target CLI args after --") {
		t.Fatalf("error=%v", err)
	}
}

func TestParseProfileInvocationRejectsPromptConflict(t *testing.T) {
	_, err := parseProfileInvocation([]string{"codex", "inline", "--prompt-file", "prompt.md"})
	if err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("error=%v", err)
	}
}

func TestParseProfileInvocationRejectsRemovedUnderscoreOption(t *testing.T) {
	_, err := parseProfileInvocation([]string{"codex", "--prompt_file", "prompt.md"})
	if err == nil {
		t.Fatal("removed --prompt_file option was accepted")
	}
}

func TestPrintProvidersUsesInternalRegistry(t *testing.T) {
	root := t.TempDir()
	writeCLIProfile(t, root, "fake", `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key_env":"UNSET","mock":true}}`)
	stdout := captureStdout(t, func() {
		if err := printProviders(root); err != nil {
			t.Fatalf("printProviders returned error: %v", err)
		}
	})
	if !strings.Contains(stdout, `"source": "configs"`) {
		t.Fatalf("stdout=%q, want configs source", stdout)
	}
	if !strings.Contains(stdout, `"fake"`) {
		t.Fatalf("stdout=%q, want fake provider", stdout)
	}
}

func TestInteractiveProfilePassesRawArgsWithoutRunArtifacts(t *testing.T) {
	home := t.TempDir()
	output := filepath.Join(home, "argv.txt")
	script := filepath.Join(home, "tool.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OUTPUT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := provider.Config{ID: "direct", Type: provider.TypeCLI, CLI: &provider.CLIConfig{
		Driver: "generic", Executor: provider.ExecutorCommand,
		Command: provider.CommandConfig{Binary: script, Args: []string{"common"}, Env: map[string]string{"OUTPUT": output}},
		Runtime: provider.CLIRuntime{PromptDelivery: "stdin", ManagedArgs: []string{"managed"}},
	}}
	code, err := runInteractiveProfile(&config.Config{Home: home}, profile, []string{"--help", "raw value"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "common\n--help\nraw value\n" {
		t.Fatalf("argv=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(home, "runs")); !os.IsNotExist(err) {
		t.Fatalf("interactive invocation created managed runs: %v", err)
	}
}

func TestRouteCommandProfileArgs(t *testing.T) {
	tests := []struct {
		name       string
		input      []string
		managed    bool
		directArgs []string
	}{
		{name: "no args", input: nil, directArgs: nil},
		{name: "flag", input: []string{"--help"}, directArgs: []string{"--help"}},
		{name: "short flag", input: []string{"-p", "hello"}, directArgs: []string{"-p", "hello"}},
		{name: "separator", input: []string{"--", "exec", "hello"}, directArgs: []string{"exec", "hello"}},
		{name: "plain text", input: []string{"hello", "world"}, managed: true, directArgs: []string{"hello", "world"}},
		{name: "native word requires separator", input: []string{"exec", "hello"}, managed: true, directArgs: []string{"exec", "hello"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			managed, directArgs := routeCommandProfileArgs(tt.input)
			if managed != tt.managed || !reflect.DeepEqual(directArgs, tt.directArgs) {
				t.Fatalf("managed=%v directArgs=%#v, want managed=%v directArgs=%#v", managed, directArgs, tt.managed, tt.directArgs)
			}
		})
	}
}

func TestCommandProfileDoubleDashForcesRawArgs(t *testing.T) {
	home := t.TempDir()
	output := filepath.Join(home, "argv.txt")
	script := filepath.Join(home, "tool.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$OUTPUT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := provider.Config{ID: "direct", Type: provider.TypeCLI, CLI: &provider.CLIConfig{
		Driver: "generic", Executor: provider.ExecutorCommand,
		Command: provider.CommandConfig{Binary: script, Args: []string{"common"}, Env: map[string]string{"OUTPUT": output}},
	}}
	code, err := runCommandProfile(&config.Config{Home: home}, profile, []string{"direct", "--", "exec", "hello"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	data, err := os.ReadFile(output)
	if err != nil || string(data) != "common\nexec\nhello\n" {
		t.Fatalf("argv=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(home, "runs")); !os.IsNotExist(err) {
		t.Fatalf("raw invocation created managed runs: %v", err)
	}
}

func TestCommandProfilePlainTextUsesAgentRunAndManagedArgs(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, "tool.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s|' \"$*\"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIProfile(t, home, "direct", fmt.Sprintf(`{"type":"cli","cli":{"driver":"generic","executor":"command","command":{"binary":%q,"args":["common"],"model":""},"runtime":{"prompt_delivery":"stdin","managed_args":["managed"],"result_contract":"optional"}}}`, script))
	profile, ok := resolveProfile(home, "direct")
	if !ok {
		t.Fatal("resolveProfile(direct)=false")
	}
	stdout := captureStdout(t, func() {
		code, err := runCommandProfile(&config.Config{Home: home}, profile, []string{"direct", "hello", "world"})
		if err != nil || code != 0 {
			t.Fatalf("code=%d err=%v", code, err)
		}
	})
	if !strings.Contains(stdout, "common managed|hello world") {
		t.Fatalf("stdout=%q", stdout)
	}
	matches, err := filepath.Glob(filepath.Join(home, "runs", "task", "*", "*", "result.json"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("result artifacts=%v err=%v", matches, err)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = original
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, reader); err != nil {
		t.Fatalf("read stdout pipe: %v", err)
	}
	return buf.String()
}

func writeCLIProfile(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "configs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create configs dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".json"), []byte(body), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}
