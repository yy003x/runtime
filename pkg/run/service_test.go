package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
	sqlitestore "github.com/yy003x/runtime/pkg/store/sqlite"
)

type fakeExecutor struct {
	outcome runtime.ExecutionOutcome
	event   contract.Event
}

type sequenceExecutor struct {
	mu    sync.Mutex
	count int
}

type cancellationFinalizerExecutor struct {
	mu    sync.Mutex
	count int
}

type crossProcessCancellationExecutor struct {
	started chan struct{}
}

type terminalSettleAckStore struct {
	runtime.Store

	mu              sync.Mutex
	failures        int
	commitBeforeErr bool
}

func (store *terminalSettleAckStore) Settle(
	ctx context.Context,
	runID string,
	state runtime.State,
	result json.RawMessage,
	runtimeErr *contract.RuntimeError,
) (runtime.Record, error) {
	store.mu.Lock()
	fail := store.failures > 0
	if fail {
		store.failures--
	}
	commit := store.commitBeforeErr
	store.mu.Unlock()
	if !fail {
		return store.Store.Settle(
			ctx, runID, state, result, runtimeErr,
		)
	}
	if commit {
		if _, err := store.Store.Settle(
			ctx, runID, state, result, runtimeErr,
		); err != nil {
			return runtime.Record{}, err
		}
	}
	return runtime.Record{}, errors.New(
		"fixture lost terminal settlement acknowledgement",
	)
}

type deadlineResumeExecutor struct {
	notAfter time.Time
	advance  func()
}

func (*deadlineResumeExecutor) Validate(runtime.Request) error { return nil }

func (*deadlineResumeExecutor) Execute(
	context.Context,
	runtime.Record,
	contract.EventSink,
) runtime.ExecutionOutcome {
	panic("expired resume must not execute the Run")
}

func (executor *deadlineResumeExecutor) ValidateResume(
	_ context.Context,
	record runtime.Record,
	_ json.RawMessage,
) (runtime.ResumeConstraint, error) {
	if executor.advance != nil {
		executor.advance()
	}
	return runtime.ResumeConstraint{
		Pause: append([]byte(nil), record.Pause...),
		NotAfter: func() *time.Time {
			value := executor.notAfter
			return &value
		}(),
	}, nil
}

func (*cancellationFinalizerExecutor) Validate(runtime.Request) error {
	return nil
}

func (*crossProcessCancellationExecutor) Validate(runtime.Request) error {
	return nil
}

func (executor *crossProcessCancellationExecutor) Execute(
	ctx context.Context,
	_ runtime.Record,
	_ contract.EventSink,
) runtime.ExecutionOutcome {
	close(executor.started)
	<-ctx.Done()
	return runtime.ExecutionOutcome{
		State: runtime.StateCancelled,
		Error: &contract.RuntimeError{
			Code: contract.ErrorCancelled, Phase: contract.PhaseRun,
			Message: "observed durable cancellation",
		},
	}
}

func (*cancellationFinalizerExecutor) Execute(
	context.Context,
	runtime.Record,
	contract.EventSink,
) runtime.ExecutionOutcome {
	panic("cancellation reconciliation must not execute the Run")
}

