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

	"agent-runtime/internal/agentrun"
	"agent-runtime/internal/cli/config"
	"agent-runtime/internal/provider"
)

func TestPromptProfilePrintsFinalTextWithoutArtifacts(t *testing.T) {
	root := t.TempDir()
	writeCLIProfile(t, root, "fake", `{
  "type": "api",
  "api": {
    "protocol": "openai",
    "base_url": "https://example.test/v1",
    "model": "mock-model",
    "api_key":"${UNSET_TEST_KEY}",
    "mock": true
  }
}`)

	profile, ok := resolveProfile(root, "fake")
	if !ok {
		t.Fatal("resolveProfile(fake)=false")
	}
	stdout := captureStdout(t, func() {
		code, err := runResolvedProfile(&config.Config{Home: root}, profile, []string{"hello"})
		if err != nil || code != 0 {
			t.Fatalf("code=%d err=%v", code, err)
		}
	})

	if strings.TrimSpace(stdout) != "[mock openai:mock-model] 5 chars" {
		t.Fatalf("stdout=%q", stdout)
	}
	if _, err := os.Stat(filepath.Join(root, "runs")); !os.IsNotExist(err) {
		t.Fatalf("direct prompt created Run artifacts: %v", err)
	}
}

func TestDirectTerminalSinkPrefersStructuredFinalText(t *testing.T) {
	stdout := captureStdout(t, func() {
		sink := &directTerminalSink{finalText: true}
		if err := sink.Stdout([]byte(`{"choices":[{"message":{"content":"OK"}}]}`)); err != nil {
			t.Fatal(err)
		}
		if err := sink.flushResult(provider.Result{Stdout: "raw protocol response", FinalText: "OK"}); err != nil {
			t.Fatal(err)
		}
	})
	if stdout != "OK\n" {
		t.Fatalf("stdout=%q", stdout)
	}
}

