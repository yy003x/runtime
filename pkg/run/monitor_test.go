package run

import (
	"context"
	"errors"
	"sync/atomic"
	"testing"
	"time"
)

// failingGetStore satisfies Store for monitorCancellation by embedding the
// interface; only Get is exercised by the monitor, and it is overridden to
// fail. Promoted methods would panic if called, which is intentional so any
// drift in what the monitor touches surfaces loudly.
type failingGetStore struct {
	Store
	getErr error
}

func (store *failingGetStore) Get(_ context.Context, _ string) (Record, error) {
	return Record{}, store.getErr
}

// TestMonitorCancellationSurfacesStoreErrorWithoutCancelling verifies the
// onStoreError diagnostic sink fires on every failed store read while the
// monitor keeps polling and never cancels execution: a store read failure
// does not make the outcome knowable, so control flow stays unchanged.
func TestMonitorCancellationSurfacesStoreErrorWithoutCancelling(t *testing.T) {
	var sinkCalls atomic.Int32
	var cancelCalled atomic.Bool
	service := &Service{
		store:              &failingGetStore{getErr: errors.New("transient sqlite failure")},
		cancelPollInterval: time.Millisecond,
		onStoreError: func(runID string, err error) {
			if runID != "run_x" {
				t.Errorf("onStoreError runID=%q want run_x", runID)
			}
			if err == nil {
				t.Error("onStoreError received nil error")
			}
			sinkCalls.Add(1)
		},
	}

	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	done := make(chan struct{})
	go func() {
		service.monitorCancellation(ctx, "run_x", func() { cancelCalled.Store(true) })
		close(done)
	}()

	deadline := time.Now().Add(2 * time.Second)
	for sinkCalls.Load() < 3 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if sinkCalls.Load() < 3 {
		t.Fatalf("onStoreError fired %d times, want >=3 (monitor must keep polling)", sinkCalls.Load())
	}
	if cancelCalled.Load() {
		t.Fatal("cancel must not be invoked when the store read fails")
	}

	cancel()
	select {
	case <-done:
	case <-time.After(2 * time.Second):
		t.Fatal("monitor did not stop after context cancellation")
	}
}
