package agentrun

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/provider"
)

func TestManagedRunRequiresAndAcceptsResultContract(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "provider.sh")
	writeExecutable(t, script, `#!/bin/sh
printf '{"schema_version":1,"run_id":"%s","outcome":"succeeded","summary":"managed ok","artifacts":[],"errors":[],"validation":{"commands":[],"passed":true}}\n' "$AGENTRUN_RUN_ID" > "$AGENTRUN_RESULT_FILE"
printf 'stdout is only a log\n'
`)
	writeProfile(t, root, "managed", script)
	service := New(root)
	result, err := service.Run(context.Background(), RunOptions{Profile: "managed", Prompt: "do it", ExecutionMode: ModeManaged})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.State != StateDone {
		t.Fatalf("state=%q", result.State)
	}
	contract, err := service.store.ReadResult(mustPaths(t, service, result))
	if err != nil || contract.Summary != "managed ok" {
		t.Fatalf("result=%#v err=%v", contract, err)
	}
}

func TestManagedOutputLogRemainsAppendOnlyWithoutDuplicatingStream(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "output.log")
	prepared := provider.PreparedRequest{CLI: &provider.CLIRequest{Argv: []string{"provider", "managed"}}}
	if err := initializeOutputLog(path, prepared); err != nil {
		t.Fatal(err)
	}
	sink := &runProviderSink{paths: Paths{OutputLog: path}}
	if err := sink.Stdout([]byte("first\n")); err != nil {
		t.Fatal(err)
	}
	before, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := finalizeOutputLog(path, sink, provider.Result{Stdout: "first\n", Stderr: "last\n", ExitCode: 0}); err != nil {
		t.Fatal(err)
	}
	after, err := os.Stat(path)
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	text := string(data)
	if after.Size() < before.Size() || strings.Count(text, "[stdout] first") != 1 || strings.Count(text, "[stderr] last") != 1 || !strings.Contains(text, "[runtime] returncode=0") {
		t.Fatalf("before=%d after=%d output=%q", before.Size(), after.Size(), text)
	}
}

func TestOrdinaryRunDoesNotCreateLogicalSession(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "capture.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf 'ok\\n'\n")
	writeProfile(t, root, "capture", script)
	service := New(root)
	run, err := service.Run(context.Background(), RunOptions{Profile: "capture", Prompt: "hello", ExecutionMode: ModeCapture})
	if err != nil || run.SessionID != "" || run.TurnID != "" {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	values, err := NewSessionManager(service).Store().List(SessionFilter{})
	if err != nil || len(values) != 0 {
		t.Fatalf("implicit sessions=%#v err=%v", values, err)
	}
}

func TestExistingSessionRejectsTurnRetentionOverride(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "capture.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf 'ok\\n'\n")
	writeProfile(t, root, "capture", script)
	service := New(root)
	session, err := NewSessionManager(service).EnsureSession("session-20260717-210000-retention", service.DefaultProject, root, "retention",
		RecordDecision{RecordMode: RecordFull, Retention: RetentionStandard, CaptureQuality: CaptureStructured})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.Run(context.Background(), RunOptions{Profile: "capture", Prompt: "hello", ExecutionMode: ModeCapture,
		SessionID: session.SessionID, Retention: RetentionPinned}); err == nil || !strings.Contains(err.Error(), "Session-level") {
		t.Fatalf("err=%v", err)
	}
}

func TestSessionContextCrossesProfilesAndFreezesTurnProfile(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "capture.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf 'cli answer\\n'\n")
	writeProfile(t, root, "capture", script)
	contextScript := filepath.Join(root, "context.sh")
	writeExecutable(t, contextScript, "#!/bin/sh\ncat\n")
	writeProfile(t, root, "alternate", contextScript)
	service := New(root)
	first, err := service.Run(context.Background(), RunOptions{RunID: "task-20260717-140000-cli", Profile: "capture",
		Prompt: "first question", ExecutionMode: ModeCapture, CreateSession: true})
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.Run(context.Background(), RunOptions{RunID: "turn-20260717-140001-alternate", RunType: RunTurn,
		Profile: "alternate", Prompt: "second question", SessionID: first.SessionID, ExecutionMode: ModeCapture})
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(second.FinalText, "first question") || !strings.Contains(second.FinalText, "cli answer") || !strings.Contains(second.FinalText, "second question") {
		t.Fatalf("context=%s", second.FinalText)
	}
	view, err := NewSessionManager(service).Store().View(first.SessionID)
	if err != nil || len(view.Turns) != 2 {
		t.Fatalf("view=%#v err=%v", view, err)
	}
	if view.Turns[0].Profile != "capture" || view.Turns[1].Profile != "alternate" || view.Turns[1].Provider != provider.TypeCLI || view.Turns[1].ResultRef == nil {
		t.Fatalf("turn identity=%#v", view.Turns)
	}
}

