package native

import (
	"context"
	"path/filepath"
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
