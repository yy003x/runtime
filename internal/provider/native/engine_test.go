package native

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"
)

func TestEngineCompletesAndPersistsSnapshot(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "snapshot.json"))
	engine := NewEngine(store, &MockClient{Responses: []string{"done"}, DoneAfter: 1}, Config{MaxRounds: 2}, nil)
	snapshot, err := engine.Start(context.Background(), "run-1", Context{Messages: []Message{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if snapshot.State != StateCompleted || snapshot.Round != 1 || FinalText(snapshot) != "done" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	persisted, err := store.Load()
	if err != nil || persisted.State != StateCompleted {
		t.Fatalf("persisted=%#v err=%v", persisted, err)
	}
}

func TestEngineExecutesToolAndFeedsResultBack(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "snapshot.json"))
	client := &scriptedClient{responses: []Response{
		{Message: Message{Content: "calling tool"}, ToolCalls: []ToolCall{{ID: "call-1", Name: "echo", Arguments: map[string]any{"value": "ok"}}}, FinishReason: "tool_calls", InputTokens: 10, OutputTokens: 2},
		{Message: Message{Content: "finished"}, Done: true, FinishReason: "stop", InputTokens: 6, OutputTokens: 1},
	}}
	engine := NewEngine(store, client, Config{
		MaxRounds: 3, Tools: []Tool{{Name: "echo", Parameters: map[string]any{"type": "object"}}},
		Executor: ToolExecutorFunc(func(_ context.Context, call ToolCall) (any, error) {
			return call.Arguments, nil
		}),
	}, nil)
	snapshot, err := engine.Start(context.Background(), "run-tool", Context{Messages: []Message{{Role: "user", Content: "hello"}}})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != StateCompleted || snapshot.Round != 2 || FinalText(snapshot) != "finished" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
	if snapshot.InputTokens != 16 || snapshot.OutputTokens != 3 || snapshot.LastFinishReason != "stop" {
		t.Fatalf("usage=%#v", snapshot)
	}
	requests := client.Requests()
	last := requests[1].Messages[len(requests[1].Messages)-1]
	if last.Role != "tool" || last.ToolCallID != "call-1" || last.Content != `{"value":"ok"}` {
		t.Fatalf("tool result=%#v", last)
	}
}

func TestEngineBlocksUnauthorizedToolCall(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "snapshot.json"))
	client := &scriptedClient{responses: []Response{{ToolCalls: []ToolCall{{ID: "call-1", Name: "unsafe"}}}}}
	engine := NewEngine(store, client, Config{MaxRounds: 2}, nil)
	snapshot, err := engine.Start(context.Background(), "run-blocked-tool", Context{})
	if err != nil {
		t.Fatal(err)
	}
	if snapshot.State != StateBlocked || snapshot.LastError != "tool execution is not configured: unsafe" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestEngineFailsWhenMaxRoundsCannotFinish(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "snapshot.json"))
	engine := NewEngine(store, &MockClient{Responses: []string{"continue"}, DoneAfter: 2}, Config{MaxRounds: 1}, nil)
	snapshot, err := engine.Start(context.Background(), "run-max", Context{})
	if err == nil || snapshot.State != StateFailed || snapshot.LastError != "agent max rounds exceeded: 1" {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

type scriptedClient struct {
	mu        sync.Mutex
	responses []Response
	requests  []Request
}

func (client *scriptedClient) Generate(_ context.Context, request Request) (Response, error) {
	client.mu.Lock()
	defer client.mu.Unlock()
	client.requests = append(client.requests, request)
	index := len(client.requests) - 1
	if index >= len(client.responses) {
		return Response{}, fmt.Errorf("unexpected call %d", index+1)
	}
	return client.responses[index], nil
}

func (client *scriptedClient) Requests() []Request {
	client.mu.Lock()
	defer client.mu.Unlock()
	return append([]Request(nil), client.requests...)
}

func TestEngineWaitsForHumanAndPatchResumes(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "snapshot.json"))
	engine := NewEngine(store, &MockClient{Responses: []string{"recovered"}}, Config{MaxRounds: 1, LLMTimeout: 30 * time.Millisecond}, nil)
	snapshot, err := engine.Start(context.Background(), "run-timeout", Context{Messages: []Message{{Role: "user", Content: "timeout"}}})
	if err != nil {
		t.Fatalf("Start: %v", err)
	}
	if snapshot.State != StateWaitingHuman {
		t.Fatalf("state=%s", snapshot.State)
	}
	patch := ContextPatch{Operation: PatchAppend, Messages: []Message{{Role: "user", Content: "recovered context"}}}
	snapshot, err = engine.Resume(context.Background(), &patch)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if snapshot.State != StateCompleted || FinalText(snapshot) != "recovered" {
		t.Fatalf("snapshot=%#v", snapshot)
	}
}

func TestEngineObservesBlockControlAndContinues(t *testing.T) {
	store := NewFileStore(filepath.Join(t.TempDir(), "snapshot.json"))
	engine := NewEngine(store, &MockClient{Latency: 500 * time.Millisecond, Responses: []string{"continued"}}, Config{MaxRounds: 1, LLMTimeout: time.Second}, nil)
	done := make(chan Snapshot, 1)
	go func() {
		snapshot, _ := engine.Start(context.Background(), "run-block", Context{Messages: []Message{{Role: "user", Content: "hello"}}})
		done <- snapshot
	}()
	waitNativeState(t, store, StateWaitingLLM)
	if _, err := ControlRun(store, "block", "operator pause"); err != nil {
		t.Fatalf("ControlRun: %v", err)
	}
	blocked := <-done
	if blocked.State != StateBlocked {
		t.Fatalf("state=%s", blocked.State)
	}
	continued, err := engine.Resume(context.Background(), nil)
	if err != nil {
		t.Fatalf("Resume: %v", err)
	}
	if continued.State != StateCompleted || FinalText(continued) != "continued" {
		t.Fatalf("snapshot=%#v", continued)
	}
}

func waitNativeState(t *testing.T, store *FileStore, state State) Snapshot {
	t.Helper()
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		snapshot, err := store.Load()
		if err == nil && snapshot.State == state {
			return snapshot
		}
		time.Sleep(10 * time.Millisecond)
	}
	snapshot, err := store.Load()
	t.Fatalf("state=%s not reached: snapshot=%#v err=%v", state, snapshot, err)
	return Snapshot{}
}
