package agentrun

import (
	"context"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestPersistentLoopStartStepStatusAndCancel(t *testing.T) {
	service := New(t.TempDir())
	status, err := service.LoopStart(LoopStartOptions{Input: "question", MaxSteps: 3, Actions: []Action{
		{Type: "tool", Name: "echo", Arguments: map[string]any{"value": "ok"}},
		{Type: "respond", Content: "done"},
	}})
	if err != nil {
		t.Fatal(err)
	}
	status, err = service.LoopStep(context.Background(), status.LoopID)
	if err != nil || status.Step != 1 || status.Outcome != "" {
		t.Fatalf("step1=%#v err=%v", status, err)
	}
	status, err = service.LoopStep(context.Background(), status.LoopID)
	if err != nil || status.Outcome != LoopOutcomeDone || status.Output != "done" {
		t.Fatalf("step2=%#v err=%v", status, err)
	}
	loaded, err := service.LoopStatus(status.LoopID)
	if err != nil || loaded.Outcome != LoopOutcomeDone {
		t.Fatalf("loaded=%#v err=%v", loaded, err)
	}
	var result Result
	if err := readJSON(service.loopPaths(status.LoopID).ResultFile, &result); err != nil {
		t.Fatal(err)
	}
	if result.RunID != status.LoopID || result.Outcome != OutcomeSucceeded || result.Summary != "done" {
		t.Fatalf("result=%#v", result)
	}

	other, err := service.LoopStart(LoopStartOptions{Input: "cancel", Actions: []Action{{Type: "respond", Content: "unused"}}})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := service.LoopCancel(other.LoopID)
	if err != nil || cancelled.Outcome != OutcomeCancelled {
		t.Fatalf("cancelled=%#v err=%v", cancelled, err)
	}
}

func TestLoopRunAgentRequiresAllowedCapability(t *testing.T) {
	service := New(t.TempDir())
	status, err := service.LoopStart(LoopStartOptions{
		Input: "run", Capabilities: []string{"agent.run"}, Forbidden: []string{"agent.run"},
		Actions: []Action{{Type: "run_agent", Request: map[string]any{"profile": "x", "prompt": "p"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	status, err = service.LoopStep(context.Background(), status.LoopID)
	if err != nil || status.Outcome != OutcomeBlocked || !strings.Contains(status.Message, "blocked") {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestLoopUsesConfiguredCapabilityRegistry(t *testing.T) {
	root := t.TempDir()
	toolsDir := filepath.Join(root, "resources", "tools")
	if err := os.MkdirAll(toolsDir, 0o755); err != nil {
		t.Fatal(err)
	}
	tool := "name: local-loop\ndescription: local loop tool\nkind: external\nschema:\n  type: object\n"
	if err := os.WriteFile(filepath.Join(toolsDir, "local-loop.tool.yaml"), []byte(tool), 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	status, err := service.LoopStart(LoopStartOptions{Input: "tool", Actions: []Action{{Type: "tool", Name: "local-loop", Arguments: map[string]any{}}}})
	if err != nil {
		t.Fatal(err)
	}
	status, err = service.LoopStep(context.Background(), status.LoopID)
	if err != nil || status.Outcome != OutcomeBlocked || len(status.Observations) != 1 || !strings.Contains(fmt.Sprint(status.Observations[0].Content), "external") {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestLoopIDIsIdempotentOnlyForSameRequest(t *testing.T) {
	service := New(t.TempDir())
	options := LoopStartOptions{LoopID: "loop-20260716-160007-idempotent", Input: "same", Actions: []Action{{Type: "respond", Content: "ok"}}}
	first, err := service.LoopStart(options)
	if err != nil {
		t.Fatal(err)
	}
	second, err := service.LoopStart(options)
	if err != nil || second.LoopID != first.LoopID {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	options.Input = "different"
	if _, err := service.LoopStart(options); err == nil || !strings.Contains(err.Error(), "idempotency conflict") {
		t.Fatalf("err=%v", err)
	}
}

func TestLoopStatusNormalizesLegacyDoneOutcome(t *testing.T) {
	service := New(t.TempDir())
	loopID := "loop-20260716-160008-legacy"
	paths := service.loopPaths(loopID)
	if err := os.MkdirAll(filepath.Dir(paths.StatusFile), 0o700); err != nil {
		t.Fatal(err)
	}
	legacy := PersistentLoopStatus{SchemaVersion: 1, LoopID: loopID, State: StateDone, Outcome: "done"}
	if err := writeJSONAtomic(paths.StatusFile, legacy); err != nil {
		t.Fatal(err)
	}
	status, err := service.LoopStatus(loopID)
	if err != nil || status.Outcome != OutcomeSucceeded {
		t.Fatalf("status=%#v err=%v", status, err)
	}
}

func TestLoopRequiresExistingSessionAndPreservesReference(t *testing.T) {
	service := New(t.TempDir())
	if _, err := service.LoopStart(LoopStartOptions{SessionID: "session-20260717-180000-missing", Input: "x",
		Actions: []Action{{Type: "respond", Content: "ok"}}}); err == nil || !strings.Contains(err.Error(), "does not exist") {
		t.Fatalf("err=%v", err)
	}
	manager := NewSessionManager(service)
	session, err := manager.EnsureSession("session-20260717-180001-loop", service.DefaultProject, t.TempDir(), "loop",
		RecordDecision{RecordMode: RecordFull, Retention: RetentionStandard, CaptureQuality: CaptureStructured})
	if err != nil {
		t.Fatal(err)
	}
	status, err := service.LoopStart(LoopStartOptions{SessionID: session.SessionID, Input: "x",
		Actions: []Action{{Type: "respond", Content: "ok"}}})
	if err != nil {
		t.Fatal(err)
	}
	var request LoopRequest
	if err := readJSON(service.loopPaths(status.LoopID).RequestFile, &request); err != nil || request.SessionID != session.SessionID {
		t.Fatalf("request=%#v err=%v", request, err)
	}
}
