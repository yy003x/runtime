package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/agentrun"
	"github.com/yy003x/runtime/internal/cli/config"
	"github.com/yy003x/runtime/internal/provider"
)

func TestDirectCLIProfilePrintsOutputWithoutArtifacts(t *testing.T) {
	root := t.TempDir()
	writeFixtureCLIProfile(t, root, "fake", "direct ok")

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

	if strings.TrimSpace(stdout) != "direct ok" {
		t.Fatalf("stdout=%q", stdout)
	}
	if _, err := os.Stat(filepath.Join(root, "runs")); !os.IsNotExist(err) {
		t.Fatalf("direct CLI created Run artifacts: %v", err)
	}
}

func TestProfileExecUsesRuntimeDefaultDeadline(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "slow.sh")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nsleep 5\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIProfile(t, root, "slow", fmt.Sprintf(`{"command":%q}`, script))
	if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), []byte("default_deadline_seconds: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	profile, ok := resolveProfile(root, "slow")
	if !ok {
		t.Fatal("resolveProfile(slow)=false")
	}
	started := time.Now()
	code, err := runUnrecordedProfile(&config.Config{Home: root}, profile, []string{"hello"})
	if err == nil || code != 1 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatalf("deadline took too long: %s", time.Since(started))
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
	if err := os.MkdirAll(filepath.Join(home, "resources", "skills", "review"), 0o755); err != nil {
		t.Fatal(err)
	}
	argvFile := filepath.Join(home, "skill-argv.txt")
	script := filepath.Join(home, "codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SN_TEST_ARGV\"\nprintf 'ok\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIProfile(t, home, "skill-cx", fmt.Sprintf(`{"command":%q,"env":{"SN_TEST_ARGV":%q}}`, script, argvFile))
	skill := "name: review\ndescription: review code\ndefault_profile: skill-cx\nprompt_template: 'Review {{input}}'\n"
	if err := os.WriteFile(filepath.Join(home, "resources", "skills", "review", "skill.yaml"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	code, output := captureMain(t, []string{"skill", "run", "review", "--input", "main.go", "--search"})
	if code != 0 || strings.TrimSpace(output) != "ok" {
		t.Fatalf("code=%d output=%q", code, output)
	}
	argv, err := os.ReadFile(argvFile)
	if err != nil || string(argv) != "--search\nReview main.go\n" {
		t.Fatalf("skill argv=%q err=%v", argv, err)
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
	writeCLIProfile(t, root, "fake", `{"protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key":"${UNSET}"}`)
	writeCLIProfile(t, root, "fake-fast", `{"protocol":"openai","base_url":"https://example.test/v1","model":"fast","api_key":"${UNSET}"}`)

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
		t.Fatal("profileConfigExists did not resolve standalone profile")
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "json.json"), []byte(`{"protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key":"${UNSET}"}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !profileConfigExists(root, "json") {
		t.Fatal("profileConfigExists(json)=false, want true")
	}
}

func TestAPIProviderArgsParseTypedOptionsAndFinalPrompt(t *testing.T) {
	prompt, overrides, err := parseAPIProviderArgs([]string{
		"--model", "next", "--max-tokens", "2048", "--temperature", "0.2", "--stream", "hello world",
	})
	if err != nil {
		t.Fatalf("err=%v", err)
	}
	if prompt != "hello world" || overrides["model"] != "next" || overrides["max_tokens"] != 2048 || overrides["temperature"] != 0.2 || overrides["stream"] != true {
		t.Fatalf("prompt=%q overrides=%#v", prompt, overrides)
	}
	if _, _, err := parseAPIProviderArgs([]string{"hello", "world"}); err == nil {
		t.Fatal("API parser accepted an unquoted multi-token prompt")
	}
	if _, _, err := parseAPIProviderArgs([]string{"--unknown", "hello"}); err == nil {
		t.Fatal("API parser accepted an unknown option")
	}
}

func TestDirectAPIProfileMapsTypedProviderArgsWithoutArtifacts(t *testing.T) {
	t.Setenv("SN_TEST_API_KEY", "secret")
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Test") != "configured" {
			t.Errorf("X-Test=%q", request.Header.Get("X-Test"))
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode payload: %v", err)
		}
		requests <- payload
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"message":{"content":"OK"}}]}`)
	}))
	defer server.Close()

	home := t.TempDir()
	writeCLIProfile(t, home, "api", fmt.Sprintf(
		`{"protocol":"openai","base_url":%q,"model":"base","api_key":"${SN_TEST_API_KEY}","headers":{"X-Test":"configured"},"max_tokens":16384}`,
		server.URL,
	))
	profile := mustResolveProfile(t, home, "api")
	stdout := captureStdout(t, func() {
		code, err := runUnrecordedProfile(&config.Config{Home: home}, profile, []string{
			"--model", "next", "--max-tokens", "2048", "--temperature", "0.2", "reply OK",
		})
		if err != nil || code != 0 {
			t.Fatalf("code=%d err=%v", code, err)
		}
	})
	if strings.TrimSpace(stdout) != "OK" {
		t.Fatalf("stdout=%q", stdout)
	}
	payload := <-requests
	if payload["model"] != "next" || payload["max_tokens"] != float64(2048) || payload["temperature"] != 0.2 {
		t.Fatalf("payload=%#v", payload)
	}
	messages, _ := payload["messages"].([]any)
	message, _ := messages[0].(map[string]any)
	if message["content"] != "reply OK" {
		t.Fatalf("messages=%#v", messages)
	}
	if files := regularFilesUnder(t, filepath.Join(home, "runs")); len(files) != 0 {
		t.Fatalf("direct API created Run artifacts: %v", files)
	}
}

