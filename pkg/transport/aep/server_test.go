package aep

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	runtimecommand "github.com/yy003x/sn-code/pkg/command"
	"github.com/yy003x/sn-code/pkg/model"
	"github.com/yy003x/sn-code/pkg/profile"
	"github.com/yy003x/sn-code/pkg/provider/openai"
	runtime "github.com/yy003x/sn-code/pkg/run"
	"github.com/yy003x/sn-code/pkg/session"
	sqlitestore "github.com/yy003x/sn-code/pkg/store/sqlite"
)

// testEnvironment 组装真实组件：sqlite Run store、文件 Session store、真实
// Profile catalog 与 fake codex 可执行脚本；不用任何 mock。PATH 收敛到测试
// bin 目录与系统目录，保证执行器探测结果确定。
type testEnvironment struct {
	temp         string
	server       *Server
	runs         *runtime.Service
	workerCtx    context.Context
	workerCancel context.CancelFunc
	workerDone   chan struct{}
	stream       *messageStream
	stdin        *io.PipeWriter
	serveDone    chan error
	cancelServe  context.CancelFunc
	diagMutex    sync.Mutex
	diags        []string
}

func (environment *testEnvironment) recordDiag(line string) {
	environment.diagMutex.Lock()
	defer environment.diagMutex.Unlock()
	environment.diags = append(environment.diags, line)
}

func (environment *testEnvironment) takeDiags() []string {
	environment.diagMutex.Lock()
	defer environment.diagMutex.Unlock()
	return append([]string(nil), environment.diags...)
}

type testEnvironmentOptions struct {
	profiles func(temp, binDir string) map[string]runtimecommand.Profile
	output   io.Writer
}

