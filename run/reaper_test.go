package run_test

import (
	"context"
	"encoding/json"
	"testing"
	"time"

	"github.com/yy003x/runtime/contract"
	runtime "github.com/yy003x/runtime/run"
)

type pausingExecutor struct{}

func (*pausingExecutor) Validate(runtime.Request) error { return nil }

func (*pausingExecutor) Execute(
	context.Context,
	runtime.Record,
	contract.EventSink,
) runtime.ExecutionOutcome {
	return runtime.ExecutionOutcome{
		State: runtime.StatePaused,
		Pause: json.RawMessage(`{}`),
	}
}

func TestSweepReaperSettlesPausedRunPastTTL(t *testing.T) {
	service := newRunService(t, &pausingExecutor{})
	submitted, submitErr := service.Submit(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "hi",
	})
	if submitErr != nil {
		t.Fatalf("submit: %v", submitErr)
	}
	if _, found, execErr := service.ExecuteNext(
		context.Background(), "worker-1",
	); execErr != nil || !found {
		t.Fatalf("execute: found=%v err=%v", found, execErr)
	}
	record, err := service.Get(context.Background(), submitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != runtime.StatePaused {
		t.Fatalf("expected paused before sweep, got %s", record.State)
	}
	// Fast-forward one hour; the paused Run's updated_at is ~now, well past the
	// 1-minute TTL.
	settled, err := service.SweepReaper(
		context.Background(),
		runtime.ReaperOptions{PausedTTL: time.Minute},
		time.Now().Add(time.Hour),
	)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if settled != 1 {
		t.Fatalf("expected 1 settled, got %d", settled)
	}
	record, err = service.Get(context.Background(), submitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != runtime.StateFailed {
		t.Fatalf("expected failed after reaper, got %s", record.State)
	}
	if record.Error == nil || record.Error.Code != contract.ErrorTimeout {
		t.Fatalf("expected timeout error, got %+v", record.Error)
	}
}

func TestSweepReaperLeavesRecentPausedRunAlone(t *testing.T) {
	service := newRunService(t, &pausingExecutor{})
	submitted, submitErr := service.Submit(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "hi",
	})
	if submitErr != nil {
		t.Fatalf("submit: %v", submitErr)
	}
	service.ExecuteNext(context.Background(), "worker-1")
	// now is the same instant the Run was paused: nothing is past the 1-minute TTL.
	settled, err := service.SweepReaper(
		context.Background(),
		runtime.ReaperOptions{PausedTTL: time.Minute},
		time.Now(),
	)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if settled != 0 {
		t.Fatalf("recent paused run must not be reaped, got %d", settled)
	}
	record, err := service.Get(context.Background(), submitted.ID)
	if err != nil {
		t.Fatal(err)
	}
	if record.State != runtime.StatePaused {
		t.Fatalf("expected still paused, got %s", record.State)
	}
}

func TestSweepReaperZeroTTLDisabled(t *testing.T) {
	service := newRunService(t, &pausingExecutor{})
	service.Submit(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "hi",
	})
	service.ExecuteNext(context.Background(), "worker-1")
	// Zero TTL disables the paused sweep entirely.
	settled, err := service.SweepReaper(
		context.Background(),
		runtime.ReaperOptions{PausedTTL: 0},
		time.Now().Add(time.Hour),
	)
	if err != nil {
		t.Fatalf("sweep: %v", err)
	}
	if settled != 0 {
		t.Fatalf("zero TTL must disable sweep, got %d", settled)
	}
}
