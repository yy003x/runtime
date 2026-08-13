package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"sort"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
)

type pausingExecutor struct{}

type reaperFixtureStore struct {
	runtime.Store
	records      []runtime.Record
	listErr      error
	settleErrors map[string]error
	listFilters  []runtime.ExpiredRunFilter
	settled      []string
}

func (store *reaperFixtureStore) ListExpiredRuns(
	_ context.Context,
	filter runtime.ExpiredRunFilter,
) ([]runtime.Record, error) {
	filter, err := runtime.NormalizeExpiredRunFilter(filter)
	if err != nil {
		return nil, err
	}
	store.listFilters = append(store.listFilters, filter)
	if store.listErr != nil {
		return nil, store.listErr
	}
	records := append([]runtime.Record(nil), store.records...)
	sort.Slice(records, func(left, right int) bool {
		if records[left].UpdatedAt.Equal(records[right].UpdatedAt) {
			return records[left].ID < records[right].ID
		}
		return records[left].UpdatedAt.Before(records[right].UpdatedAt)
	})
	values := make([]runtime.Record, 0, filter.Limit)
	for _, record := range records {
		if record.State != filter.State || record.CancelRequested ||
			!record.UpdatedAt.Before(filter.UpdatedBefore) {
			continue
		}
		if filter.After != nil &&
			(record.UpdatedAt.Before(filter.After.UpdatedAt) ||
				record.UpdatedAt.Equal(filter.After.UpdatedAt) &&
					record.ID <= filter.After.RunID) {
			continue
		}
		values = append(values, record)
		if len(values) == filter.Limit {
			break
		}
	}
	return values, nil
}

func (store *reaperFixtureStore) SettleExpiredRun(
	_ context.Context,
	runID string,
	_ runtime.State,
	_ time.Time,
	_ *contract.RuntimeError,
) (runtime.Record, error) {
	if err := store.settleErrors[runID]; err != nil {
		return runtime.Record{}, err
	}
	store.settled = append(store.settled, runID)
	return runtime.Record{ID: runID, State: runtime.StateFailed}, nil
}

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

func newReaperFixtureService(
	t *testing.T,
	store runtime.Store,
) *runtime.Service {
	t.Helper()
	service, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &pausingExecutor{},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}

func TestSweepReaperRejectsStoreWithoutMaintenanceCapability(t *testing.T) {
	service := newReaperFixtureService(t, &struct{ runtime.Store }{})
	_, err := service.SweepReaper(
		context.Background(),
		runtime.ReaperOptions{PausedTTL: time.Minute},
		time.Now(),
	)
	if err == nil || !strings.Contains(err.Error(), "reaper maintenance") {
		t.Fatalf("SweepReaper error=%v", err)
	}
}

func TestSweepReaperNeedsNoMaintenanceCapabilityWhenAllTTLsDisabled(
	t *testing.T,
) {
	service := newReaperFixtureService(t, &struct{ runtime.Store }{})
	settled, err := service.SweepReaper(
		context.Background(), runtime.ReaperOptions{}, time.Now(),
	)
	if err != nil || settled != 0 {
		t.Fatalf("SweepReaper settled=%d error=%v", settled, err)
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

func TestSweepReaperScansPastFirstThousandCandidates(t *testing.T) {
	updatedAt := time.Date(2026, 8, 12, 1, 0, 0, 0, time.UTC)
	store := &reaperFixtureStore{}
	for index := 1; index <= runtime.MaxListLimit+1; index++ {
		store.records = append(store.records, runtime.Record{
			ID:    fmt.Sprintf("run_%032x", index),
			State: runtime.StatePaused, UpdatedAt: updatedAt,
		})
	}
	service := newReaperFixtureService(t, store)
	settled, err := service.SweepReaper(
		context.Background(),
		runtime.ReaperOptions{PausedTTL: time.Minute},
		updatedAt.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if settled != runtime.MaxListLimit+1 ||
		len(store.settled) != runtime.MaxListLimit+1 {
		t.Fatalf(
			"settled=%d ids=%d, want %d",
			settled, len(store.settled), runtime.MaxListLimit+1,
		)
	}
	if len(store.listFilters) != 2 {
		t.Fatalf("page queries=%d, want 2", len(store.listFilters))
	}
	second := store.listFilters[1]
	if second.After == nil ||
		second.After.RunID != fmt.Sprintf("run_%032x", runtime.MaxListLimit) ||
		!second.After.UpdatedAt.Equal(updatedAt) {
		t.Fatalf("second page cursor=%#v", second.After)
	}
}

func TestSweepReaperSkipsOnlyConcurrentOwnershipErrors(t *testing.T) {
	updatedAt := time.Date(2026, 8, 12, 1, 0, 0, 0, time.UTC)
	conflicted := "run_00000000000000000000000000000001"
	cancelled := "run_00000000000000000000000000000002"
	settleable := "run_00000000000000000000000000000003"
	store := &reaperFixtureStore{
		records: []runtime.Record{
			{ID: conflicted, State: runtime.StatePaused, UpdatedAt: updatedAt},
			{ID: cancelled, State: runtime.StatePaused, UpdatedAt: updatedAt},
			{ID: settleable, State: runtime.StatePaused, UpdatedAt: updatedAt},
		},
		settleErrors: map[string]error{
			conflicted: fmt.Errorf("%w: resumed", runtime.ErrConflict),
			cancelled:  fmt.Errorf("%w: cancellation won", runtime.ErrCancelReserved),
		},
	}
	service := newReaperFixtureService(t, store)
	settled, err := service.SweepReaper(
		context.Background(),
		runtime.ReaperOptions{PausedTTL: time.Minute},
		updatedAt.Add(time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if settled != 1 || len(store.settled) != 1 ||
		store.settled[0] != settleable {
		t.Fatalf("settled=%d ids=%v", settled, store.settled)
	}
}

func TestSweepReaperReturnsUnknownSettlementError(t *testing.T) {
	updatedAt := time.Date(2026, 8, 12, 1, 0, 0, 0, time.UTC)
	storageErr := errors.New("fixture SQLite write failed")
	runID := "run_00000000000000000000000000000001"
	store := &reaperFixtureStore{
		records: []runtime.Record{{
			ID: runID, State: runtime.StatePaused, UpdatedAt: updatedAt,
		}},
		settleErrors: map[string]error{runID: storageErr},
	}
	service := newReaperFixtureService(t, store)
	settled, err := service.SweepReaper(
		context.Background(),
		runtime.ReaperOptions{PausedTTL: time.Minute},
		updatedAt.Add(time.Hour),
	)
	if settled != 0 || !errors.Is(err, storageErr) {
		t.Fatalf("settled=%d error=%v", settled, err)
	}
}

func TestRunReaperReturnsSweepError(t *testing.T) {
	listErr := errors.New("fixture SQLite query failed")
	service := newReaperFixtureService(t, &reaperFixtureStore{
		listErr: listErr,
	})
	ctx, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	err := service.RunReaper(ctx, runtime.ReaperOptions{
		Interval: time.Millisecond, PausedTTL: time.Minute,
	})
	if !errors.Is(err, listErr) {
		t.Fatalf("RunReaper error=%v", err)
	}
}