func newTestEnvironment(
	t *testing.T,
	scriptName, scriptBody string,
	options ...testEnvironmentOptions,
) *testEnvironment {
	t.Helper()
	temp := t.TempDir()
	binDir := filepath.Join(temp, "bin")
	if err := os.Mkdir(binDir, 0o700); err != nil {
		t.Fatal(err)
	}
	script := filepath.Join(binDir, scriptName)
	if err := os.WriteFile(script, []byte(scriptBody), 0o700); err != nil {
		t.Fatal(err)
	}
	stateDir := filepath.Join(temp, "state")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		t.Fatal(err)
	}
	t.Setenv("PATH", binDir+string(os.PathListSeparator)+"/usr/bin:/bin")
	mismatchCommand := "claude"
	if scriptName == "claude" {
		mismatchCommand = "codex"
	}
	var option testEnvironmentOptions
	if len(options) != 0 {
		option = options[0]
	}
	commandProfiles := map[string]runtimecommand.Profile{
		"cx":       {Command: script},
		"cxpath":   {Command: scriptName},
		"mismatch": {Command: mismatchCommand},
	}
	if option.profiles != nil {
		commandProfiles = option.profiles(temp, binDir)
	}
	commands, err := runtimecommand.NewCatalog(commandProfiles)
	if err != nil {
		t.Fatal(err)
	}
	models, err := model.NewCatalog(map[string]model.Profile{})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewCatalog(commands, models)
	if err != nil {
		t.Fatal(err)
	}
	generator, err := model.NewService(
		models,
		map[model.DriverName]model.Driver{
			model.DriverOpenAIChatCompletions: openai.New(nil),
		},
		model.ServiceOptions{},
	)
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := session.NewStore(
		filepath.Join(temp, "sessions"), stateDir,
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
		LogsDir: filepath.Join(temp, "logs"),
	})
	if err != nil {
		t.Fatal(err)
	}
	runStore, err := sqlitestore.Open(
		filepath.Join(stateDir, "runtime.db"),
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runStore.Close() })
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: runStore,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindSession: &runtime.SessionExecutor{
				Profiles: profiles, Sessions: sessions,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	environment := &testEnvironment{
		temp: temp, runs: runs,
		workerDone: make(chan struct{}),
		serveDone:  make(chan error, 1),
	}
	server, err := NewServer(ServerConfig{
		Profiles: profiles, Runs: runs,
		Server:         ServerInfo{Name: "sn-runtime-test", Version: "0.0.0-test"},
		InvocationBase: temp,
		ShutdownGrace:  10 * time.Second,
		// 诊断保留到测试失败时输出，避免 settle/取消路径的偶发问题无证据。
		Diagnostics: func(format string, args ...any) {
			environment.recordDiag(fmt.Sprintf(format, args...))
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	environment.server = server
	t.Cleanup(func() {
		if t.Failed() {
			for _, line := range environment.takeDiags() {
				t.Logf("server diag: %s", line)
			}
		}
	})
	environment.workerCtx, environment.workerCancel =
		context.WithCancel(context.Background())
	go func() {
		defer close(environment.workerDone)
		_ = runs.Worker(environment.workerCtx, "aep-test-worker", 5*time.Millisecond)
	}()
	stdinReader, stdinWriter := io.Pipe()
	environment.stdin = stdinWriter
	stdoutReader, stdoutWriter := io.Pipe()
	output := option.output
	if output == nil {
		output = stdoutWriter
		environment.stream = newMessageStream(stdoutReader)
	} else {
		_ = stdoutReader.Close()
		_ = stdoutWriter.Close()
	}
	serveContext, cancelServe := context.WithCancel(context.Background())
	environment.cancelServe = cancelServe
	t.Cleanup(func() {
		cancelServe()
		_ = stdinWriter.Close()
		_ = stdoutWriter.Close()
		environment.workerCancel()
		select {
		case <-environment.workerDone:
		case <-time.After(5 * time.Second):
			t.Errorf("worker did not stop")
		}
	})
	go func() {
		environment.serveDone <- server.Serve(
			serveContext, stdinReader, output,
		)
		_ = stdoutWriter.Close()
	}()
	// 每个测试自行完成握手；TestMethodsRequireInitialize 需要未初始化状态。
	return environment
}

func (environment *testEnvironment) initialize(t *testing.T) {
	t.Helper()
	environment.request(t, "init", "initialize", map[string]any{})
	response := environment.waitFor(t, func(value map[string]any) bool {
		return value["id"] == "init"
	})
	result, ok := response["result"].(map[string]any)
	if !ok || result["protocol"] != ProtocolName ||
		result["version"] != ProtocolVersion {
		t.Fatalf("initialize result=%#v", response)
	}
}

// messageStream 顺序解析服务端 stdout 的每一行；任何非 JSON 行都让后续
// 等待失败（stdout 纯净性断言）。
type messageStream struct {
	mutex    sync.Mutex
	messages []map[string]any
	failure  error
}

func newMessageStream(reader io.Reader) *messageStream {
	stream := &messageStream{}
	go func() {
		scanner := bufio.NewScanner(reader)
		for scanner.Scan() {
			var value map[string]any
			if err := json.Unmarshal(scanner.Bytes(), &value); err != nil {
				stream.mutex.Lock()
				stream.failure = fmt.Errorf(
					"non-protocol stdout line %q: %w", scanner.Text(), err,
				)
				stream.mutex.Unlock()
				return
			}
			stream.mutex.Lock()
			stream.messages = append(stream.messages, value)
			stream.mutex.Unlock()
		}
		if err := scanner.Err(); err != nil {
			stream.mutex.Lock()
			stream.failure = fmt.Errorf("read stdout: %w", err)
			stream.mutex.Unlock()
		}
	}()
	return stream
}

func (stream *messageStream) waitFor(
	t *testing.T,
	match func(map[string]any) bool,
) map[string]any {
	t.Helper()
	deadline := time.Now().Add(20 * time.Second)
	for {
		stream.mutex.Lock()
		if stream.failure != nil {
			stream.mutex.Unlock()
			t.Fatalf("stdout purity violated: %v", stream.failure)
		}
		for _, value := range stream.messages {
			if match(value) {
				stream.mutex.Unlock()
				return value
			}
		}
		count := len(stream.messages)
		stream.mutex.Unlock()
		if time.Now().After(deadline) {
			t.Fatalf("timed out waiting for message; received=%d", count)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (environment *testEnvironment) request(
	t *testing.T,
	id string,
	method string,
	params map[string]any,
) {
	t.Helper()
	frame := map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method,
	}
	if params != nil {
		frame["params"] = params
	}
	line, err := json.Marshal(frame)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := environment.stdin.Write(append(line, '\n')); err != nil {
		t.Fatal(err)
	}
}

func (environment *testEnvironment) waitFor(
	t *testing.T,
	match func(map[string]any) bool,
) map[string]any {
	t.Helper()
	return environment.stream.waitFor(t, match)
}

func (environment *testEnvironment) waitForResult(
	t *testing.T,
	id string,
) map[string]any {
	t.Helper()
	response := environment.waitFor(t, func(value map[string]any) bool {
		return value["id"] == id
	})
	if failure, exists := response["error"]; exists {
		t.Fatalf("request %s failed: %v", id, failure)
	}
	result, ok := response["result"].(map[string]any)
	if !ok {
		t.Fatalf("request %s returned non-object result: %#v", id, response)
	}
	return result
}

func (environment *testEnvironment) waitForError(
	t *testing.T,
	id any,
) map[string]any {
	t.Helper()
	response := environment.waitFor(t, func(value map[string]any) bool {
		return value["id"] == id
	})
	failure, ok := response["error"].(map[string]any)
	if !ok {
		t.Fatalf("request %v succeeded unexpectedly: %#v", id, response)
	}
	return failure
}

const okCodexScript = `#!/bin/sh
printf '%s\n' \
  '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}' \
  '{"type":"turn.completed"}'
`

const sleepingCodexScript = `#!/bin/sh
sleep 60
`

const okPiScript = `#!/bin/sh
printf '%s\n' \
  '{"type":"session","version":3,"id":"s1","cwd":"/tmp"}' \
  '{"type":"agent_start"}' \
  '{"type":"turn_start"}' \
  '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"ok"}}' \
  '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"ok"}],"stopReason":"stop"}}' \
  '{"type":"turn_end"}' \
  '{"type":"agent_end"}' \
  '{"type":"agent_settled","aborted":false}'
`

func TestMethodsRequireInitialize(t *testing.T) {
	environment := newTestEnvironment(t, "codex", okCodexScript)
	environment.request(t, "g1", "run.get", map[string]any{
		"runId": "run_00000000000000000000000000000000",
	})
	failure := environment.waitForError(t, "g1")
	if failure["code"].(float64) != float64(codeServerNotInitialized) {
		t.Fatalf("error=%v", failure)
	}
}

func TestInitializeMayOnlyBeCalledOnce(t *testing.T) {
	environment := newTestEnvironment(t, "codex", okCodexScript)
	environment.initialize(t)
	environment.request(t, "init2", "initialize", map[string]any{})
	failure := environment.waitForError(t, "init2")
	if failure["code"].(float64) != float64(codeInvalidRequest) {
		t.Fatalf("error=%v", failure)
	}
}

func TestUnknownMethodAndParseErrors(t *testing.T) {
	environment := newTestEnvironment(t, "codex", okCodexScript)
	environment.initialize(t)
	environment.request(t, "m1", "session.list", nil)
	failure := environment.waitForError(t, "m1")
	if failure["code"].(float64) != float64(codeMethodNotFound) {
		t.Fatalf("error=%v", failure)
	}
	if _, err := environment.stdin.Write([]byte("not-json\n")); err != nil {
		t.Fatal(err)
	}
	parseFailure := environment.waitForError(t, nil)
	if parseFailure["code"].(float64) != float64(codeParseError) {
		t.Fatalf("error=%v", parseFailure)
	}
	// 通知不产生任何响应；后续请求继续工作。
	if _, err := environment.stdin.Write([]byte(
		`{"jsonrpc":"2.0","method":"ignored.notification"}` + "\n",
	)); err != nil {
		t.Fatal(err)
	}
	environment.request(t, "m2", "executor.list", map[string]any{})
	environment.waitForResult(t, "m2")
}

func TestExecutorDiscoveryReportsTruthfulAvailability(t *testing.T) {
	environment := newTestEnvironment(t, "codex", okCodexScript)
	environment.initialize(t)
	environment.request(t, "list", "executor.list", map[string]any{})
	result := environment.waitForResult(t, "list")
	executors, ok := result["executors"].([]any)
	if !ok || len(executors) != 4 {
		t.Fatalf("executors=%#v", result)
	}
	byID := make(map[string]map[string]any)
	for _, value := range executors {
		descriptor := value.(map[string]any)
		byID[descriptor["id"].(string)] = descriptor
	}
	if !byID["codex"]["available"].(bool) {
		t.Fatalf("codex descriptor=%#v", byID["codex"])
	}
	if byID["claude-code"]["available"].(bool) {
		t.Fatalf("claude-code descriptor=%#v", byID["claude-code"])
	}
	if byID["pi"]["available"].(bool) {
		t.Fatalf("pi descriptor=%#v", byID["pi"])
	}
	if byID["acp"]["available"].(bool) {
		t.Fatalf("acp descriptor=%#v", byID["acp"])
	}
	// sessionResume 如实上报：已登记执行器支持 resumeFromRunId（同 Session
	// 新 Turn），未登记的 acp 为 false。
	for _, id := range []string{"codex", "claude-code", "pi"} {
		capabilities, _ := byID[id]["capabilities"].(map[string]any)
		if !capabilities["sessionResume"].(bool) {
			t.Fatalf("%s capabilities=%#v", id, byID[id]["capabilities"])
		}
	}
	if acpCapabilities, _ := byID["acp"]["capabilities"].(map[string]any); acpCapabilities["sessionResume"].(bool) {
		t.Fatalf("acp capabilities=%#v", byID["acp"]["capabilities"])
	}
	environment.request(t, "get1", "executor.get", map[string]any{
		"executor": "codex",
	})
	descriptor := environment.waitForResult(t, "get1")
	if descriptor["available"] != true {
		t.Fatalf("descriptor=%#v", descriptor)
	}
	environment.request(t, "get2", "executor.get", map[string]any{
		"executor": "nope",
	})
	failure := environment.waitForError(t, "get2")
	if failure["code"].(float64) != float64(codeNotFound) {
		t.Fatalf("error=%v", failure)
	}
}

func TestDiscoveryAndRunStartUseEffectiveCommandEnvironment(t *testing.T) {
	for _, mode := range []string{"profile_path", "relative_path", "relative_command"} {
		t.Run(mode, func(t *testing.T) {
			environment := newTestEnvironment(t, "codex", okCodexScript, testEnvironmentOptions{
				profiles: func(temp, binDir string) map[string]runtimecommand.Profile {
					path := binDir
					command := "codex"
					if mode == "relative_path" {
						path = "bin"
					}
					if mode == "relative_command" {
						command = "./bin/codex"
					}
					return map[string]runtimecommand.Profile{
						"cx": {Command: command, CWD: temp, Env: map[string]*string{"PATH": &path}},
					}
				},
			})
			// 可执行文件只存在于 Profile 的有效 PATH/cwd，不在进程 PATH。
			t.Setenv("PATH", "/usr/bin:/bin")
			environment.initialize(t)
			environment.request(t, "list", "executor.list", map[string]any{})
			list := environment.waitForResult(t, "list")
			found := false
			for _, item := range list["executors"].([]any) {
				descriptor := item.(map[string]any)
				if descriptor["id"] == "codex" {
					found = descriptor["available"] == true
				}
			}
			if !found {
				t.Fatalf("executor.list=%#v", list)
			}
			environment.request(t, "get", "executor.get", map[string]any{"executor": "codex"})
			if descriptor := environment.waitForResult(t, "get"); descriptor["available"] != true {
				t.Fatalf("executor.get=%#v", descriptor)
			}
			cwd := t.TempDir()
			if mode != "profile_path" {
				if err := os.Mkdir(filepath.Join(cwd, "bin"), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.WriteFile(filepath.Join(cwd, "bin", "codex"), []byte(okCodexScript), 0o700); err != nil {
					t.Fatal(err)
				}
				if err := os.Remove(filepath.Join(environment.temp, "bin", "codex")); err != nil {
					t.Fatal(err)
				}
			}
			environment.request(t, "start", "run.start", map[string]any{
				"executor": "codex", "profile": "cx",
				"input": map[string]any{"prompt": "say ok", "cwd": cwd},
			})
			start := environment.waitForResult(t, "start")
			runID := start["runId"].(string)
			environment.waitFor(t, func(value map[string]any) bool {
				params, _ := value["params"].(map[string]any)
				return value["method"] == "run.event" && params != nil &&
					params["runId"] == runID && params["type"] == "run.completed"
			})
			environment.request(t, "result", "run.result", map[string]any{"runId": runID})
			if result := environment.waitForResult(t, "result"); result["summary"] != "ok" {
				t.Fatalf("run.result=%#v", result)
			}
		})
	}
}

func TestRunStartRejectsInvalidRequests(t *testing.T) {
	environment := newTestEnvironment(t, "codex", okCodexScript)
	environment.initialize(t)
	cwd := environment.temp
	for _, test := range []struct {
		id     string
		params map[string]any
		code   int32
	}{
		{
			"acp",
			map[string]any{"executor": "acp", "profile": "cx",
				"input": map[string]any{"prompt": "hi", "cwd": cwd}},
			codeInvalidParams,
		},
		{
			"unknown_executor",
			map[string]any{"executor": "grok", "profile": "cx",
				"input": map[string]any{"prompt": "hi", "cwd": cwd}},
			codeInvalidParams,
		},
		{
			"pi_profile_mismatch",
			map[string]any{"executor": "pi", "profile": "cx",
				"input": map[string]any{"prompt": "hi", "cwd": cwd}},
			codeInvalidParams,
		},
		{
			"profile_mismatch",
			map[string]any{"executor": "codex", "profile": "mismatch",
				"input": map[string]any{"prompt": "hi", "cwd": cwd}},
			codeInvalidParams,
		},
		{
			"unknown_profile",
			map[string]any{"executor": "codex", "profile": "ghost",
				"input": map[string]any{"prompt": "hi", "cwd": cwd}},
			codeInvalidParams,
		},
		{
			"timeout_ms_negative",
			map[string]any{"executor": "codex", "profile": "cx",
				"input": map[string]any{"prompt": "hi", "cwd": cwd,
					"options": map[string]any{"timeoutMs": -1}}},
			codeInvalidParams,
		},
		{
			"permissions_top_level_rejected",
			map[string]any{"executor": "codex", "profile": "cx",
				"input": map[string]any{"prompt": "hi", "cwd": cwd,
					"permissions": map[string]any{"mode": "read_only"}}},
			codeInvalidParams,
		},
		{
			"permissions_mode_invalid",
			map[string]any{"executor": "codex", "profile": "cx",
				"input": map[string]any{"prompt": "hi", "cwd": cwd,
					"options": map[string]any{"permissions": map[string]any{
						"mode": "full_access"}}}},
			codeInvalidParams,
		},
		{
			"approval_policy_rejected",
			map[string]any{"executor": "codex", "profile": "cx",
				"input": map[string]any{"prompt": "hi", "cwd": cwd,
					"options": map[string]any{"permissions": map[string]any{
						"mode": "read_only", "approvalPolicy": "auto"}}}},
			codeInvalidParams,
		},
		{
			"resume_from_missing_run",
			map[string]any{"executor": "codex", "profile": "cx",
				"input": map[string]any{"prompt": "hi", "cwd": cwd,
					"resumeFromRunId": "run_does_not_exist"}},
			codeInvalidParams,
		},
		{
			"empty_prompt",
			map[string]any{"executor": "codex", "profile": "cx",
				"input": map[string]any{"prompt": "  ", "cwd": cwd}},
			codeInvalidParams,
		},
		{
			"relative_cwd",
			map[string]any{"executor": "codex", "profile": "cx",
				"input": map[string]any{"prompt": "hi", "cwd": "relative/dir"}},
			codeInvalidParams,
		},
		{
			"missing_cwd",
			map[string]any{"executor": "codex", "profile": "cx",
				"input": map[string]any{"prompt": "hi",
					"cwd": filepath.Join(cwd, "does-not-exist")}},
			codeInvalidParams,
		},
		{
			"bad_session_id",
			map[string]any{"executor": "codex", "profile": "cx",
				"input": map[string]any{"prompt": "hi", "cwd": cwd,
					"sessionId": "not-a-session-id"}},
			codeInvalidParams,
		},
	} {
		environment.request(t, test.id, "run.start", test.params)
		failure := environment.waitForError(t, test.id)
		if failure["code"].(float64) != float64(test.code) {
			t.Fatalf("%s error=%v want code=%d", test.id, failure, test.code)
		}
		if message, _ := failure["message"].(string); message == "" {
			t.Fatalf("%s error has empty message: %v", test.id, failure)
		}
	}
}

func TestRunLifecycleSucceedsEndToEnd(t *testing.T) {
	environment := newTestEnvironment(t, "codex", okCodexScript)
	environment.initialize(t)
	environment.request(t, "start", "run.start", map[string]any{
		"executor": "codex", "profile": "cx",
		"input": map[string]any{
			"prompt": "say ok", "cwd": environment.temp,
			"options": map[string]any{"model": "gpt-test"},
		},
	})
	start := environment.waitForResult(t, "start")
	runID, ok := start["runId"].(string)
	if !ok {
		t.Fatalf("start=%#v", start)
	}
	// 提交与响应之间 worker 可能已领取 Run；pending/running 都是真值。
	if status := start["status"]; status != statusPending && status != statusRunning {
		t.Fatalf("start=%#v", start)
	}
	completed := environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "run.completed"
	})
	completedParams := completed["params"].(map[string]any)
	if completedParams["runId"] != runID ||
		completedParams["protocol"] != ProtocolName ||
		completedParams["version"] != ProtocolVersion {
		t.Fatalf("run.completed envelope=%#v", completedParams)
	}
	data := completedParams["data"].(map[string]any)
	if data["status"] != statusSucceeded {
		t.Fatalf("run.completed data=%#v", data)
	}
	if completedParams["eventId"] !=
		runID+":"+fmt.Sprint(completedParams["seq"]) {
		t.Fatalf("eventId=%v", completedParams["eventId"])
	}
	// 流式归一化：assistant 消息在 run.completed 之前以 message.completed
	// 推送，source 标注执行器与原生记录类型。
	message := environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "message.completed" && params["runId"] == runID
	})
	messageParams := message["params"].(map[string]any)
	if data, _ := messageParams["data"].(map[string]any); data["text"] != "ok" {
		t.Fatalf("message.completed data=%#v", messageParams["data"])
	}
	if source, _ := messageParams["source"].(map[string]any); source["executor"] != "codex" ||
		source["nativeEventType"] != "item.completed" {
		t.Fatalf("message.completed source=%#v", messageParams["source"])
	}
	if messageParams["seq"].(float64) >= completedParams["seq"].(float64) {
		t.Fatalf("message.completed seq=%v not before run.completed seq=%v",
			messageParams["seq"], completedParams["seq"])
	}
	environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "run.status_changed"
	})

	environment.request(t, "get", "run.get", map[string]any{"runId": runID})
	view := environment.waitForResult(t, "get")
	if view["status"] != statusSucceeded || view["executor"] != "codex" {
		t.Fatalf("run.get=%#v", view)
	}

	environment.request(t, "result", "run.result", map[string]any{"runId": runID})
	final := environment.waitForResult(t, "result")
	if final["status"] != statusSucceeded || final["summary"] != "ok" {
		t.Fatalf("run.result=%#v", final)
	}
	if final["completeness"] != "complete" || final["finishedAt"] == "" {
		t.Fatalf("run.result=%#v", final)
	}
	if artifacts, ok := final["artifacts"].([]any); !ok || len(artifacts) != 0 {
		t.Fatalf("artifacts=%#v", final["artifacts"])
	}

	environment.request(t, "events1", "run.events", map[string]any{"runId": runID})
	events := environment.waitForResult(t, "events1")["events"].([]any)
	if len(events) < 2 {
		t.Fatalf("events=%#v", events)
	}
	previous := float64(0)
	for _, value := range events {
		envelope := value.(map[string]any)
		if envelope["seq"].(float64) <= previous {
			t.Fatalf("events not monotonic: %#v", events)
		}
		previous = envelope["seq"].(float64)
	}
	environment.request(t, "events2", "run.events", map[string]any{
		"runId": runID, "afterSeq": 1,
	})
	backfill := environment.waitForResult(t, "events2")["events"].([]any)
	if len(backfill) != len(events)-1 {
		t.Fatalf("backfill=%#v events=%#v", backfill, events)
	}
	environment.request(t, "events3", "run.events", map[string]any{
		"runId": runID, "limit": 10000,
	})
	if failure := environment.waitForError(t, "events3"); failure["code"].(float64) !=
		float64(codeInvalidParams) {
		t.Fatalf("error=%v", failure)
	}

	environment.request(t, "shutdown", "shutdown", map[string]any{})
	shutdown := environment.waitForResult(t, "shutdown")
	if len(shutdown) != 0 {
		t.Fatalf("shutdown=%#v", shutdown)
	}
	select {
	case err := <-environment.serveDone:
		if err != nil {
			t.Fatalf("serve error=%v", err)
		}
	case <-time.After(15 * time.Second):
		t.Fatal("serve did not return after shutdown")
	}
}