func TestSkillRunUsesDirectProfileWithoutArtifacts(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "configs", "skills", "review"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIProfile(t, home, "native-mock", `{"type":"native","native":{"system_prompt":"test","max_rounds":2,"mock":{"responses":["ok"],"done_after":1}}}`)
	skill := "name: review\ndescription: review code\ndefault_profile: native-mock\nprompt_template: 'Review {{input}}'\n"
	if err := os.WriteFile(filepath.Join(home, "configs", "skills", "review", "skill.yaml"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	code, output := captureMain(t, []string{"skill", "run", "review", "--input", "main.go"})
	if code != 0 || strings.TrimSpace(output) != "ok" {
		t.Fatalf("code=%d output=%q", code, output)
	}
	if files := regularFilesUnder(t, filepath.Join(home, "runs")); len(files) != 0 {
		t.Fatalf("skill run created Run artifacts: %v", files)
	}
	values, err := agentrun.NewSessionManager(agentrun.New(home)).Store().List(agentrun.SessionFilter{})
	if err != nil || len(values) != 0 {
		t.Fatalf("skill run created logical sessions: %#v err=%v", values, err)
	}
}

func TestProfileConfigExists(t *testing.T) {
	root := t.TempDir()
	writeCLIProfile(t, root, "fake", `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key":"${UNSET}","mock":true},"presets":{"fake-fast":{"overrides":{"model":"fast"}}}}`)

	if !profileConfigExists(root, "fake") {
		t.Fatal("profileConfigExists(fake)=false, want true")
	}
	if profileConfigExists(root, "../fake") {
		t.Fatal("profileConfigExists accepted path traversal")
	}
	if profileConfigExists(root, "missing") {
		t.Fatal("profileConfigExists(missing)=true, want false")
	}
	if !profileConfigExists(root, "fake-fast") {
		t.Fatal("profileConfigExists did not resolve preset")
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "json.json"), []byte(`{"type":"api","api":{"protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key":"${UNSET}","mock":true}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !profileConfigExists(root, "json") {
		t.Fatal("profileConfigExists(json)=false, want true")
	}
}

func TestProfileRouteSeparatesInteractivePromptAndPassthrough(t *testing.T) {
	tests := []struct {
		name  string
		input []string
		route profileRoute
		args  []string
	}{
		{name: "interactive", route: profileRouteInteractive},
		{name: "raw", input: []string{"--", "exec", "hello"}, route: profileRoutePassthrough, args: []string{"exec", "hello"}},
		{name: "prompt", input: []string{"hello", "world"}, route: profileRoutePrompt, args: []string{"hello", "world"}},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			route, args, err := routeProfileArgs(tt.input)
			if err != nil || route != tt.route || !reflect.DeepEqual(args, tt.args) {
				t.Fatalf("route=%q args=%#v err=%v", route, args, err)
			}
		})
	}
	for _, input := range [][]string{{"run", "hello"}, {"submit", "hello"}} {
		if _, _, err := routeProfileArgs(input); err == nil {
			t.Fatalf("removed profile action accepted %#v", input)
		}
	}
}

func TestProfilePromptRejectsHelpLikeTokensBeforeSeparator(t *testing.T) {
	if _, _, err := parseProfilePrompt([]string{"--help"}); err == nil || !strings.Contains(err.Error(), "must follow --") {
		t.Fatalf("err=%v", err)
	}
}

func TestProfilePromptAcceptsHyphenLeadingPromptWithoutSeparator(t *testing.T) {
	prompt, rawArgs, err := parseProfilePrompt([]string{"-foo", "-bar"})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if prompt != "-foo -bar" || len(rawArgs) != 0 {
		t.Fatalf("prompt=%q raw=%v", prompt, rawArgs)
	}
}

func TestPrintProvidersUsesInternalRegistry(t *testing.T) {
	root := t.TempDir()
	writeCLIProfile(t, root, "fake", `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key":"${UNSET}","mock":true}}`)
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

func TestPrintHelpDocumentsCanonicalNamespacesOnly(t *testing.T) {
	stdout := captureStdout(t, printHelp)
	for _, text := range []string{"<profile> [prompt...]", "run list|show", "session run|submit|open", "profile list|show", "system doctor|start|status|stop|restart|migrate-config|update"} {
		if !strings.Contains(stdout, text) {
			t.Fatalf("help missing %q:\n%s", text, stdout)
		}
	}
	for _, removed := range []string{"Legacy aliases", "providers ->", "upgrade ->", "sn-cli task ", "sn-cli history ", "<profile> run|submit", "reconcile|command"} {
		if strings.Contains(stdout, removed) {
			t.Fatalf("help contains removed command %q:\n%s", removed, stdout)
		}
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
	values, err := agentrun.NewSessionManager(agentrun.New(home)).Store().List(agentrun.SessionFilter{})
	if err != nil || len(values) != 0 {
		t.Fatalf("raw direct invocation created logical sessions: %#v err=%v", values, err)
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
	code, err := runResolvedProfile(&config.Config{Home: home}, profile, []string{"--", "exec", "hello"})
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

func TestCommandProfilePromptUsesManagedArgsWithoutArtifacts(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, "tool.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s|' \"$*\"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIProfile(t, home, "direct", fmt.Sprintf(`{"type":"cli","cli":{"driver":"generic","executor":"command","command":{"binary":%q,"args":["common"],"model":""},"runtime":{"prompt_delivery":"stdin","managed_args":["managed"]}}}`, script))
	profile, ok := resolveProfile(home, "direct")
	if !ok {
		t.Fatal("resolveProfile(direct)=false")
	}
	stdout := captureStdout(t, func() {
		code, err := runResolvedProfile(&config.Config{Home: home}, profile, []string{"hello", "world"})
		if err != nil || code != 0 {
			t.Fatalf("code=%d err=%v", code, err)
		}
	})
	if !strings.Contains(stdout, "common managed|hello world") {
		t.Fatalf("stdout=%q", stdout)
	}
	if _, err := os.Stat(filepath.Join(home, "runs")); !os.IsNotExist(err) {
		t.Fatalf("direct prompt created Run artifacts: %v", err)
	}
	values, err := agentrun.NewSessionManager(agentrun.New(home)).Store().List(agentrun.SessionFilter{})
	if err != nil || len(values) != 0 {
		t.Fatalf("direct prompt created logical sessions: %#v err=%v", values, err)
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

func regularFilesUnder(t *testing.T, root string) []string {
	t.Helper()
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if os.IsNotExist(err) {
			return nil
		}
		if err != nil {
			return err
		}
		if entry.Type().IsRegular() {
			files = append(files, path)
		}
		return nil
	})
	if err != nil && !os.IsNotExist(err) {
		t.Fatal(err)
	}
	return files
}

func captureStderr(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stderr
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stderr pipe: %v", err)
	}
	os.Stderr = writer
	defer func() {
		os.Stderr = original
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("close stderr writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, reader); err != nil {
		t.Fatalf("read stderr pipe: %v", err)
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