func (executor *cancellationFinalizerExecutor) FinalizeCancellation(
	context.Context,
	runtime.Record,
) runtime.ExecutionOutcome {
	executor.mu.Lock()
	executor.count++
	executor.mu.Unlock()
	return runtime.ExecutionOutcome{State: runtime.StateCancelled}
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

func TestRunTerminalSettlementHandlesCommitAndPrecommitAckLoss(
	t *testing.T,
) {
	testCases := []struct {
		name            string
		failures        int
		commitBeforeErr bool
		wantState       runtime.State
		wantRuntimeErr  bool
	}{
		{
			name: "commit_ack_lost", failures: 1,
			commitBeforeErr: true, wantState: runtime.StateCompleted,
		},
		{
			name: "precommit_transient", failures: 1,
			wantState: runtime.StateCompleted,
		},
		{
			name: "precommit_persistent", failures: 2,
			wantState:      runtime.StateRunning,
			wantRuntimeErr: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			sqliteStore, err := sqlitestore.Open(
				filepath.Join(t.TempDir(), "runtime.db"),
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			store := &terminalSettleAckStore{
				Store: sqliteStore, failures: testCase.failures,
				commitBeforeErr: testCase.commitBeforeErr,
			}
			service, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: fakeExecutor{
						outcome: runtime.ExecutionOutcome{
							State:  runtime.StateCompleted,
							Result: json.RawMessage(`{"ok":true}`),
						},
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.Close() })
			record, runtimeErr := service.RunNow(
				ctx,
				runtime.Request{
					Kind:      runtime.KindAgent,
					ProfileID: "api", Input: "start",
				},
				nil,
			)
			if record.State != testCase.wantState ||
				(runtimeErr != nil) != testCase.wantRuntimeErr {
				t.Fatalf("record=%#v error=%#v", record, runtimeErr)
			}
			if testCase.wantState == runtime.StateCompleted {
				if record.SettledSequence == 0 ||
					string(record.Result) != `{"ok":true}` ||
					record.Error != nil {
					t.Fatalf("completed=%#v", record)
				}
			} else if record.SettledSequence != 0 ||
				record.Error != nil {
				t.Fatalf("uncommitted terminal state=%#v", record)
			}
		})
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

func TestRunningCancellationIsObservedAcrossServiceProcesses(t *testing.T) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "runtime.db")
	executionStore, err := sqlitestore.Open(
		databasePath,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	controlStore, err := sqlitestore.Open(
		databasePath,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		_ = executionStore.Close()
		t.Fatal(err)
	}
	executor := &crossProcessCancellationExecutor{
		started: make(chan struct{}),
	}
	executionService, err := runtime.NewService(runtime.ServiceOptions{
		Store: executionStore,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: executor,
		},
		CancelPollInterval: 5 * time.Millisecond,
	})
	if err != nil {
		_ = controlStore.Close()
		_ = executionStore.Close()
		t.Fatal(err)
	}
	controlService, err := runtime.NewService(runtime.ServiceOptions{
		Store: controlStore,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: fakeExecutor{},
		},
	})
	if err != nil {
		_ = executionService.Close()
		_ = controlStore.Close()
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = controlService.Close()
		_ = executionService.Close()
	})

	submitted, runtimeErr := executionService.Submit(
		ctx,
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api",
			Input: "wait for cross-process cancel",
		},
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	type executionResult struct {
		record runtime.Record
		found  bool
		err    *contract.RuntimeError
	}
	done := make(chan executionResult, 1)
	go func() {
		record, found, runtimeErr := executionService.ExecuteNext(
			ctx, "server-worker",
		)
		done <- executionResult{
			record: record, found: found, err: runtimeErr,
		}
	}()
	select {
	case <-executor.started:
	case <-time.After(3 * time.Second):
		t.Fatal("server-owned Run did not start")
	}
	reserved, err := controlService.Cancel(ctx, submitted.ID)
	if err != nil ||
		reserved.State != runtime.StateRunning ||
		!reserved.CancelRequested {
		t.Fatalf("reserved=%#v error=%v", reserved, err)
	}
	select {
	case result := <-done:
		if !result.found || result.err != nil ||
			result.record.State != runtime.StateCancelled ||
			!result.record.CancelRequested ||
			result.record.SettledSequence == 0 {
			t.Fatalf("execution result=%#v", result)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("SQLite cancellation polling did not stop running Run")
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

func TestReconcileRunRejectsUnrelatedTerminalAgent(t *testing.T) {
	service := newRunService(t, fakeExecutor{
		outcome: runtime.ExecutionOutcome{
			State:  runtime.StateCompleted,
			Result: json.RawMessage(`{"ok":true}`),
		},
	})
	record, runtimeErr := service.RunNow(
		context.Background(),
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api", Input: "hello",
		},
		nil,
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if _, runtimeErr := service.ReconcileRun(
		context.Background(), record.ID,
	); runtimeErr == nil || runtimeErr.Code != contract.ErrorConflict {
		t.Fatalf("reconcile error=%v", runtimeErr)
	}
}

func TestReconcileDrainsAllCancellationReservationsWithoutListTruncation(
	t *testing.T,
) {
	ctx := context.Background()
	store, err := sqlitestore.Open(
		filepath.Join(t.TempDir(), "runtime.db"),
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	executor := &cancellationFinalizerExecutor{}
	service, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: executor,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	const reservations = 1001
	candidateIDs := make([]string, 0, reservations)
	for index := 0; index < reservations; index++ {
		runID := fmt.Sprintf("run_%032x", index+1)
		if _, err := store.Create(ctx, runID, runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api", Input: "cancel",
		}); err != nil {
			t.Fatal(err)
		}
		if _, err := store.RequestCancel(ctx, runID); err != nil {
			t.Fatal(err)
		}
		candidateIDs = append(candidateIDs, runID)
	}
	// These newer non-candidates would hide the reservations behind the old
	// public List limit/order, but do not affect the dedicated keyset scan.
	for index := 0; index < 1001; index++ {
		runID := fmt.Sprintf("run_%032x", reservations+index+1)
		if _, err := store.Create(ctx, runID, runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api", Input: "queued",
		}); err != nil {
			t.Fatal(err)
		}
	}
	if err := service.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	executor.mu.Lock()
	finalized := executor.count
	executor.mu.Unlock()
	if finalized != reservations {
		t.Fatalf("finalized=%d want=%d", finalized, reservations)
	}
	for _, runID := range candidateIDs {
		value, err := store.Get(ctx, runID)
		if err != nil {
			t.Fatal(err)
		}
		if value.State != runtime.StateCancelled ||
			!value.CancelRequested ||
			value.SettledSequence == 0 {
			t.Fatalf("reservation %s not settled: %#v", runID, value)
		}
	}
}

func TestReconcileAfterDatabaseReopenFinalizesReservedCancellation(
	t *testing.T,
) {
	for _, state := range []runtime.State{
		runtime.StateQueued,
		runtime.StatePaused,
		runtime.StateRunning,
	} {
		t.Run(string(state), func(t *testing.T) {
			ctx := context.Background()
			databasePath := filepath.Join(t.TempDir(), "runtime.db")
			store, err := sqlitestore.Open(
				databasePath,
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			runID := "run_99999999999999999999999999999999"
			if _, err := store.Create(ctx, runID, runtime.Request{
				Kind:      runtime.KindAgent,
				ProfileID: "api",
				Input:     "cancel after crash",
			}); err != nil {
				t.Fatal(err)
			}
			switch state {
			case runtime.StatePaused:
				if _, err := store.Start(ctx, runID); err != nil {
					t.Fatal(err)
				}
				if _, err := store.Pause(
					ctx, runID,
					json.RawMessage(`{"pause_id":"pause_crash"}`),
				); err != nil {
					t.Fatal(err)
				}
			case runtime.StateRunning:
				if _, err := store.Start(ctx, runID); err != nil {
					t.Fatal(err)
				}
			}
			reserved, err := store.RequestCancel(ctx, runID)
			if err != nil || !reserved.CancelRequested ||
				reserved.State != state {
				t.Fatalf("reserved=%#v err=%v", reserved, err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			store, err = sqlitestore.Open(
				databasePath,
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			executor := &cancellationFinalizerExecutor{}
			service, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: executor,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = service.Close() })
			if err := service.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			cancelled, err := service.Get(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if cancelled.State != runtime.StateCancelled ||
				!cancelled.CancelRequested ||
				cancelled.SettledSequence == 0 {
				t.Fatalf("cancelled=%#v", cancelled)
			}
			executor.mu.Lock()
			finalized := executor.count
			executor.mu.Unlock()
			if finalized != 1 {
				t.Fatalf("finalizer calls=%d", finalized)
			}
		})
	}
}

func TestResumeExpiryIsCheckedAtStoreLinearizationWithoutMutation(
	t *testing.T,
) {
	ctx := context.Background()
	before := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	expiresAt := before.Add(time.Second)
	after := expiresAt.Add(time.Nanosecond)
	var clockMu sync.Mutex
	clock := before
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	setClock := func(value time.Time) {
		clockMu.Lock()
		clock = value
		clockMu.Unlock()
	}
	store, err := sqlitestore.Open(
		filepath.Join(t.TempDir(), "runtime.db"),
		sqlitestore.Options{Now: now, SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	executor := &deadlineResumeExecutor{
		notAfter: expiresAt,
		advance:  func() { setClock(after) },
	}
	service, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: executor,
		},
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = service.Close() })
	runID := "run_88888888888888888888888888888888"
	if _, err := store.Create(ctx, runID, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(ctx, runID); err != nil {
		t.Fatal(err)
	}
	pauseJSON := json.RawMessage(`{"pause_id":"pause_linearized"}`)
	if _, err := store.Pause(ctx, runID, pauseJSON); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Resume(
		ctx, runID,
		json.RawMessage(
			`{"pause_id":"pause_linearized","input":{"approved":true}}`,
		),
	); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("resume error=%v", err)
	}
	paused, err := store.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if paused.State != runtime.StatePaused ||
		len(paused.Request.Resume) != 0 ||
		paused.ResumeAcceptedAt != nil ||
		string(paused.Pause) != string(pauseJSON) {
		t.Fatalf("expired resume mutated Run: %#v", paused)
	}
	resumes, err := store.Resumes(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumes) != 0 {
		t.Fatalf("expired resume journal rows=%#v", resumes)
	}
	if _, found, err := store.Claim(ctx, "worker"); err != nil || found {
		t.Fatalf("expired paused Run became claimable: found=%v err=%v", found, err)
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