func TestRunLifecycleWithPiExecutor(t *testing.T) {
	environment := newTestEnvironment(t, "pi", okPiScript)
	environment.initialize(t)
	environment.request(t, "start", "run.start", map[string]any{
		"executor": "pi", "profile": "cx",
		"input": map[string]any{"prompt": "say ok", "cwd": environment.temp},
	})
	start := environment.waitForResult(t, "start")
	runID := start["runId"].(string)
	completed := environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "run.completed" && params["runId"] == runID
	})
	completedSeq := completed["params"].(map[string]any)["seq"].(float64)
	// token delta 先于权威 assistant 消息，两者都先于终态。
	delta := environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "message.delta" && params["runId"] == runID
	})
	deltaParams := delta["params"].(map[string]any)
	if data, _ := deltaParams["data"].(map[string]any); data["text"] != "ok" {
		t.Fatalf("message.delta data=%#v", deltaParams["data"])
	}
	message := environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "message.completed" && params["runId"] == runID
	})
	messageParams := message["params"].(map[string]any)
	if data, _ := messageParams["data"].(map[string]any); data["text"] != "ok" {
		t.Fatalf("message.completed data=%#v", messageParams["data"])
	}
	if source, _ := messageParams["source"].(map[string]any); source["executor"] != "pi" ||
		source["nativeEventType"] != "message_end" {
		t.Fatalf("message.completed source=%#v", messageParams["source"])
	}
	if deltaParams["seq"].(float64) >= messageParams["seq"].(float64) ||
		messageParams["seq"].(float64) >= completedSeq {
		t.Fatalf("seq order delta=%v message=%v completed=%v",
			deltaParams["seq"], messageParams["seq"], completedSeq)
	}
	environment.request(t, "result", "run.result", map[string]any{"runId": runID})
	if final := environment.waitForResult(t, "result"); final["summary"] != "ok" {
		t.Fatalf("run.result=%#v", final)
	}
}

