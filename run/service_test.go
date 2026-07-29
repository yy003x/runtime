package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yy003x/runtime/contract"
	runtime "github.com/yy003x/runtime/run"
	sqlitestore "github.com/yy003x/runtime/store/sqlite"
)

type fakeExecutor struct {
	outcome runtime.ExecutionOutcome
	event   contract.Event
}

type sequenceExecutor struct {
	mu    sync.Mutex
	count int
}

func (*sequenceExecutor) Validate(runtime.Request) error { return nil }

func (executor *sequenceExecutor) Execute(
	context.Context,
	runtime.Record,
	contract.EventSink,
) runtime.ExecutionOutcome {
	executor.mu.Lock()
	defer executor.mu.Unlock()
	executor.count++
	if executor.count == 1 {
		runtimeErr := &contract.RuntimeError{
			Code:  contract.ErrorProviderUnavailable,
			Phase: contract.PhaseProvider, Message: "fixture failure",
		}
		return runtime.ExecutionOutcome{
			State: runtime.StateFailed, Error: runtimeErr,
		}
	}
	return runtime.ExecutionOutcome{
		State: runtime.StateCompleted, Result: json.RawMessage(`{"ok":true}`),
	}
}

func (executor fakeExecutor) Validate(request runtime.Request) error {
	if request.ProfileID == "invalid" {
		return errors.New("invalid profile")
	}
	return nil
}

func (executor fakeExecutor) Execute(
	_ context.Context,
	_ runtime.Record,
	sink contract.EventSink,
) runtime.ExecutionOutcome {
	if executor.event.Type != "" {
		if err := sink(executor.event); err != nil {
			return runtime.ExecutionOutcome{
				State: runtime.StateFailed,
				Error: &contract.RuntimeError{
					Code: contract.ErrorCancelled, Phase: contract.PhaseConsumer,
					Message: err.Error(),
				},
			}
		}
	}
	return executor.outcome
}

func TestRunNowPublishesSettledResult(t *testing.T) {
	service := newRunService(t, fakeExecutor{
		event: contract.Event{Type: contract.EventModelStarted},
		outcome: runtime.ExecutionOutcome{
			State:  runtime.StateCompleted,
			Result: json.RawMessage(`{"message":"done"}`),
		},
	})
	var streamed []contract.Event
	record, runtimeErr := service.RunNow(
		context.Background(),
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api", Input: "hello",
		},
		func(event contract.Event) error {
			streamed = append(streamed, event)
			return nil
		},
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if record.State != runtime.StateCompleted ||
		record.SettledSequence != 3 || len(streamed) != 1 {
		t.Fatalf("record=%#v streamed=%#v", record, streamed)
	}
	events, err := service.Events(context.Background(), record.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 || events[2].Type != contract.EventRunSettled {
		t.Fatalf("events=%#v", events)
	}
}

func TestSubmitAndWorkerClaimExactlyOnce(t *testing.T) {
	service := newRunService(t, fakeExecutor{
		outcome: runtime.ExecutionOutcome{
			State:  runtime.StateCompleted,
			Result: json.RawMessage(`{"ok":true}`),
		},
	})
	submitted, runtimeErr := service.Submit(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "hello",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if submitted.State != runtime.StateQueued {
		t.Fatalf("submitted=%#v", submitted)
	}
	completed, found, runtimeErr := service.ExecuteNext(
		context.Background(), "worker-1",
	)
	if runtimeErr != nil || !found ||
		completed.ID != submitted.ID ||
		completed.State != runtime.StateCompleted {
		t.Fatalf("completed=%#v found=%v err=%v", completed, found, runtimeErr)
	}
	if _, found, runtimeErr := service.ExecuteNext(
		context.Background(), "worker-1",
	); runtimeErr != nil || found {
		t.Fatalf("second claim found=%v err=%v", found, runtimeErr)
	}
}

func TestWorkerContinuesAfterTaskFailure(t *testing.T) {
	service := newRunService(t, &sequenceExecutor{})
	first, runtimeErr := service.Submit(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "first",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	second, runtimeErr := service.Submit(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "second",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan error, 1)
	go func() {
		done <- service.Worker(ctx, "worker", time.Millisecond)
	}()
	deadline := time.Now().Add(3 * time.Second)
	for time.Now().Before(deadline) {
		firstRecord, _ := service.Get(context.Background(), first.ID)
		secondRecord, _ := service.Get(context.Background(), second.ID)
		if firstRecord.State.Terminal() && secondRecord.State.Terminal() {
			if firstRecord.State != runtime.StateFailed ||
				secondRecord.State != runtime.StateCompleted {
				t.Fatalf("first=%s second=%s", firstRecord.State, secondRecord.State)
			}
			cancel()
			<-done
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
	t.Fatal("worker did not process both tasks")
}

func TestExecutorValidationRunsBeforeStoreMutation(t *testing.T) {
	service := newRunService(t, fakeExecutor{})
	if _, runtimeErr := service.Submit(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "invalid", Input: "hello",
	}); runtimeErr == nil || runtimeErr.Code != contract.ErrorInvalidRequest {
		t.Fatalf("err=%v", runtimeErr)
	}
	values, err := service.List(context.Background(), runtime.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("runs=%#v", values)
	}
}

func TestReconcileRunIsIdempotentForTerminalRecord(t *testing.T) {
	service := newRunService(t, fakeExecutor{
		outcome: runtime.ExecutionOutcome{
			State:  runtime.StateCompleted,
			Result: json.RawMessage(`{"ok":true}`),
		},
	})
	record, runtimeErr := service.RunNow(
		context.Background(),
		runtime.Request{
			Kind: runtime.KindSession, ProfileID: "api", Input: "hello",
			SessionID: "session_33333333333333333333333333333333",
		},
		nil,
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	repeated, runtimeErr := service.ReconcileRun(
		context.Background(), record.ID,
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if repeated.ID != record.ID ||
		repeated.State != runtime.StateCompleted ||
		repeated.SettledSequence != record.SettledSequence {
		t.Fatalf("repeated=%#v record=%#v", repeated, record)
	}
}

func newRunService(t *testing.T, executor runtime.Executor) *runtime.Service {
	t.Helper()
	store, err := sqlitestore.Open(
		filepath.Join(t.TempDir(), "runtime.db"), sqlitestore.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: executor, runtime.KindSession: executor,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	return service
}
