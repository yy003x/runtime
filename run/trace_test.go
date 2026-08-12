package run_test

import (
	"context"
	"encoding/json"
	"testing"

	runtime "github.com/yy003x/runtime/run"
)

func TestTraceByRunAggregatesRecordAndEvents(t *testing.T) {
	service := newRunService(t, fakeExecutor{
		outcome: runtime.ExecutionOutcome{
			State:  runtime.StateCompleted,
			Result: json.RawMessage(`{"ok":true}`),
		},
	})
	submitted, err := service.Submit(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "hi",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	if _, found, err := service.ExecuteNext(
		context.Background(), "worker-1",
	); err != nil || !found {
		t.Fatalf("execute: found=%v err=%v", found, err)
	}
	trace, traceErr := service.TraceByRun(context.Background(), submitted.ID)
	if traceErr != nil {
		t.Fatalf("TraceByRun: %v", traceErr)
	}
	if trace.Run.ID != submitted.ID {
		t.Fatalf("trace run %q != submitted %q", trace.Run.ID, submitted.ID)
	}
	if trace.Run.State != runtime.StateCompleted {
		t.Fatalf("expected completed run in trace, got %s", trace.Run.State)
	}
	if len(trace.Events) == 0 {
		t.Fatalf("expected settled events in trace, got none")
	}
	// fakeExecutor does not write model-call or tool-effect journals.
	if len(trace.ModelCalls) != 0 {
		t.Fatalf("expected no model calls, got %d", len(trace.ModelCalls))
	}
	if len(trace.ToolEffects) != 0 {
		t.Fatalf("expected no tool effects, got %d", len(trace.ToolEffects))
	}
}

func TestTraceByRunUnknownRunErrors(t *testing.T) {
	service := newRunService(t, fakeExecutor{
		outcome: runtime.ExecutionOutcome{State: runtime.StateCompleted},
	})
	if _, err := service.TraceByRun(
		context.Background(), "does-not-exist",
	); err == nil {
		t.Fatalf("expected error for unknown run")
	}
}