func TestManagedPromptContainsValidBuiltinResultExample(t *testing.T) {
	runID := "task-20260715-120000-contract"
	prompt := managedPrompt("完成任务", Request{RunID: runID}, Paths{ResultFile: "/tmp/result.json"})
	start := strings.Index(prompt, "{")
	end := strings.LastIndex(prompt, "}")
	if start < 0 || end < start {
		t.Fatalf("managed prompt missing JSON example: %s", prompt)
	}
	var raw map[string]any
	if err := json.Unmarshal([]byte(prompt[start:end+1]), &raw); err != nil {
		t.Fatalf("example is not valid JSON: %v", err)
	}
	if !validBuiltinResult(raw, runID) {
		t.Fatalf("example does not satisfy builtin result contract: %#v", raw)
	}
	for _, value := range []string{"succeeded、failed、blocked、partial、cancelled", "不能把数字或布尔值写成字符串", "不要包含 Markdown code fence"} {
		if !strings.Contains(prompt, value) {
			t.Fatalf("managed prompt missing %q", value)
		}
	}
}

func TestCaptureSynthesizesResultAndRunIDIsIdempotent(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "capture.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf 'capture ok\\n'\n")
	writeProfile(t, root, "capture", script)
	service := New(root)
	options := RunOptions{RunID: "task-20260713-120000-fixed", Profile: "capture", Prompt: "hello", ExecutionMode: ModeCapture}
	first, err := service.Run(context.Background(), options)
	if err != nil || first.State != StateDone {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := service.Run(context.Background(), options)
	if err != nil || !second.Idempotent {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	contract, _ := service.store.ReadResult(mustPaths(t, service, first))
	if contract.Summary != "capture ok" {
		t.Fatalf("summary=%q", contract.Summary)
	}
}

func TestManagedRunInjectsSessionContextPathsAndPersistsManifest(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "context.sh")
	writeExecutable(t, script, `#!/bin/sh
printf '%s|%s|%s|%s\n' "$AGENTRUN_SESSION_ID" "$AGENTRUN_TURN_ID" "$SN_RUNTIME_CONTEXT_MANIFEST" "$SN_RUNTIME_MEMORY_CANDIDATES_FILE"
`)
	writeProfile(t, root, "context", script)
	service := New(root)
	sessionID := "session-20260717-130000-context"
	result, err := service.Run(context.Background(), RunOptions{
		RunID: "turn-20260717-130000-context", RunType: RunTurn, SessionID: sessionID,
		Profile: "context", Prompt: "hello", ExecutionMode: ModeCapture, CreateSession: true,
	})
	if err != nil || result.State != StateDone {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	turnID := turnIDForRun("turn-20260717-130000-context")
	if !strings.Contains(result.FinalText, sessionID+"|"+turnID+"|") || !strings.Contains(result.FinalText, "context-manifest.json") || !strings.Contains(result.FinalText, "memory/candidates.json") {
		t.Fatalf("runtime environment output=%q", result.FinalText)
	}
	view, err := NewSessionManager(service).Store().View(sessionID)
	if err != nil || len(view.Turns) != 1 || len(view.Attempts) != 1 {
		t.Fatalf("view=%#v err=%v", view, err)
	}
	var manifest ContextManifest
	if err := readJSON(view.Turns[0].ContextManifest, &manifest); err != nil {
		t.Fatal(err)
	}
	if manifest.ConfigDigest == "" || manifest.PolicyDigest == "" || manifest.MessageDigest == "" || manifest.SessionID != sessionID {
		t.Fatalf("manifest=%#v", manifest)
	}
	if view.Session.Runtime != "cli" || view.Session.Profile != "context" {
		t.Fatalf("session runtime=%#v", view.Session)
	}
	if view.Attempts[0].ResultRef == nil || view.Attempts[0].ResultRef.ResultFile != result.ResultFile {
		t.Fatalf("attempt=%#v result=%#v", view.Attempts[0], result)
	}
	if view.Turns[0].TurnID == view.Attempts[0].RunID || view.Attempts[0].ExecutionID == view.Attempts[0].RunID || view.Turns[0].TurnID == view.Attempts[0].ExecutionID {
		t.Fatalf("turn, run and execution IDs must be independent: turn=%s run=%s execution=%s", view.Turns[0].TurnID, view.Attempts[0].RunID, view.Attempts[0].ExecutionID)
	}
}

func TestForceRunDoesNotReuseStaleResult(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "force.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf 'first result\\n'\n")
	writeProfile(t, root, "force", script)
	service := New(root)
	runID := "task-20260713-120000-force"
	first, err := service.Run(context.Background(), RunOptions{RunID: runID, Profile: "force", Prompt: "first", ExecutionMode: ModeCapture})
	if err != nil || first.State != StateDone {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	writeExecutable(t, script, "#!/bin/sh\nprintf 'no new result\\n'\n")
	forced, err := service.Run(context.Background(), RunOptions{RunID: runID, Profile: "force", Prompt: "second", ExecutionMode: ModeManaged, Force: true})
	if err == nil || forced.FailureReason != "result_missing" {
		t.Fatalf("forced=%#v err=%v", forced, err)
	}
}

func TestManagedRunFailsWithoutRequiredResult(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "missing.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf 'no result\\n'\n")
	writeProfile(t, root, "missing", script)
	service := New(root)
	result, err := service.Run(context.Background(), RunOptions{Profile: "missing", Prompt: "hello"})
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if result.State != StateFailed || result.FailureReason != "result_missing" {
		t.Fatalf("result=%#v", result)
	}
}