// sandboxCodexScript 先断言请求注入的 --sandbox read-only 到达执行器
// argv，再输出合法 codex 终态流；缺 flag 时以 stderr + 非零退出失败。
const sandboxCodexScript = `#!/bin/sh
case " $* " in
  *" --sandbox read-only "*) ;;
  *) printf 'missing --sandbox read-only\\n' >&2; exit 1 ;;
esac
printf '%s\n' \
  '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}' \
  '{"type":"turn.completed"}'
`

// TestRunTimeoutSettlesAsFailed 验证 timeoutMs：到期取消执行进程，Run 按
// timeout 失败收口（error code "timeout"），而非用户取消。
func TestRunTimeoutSettlesAsFailed(t *testing.T) {
	environment := newTestEnvironment(t, "codex", sleepingCodexScript)
	environment.initialize(t)
	environment.request(t, "start", "run.start", map[string]any{
		"executor": "codex", "profile": "cx",
		"input": map[string]any{"prompt": "say ok", "cwd": environment.temp,
			"options": map[string]any{"timeoutMs": 300}},
	})
	start := environment.waitForResult(t, "start")
	runID := start["runId"].(string)
	failed := environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "run.failed" && params["runId"] == runID
	})
	failureData := failed["params"].(map[string]any)["data"].(map[string]any)
	if failureData["status"] != statusFailed {
		t.Fatalf("run.failed data=%#v", failureData)
	}
	environment.request(t, "result", "run.result", map[string]any{"runId": runID})
	final := environment.waitForResult(t, "result")
	if final["status"] != statusFailed {
		t.Fatalf("run.result=%#v", final)
	}
	if failure, _ := final["error"].(map[string]any); failure["code"] != "timeout" {
		t.Fatalf("run.result error=%#v", final["error"])
	}
}

