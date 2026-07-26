package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/agentrun"
	"github.com/yy003x/runtime/internal/capability"
	"github.com/yy003x/runtime/internal/daemon"
	"github.com/yy003x/runtime/internal/provider"
)

func TestRuntimeDoctorReportsContractVersion(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	writeFixtureCLIProfile(t, home, "fixture", "ok")

	code, output := captureMain(t, []string{"system", "doctor", "--json"})
	if code != 0 {
		t.Fatalf("doctor code=%d output=%q", code, output)
	}
	var payload struct {
		ContractVersion int                    `json:"contract_version"`
		Features        map[string]int         `json:"features"`
		Scheduler       agentrun.QueueSnapshot `json:"scheduler"`
	}
	if err := json.Unmarshal([]byte(output), &payload); err != nil {
		t.Fatalf("decode doctor output: %v\n%s", err, output)
	}
	if payload.ContractVersion != agentrun.ContractVersion {
		t.Fatalf("contract_version=%d want=%d", payload.ContractVersion, agentrun.ContractVersion)
	}
	if payload.Features["durable_queue"] != 1 || !payload.Scheduler.Healthy {
		t.Fatalf("doctor payload=%#v", payload)
	}
}

func TestProfilePublicViewUsesCommandAndAdapter(t *testing.T) {
	profile := provider.Config{ID: "custom", Type: provider.TypeCLI, Context: provider.ContextPolicy{SummaryEnabled: true}, CLI: &provider.CLIConfig{
		Driver: "generic", Executor: provider.ExecutorCommand,
		Command: provider.CommandConfig{Binary: "/opt/bin/custom-agent", Model: "model"},
	}}
	view := profilePublicView(profile, map[string]provider.Config{"custom": profile}, 300)
	if view["command"] != "/opt/bin/custom-agent" || view["adapter"] != "generic" {
		t.Fatalf("view=%#v", view)
	}
	if _, exists := view["driver"]; exists {
		t.Fatalf("profile view exposes internal driver: %#v", view)
	}
	if _, exists := view["executor"]; exists {
		t.Fatalf("profile view exposes internal executor: %#v", view)
	}
	contextCapacity, ok := view["context"].(provider.ContextCapacity)
	if !ok || contextCapacity.InputBudgetTokens <= 0 || !contextCapacity.SummaryEnabled {
		t.Fatalf("profile context=%#v", view["context"])
	}

	apiProfile := provider.Config{ID: "api", Type: provider.TypeAPI, API: &provider.APIConfig{
		Protocol: "openai", Model: "model", MaxTokens: 16384,
	}}
	apiView := profilePublicView(apiProfile, map[string]provider.Config{"api": apiProfile}, 300)
	if apiView["max_tokens"] != 16384 {
		t.Fatalf("api view=%#v", apiView)
	}
}

