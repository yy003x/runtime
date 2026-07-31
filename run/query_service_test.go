package run_test

import (
	"context"
	"path/filepath"
	"reflect"
	"testing"
	"time"

	runtime "github.com/yy003x/runtime/run"
	sqlitestore "github.com/yy003x/runtime/store/sqlite"
)

type listFilterStore struct {
	runtime.Store
	filters []runtime.ListFilter
}

func (store *listFilterStore) List(
	_ context.Context,
	filter runtime.ListFilter,
) ([]runtime.Record, error) {
	store.filters = append(store.filters, filter)
	return nil, nil
}

func TestQueryServiceUsesCanonicalListFilterValidationAndDefault(t *testing.T) {
	store := &listFilterStore{}
	service, err := runtime.NewQueryService(store)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := service.List(
		context.Background(), runtime.ListFilter{},
	); err != nil {
		t.Fatal(err)
	}
	if want := []runtime.ListFilter{{Limit: runtime.DefaultListLimit}}; !reflect.DeepEqual(
		store.filters, want,
	) {
		t.Fatalf("filters=%#v want=%#v", store.filters, want)
	}
	for _, filter := range []runtime.ListFilter{
		{State: "future"},
		{Kind: "future"},
		{Limit: -1},
		{Limit: runtime.MaxListLimit + 1},
	} {
		if _, err := service.List(context.Background(), filter); err == nil {
			t.Fatalf("accepted invalid filter=%#v", filter)
		}
		if len(store.filters) != 1 {
			t.Fatalf(
				"invalid filter reached Store: filter=%#v calls=%#v",
				filter, store.filters,
			)
		}
	}
	valid := runtime.ListFilter{
		State: runtime.StateNeedsReconciliation,
		Kind:  runtime.KindAgent,
		Limit: runtime.MaxListLimit,
	}
	if _, err := service.List(context.Background(), valid); err != nil {
		t.Fatal(err)
	}
	if got := store.filters[len(store.filters)-1]; got != valid {
		t.Fatalf("filter=%#v want=%#v", got, valid)
	}
}

func TestQueryServiceUsesStoreWithoutExecutorsAndClosesIt(t *testing.T) {
	store, err := sqlitestore.Open(
		filepath.Join(t.TempDir(), "runtime.db"),
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := runtime.NewQueryService(store)
	if err != nil {
		_ = store.Close()
		t.Fatal(err)
	}
	ctx := context.Background()
	runID := "run_11111111111111111111111111111111"
	if _, err := store.Create(ctx, runID, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "query",
	}); err != nil {
		_ = service.Close()
		t.Fatal(err)
	}
	if value, err := service.Get(ctx, runID); err != nil ||
		value.ID != runID {
		_ = service.Close()
		t.Fatalf("value=%#v err=%v", value, err)
	}
	if _, err := service.GC(ctx, runtime.GCOptions{
		Before: time.Now().UTC(), Limit: 10,
	}); err != nil {
		_ = service.Close()
		t.Fatal(err)
	}
	if err := service.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := service.Get(ctx, runID); err == nil {
		t.Fatal("query service remained usable after Close")
	}
}