func TestAPIProfileSynthesizesResult(t *testing.T) {
	root := t.TempDir()
	t.Setenv("TEST_API_KEY", "test-key")
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"choices":[{"message":{"content":"api ok"}}]}`))
	}))
	defer server.Close()
	profile := fmt.Sprintf(`{"protocol":"openai","base_url":%q,"model":"test-model","api_key":"${TEST_API_KEY}"}`, server.URL+"/v1")
	if err := os.WriteFile(filepath.Join(root, "configs", "mock.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	service.HTTPClient = server.Client()
	result, err := service.Run(context.Background(), RunOptions{Profile: "mock", Prompt: "hello"})
	if err != nil || result.State != StateDone {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestInvalidExternalResultIsSchemaInvalid(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "invalid.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf '{\"schema_version\":1}' > \"$AGENTRUN_RESULT_FILE\"\n")
	writeProfile(t, root, "invalid", script)
	service := New(root)
	result, err := service.Run(context.Background(), RunOptions{Profile: "invalid", Prompt: "hello"})
	if err == nil || result.FailureReason != "schema_invalid" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestRunTimeoutIsClassified(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "slow.sh")
	writeExecutable(t, script, "#!/bin/sh\nsleep 5\n")
	writeProfile(t, root, "slow", script)
	service := New(root)
	started := time.Now()
	result, err := service.Run(context.Background(), RunOptions{Profile: "slow", Prompt: "hello", DeadlineSeconds: 1})
	if err == nil || result.FailureReason != "timeout" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatalf("timeout took too long: %s", time.Since(started))
	}
}

func TestRunUsesRuntimeDefaultDeadline(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "slow-default.sh")
	writeExecutable(t, script, "#!/bin/sh\nsleep 5\n")
	writeProfile(t, root, "slow-default", script)
	if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), []byte("default_deadline_seconds: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	result, err := service.Run(context.Background(), RunOptions{Profile: "slow-default", Prompt: "hello"})
	if err == nil || result.FailureReason != "timeout" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	request, err := service.store.ReadRequest(mustPaths(t, service, result))
	if err != nil || request.DeadlineSeconds != 1 {
		t.Fatalf("request=%#v err=%v", request, err)
	}
}

func TestEnsureProviderStatusInitializesNilMap(t *testing.T) {
	status := Status{}
	values := ensureProviderStatus(&status)
	values["alive"] = false
	if status.ProviderStatus == nil {
		t.Fatal("provider status map was not initialized")
	}
	if alive, ok := status.ProviderStatus["alive"].(bool); !ok || alive {
		t.Fatalf("provider status=%#v", status.ProviderStatus)
	}
}

func TestRunRejectsConflictingIdempotentRequest(t *testing.T) {
	root := t.TempDir()
	writeManagedFixtureProfile(t, root, "native", 0)
	service := New(root)
	runID := "task-20260716-160000-idempotent"
	first, err := service.Run(context.Background(), RunOptions{RunID: runID, Profile: "native", Prompt: "first"})
	if err != nil {
		t.Fatal(err)
	}
	reused, err := service.Run(context.Background(), RunOptions{RunID: runID, Profile: "native", Prompt: "first"})
	if err != nil || !reused.Idempotent || reused.RunID != first.RunID {
		t.Fatalf("reused=%#v err=%v", reused, err)
	}
	if _, err := service.Run(context.Background(), RunOptions{RunID: runID, Profile: "native", Prompt: "different"}); err == nil || !strings.Contains(err.Error(), "idempotency conflict") {
		t.Fatalf("conflicting request err=%v", err)
	}
}

func TestRunHonorsMaxConcurrencyAcrossServices(t *testing.T) {
	root := t.TempDir()
	writeManagedFixtureProfile(t, root, "slow", 350)
	if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), []byte("default_project: _default\ndefault_profile: slow\nmax_concurrency: 1\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	firstService, secondService := New(root), New(root)
	done := make(chan error, 1)
	go func() {
		_, err := firstService.Run(context.Background(), RunOptions{RunID: "task-20260716-160001-first", Profile: "slow", Prompt: "first"})
		done <- err
	}()
	waitForState(t, firstService, RunTask, "task-20260716-160001-first", StateRunning)
	if _, err := secondService.Run(context.Background(), RunOptions{RunID: "task-20260716-160002-second", Profile: "slow", Prompt: "second"}); err == nil || !strings.Contains(err.Error(), "max_concurrency") {
		t.Fatalf("second run err=%v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
	if _, err := secondService.Run(context.Background(), RunOptions{RunID: "task-20260716-160003-third", Profile: "slow", Prompt: "third"}); err != nil {
		t.Fatalf("slot was not released: %v", err)
	}
}

func TestStoreConcurrentEventsHaveUniqueSequence(t *testing.T) {
	root := t.TempDir()
	paths, err := RunPaths(filepath.Join(root, "runs"), RunTask, "task-20260716-160004-events")
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	request := Request{RunID: "task-20260716-160004-events", RunType: RunTask}
	const count = 40
	var wait sync.WaitGroup
	for index := 0; index < count; index++ {
		wait.Add(1)
		go func(index int) {
			defer wait.Done()
			store := Store{}
			if err := store.Event(paths, request, "test", map[string]any{"index": index}); err != nil {
				t.Errorf("Event: %v", err)
			}
		}(index)
	}
	wait.Wait()
	events, err := (&Store{}).ReadEvents(paths)
	if err != nil || len(events) != count {
		t.Fatalf("events=%d err=%v", len(events), err)
	}
	for index, event := range events {
		if event.Sequence != index+1 {
			t.Fatalf("event[%d].sequence=%d", index, event.Sequence)
		}
	}
}

func TestStoreDoesNotOverwriteCancelledStatus(t *testing.T) {
	paths, err := RunPaths(t.TempDir(), RunTask, "task-20260716-160005-status")
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	request := Request{RunID: "task-20260716-160005-status", RunType: RunTask}
	store := Store{}
	if _, err := store.WriteStatus(paths, request, StateCancelled, "interrupted", "cancelled", nil); err != nil {
		t.Fatal(err)
	}
	status, err := store.WriteStatus(paths, request, StateDone, "", "late completion", nil)
	if err != nil || status.State != StateCancelled {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func writeManagedFixtureProfile(t *testing.T, root, id string, latencyMilliseconds int) {
	t.Helper()
	script := filepath.Join(root, id+"-managed.sh")
	delay := float64(latencyMilliseconds) / 1000
	body := fmt.Sprintf(`#!/bin/sh
sleep %.3f
printf '{"schema_version":1,"run_id":"%%s","outcome":"succeeded","summary":"ok","artifacts":[],"errors":[],"validation":{"commands":[],"passed":true}}\n' "$AGENTRUN_RUN_ID" > "$AGENTRUN_RESULT_FILE"
`, delay)
	writeExecutable(t, script, body)
	writeProfile(t, root, id, script)
}

func waitForState(t *testing.T, service *Service, runType, runID, state string) {
	t.Helper()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		status, err := service.Status(runType, runID)
		if err == nil && status.State == state {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatalf("run %s did not reach state %s", runID, state)
}

func writeProfile(t *testing.T, root, id, script string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := map[string]any{"command": script}
	data, _ := json.Marshal(profile)
	if err := os.WriteFile(filepath.Join(root, "configs", id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustPaths(t *testing.T, service *Service, summary RunSummary) Paths {
	t.Helper()
	paths, err := RunPaths(service.RunsDir, summary.RunType, summary.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
