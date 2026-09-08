package sqlite

import (
	"context"
	"errors"
	"fmt"
	"math"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	runtime "github.com/yy003x/runtime/pkg/run"
)

func TestListCorrelatesRunsAndUsesStableKeysetPages(t *testing.T) {
	ctx := context.Background()
	clock := time.Date(2026, 9, 8, 0, 0, 0, 0, time.UTC)
	store, err := Open(filepath.Join(t.TempDir(), "runtime.db"), Options{
		Now: func() time.Time { return clock },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	runID := func(n int) string { return fmt.Sprintf("run_%032x", n) }
	const sessionID = "session_11111111111111111111111111111111"
	const taskID = "百工/任务 ' OR 1=1 --"
	create := func(n int, request runtime.Request) {
		t.Helper()
		request.Input = "hello"
		if _, err := store.Create(ctx, runID(n), request); err != nil {
			t.Fatal(err)
		}
	}
	create(1, runtime.Request{Kind: runtime.KindAgent, ProfileID: "api-other", TaskID: "other"})
	// Timestamp order must remain correct across fractional-second boundaries;
	// Run ID is the deterministic tie-breaker, not insertion order.
	clock = clock.Add(100 * time.Nanosecond)
	create(3, runtime.Request{Kind: runtime.KindAgent, ProfileID: "api", TaskID: taskID})
	create(2, runtime.Request{Kind: runtime.KindSession, ProfileID: "api", TaskID: taskID, SessionID: sessionID})
	clock = clock.Add(100 * time.Nanosecond)
	create(4, runtime.Request{Kind: runtime.KindAgent, ProfileID: "api", TaskID: taskID})
	assertIDs := func(filter runtime.ListFilter, want ...int) {
		t.Helper()
		values, err := store.List(ctx, filter)
		if err != nil {
			t.Fatal(err)
		}
		if values == nil {
			t.Fatal("empty lists must be arrays, not null")
		}
		gotIDs := make([]string, 0, len(values))
		wantIDs := make([]string, 0, len(want))
		for _, value := range values {
			gotIDs = append(gotIDs, value.ID)
		}
		for _, id := range want {
			wantIDs = append(wantIDs, runID(id))
		}
		if !reflect.DeepEqual(gotIDs, wantIDs) {
			t.Fatalf("filter=%#v got=%v want=%v", filter, gotIDs, wantIDs)
		}
	}
	assertIDs(runtime.ListFilter{}, 4, 3, 2, 1)
	assertIDs(runtime.ListFilter{Limit: 2}, 4, 3)
	assertIDs(runtime.ListFilter{ProfileID: "api"}, 4, 3, 2)
	assertIDs(runtime.ListFilter{TaskID: taskID}, 4, 3, 2)
	assertIDs(runtime.ListFilter{SessionID: sessionID}, 2)
	assertIDs(runtime.ListFilter{
		State: runtime.StateQueued, Kind: runtime.KindSession,
		SessionID: sessionID, TaskID: taskID, ProfileID: "api",
	}, 2)
	assertIDs(runtime.ListFilter{TaskID: "OR 1=1"})
	assertIDs(runtime.ListFilter{ProfileID: "api-absent"})
	// A newer insertion cannot shift the next page as offset pagination would.
	clock = clock.Add(time.Second)
	create(5, runtime.Request{Kind: runtime.KindAgent, ProfileID: "api", TaskID: taskID})
	assertIDs(runtime.ListFilter{BeforeRunID: runID(3), Limit: 2}, 2, 1)
	assertIDs(runtime.ListFilter{BeforeRunID: runID(3), ProfileID: "api"}, 2)
	assertIDs(runtime.ListFilter{BeforeRunID: runID(2), Kind: runtime.KindAgent}, 1)
	assertIDs(runtime.ListFilter{BeforeRunID: runID(1)})
	if _, err := store.List(ctx, runtime.ListFilter{BeforeRunID: runID(99)}); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("missing cursor error=%v", err)
	}
}

func TestListRejectsInvalidCorrelationFilters(t *testing.T) {
	store := openTestStore(t)
	for _, filter := range []runtime.ListFilter{
		{SessionID: "session_bad"}, {TaskID: "bad\x00task"},
		{TaskID: string([]byte{0xff})}, {ProfileID: "../api"},
		{BeforeRunID: "run_bad"},
	} {
		if _, err := store.List(context.Background(), filter); err == nil {
			t.Fatalf("accepted filter=%#v", filter)
		}
	}
}

func TestEventsAcceptsFullUint64CursorWithoutHidingStoreErrors(t *testing.T) {
	store := openTestStore(t)
	value := createTestRun(t, store)
	for _, after := range []uint64{math.MaxInt64, math.MaxInt64 + 1, math.MaxUint64} {
		values, err := store.Events(context.Background(), value.ID, after, 100)
		if err != nil || len(values) != 0 {
			t.Fatalf("after=%d events=%v error=%v", after, values, err)
		}
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Events(context.Background(), value.ID, math.MaxUint64, 100); err == nil {
		t.Fatal("large cursor hid a closed Store")
	}
}