func TestParseRunOptionsLoadsInjectedMemoryFile(t *testing.T) {
	path := filepath.Join(t.TempDir(), "memory.json")
	if err := os.WriteFile(path, []byte(`[{"id":"route","type":"workbench_route","content":"{\"skill\":\"wb-work\"}","source":"workbench"}]`), 0o600); err != nil {
		t.Fatal(err)
	}
	options, providerArgs, err := parseRunOptions(agentrun.RunTurn, []string{
		"--memory-file", path, "cx", "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Profile != "cx" || len(options.InjectedMemory) != 1 ||
		options.InjectedMemory[0].Type != "workbench_route" ||
		!reflect.DeepEqual(providerArgs, []string{"hello"}) {
		t.Fatalf("options=%#v provider_args=%#v", options, providerArgs)
	}
}

func TestParseRunOptionsRejectsUnsafeInjectedMemoryFiles(t *testing.T) {
	dir := t.TempDir()
	unknownField := filepath.Join(dir, "unknown.json")
	if err := os.WriteFile(
		unknownField,
		[]byte(`[{"id":"route","content":"context","unexpected":true}]`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseRunOptions(agentrun.RunTurn, []string{
		"--memory-file", unknownField, "cx", "hello",
	}); err == nil || !strings.Contains(err.Error(), "unknown field") {
		t.Fatalf("unknown field err=%v", err)
	}

	target := filepath.Join(dir, "target.json")
	link := filepath.Join(dir, "link.json")
	if err := os.WriteFile(target, []byte(`[]`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, _, err := parseRunOptions(agentrun.RunTurn, []string{
		"--memory-file", link, "cx", "hello",
	}); err == nil || !strings.Contains(err.Error(), "regular file") {
		t.Fatalf("symlink err=%v", err)
	}
}

func TestParseRunOptionsStopsAtProviderAndPreservesProviderTail(t *testing.T) {
	options, providerArgs, err := parseRunOptions(agentrun.RunTask, []string{
		"--project", "project", "--session-id", "session-1", "cx",
		"--model", "final", "--image", "one.png", "--search", "prompt",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.Profile != "cx" || options.ProjectID != "project" || options.SessionID != "session-1" {
		t.Fatalf("options=%#v", options)
	}
	if !reflect.DeepEqual(providerArgs, []string{"--model", "final", "--image", "one.png", "--search", "prompt"}) {
		t.Fatalf("provider_args=%#v", providerArgs)
	}
}

func TestParseRunOptionsAcceptsQueueTimeout(t *testing.T) {
	options, providerArgs, err := parseRunOptions(agentrun.RunTask, []string{"--queue-timeout-seconds", "45", "cx", "prompt"})
	if err != nil || options.QueueTimeout != 45 {
		t.Fatalf("options=%#v err=%v", options, err)
	}
	if !reflect.DeepEqual(providerArgs, []string{"prompt"}) {
		t.Fatalf("provider_args=%#v", providerArgs)
	}
	if _, _, err := parseRunOptions(agentrun.RunTask, []string{"--queue-timeout-seconds", "-1", "cx", "prompt"}); err == nil {
		t.Fatal("negative queue timeout was accepted")
	}
}

func TestMainCoversLocalControlPlaneCommands(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	for _, dir := range []string{"configs", "resources/skills/review", "resources/tools"} {
		if err := os.MkdirAll(filepath.Join(home, dir), 0o755); err != nil {
			t.Fatal(err)
		}
	}
	writeFixtureCLIProfile(t, home, "native-mock", "ok")
	shellProfile := `{"command":"/bin/sh","args":["-c","printf 'ready\\n'; while IFS= read -r line; do printf 'reply:%s\\n' \"$line\"; done"]}`
	if err := os.WriteFile(filepath.Join(home, "configs", "shell.json"), []byte(shellProfile), 0o644); err != nil {
		t.Fatal(err)
	}
	skill := "name: review\ndescription: review code\nkeywords: [review]\ndefault_profile: native-mock\nprompt_template: 'Review {{input}}'\n"
	if err := os.WriteFile(filepath.Join(home, "resources", "skills", "review", "skill.yaml"), []byte(skill), 0o644); err != nil {
		t.Fatal(err)
	}
	candidates, err := capability.OpenMemory(filepath.Join(home, "memory", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if err := candidates.Write([]capability.MemoryItem{{ID: "candidate-1", Type: "fact", Content: "promote me", Source: "test"}}); err != nil {
		t.Fatal(err)
	}
	historyExport := filepath.Join(home, "history-export.json")

	commands := [][]string{
		{}, {"--help"}, {"--version"},
		{"profile", "list"}, {"profile", "show", "native-mock"}, {"profile", "validate", "native-mock"}, {"profile", "command", "shell"},
		{"profile", "command", "shell", "--mode", "exec"}, {"profile", "exec", "native-mock", "hello"},
		{"system", "doctor"}, {"run", "list", "--limit", "5"}, {"run", "reconcile", "--dry-run"},
		{"tool", "list"}, {"tool", "show", "echo"}, {"tool", "call", "echo", "--args", `{"value":"ok"}`},
		{"skill", "list"}, {"skill", "show", "review"},
		{"memory", "add", "fact-1", "runtime fact", "--source", "test"},
		{"memory", "recall", "runtime"}, {"memory", "list"},
		{"memory", "list", "--state", "candidate"}, {"memory", "promote", "candidate-1"},
		{"memory", "remove", "fact-1"},
		{"session", "run", "--session-id", "session-20260716-165900-cli", "--project", "project", "native-mock", "hello"},
		{"session", "list", "--project", "project"},
		{"session", "show", "--session-id", "session-20260716-165900-cli"},
		{"session", "messages", "--session-id", "session-20260716-165900-cli"},
		{"session", "events", "--session-id", "session-20260716-165900-cli"},
		{"session", "configure", "--session-id", "session-20260716-165900-cli", "--runtime", "cli", "--profile", "shell"},
		{"session", "configure", "--session-id", "session-20260716-165900-cli", "--runtime", "terminal", "--profile", "shell"},
		{"session", "export", "--session-id", "session-20260716-165900-cli", "--output", historyExport},
		{"system", "update", "--dry-run", "--version", "v1.2.3"},
		{"session", "run", "--session-id", "session-20260716-170000-clitest", "--run-id", "turn-20260716-170000-clitest", "native-mock", "hello"},
		{"run", "show", "--run-id", "turn-20260716-170000-clitest"},
		{"run", "logs", "--run-id", "turn-20260716-170000-clitest", "--tail", "5"},
		{"run", "result", "--run-id", "turn-20260716-170000-clitest"},
		{"run", "watch", "--run-id", "turn-20260716-170000-clitest", "--seconds", "1", "--poll-seconds", "0.01"},
		{"native-mock", "hello"},
		{"skill", "run", "review", "--input", "main.go"},
		{"loop", "run", "--loop-id", "loop-20260716-170003-cli", "--input", "hello", "--actions-json", `[{"type":"respond","content":"done"}]`},
		{"loop", "show", "--loop-id", "loop-20260716-170003-cli"},
		{"loop", "list", "--limit", "5"},
		{"loop", "logs", "--loop-id", "loop-20260716-170003-cli", "--tail", "5"},
	}
	for _, command := range commands {
		code, output := captureMain(t, command)
		if code != 0 {
			t.Fatalf("Main(%q) code=%d output=%q", command, code, output)
		}
	}
	if _, err := exec.LookPath("tmux"); err == nil {
		service := agentrun.New(home)
		daemonContext, cancelDaemon := context.WithCancel(context.Background())
		daemonDone := make(chan error, 1)
		server := daemon.NewServer(service.DaemonConfig())
		go func() { daemonDone <- server.Run(daemonContext) }()
		for deadline := time.Now().Add(3 * time.Second); ; {
			if _, statusErr := service.DaemonClient().Status(context.Background()); statusErr == nil {
				break
			}
			if time.Now().After(deadline) {
				t.Fatal("CLI test daemon did not start")
			}
			time.Sleep(20 * time.Millisecond)
		}
		t.Cleanup(func() {
			_, _ = service.SessionStop(context.Background(), "session-20260716-170010-exec")
			cancelDaemon()
			select {
			case <-daemonDone:
			case <-time.After(time.Second):
			}
		})
		tmuxCommands := [][]string{
			{"system", "status"},
			{"session", "open", "--carrier", "tmux", "--session-id", "session-20260716-170010-cli", "--run-id", "session-20260716-170010-exec", "shell"},
			{"session", "show", "--session-id", "session-20260716-170010-cli"},
			{"session", "send", "--session-id", "session-20260716-170010-cli", "hello"},
			{"session", "logs", "--session-id", "session-20260716-170010-cli", "--tail", "10"},
			{"session", "stop", "--session-id", "session-20260716-170010-cli"},
			{"run", "watch", "--run-id", "session-20260716-170010-exec", "--seconds", "1", "--poll-seconds", "0.01"},
		}
		for _, command := range tmuxCommands {
			code, output := captureMain(t, command)
			if code != 0 {
				t.Fatalf("Main(%q) code=%d output=%q", command, code, output)
			}
		}
	}
	if code, output := captureMain(t, []string{"unknown-command"}); code != 1 || !strings.Contains(output, "unknown command") {
		t.Fatalf("unknown code=%d output=%q", code, output)
	}
	invalidCommands := [][]string{
		{"profile"}, {"profile", "unknown"}, {"profile", "show"}, {"profile", "command"}, {"profile", "exec"},
		{"profile", "validate", "-c", "native-mock"}, {"profile", "validate", "native-mock", "--provider", "native"},
		{"profile", "command", "shell", "-c", "other"}, {"profile", "command", "shell", "--mode", "unknown"},
		{"profile", "exec", "missing", "hello"},
		{"run"}, {"run", "unknown"}, {"run", "show"}, {"run", "command"},
		{"run", "command", "shell", "-c", "other", "--", "/bin/true"},
		{"session"}, {"session", "unknown"}, {"session", "list", "extra"}, {"session", "run"}, {"session", "run", "--config", "native-mock", "hello"}, {"session", "run", "--mode", "capture", "native-mock", "hello"}, {"session", "send"},
		{"system"}, {"system", "unknown"}, {"system", "status", "extra"}, {"system", "update", "--unknown"},
		{"skill"}, {"skill", "unknown"}, {"skill", "route"}, {"skill", "run-auto"},
		{"tool"}, {"tool", "unknown"}, {"tool", "schemas"}, {"tool", "open-url"}, {"tool", "describe-external"},
		{"memory"}, {"memory", "unknown"}, {"memory", "write"}, {"memory", "forget"}, {"memory", "candidates"}, {"memory", "sources"}, {"memory", "demo"},
		{"loop"}, {"loop", "unknown"}, {"loop", "start"}, {"loop", "step"}, {"loop", "status"},
		{"help"}, {"version"}, {"task"}, {"turn"}, {"runs"}, {"history"}, {"config"}, {"profiles"},
		{"providers"}, {"upgrade"}, {"capabilities"}, {"tools"}, {"command"}, {"clean"}, {"update"}, {"daemon"}, {"doctor"},
	}
	durable, err := capability.OpenMemory(filepath.Join(home, "memory", "durable.json"))
	if err != nil {
		t.Fatal(err)
	}
	promoted := durable.Recall("promote", "fact", 5)
	if len(promoted) != 1 || promoted[0].PromotedAt == nil {
		t.Fatalf("promoted memory=%#v", durable.Items())
	}
	remaining, err := capability.OpenMemory(filepath.Join(home, "memory", "candidates.json"))
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining.Items()) != 0 {
		t.Fatalf("remaining candidates=%#v", remaining.Items())
	}
	for _, command := range invalidCommands {
		if code, output := captureMain(t, command); code != 1 || !strings.Contains(output, "error:") {
			t.Fatalf("Main(%q) code=%d output=%q", command, code, output)
		}
	}
	if code, output := captureMain(t, []string{"system", "update", "--help"}); code != 0 || !strings.Contains(output, "sn-cli system update") {
		t.Fatalf("update help code=%d output=%q", code, output)
	}
}

func TestSessionRunAndSubmitPersistStructuredCrossProfileTurns(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	if err := os.MkdirAll(filepath.Join(home, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	for _, profile := range []string{"alpha", "beta"} {
		writeFixtureCLIProfile(t, home, profile, profile+" reply")
	}
	sessionID := "session-20260721-160000-cross-profile"
	commands := [][]string{
		{"session", "run", "--session-id", sessionID, "alpha", "first"},
		{"session", "submit", "--session-id", sessionID, "beta", "second"},
	}
	for _, command := range commands {
		if code, output := captureMain(t, command); code != 0 {
			t.Fatalf("Main(%q) code=%d output=%q", command, code, output)
		}
	}
	view, err := agentrun.NewSessionManager(agentrun.New(home)).Store().View(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Session.Retention != agentrun.RetentionStandard || view.Session.RecordMode != agentrun.RecordFull || view.Session.CaptureQuality != agentrun.CaptureStructured {
		t.Fatalf("session policy=%#v", view.Session)
	}
	if len(view.Turns) != 2 || view.Turns[0].Profile != "alpha" || view.Turns[1].Profile != "beta" {
		t.Fatalf("turns=%#v", view.Turns)
	}
	if len(view.Messages) != 4 || view.Messages[0].Role != "user" || view.Messages[1].Role != "assistant" || view.Messages[2].Role != "user" || view.Messages[3].Role != "assistant" {
		t.Fatalf("messages=%#v", view.Messages)
	}
	if len(view.Executions) != 2 {
		t.Fatalf("executions=%#v", view.Executions)
	}
}

func TestSessionRunPassesCLIProviderArgsAndRecordsOnlyFinalPrompt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	argvFile := filepath.Join(home, "provider-argv.txt")
	stdinFile := filepath.Join(home, "provider-stdin.txt")
	script := filepath.Join(home, "codex")
	scriptBody := `#!/bin/sh
printf '%s\n' "$@" > "$SN_TEST_ARGV_FILE"
cat > "$SN_TEST_STDIN_FILE"
printf '{"schema_version":1,"run_id":"%s","outcome":"succeeded","summary":"routing ok","artifacts":[],"errors":[],"validation":{"commands":[],"passed":true}}\n' "$AGENTRUN_RUN_ID" > "$AGENTRUN_RESULT_FILE"
`
	if err := os.WriteFile(script, []byte(scriptBody), 0o755); err != nil {
		t.Fatal(err)
	}
	writeCLIProfile(t, home, "routing", fmt.Sprintf(
		`{"command":%q,"args":["--base","configured"],"env":{"SN_TEST_ARGV_FILE":%q,"SN_TEST_STDIN_FILE":%q}}`,
		script,
		argvFile,
		stdinFile,
	))

	sessionID := "session-20260723-120000-routing"
	runID := "turn-20260723-120001-routing"
	code, output := captureMain(t, []string{
		"session", "run",
		"--session-id", sessionID,
		"--run-id", runID,
		"routing",
		"--skip-git-repo-check",
		"--model", "next",
		"reply OK",
	})
	if code != 0 {
		t.Fatalf("session run code=%d output=%q", code, output)
	}

	argv, err := os.ReadFile(argvFile)
	if err != nil {
		t.Fatal(err)
	}
	wantArgv := "--base\nconfigured\nexec\n--skip-git-repo-check\n--model\nnext\n"
	if string(argv) != wantArgv {
		t.Fatalf("provider argv=%q want=%q", argv, wantArgv)
	}
	stdin, err := os.ReadFile(stdinFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.HasPrefix(string(stdin), "reply OK\n\n## AgentRun result contract") {
		t.Fatalf("provider stdin did not begin with the final prompt and result contract: %q", stdin)
	}

	paths, err := agentrun.RunPaths(agentrun.New(home).RunsDir, agentrun.RunTurn, runID)
	if err != nil {
		t.Fatal(err)
	}
	requestBody, err := os.ReadFile(paths.RequestFile)
	if err != nil {
		t.Fatal(err)
	}
	var request agentrun.Request
	if err := json.Unmarshal(requestBody, &request); err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(request.RawCLIArgs, []string{"--skip-git-repo-check", "--model", "next"}) {
		t.Fatalf("request.raw_cli_args=%#v", request.RawCLIArgs)
	}
	if _, exists := request.ProviderOverrides["model"]; exists {
		t.Fatalf("CLI Provider argument was parsed as a Runtime override: %#v", request.ProviderOverrides)
	}

	view, err := agentrun.NewSessionManager(agentrun.New(home)).Store().View(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) < 1 || view.Messages[0].Role != "user" || view.Messages[0].Content != "reply OK" {
		t.Fatalf("session messages=%#v", view.Messages)
	}
}

func TestSessionRunMapsTypedAPIOptionsWithoutRawCLIArgs(t *testing.T) {
	t.Setenv("SN_TEST_SESSION_API_KEY", "secret")
	requests := make(chan map[string]any, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if request.Header.Get("X-Client") != "runtime-test" {
			t.Errorf("X-Client=%q", request.Header.Get("X-Client"))
		}
		var payload map[string]any
		if err := json.NewDecoder(request.Body).Decode(&payload); err != nil {
			t.Errorf("decode API payload: %v", err)
		}
		requests <- payload
		writer.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(writer, `{"choices":[{"message":{"content":"API OK"}}]}`)
	}))
	defer server.Close()

	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	writeCLIProfile(t, home, "api-routing", fmt.Sprintf(
		`{"protocol":"openai","base_url":%q,"model":"base","api_key":"${SN_TEST_SESSION_API_KEY}","headers":{"X-Client":"runtime-test"},"max_tokens":16384}`,
		server.URL,
	))
	sessionID := "session-20260723-120010-api-routing"
	runID := "turn-20260723-120011-api-routing"
	code, output := captureMain(t, []string{
		"session", "run",
		"--session-id", sessionID,
		"--run-id", runID,
		"api-routing",
		"--model", "next",
		"--max-tokens", "77",
		"--temperature", "0.2",
		"reply API",
	})
	if code != 0 {
		t.Fatalf("session run code=%d output=%q", code, output)
	}
	payload := <-requests
	if payload["model"] != "next" || payload["max_tokens"] != float64(77) || payload["temperature"] != 0.2 {
		t.Fatalf("API payload=%#v", payload)
	}
	messages, _ := payload["messages"].([]any)
	message, _ := messages[0].(map[string]any)
	if message["content"] != "reply API" {
		t.Fatalf("API messages=%#v", messages)
	}

	paths, err := agentrun.RunPaths(agentrun.New(home).RunsDir, agentrun.RunTurn, runID)
	if err != nil {
		t.Fatal(err)
	}
	requestBody, err := os.ReadFile(paths.RequestFile)
	if err != nil {
		t.Fatal(err)
	}
	var stored agentrun.Request
	if err := json.Unmarshal(requestBody, &stored); err != nil {
		t.Fatal(err)
	}
	if len(stored.RawCLIArgs) != 0 || stored.ProviderOverrides["model"] != "next" ||
		stored.ProviderOverrides["max_tokens"] != float64(77) {
		t.Fatalf("stored request=%#v", stored)
	}
	view, err := agentrun.NewSessionManager(agentrun.New(home)).Store().View(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) != 2 || view.Messages[0].Content != "reply API" || view.Messages[1].Content != "API OK" {
		t.Fatalf("session messages=%#v", view.Messages)
	}
}

func TestSessionRunTreatsEmptyPipeAsNoPrompt(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	writeFixtureCLIProfile(t, home, "empty-pipe", "ok")
	withStdin(t, "", func() {
		code, output := captureMain(t, []string{
			"session", "run",
			"--session-id", "session-20260723-120020-empty-pipe",
			"--run-id", "turn-20260723-120021-empty-pipe",
			"empty-pipe", "positional prompt",
		})
		if code != 0 {
			t.Fatalf("session run code=%d output=%q", code, output)
		}
	})
	view, err := agentrun.NewSessionManager(agentrun.New(home)).Store().View("session-20260723-120020-empty-pipe")
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) < 1 || view.Messages[0].Content != "positional prompt" {
		t.Fatalf("session messages=%#v", view.Messages)
	}
}

func captureMain(t *testing.T, args []string) (int, string) {
	t.Helper()
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stdout, stderr := os.Stdout, os.Stderr
	os.Stdout, os.Stderr = writer, writer
	code := Main(args)
	os.Stdout, os.Stderr = stdout, stderr
	if err := writer.Close(); err != nil {
		t.Fatal(err)
	}
	data, err := io.ReadAll(reader)
	if err != nil {
		t.Fatal(err)
	}
	_ = reader.Close()
	return code, string(data)
}

func TestConfigCommandPreviewsArgvWithoutExecutionAndRedactsSecrets(t *testing.T) {
	home := t.TempDir()
	t.Setenv("SN_CLI_HOME", home)
	t.Setenv("MY_API_KEY", "environment-secret")
	if err := os.MkdirAll(filepath.Join(home, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := `{"command":"codex","args":["--api-key","literal-secret","--endpoint=https://user:pass@example.test/v1?access=query-secret","environment-secret"],"model":"preview-model"}`
	if err := os.WriteFile(filepath.Join(home, "configs", "preview.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	apiProfile := `{"protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key":"${UNSET}"}`
	if err := os.WriteFile(filepath.Join(home, "configs", "api.json"), []byte(apiProfile), 0o644); err != nil {
		t.Fatal(err)
	}

	code, output := captureMain(t, []string{"profile", "command", "preview", "--json"})
	if code != 0 {
		t.Fatalf("code=%d output=%q", code, output)
	}
	for _, secret := range []string{"literal-secret", "query-secret", "environment-secret", "user:pass"} {
		if strings.Contains(output, secret) {
			t.Fatalf("preview leaked %q: %s", secret, output)
		}
	}
	for _, expected := range []string{"codex", "--model", "preview-model", "[REDACTED]"} {
		if !strings.Contains(output, expected) {
			t.Fatalf("preview missing %q: %s", expected, output)
		}
	}
	if !strings.Contains(output, `"mode": "direct"`) || strings.Contains(output, `"exec"`) {
		t.Fatalf("direct preview selected managed mode: %s", output)
	}

	code, output = captureMain(t, []string{"profile", "command", "preview", "--mode", "exec", "--json"})
	if code != 0 {
		t.Fatalf("exec preview code=%d output=%q", code, output)
	}
	if !strings.Contains(output, `"mode": "exec"`) || !strings.Contains(output, `"exec"`) {
		t.Fatalf("exec preview missing managed mode: %s", output)
	}

	if code, output := captureMain(t, []string{"profile", "command", "api"}); code != 1 || !strings.Contains(output, "not a CLI profile") {
		t.Fatalf("non-CLI preview code=%d output=%q", code, output)
	}
}

func TestCLIEnvironmentDiagnosticsReportHomesAndAuthConflict(t *testing.T) {
	t.Setenv("CODEX_HOME", "/tmp/codex-aip")
	t.Setenv("ANTHROPIC_API_KEY", "api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "auth-token")

	codexResult := map[string]any{}
	addCLIEnvironmentDiagnostics(provider.Config{CLI: &provider.CLIConfig{
		Driver: "codex", Command: provider.CommandConfig{},
	}}, codexResult)
	codexEnvironment, _ := codexResult["environment"].(map[string]any)
	if codexEnvironment["CODEX_HOME"] != "/tmp/codex-aip" {
		t.Fatalf("codex environment=%#v", codexEnvironment)
	}

	claudeResult := map[string]any{}
	addCLIEnvironmentDiagnostics(provider.Config{CLI: &provider.CLIConfig{
		Driver: "claude", Command: provider.CommandConfig{Env: map[string]string{"CLAUDE_CONFIG_DIR": "/tmp/claude-aip"}},
	}}, claudeResult)
	claudeEnvironment, _ := claudeResult["environment"].(map[string]any)
	if claudeEnvironment["CLAUDE_CONFIG_DIR"] != "/tmp/claude-aip" {
		t.Fatalf("claude environment=%#v", claudeEnvironment)
	}
	if warnings, _ := claudeResult["warnings"].([]string); len(warnings) != 1 {
		t.Fatalf("warnings=%#v", claudeResult["warnings"])
	}
}

func TestCLIEnvironmentDiagnosticsHonorEnvUnset(t *testing.T) {
	t.Setenv("ANTHROPIC_API_KEY", "api-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "auth-token")
	result := map[string]any{}
	addCLIEnvironmentDiagnostics(provider.Config{CLI: &provider.CLIConfig{
		Driver: "claude", Command: provider.CommandConfig{EnvUnset: []string{"ANTHROPIC_AUTH_TOKEN"}},
	}}, result)
	if _, exists := result["warnings"]; exists {
		t.Fatalf("unexpected warnings=%#v", result["warnings"])
	}
	environment, _ := result["environment"].(map[string]any)
	if got := environment["active_auth_env"]; !reflect.DeepEqual(got, []string{"ANTHROPIC_API_KEY"}) {
		t.Fatalf("active_auth_env=%#v", got)
	}
}

func TestParsersRejectRemovedProfileOption(t *testing.T) {
	if _, _, err := parseRunOptions(agentrun.RunTask, []string{"--profile", "cx", "hello"}); err == nil {
		t.Fatal("task run accepted removed --profile option")
	}
}

func TestParseSessionStartOptionsUsesRuntimePrefixAndPassesProviderArgs(t *testing.T) {
	options, err := parseSessionStartOptions([]string{"--carrier", "tmux", "--session-id", "session-1", "cx", "--no-alt-screen", "review repo"})
	if err != nil {
		t.Fatal(err)
	}
	if options.Profile != "cx" || options.Carrier != "tmux" || options.SessionID != "session-1" || options.Prompt != "" || !reflect.DeepEqual(options.RawCLIArgs, []string{"--no-alt-screen", "review repo"}) {
		t.Fatalf("options=%#v", options)
	}
	if _, err := parseSessionStartOptions(nil); err == nil {
		t.Fatal("session open accepted a missing profile")
	}
}

func TestSessionLifecycleRequiresNamedRunID(t *testing.T) {
	if _, err := parseRequiredID([]string{"session-old-positional"}, "--run-id", nil, nil); err == nil {
		t.Fatal("positional run id was accepted")
	}
	got, err := parseRequiredID([]string{"--run-id", "session-1"}, "--run-id", nil, nil)
	if err != nil || got != "session-1" {
		t.Fatalf("run_id=%q err=%v", got, err)
	}
}

func TestParseSessionGCOptionsDefaultsAndValidatesBounds(t *testing.T) {
	hours, limit, apply, err := parseSessionGCOptions(nil)
	if err != nil || hours != 24 || limit != 100 || apply {
		t.Fatalf("defaults hours=%d limit=%d apply=%v err=%v", hours, limit, apply, err)
	}
	hours, limit, apply, err = parseSessionGCOptions([]string{
		"--older-than-hours", "48", "--limit", "3", "--apply",
	})
	if err != nil || hours != 48 || limit != 3 || !apply {
		t.Fatalf("options hours=%d limit=%d apply=%v err=%v", hours, limit, apply, err)
	}
	for _, args := range [][]string{
		{"--older-than-hours", "0"},
		{"--limit", "0"},
		{"--unknown"},
	} {
		if _, _, _, err := parseSessionGCOptions(args); err == nil {
			t.Fatalf("invalid options accepted: %v", args)
		}
	}
}

func TestParseLoopOptionsSupportsContractFields(t *testing.T) {
	options, err := parseLoopOptions([]string{"--input", "work", "--actions-json", `[{"type":"respond","content":"ok"}]`, "--result-schema", "custom.yaml", "--deadline-seconds", "9"})
	if err != nil {
		t.Fatal(err)
	}
	if options.ResultSchema != "custom.yaml" || options.DeadlineSeconds != 9 || len(options.Actions) != 1 {
		t.Fatalf("options=%#v", options)
	}
}

func TestParseLoopOptionsRejectsRemovedAliases(t *testing.T) {
	if _, err := parseLoopOptions([]string{"--actions", `[{"type":"respond"}]`}); err == nil {
		t.Fatal("loop accepted removed --actions option")
	}
	if _, err := parseLoopOptions([]string{"--planner-profile", "cx"}); err == nil {
		t.Fatal("loop accepted removed --planner-profile option")
	}
}

func TestDaemonRestartRejectsActiveRuntimeState(t *testing.T) {
	status := &daemon.Status{
		Processes:    []daemon.ProcessStatus{{ID: "task/active", Alive: true}},
		Dependencies: []daemon.DependencyStatus{{Command: "service", Healthy: true}},
	}
	if err := ensureDaemonRestartSafe(status); err == nil {
		t.Fatal("active daemon state was accepted for restart")
	}
	status.Processes = nil
	status.Dependencies = nil
	status.UptimeSeconds = int64(time.Second.Seconds())
	if err := ensureDaemonRestartSafe(status); err != nil {
		t.Fatal(err)
	}
}

func TestValidateNativeProfileDistinguishesDependencyAndCredentialFailures(t *testing.T) {
	nativeProfile := provider.Config{ID: "native", Type: provider.TypeNative, Native: &provider.NativeConfig{ModelProfile: "api"}}
	result := validateProfile(nativeProfile, map[string]provider.Config{"native": nativeProfile}, false)
	if result["message"] != "native model_profile 不存在: api" {
		t.Fatalf("result=%#v", result)
	}
	apiProfile := provider.Config{ID: "api", Type: provider.TypeAPI, API: &provider.APIConfig{APIKey: "${NATIVE_TEST_KEY}"}}
	result = validateProfile(nativeProfile, map[string]provider.Config{"native": nativeProfile, "api": apiProfile}, false)
	if !strings.Contains(fmt.Sprint(result["message"]), "环境变量未设置: NATIVE_TEST_KEY") {
		t.Fatalf("result=%#v", result)
	}
	t.Setenv("NATIVE_TEST_KEY", "secret")
	result = validateProfile(nativeProfile, map[string]provider.Config{"native": nativeProfile, "api": apiProfile}, false)
	if result["ok"] != true {
		t.Fatalf("result=%#v", result)
	}
}

func TestValidateAPIProfileRejectsProgrammaticPlaintextKey(t *testing.T) {
	profile := provider.Config{ID: "api", Type: provider.TypeAPI, API: &provider.APIConfig{APIKey: "secret"}}
	result := validateProfile(profile, map[string]provider.Config{"api": profile}, false)
	if result["ok"] == true || !strings.Contains(fmt.Sprint(result["message"]), "${ENV_VAR}") {
		t.Fatalf("result=%#v", result)
	}
}

func TestValidateAPIProfileChecksHeaderEnvironment(t *testing.T) {
	t.Setenv("SN_TEST_VALIDATE_API_KEY", "secret")
	const headerEnvironment = "SN_TEST_VALIDATE_HEADER"
	previous, existed := os.LookupEnv(headerEnvironment)
	if err := os.Unsetenv(headerEnvironment); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv(headerEnvironment, previous)
		} else {
			_ = os.Unsetenv(headerEnvironment)
		}
	})
	profile := provider.Config{ID: "api", Type: provider.TypeAPI, API: &provider.APIConfig{
		Protocol: "openai", BaseURL: "https://example.test", Model: "model",
		APIKey:  "${SN_TEST_VALIDATE_API_KEY}",
		Headers: map[string]string{"X-Client": "${" + headerEnvironment + "}"},
	}}
	result := validateProfile(profile, map[string]provider.Config{"api": profile}, false)
	if result["ok"] == true || !strings.Contains(fmt.Sprint(result["message"]), headerEnvironment) {
		t.Fatalf("result=%#v", result)
	}
	if err := os.Setenv(headerEnvironment, "client"); err != nil {
		t.Fatal(err)
	}
	result = validateProfile(profile, map[string]provider.Config{"api": profile}, false)
	if result["ok"] != true {
		t.Fatalf("result=%#v", result)
	}
}

func TestValidateAPIAgentRuntimeReportsCapabilitiesAndMissingMCPCommand(t *testing.T) {
	profile := provider.Config{ID: "api-agent", Type: provider.TypeAPI, API: &provider.APIConfig{
		Mock: true, Runtime: &provider.APIRuntimeConfig{
			Enabled: true, AutoRouteSkills: true, Skills: []string{"review"}, Memory: &provider.APIMemoryConfig{Enabled: true},
			MCPServers: []provider.MCPServerConfig{{Name: "fixture", Command: "/definitely/missing/mcp-server"}},
		},
	}}
	result := validateProfile(profile, map[string]provider.Config{"api-agent": profile}, false)
	if result["ok"] == true || !strings.Contains(fmt.Sprint(result["message"]), "MCP server fixture 命令不可用") {
		t.Fatalf("result=%#v", result)
	}
	profile.API.Runtime.MCPServers[0].Command = "/bin/sh"
	result = validateProfile(profile, map[string]provider.Config{"api-agent": profile}, false)
	if result["ok"] != true || result["agent_runtime"] != true || result["memory_enabled"] != true {
		t.Fatalf("result=%#v", result)
	}
}