// TestRunPermissionsInjectAdapterFlags 验证 permissions.mode=read_only 全链
// 路注入 codex --sandbox read-only（脚本断言 argv 后才输出终态流）。
func TestRunPermissionsInjectAdapterFlags(t *testing.T) {
	environment := newTestEnvironment(t, "codex", sandboxCodexScript)
	environment.initialize(t)
	environment.request(t, "start", "run.start", map[string]any{
		"executor": "codex", "profile": "cx",
		"input": map[string]any{"prompt": "say ok", "cwd": environment.temp,
			"options": map[string]any{"permissions": map[string]any{
				"mode": "read_only",
			}}},
	})
	start := environment.waitForResult(t, "start")
	runID := start["runId"].(string)
	environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "run.completed" && params["runId"] == runID
	})
	environment.request(t, "result", "run.result", map[string]any{"runId": runID})
	if final := environment.waitForResult(t, "result"); final["summary"] != "ok" {
		t.Fatalf("run.result=%#v", final)
	}
}

// TestRunPermissionsUnsupportedModeFailsRun 验证无等价 flag 的档位在执行面
// 显式失败（pi 无 workspace_write 等价），而不是静默放宽权限。
func TestRunPermissionsUnsupportedModeFailsRun(t *testing.T) {
	environment := newTestEnvironment(t, "pi", okPiScript)
	environment.initialize(t)
	environment.request(t, "start", "run.start", map[string]any{
		"executor": "pi", "profile": "cx",
		"input": map[string]any{"prompt": "say ok", "cwd": environment.temp,
			"options": map[string]any{"permissions": map[string]any{
				"mode": "workspace_write",
			}}},
	})
	start := environment.waitForResult(t, "start")
	runID := start["runId"].(string)
	environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "run.failed" && params["runId"] == runID
	})
	environment.request(t, "result", "run.result", map[string]any{"runId": runID})
	final := environment.waitForResult(t, "result")
	if final["status"] != statusFailed {
		t.Fatalf("run.result=%#v", final)
	}
	if failure, _ := final["error"].(map[string]any); failure["code"] != "invalid_request" {
		t.Fatalf("run.result error=%#v", final["error"])
	}
}