func TestSessionProviderInputSeparatesContextPromptFromCLITail(t *testing.T) {
	cliProfile := provider.Config{ID: "cx", Type: provider.TypeCLI, CLI: &provider.CLIConfig{}}
	prompt, rawArgs, overrides, err := parseSessionProviderInput(
		cliProfile,
		[]string{"--skip-git-repo-check", "--model", "next", "reply OK"},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "reply OK" || !reflect.DeepEqual(rawArgs, []string{"--skip-git-repo-check", "--model", "next"}) || len(overrides) != 0 {
		t.Fatalf("prompt=%q raw=%#v overrides=%#v", prompt, rawArgs, overrides)
	}

	prompt, rawArgs, _, err = parseSessionProviderInput(cliProfile, []string{"--ephemeral"}, true)
	if err != nil || prompt != "" || !reflect.DeepEqual(rawArgs, []string{"--ephemeral"}) {
		t.Fatalf("external prompt=%q raw=%#v err=%v", prompt, rawArgs, err)
	}

	apiProfile := provider.Config{ID: "api-cx", Type: provider.TypeAPI, API: &provider.APIConfig{}}
	prompt, rawArgs, overrides, err = parseSessionProviderInput(
		apiProfile,
		[]string{"--model", "next", "--temperature", "0.2", "reply API"},
		false,
	)
	if err != nil {
		t.Fatal(err)
	}
	if prompt != "reply API" || len(rawArgs) != 0 || overrides["model"] != "next" || overrides["temperature"] != 0.2 {
		t.Fatalf("API prompt=%q raw=%#v overrides=%#v", prompt, rawArgs, overrides)
	}
	if _, _, _, err := parseSessionProviderInput(apiProfile, []string{"--unknown", "reply API"}, false); err == nil {
		t.Fatal("API session accepted an unknown provider option")
	}
}

func TestPrintProvidersUsesInternalRegistry(t *testing.T) {
	root := t.TempDir()
	writeCLIProfile(t, root, "fake", `{"protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key":"${UNSET}"}`)
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
	for _, text := range []string{
		"<profile> [native-cli-args...]",
		"profile exec <profile> [provider-input...]",
		"session run|submit [runtime-options...] <profile> [provider-input...]",
		"session open [runtime-options...] <profile> [native-cli-args...]",
		"run list|show",
		"session run|submit|open",
		"profile list|show|validate|command|exec",
		"system doctor|start|status|stop|restart|update",
		"llm generate --request-file <path|-> [--stream]",
		"<CLI profile>      native direct execution",
		"<API profile>      direct typed API request",
		"--version        Show the Git tag version and build metadata.",
		"Runtime options must appear before <profile>",
		"${SN_CLI_HOME:-~/.sn}",
	} {
		if !strings.Contains(stdout, text) {
			t.Fatalf("help missing %q:\n%s", text, stdout)
		}
	}
	for _, removed := range []string{"Legacy aliases", "providers ->", "upgrade ->", "sn-cli task ", "sn-cli history ", "sn-cli version", "<profile> run|submit", "reconcile|command", "<profile> -- [raw-cli-args", "Runtime separator", "CLI receives it literally", "Installed binary: ~/.sn/bin/sn-cli"} {
		if strings.Contains(stdout, removed) {
			t.Fatalf("help contains removed command %q:\n%s", removed, stdout)
		}
	}
}

func TestGlobalHelpAndVersionDoNotRequireRuntimeHome(t *testing.T) {
	blockedHome := filepath.Join(t.TempDir(), "not-a-directory")
	if err := os.WriteFile(blockedHome, []byte("blocked"), 0o600); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SN_CLI_HOME", blockedHome)
	for _, args := range [][]string{{}, {"--help"}, {"-h"}, {"--version"}} {
		if code, output := captureMain(t, args); code != 0 {
			t.Fatalf("Main(%q) code=%d output=%q", args, code, output)
		}
	}
	if code, output := captureMain(t, []string{"profile", "list"}); code == 0 || !strings.Contains(output, "create runtime directory") {
		t.Fatalf("profile list code=%d output=%q, want runtime home error", code, output)
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
		Runtime: provider.CLIRuntime{PromptDelivery: "stdin"},
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

func TestCommandProfilePassesDoubleDashToNativeCLI(t *testing.T) {
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
	if err != nil || string(data) != "common\n--\nexec\nhello\n" {
		t.Fatalf("argv=%q err=%v", data, err)
	}
	if _, err := os.Stat(filepath.Join(home, "runs")); !os.IsNotExist(err) {
		t.Fatalf("raw invocation created managed runs: %v", err)
	}
}

func TestCommandProfileArgumentsUseNativeDirectModeWithoutArtifacts(t *testing.T) {
	home := t.TempDir()
	output := filepath.Join(home, "argv.txt")
	script := filepath.Join(home, "codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$SN_TEST_OUTPUT\"\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIProfile(t, home, "direct", fmt.Sprintf(`{"command":%q,"args":["common"],"env":{"SN_TEST_OUTPUT":%q}}`, script, output))
	profile := mustResolveProfile(t, home, "direct")
	code, err := runResolvedProfile(&config.Config{Home: home}, profile, []string{"--help", "hello", "world"})
	if err != nil || code != 0 {
		t.Fatalf("code=%d err=%v", code, err)
	}
	argv, err := os.ReadFile(output)
	if err != nil || string(argv) != "common\n--help\nhello\nworld\n" {
		t.Fatalf("argv=%q err=%v", argv, err)
	}
	if _, err := os.Stat(filepath.Join(home, "runs")); !os.IsNotExist(err) {
		t.Fatalf("direct prompt created Run artifacts: %v", err)
	}
	values, err := agentrun.NewSessionManager(agentrun.New(home)).Store().List(agentrun.SessionFilter{})
	if err != nil || len(values) != 0 {
		t.Fatalf("direct prompt created logical sessions: %#v err=%v", values, err)
	}
}

func TestPipedStdinDoesNotSelectManagedMode(t *testing.T) {
	home := t.TempDir()
	argvFile := filepath.Join(home, "argv.txt")
	stdinFile := filepath.Join(home, "stdin.txt")
	script := filepath.Join(home, "codex")
	content := "#!/bin/sh\nprintf '%s\\n' \"$@\" > \"$ARGV_FILE\"\ncat > \"$STDIN_FILE\"\n"
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIProfile(t, home, "direct", fmt.Sprintf(
		`{"command":%q,"args":["common"],"env":{"ARGV_FILE":%q,"STDIN_FILE":%q}}`,
		script, argvFile, stdinFile,
	))
	profile := mustResolveProfile(t, home, "direct")
	withStdin(t, "hello from stdin", func() {
		code, err := runResolvedProfile(&config.Config{Home: home}, profile, nil)
		if err != nil || code != 0 {
			t.Fatalf("code=%d err=%v", code, err)
		}
	})
	argv, err := os.ReadFile(argvFile)
	if err != nil || string(argv) != "common\n" {
		t.Fatalf("argv=%q err=%v", argv, err)
	}
	stdin, err := os.ReadFile(stdinFile)
	if err != nil || string(stdin) != "hello from stdin" {
		t.Fatalf("stdin=%q err=%v", stdin, err)
	}
	if files := regularFilesUnder(t, filepath.Join(home, "runs")); len(files) != 0 {
		t.Fatalf("piped direct CLI created Run artifacts: %v", files)
	}
}

func TestProfileExecUsesManagedModeWithoutArtifacts(t *testing.T) {
	home := t.TempDir()
	script := filepath.Join(home, "codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\nprintf '%s|' \"$*\"\ncat\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIProfile(t, home, "batch", fmt.Sprintf(`{"command":%q,"args":["common"]}`, script))
	profile := mustResolveProfile(t, home, "batch")
	stdout := captureStdout(t, func() {
		code, err := runUnrecordedProfile(&config.Config{Home: home}, profile, []string{"hello", "world"})
		if err != nil || code != 0 {
			t.Fatalf("code=%d err=%v", code, err)
		}
	})
	if !strings.Contains(stdout, "common exec hello world|") {
		t.Fatalf("stdout=%q", stdout)
	}
	if files := regularFilesUnder(t, filepath.Join(home, "runs")); len(files) != 0 {
		t.Fatalf("profile exec created Run artifacts: %v", files)
	}
	values, err := agentrun.NewSessionManager(agentrun.New(home)).Store().List(agentrun.SessionFilter{})
	if err != nil || len(values) != 0 {
		t.Fatalf("profile exec created logical sessions: %#v err=%v", values, err)
	}
}

func TestMainDirectCLIAndProfileExecDoNotCreateHistoryRecords(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	script := filepath.Join(home, "codex")
	if err := os.WriteFile(script, []byte("#!/bin/sh\ncat >/dev/null\nprintf 'ok\\n'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIProfile(t, home, "cx-test", fmt.Sprintf(`{"command":%q}`, script))

	if code, output := captureMain(t, []string{"cx-test", "hello"}); code != 0 || strings.TrimSpace(output) != "ok" {
		t.Fatalf("direct code=%d output=%q", code, output)
	}
	withStdin(t, "hello", func() {
		if code, output := captureMain(t, []string{"profile", "exec", "cx-test"}); code != 0 || strings.TrimSpace(output) != "ok" {
			t.Fatalf("exec code=%d output=%q", code, output)
		}
	})

	for _, directory := range []string{"runs", "sessions", "history"} {
		if files := regularFilesUnder(t, filepath.Join(home, directory)); len(files) != 0 {
			t.Fatalf("%s contains history records: %v", directory, files)
		}
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

func withStdin(t *testing.T, content string, fn func()) {
	t.Helper()
	original := os.Stdin
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	if _, err := writer.WriteString(content); err != nil {
		t.Fatal(err)
	}
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	os.Stdin = reader
	defer func() {
		os.Stdin = original
		_ = reader.Close()
	}()
	fn()
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

func mustResolveProfile(t *testing.T, root, name string) provider.Config {
	t.Helper()
	profiles, err := agentrun.New(root).Profiles()
	if err != nil {
		t.Fatalf("load profiles: %v", err)
	}
	profile, ok := provider.Resolve(profiles, name)
	if !ok {
		t.Fatalf("profile %q was not loaded", name)
	}
	return profile
}

func writeFixtureCLIProfile(t *testing.T, root, name, reply string) {
	t.Helper()
	script := filepath.Join(root, name+"-fixture")
	content := `#!/bin/sh
cat >/dev/null
printf '%s\n' "$SN_TEST_REPLY"
if [ -n "$AGENTRUN_RESULT_FILE" ]; then
  printf '{"schema_version":1,"run_id":"%s","outcome":"succeeded","summary":"%s","artifacts":[],"errors":[],"validation":{"commands":[],"passed":true}}\n' "$AGENTRUN_RUN_ID" "$SN_TEST_REPLY" > "$AGENTRUN_RESULT_FILE"
fi
`
	if err := os.WriteFile(script, []byte(content), 0o755); err != nil {
		t.Fatalf("write fixture command: %v", err)
	}
	body, err := json.Marshal(map[string]any{
		"command": script,
		"env":     map[string]string{"SN_TEST_REPLY": reply},
	})
	if err != nil {
		t.Fatalf("marshal fixture profile: %v", err)
	}
	writeCLIProfile(t, root, name, string(body))
}
