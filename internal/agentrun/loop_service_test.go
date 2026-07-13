package agentrun

import (
	"context"
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

	other, err := service.LoopStart(LoopStartOptions{Input: "cancel", Actions: []Action{{Type: "respond", Content: "unused"}}})
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := service.LoopCancel(other.LoopID)
	if err != nil || cancelled.Outcome != OutcomeCancelled {
		t.Fatalf("cancelled=%#v err=%v", cancelled, err)
	}
}