// TestRunResumeReusesSession 验证 resumeFromRunId：新 Run 复用源 Run 的
// managed Session（同 sessionId），作为同一 Session 的新 Turn 完成。
func TestRunResumeReusesSession(t *testing.T) {
	environment := newTestEnvironment(t, "codex", okCodexScript)
	environment.initialize(t)
	environment.request(t, "start", "run.start", map[string]any{
		"executor": "codex", "profile": "cx",
		"input": map[string]any{"prompt": "say ok", "cwd": environment.temp},
	})
	first := environment.waitForResult(t, "start")
	firstRun := first["runId"].(string)
	firstSession := first["sessionId"].(string)
	environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "run.completed" && params["runId"] == firstRun
	})
	environment.request(t, "resume", "run.start", map[string]any{
		"executor": "codex", "profile": "cx",
		"input": map[string]any{"prompt": "say ok again", "cwd": environment.temp,
			"resumeFromRunId": firstRun},
	})
	second := environment.waitForResult(t, "resume")
	if second["sessionId"].(string) != firstSession {
		t.Fatalf("resume session=%v want %v", second["sessionId"], firstSession)
	}
	secondRun := second["runId"].(string)
	if secondRun == firstRun {
		t.Fatalf("resume reused run id %v", secondRun)
	}
	environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "run.completed" && params["runId"] == secondRun
	})
	environment.request(t, "result", "run.result", map[string]any{"runId": secondRun})
	if final := environment.waitForResult(t, "result"); final["summary"] != "ok" {
		t.Fatalf("run.result=%#v", final)
	}
}

