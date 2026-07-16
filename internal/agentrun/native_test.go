package agentrun

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"agent-runtime/internal/provider"
)

func TestNativeProviderProducesStandardArtifacts(t *testing.T) {
	root := nativeTestRoot(t, `{
  "type":"native",
  "native":{"system_prompt":"test","max_rounds":1,"mock":{"responses":["native ok"],"done_after":1}}
}`)
	service := New(root)
	run, err := service.Run(context.Background(), RunOptions{Profile: "native-test", Prompt: "hello", ExecutionMode: ModeManaged})
	if err != nil || run.State != StateDone {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	result, err := service.ReadResult(RunTask, run.RunID)
	if err != nil || result.Summary != "native ok" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if _, err := os.Stat(filepath.Join(run.RunDir, "native-snapshot.json")); err != nil {
		t.Fatalf("native snapshot: %v", err)
	}
	status, _ := service.Status(RunTask, run.RunID)
	if status.Provider != provider.TypeNative || status.ProviderStatus["native_state"] != "completed" {
		t.Fatalf("status=%#v", status)
	}
}

func TestNativeProviderPatchResumeUsesSameRun(t *testing.T) {
	root := nativeTestRoot(t, `{
  "type":"native",
  "native":{"system_prompt":"test","max_rounds":1,"llm_timeout_seconds":0.03,"mock":{"responses":["recovered"],"done_after":1}}
}`)
	service := New(root)
	run, err := service.Run(context.Background(), RunOptions{Profile: "native-test", Prompt: "timeout", ExecutionMode: ModeManaged})
	if err != nil || run.State != StateBlocked {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	patch := provider.NativePatch{Operation: "append", Messages: []provider.NativeMessage{{Role: "user", Content: "recovered context"}}}
	resumed, err := service.ResumeNative(context.Background(), RunTask, run.RunID, &patch)
	if err != nil || resumed.State != StateDone || resumed.RunID != run.RunID {
		t.Fatalf("resumed=%#v err=%v", resumed, err)
	}
	result, _ := service.ReadResult(RunTask, run.RunID)
	if result.Summary != "recovered" {
		t.Fatalf("result=%#v", result)
	}
}

func TestAPIAgentProviderPersistsAndPatchResumesLocalContext(t *testing.T) {
	root := apiAgentTestRoot(t, `{
  "type":"api",
  "api":{
    "protocol":"openai","base_url":"https://example.test/v1","model":"mock","api_key_env":"UNUSED_API_AGENT_KEY","mock":true,
    "runtime":{"enabled":true,"max_rounds":2,"llm_timeout_seconds":1}
  }
}`)
	service := New(root)
	run, err := service.Run(context.Background(), RunOptions{Profile: "api-agent-test", Prompt: "fail first", ExecutionMode: ModeManaged})
	if err != nil || run.State != StateBlocked {
		t.Fatalf("run=%#v err=%v", run, err)
	}
	contextFile := filepath.Join(run.RunDir, "context-snapshot.json")
	if _, err := os.Stat(contextFile); err != nil {
		t.Fatalf("context snapshot: %v", err)
	}
	patch := provider.NativePatch{Operation: "append", Messages: []provider.NativeMessage{{Role: "user", Content: "continue"}}}
	resumed, err := service.ResumeNative(context.Background(), RunTask, run.RunID, &patch)
	if err != nil || resumed.State != StateDone || resumed.RunID != run.RunID {
		t.Fatalf("resumed=%#v err=%v", resumed, err)
	}
	result, err := service.ReadResult(RunTask, run.RunID)
	if err != nil || !strings.Contains(result.Summary, "agent runtime") {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if len(result.Artifacts) != 2 || result.Artifacts[1]["type"] != "context_snapshot" {
		t.Fatalf("artifacts=%#v", result.Artifacts)
	}
	status, err := service.Status(RunTask, run.RunID)
	if err != nil || status.ProviderStatus["kind"] != "api-agent" || status.ProviderStatus["context_file"] != contextFile {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestNativeProviderCancelStopsInflightRun(t *testing.T) {
	root := nativeTestRoot(t, `{
  "type":"native",
  "native":{"system_prompt":"test","max_rounds":1,"llm_timeout_seconds":3,"mock":{"responses":["late"],"latency_milliseconds":1000,"done_after":1}}
}`)
	service := New(root)
	type outcome struct {
		run RunSummary
		err error
	}
	done := make(chan outcome, 1)
	go func() {
		run, err := service.Run(context.Background(), RunOptions{RunID: "task-20260714-000000-nativecancel", Profile: "native-test", Prompt: "hello", ExecutionMode: ModeManaged})
		done <- outcome{run: run, err: err}
	}()
	waitRunState(t, service, RunTask, "task-20260714-000000-nativecancel", StateRunning)
	cancelled, err := service.Cancel(RunTask, "task-20260714-000000-nativecancel")
	if err != nil || cancelled.State != StateCancelled {
		t.Fatalf("cancelled=%#v err=%v", cancelled, err)
	}
	finished := <-done
	if finished.run.State != StateCancelled || !errors.Is(finished.err, context.Canceled) {
		t.Fatalf("finished=%#v err=%v", finished.run, finished.err)
	}
}

func nativeTestRoot(t *testing.T, profile string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "native-test.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func apiAgentTestRoot(t *testing.T, profile string) string {
	t.Helper()
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "api-agent-test.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	return root
}

func waitRunState(t *testing.T, service *Service, runType, runID, state string) Status {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		status, err := service.Status(runType, runID)
		if err == nil && status.State == state {
			if nativeState, _ := status.ProviderStatus["native_state"].(string); nativeState == "waiting_llm" {
				return status
			}
		}
		time.Sleep(10 * time.Millisecond)
	}
	status, err := service.Status(runType, runID)
	t.Fatalf("state=%s not reached: status=%#v err=%v", state, status, err)
	return Status{}
}