func TestRunStartReportsExecutorNotInstalled(t *testing.T) {
	// Profile 指向 bare `pi` 但可执行文件不存在：probe 失败投影
	// -32001 EXECUTOR_NOT_INSTALLED，而不是 invalid params。
	environment := newTestEnvironment(t, "codex", okCodexScript, testEnvironmentOptions{
		profiles: func(temp, binDir string) map[string]runtimecommand.Profile {
			return map[string]runtimecommand.Profile{"cx": {Command: "pi"}}
		},
	})
	environment.initialize(t)
	environment.request(t, "start", "run.start", map[string]any{
		"executor": "pi", "profile": "cx",
		"input": map[string]any{"prompt": "hi", "cwd": environment.temp},
	})
	failure := environment.waitForError(t, "start")
	if failure["code"].(float64) != float64(codeExecutorUnavailable) {
		t.Fatalf("error=%v", failure)
	}
	if data, _ := failure["data"].(map[string]any); data["reason"] != "EXECUTOR_NOT_INSTALLED" {
		t.Fatalf("error data=%#v", failure["data"])
	}
}

func TestRunCancelSettlesAsCancelled(t *testing.T) {
	environment := newTestEnvironment(t, "codex", sleepingCodexScript)
	environment.initialize(t)
	environment.request(t, "start", "run.start", map[string]any{
		"executor": "codex", "profile": "cx",
		"input": map[string]any{"prompt": "sleep", "cwd": environment.temp},
	})
	start := environment.waitForResult(t, "start")
	runID := start["runId"].(string)
	environment.request(t, "cancel", "run.cancel", map[string]any{"runId": runID})
	cancelResult := environment.waitForResult(t, "cancel")
	if status, _ := cancelResult["status"].(string); status != statusCancelled &&
		status != statusCancelling {
		t.Fatalf("cancel=%#v", cancelResult)
	}
	environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "run.cancelled"
	})
	deadline := time.Now().Add(20 * time.Second)
	for attempt := 1; time.Now().Before(deadline); attempt++ {
		id := fmt.Sprintf("result-%d", attempt)
		environment.request(t, id, "run.result", map[string]any{"runId": runID})
		response := environment.waitFor(t, func(value map[string]any) bool {
			return value["id"] == id
		})
		result, ok := response["result"].(map[string]any)
		if !ok {
			continue
		}
		if result["status"] != statusCancelled {
			t.Fatalf("run.result=%#v", result)
		}
		if _, has := result["exitCode"]; has {
			t.Fatalf("signal-killed run must not report exitCode: %#v", result)
		}
		return
	}
	t.Fatal("run did not settle as cancelled")
}

func TestOutputBackpressureDoesNotPreventRunSettlement(t *testing.T) {
	// 干净 EOF 必须等最后一帧处理完才 settle；响应写仍阻塞时由取消或
	// 输出故障驱动收口，避免重新引入 EOF 与 Run 登记的竞态。
	for _, shutdown := range []string{"cancel", "output_failure"} {
		t.Run(shutdown, func(t *testing.T) {
			stdoutReader, stdoutWriter, err := os.Pipe()
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() {
				_ = stdoutReader.Close()
				_ = stdoutWriter.Close()
			})
			environment := newTestEnvironment(t, "codex", sleepingCodexScript,
				testEnvironmentOptions{output: stdoutWriter})
			reader := bufio.NewReader(stdoutReader)
			readResult := func(id string) map[string]any {
				t.Helper()
				if err := stdoutReader.SetReadDeadline(time.Now().Add(10 * time.Second)); err != nil {
					t.Fatal(err)
				}
				for {
					line, err := reader.ReadBytes('\n')
					if err != nil {
						t.Fatal(err)
					}
					var value map[string]any
					if err := json.Unmarshal(line, &value); err != nil {
						t.Fatal(err)
					}
					if value["id"] == id {
						result, ok := value["result"].(map[string]any)
						if !ok {
							t.Fatalf("response=%#v", value)
						}
						return result
					}
				}
			}
			environment.request(t, "init", "initialize", map[string]any{})
			readResult("init")
			environment.request(t, "start", "run.start", map[string]any{
				"executor": "codex", "profile": "cx",
				"input": map[string]any{"prompt": "sleep", "cwd": environment.temp},
			})
			runID := readResult("start")["runId"].(string)
			deadline := time.Now().Add(10 * time.Second)
			for {
				record, err := environment.runs.Get(context.Background(), runID)
				if err != nil {
					t.Fatal(err)
				}
				if record.State == runtime.StateRunning {
					break
				}
				if time.Now().After(deadline) {
					t.Fatalf("run never started: %#v", record)
				}
				time.Sleep(5 * time.Millisecond)
			}
			// 不再消费 stdout；大响应无法完整放入真实 OS pipe。
			environment.request(t, strings.Repeat("x", 768*1024), "executor.list", map[string]any{})
			deadline = time.Now().Add(5 * time.Second)
			for len(environment.server.writer.lock) == 0 {
				if time.Now().After(deadline) {
					t.Fatal("output write did not start")
				}
				time.Sleep(time.Millisecond)
			}
			switch shutdown {
			case "cancel":
				environment.cancelServe()
			case "eof":
				if err := environment.stdin.Close(); err != nil {
					t.Fatal(err)
				}
			case "output_failure":
				if err := stdoutReader.Close(); err != nil {
					t.Fatal(err)
				}
			}
			select {
			case err := <-environment.serveDone:
				if shutdown == "output_failure" && err == nil {
					t.Fatal("output failure was not returned")
				}
				if shutdown != "output_failure" && err != nil {
					t.Fatalf("serve error=%v", err)
				}
			case <-time.After(15 * time.Second):
				t.Fatal("backpressure prevented shutdown")
			}
			record, err := environment.runs.Get(context.Background(), runID)
			if err != nil || record.State != runtime.StateCancelled {
				t.Fatalf("run did not settle: state=%s err=%v", record.State, err)
			}
		})
	}
}

func TestRunGetUnknownRunIsNotFound(t *testing.T) {
	environment := newTestEnvironment(t, "codex", okCodexScript)
	environment.initialize(t)
	environment.request(t, "get", "run.get", map[string]any{
		"runId": "run_0000000000000000000000000000dead",
	})
	failure := environment.waitForError(t, "get")
	if failure["code"].(float64) != float64(codeNotFound) {
		t.Fatalf("error=%v", failure)
	}
	// 语法非法的 runId 是客户端输入错误，必须是 invalid params 而不是
	// internal error。
	environment.request(t, "bad", "run.get", map[string]any{
		"runId": "not-a-run-id",
	})
	malformed := environment.waitForError(t, "bad")
	if malformed["code"].(float64) != float64(codeInvalidParams) {
		t.Fatalf("error=%v", malformed)
	}
}

// TestRunResultAvailableIncludesNeedsReconciliation 锁定 run.result 的可出
// 结果判定：needs_reconciliation 按 D4 投影为 failed 并允许取 result。
func TestRunResultAvailableIncludesNeedsReconciliation(t *testing.T) {
	if !runResultAvailable(runtime.StateNeedsReconciliation) {
		t.Fatal("needs_reconciliation must expose a projected result")
	}
	for _, state := range []runtime.State{
		runtime.StateQueued, runtime.StateRunning, runtime.StatePaused,
	} {
		if runResultAvailable(state) {
			t.Fatalf("state %s must not expose results", state)
		}
	}
	for _, state := range []runtime.State{
		runtime.StateCompleted, runtime.StateFailed, runtime.StateCancelled,
	} {
		if !runResultAvailable(state) {
			t.Fatalf("terminal state %s must expose results", state)
		}
	}
}

// TestEOFSettleStillEmitsTerminalEvent 验证干净 EOF（半关连接：客户端不再
// 发送但仍读取 stdout）后 writer 保持存活：graceful settle 取消 Run 并推送
// run.cancelled 终态事件，而不是提前关闭输出丢掉事件。
func TestEOFSettleStillEmitsTerminalEvent(t *testing.T) {
	environment := newTestEnvironment(t, "codex", sleepingCodexScript)
	environment.initialize(t)
	environment.request(t, "start", "run.start", map[string]any{
		"executor": "codex", "profile": "cx",
		"input": map[string]any{"prompt": "sleep", "cwd": environment.temp},
	})
	start := environment.waitForResult(t, "start")
	runID := start["runId"].(string)
	if err := environment.stdin.Close(); err != nil {
		t.Fatal(err)
	}
	cancelled := environment.waitFor(t, func(value map[string]any) bool {
		params, _ := value["params"].(map[string]any)
		return value["method"] == "run.event" && params != nil &&
			params["type"] == "run.cancelled" && params["runId"] == runID
	})
	data, _ := cancelled["params"].(map[string]any)["data"].(map[string]any)
	if data["status"] != statusCancelled {
		t.Fatalf("run.cancelled data=%#v", data)
	}
	select {
	case err := <-environment.serveDone:
		if err != nil {
			t.Fatalf("Serve returned %v", err)
		}
	case <-time.After(20 * time.Second):
		t.Fatal("Serve did not return after EOF settle")
	}
}
