package sqlite

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
	runtime "github.com/yy003x/runtime/run"
)

func TestOpenRejectsSchemaOneBeforeEnablingWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 1"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); err == nil ||
		!strings.Contains(err.Error(), "unsupported Runtime database schema 1") {
		t.Fatalf("error=%v", err)
	}
	db, err = sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	defer db.Close()
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode").Scan(&journalMode); err != nil {
		t.Fatal(err)
	}
	if !strings.EqualFold(journalMode, "delete") {
		t.Fatalf("journal_mode=%q, want delete", journalMode)
	}
}

func TestOpenRejectsUnknownRunState(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	store, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	record := createTestRun(t, store)
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec(
		`UPDATE runs SET state = 'future_state' WHERE run_id = ?`,
		record.ID,
	); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); err == nil ||
		!strings.Contains(err.Error(), "unsupported state") {
		t.Fatalf("error=%v", err)
	}
}

func TestTerminalPublishBarrierCommitsAtomically(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	record := createTestRun(t, store)
	if _, err := store.Start(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	first, err := store.AppendEvent(ctx, record.ID, contract.Event{
		Type: contract.EventModelStarted,
	})
	if err != nil {
		t.Fatal(err)
	}
	if first.Sequence != 1 {
		t.Fatalf("first=%#v", first)
	}
	result := json.RawMessage(`{"message":"done"}`)
	settled, err := store.Settle(
		ctx, record.ID, runtime.StateCompleted, result, nil,
	)
	if err != nil {
		t.Fatal(err)
	}
	if settled.State != runtime.StateCompleted ||
		settled.SettledSequence != 3 ||
		string(settled.Result) != string(result) {
		t.Fatalf("settled=%#v", settled)
	}
	events, err := store.Events(ctx, record.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 3 ||
		events[1].Type != contract.EventRunCompleted ||
		events[2].Type != contract.EventRunSettled ||
		events[2].Sequence != settled.SettledSequence {
		t.Fatalf("events=%#v", events)
	}
	if _, err := store.AppendEvent(ctx, record.ID, contract.Event{
		Type: contract.EventModelStarted,
	}); err == nil || !strings.Contains(err.Error(), "settled") {
		t.Fatalf("append after settled err=%v", err)
	}
}

func TestTerminalPublishBarrierRollsBackAllFacts(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	record := createTestRun(t, store)
	if _, err := store.Start(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.Exec(`
		CREATE TRIGGER fail_terminal
		BEFORE UPDATE OF settled_sequence ON runs
		BEGIN
		  SELECT RAISE(ABORT, 'injected terminal failure');
		END
	`); err != nil {
		t.Fatal(err)
	}
	_, err := store.Settle(
		ctx, record.ID, runtime.StateCompleted,
		json.RawMessage(`{"ok":true}`), nil,
	)
	if err == nil || !strings.Contains(err.Error(), "injected terminal failure") {
		t.Fatalf("settle err=%v", err)
	}
	current, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != runtime.StateRunning ||
		current.SettledSequence != 0 || len(current.Result) != 0 {
		t.Fatalf("current=%#v", current)
	}
	events, err := store.Events(ctx, record.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != 0 {
		t.Fatalf("events leaked from rolled back transaction: %#v", events)
	}
}

func TestReconcileSeparatesSafeReplayFromUnknownToolEffect(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	store, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	safe := createTestRun(t, store)
	unknown := createTestRun(t, store)
	sessionRunID := nextTestRunID()
	sessionRun, err := store.Create(ctx, sessionRunID, runtime.Request{
		Kind: runtime.KindSession, ProfileID: "cli", Input: "hello",
		SessionID: "session_" + strings.Repeat("1", 32),
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(ctx, safe.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(ctx, unknown.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(ctx, sessionRun.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.PrepareToolEffect(ctx, runtime.ToolEffect{
		RunID: unknown.ID, CallID: "call_1", IdempotencyKey: "idem_1",
		Name: "write", Request: json.RawMessage(`{"path":"x"}`),
	}); err != nil {
		t.Fatal(err)
	}
	if err := store.StartToolEffect(ctx, unknown.ID, "call_1"); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	safeValue, err := store.Get(ctx, safe.ID)
	if err != nil {
		t.Fatal(err)
	}
	unknownValue, err := store.Get(ctx, unknown.ID)
	if err != nil {
		t.Fatal(err)
	}
	sessionValue, err := store.Get(ctx, sessionRun.ID)
	if err != nil {
		t.Fatal(err)
	}
	if safeValue.State != runtime.StateQueued ||
		unknownValue.State != runtime.StateNeedsReconciliation ||
		unknownValue.Error == nil ||
		sessionValue.State != runtime.StateNeedsReconciliation {
		t.Fatalf(
			"safe=%#v unknown=%#v session=%#v",
			safeValue, unknownValue, sessionValue,
		)
	}
}

func TestPrivateSessionRequestIsStoredSeparatelyAndRedacted(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	runID := nextTestRunID()
	private := json.RawMessage(
		`{"schema_version":2,"base_prompt":"private-marker"}`,
	)
	record, err := store.Create(ctx, runID, runtime.Request{
		Kind: runtime.KindSession, ProfileID: "cli", Input: "hello",
		SessionID:      "session_" + strings.Repeat("2", 32),
		PrivateRequest: private,
	})
	if err != nil {
		t.Fatal(err)
	}
	publicJSON, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(publicJSON), "private-marker") {
		t.Fatalf("private request leaked: %s", publicJSON)
	}
	var requestJSON, privateJSON []byte
	if err := store.db.QueryRow(
		`SELECT request_json, private_request_json FROM runs WHERE run_id = ?`,
		runID,
	).Scan(&requestJSON, &privateJSON); err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(requestJSON), "private-marker") ||
		!strings.Contains(string(privateJSON), "private-marker") {
		t.Fatalf(
			"request_json=%s private_request_json=%s",
			requestJSON, privateJSON,
		)
	}
}

func TestOnlyOneOpenDurableRunMayOwnSession(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	sessionID := "session_" + strings.Repeat("3", 32)
	first, err := store.Create(ctx, nextTestRunID(), runtime.Request{
		Kind: runtime.KindSession, ProfileID: "cli", Input: "one",
		SessionID: sessionID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, nextTestRunID(), runtime.Request{
		Kind: runtime.KindSession, ProfileID: "cli", Input: "two",
		SessionID: sessionID,
	}); !errors.Is(err, runtime.ErrSessionRunOpen) {
		t.Fatalf("second open run error=%v", err)
	}
	if _, err := store.Settle(
		ctx, first.ID, runtime.StateFailed, nil,
		&contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseRun,
			Message: "done",
		},
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Create(ctx, nextTestRunID(), runtime.Request{
		Kind: runtime.KindSession, ProfileID: "cli", Input: "three",
		SessionID: sessionID,
	}); err != nil {
		t.Fatalf("terminal run did not release Session ownership: %v", err)
	}
}

func TestPauseResumeAndQueuedCancellation(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	paused := createTestRun(t, store)
	if _, err := store.Start(ctx, paused.ID); err != nil {
		t.Fatal(err)
	}
	value, err := store.Pause(
		ctx, paused.ID, json.RawMessage(`{"pause_id":"pause_1"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != runtime.StatePaused {
		t.Fatalf("paused=%#v", value)
	}
	value, err = store.Resume(
		ctx, paused.ID,
		json.RawMessage(`{"pause_id":"pause_1","input":{"approved":true}}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != runtime.StateQueued ||
		!strings.Contains(string(value.Request.Resume), "approved") {
		t.Fatalf("resumed=%#v", value)
	}
	queued := createTestRun(t, store)
	cancelled, err := store.RequestCancel(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != runtime.StateCancelled ||
		cancelled.SettledSequence != 2 {
		t.Fatalf("cancelled=%#v", cancelled)
	}
}

func TestOpenRejectsDatabaseSymlink(t *testing.T) {
	temp := t.TempDir()
	target := filepath.Join(temp, "target.db")
	if err := os.WriteFile(target, nil, 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(temp, "runtime.db")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(link, Options{}); err == nil ||
		!strings.Contains(err.Error(), "symlink") {
		t.Fatalf("err=%v", err)
	}
}

func TestGCOnlyDeletesSettledRunsAfterExplicitApply(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	settled := createTestRun(t, store)
	if _, err := store.Settle(
		ctx, settled.ID, runtime.StateCompleted,
		json.RawMessage(`{"ok":true}`), nil,
	); err != nil {
		t.Fatal(err)
	}
	active := createTestRun(t, store)
	cutoff := time.Now().UTC().Add(time.Hour)
	preview, err := store.GC(ctx, runtime.GCOptions{
		Before: cutoff, Limit: 10,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Candidates) != 1 ||
		preview.Candidates[0] != settled.ID ||
		preview.Applied || len(preview.Deleted) != 0 {
		t.Fatalf("preview=%#v", preview)
	}
	if _, err := store.Get(ctx, settled.ID); err != nil {
		t.Fatalf("preview deleted run: %v", err)
	}
	applied, err := store.GC(ctx, runtime.GCOptions{
		Before: cutoff, Limit: 10, Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Deleted) != 1 || applied.Deleted[0] != settled.ID {
		t.Fatalf("applied=%#v", applied)
	}
	if _, err := store.Get(ctx, settled.ID); err == nil {
		t.Fatal("settled Run still exists after GC")
	}
	if _, err := store.Get(ctx, active.ID); err != nil {
		t.Fatalf("GC deleted active Run: %v", err)
	}
}

func openTestStore(t *testing.T) *Store {
	t.Helper()
	store, err := Open(filepath.Join(t.TempDir(), "runtime.db"), Options{})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := store.Close(); err != nil {
			t.Error(err)
		}
	})
	return store
}

func createTestRun(t *testing.T, store *Store) runtime.Record {
	t.Helper()
	runID := nextTestRunID()
	value, err := store.Create(context.Background(), runID, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "hello",
		AgentBudget: agent.DefaultBudget(),
	})
	if err != nil {
		t.Fatal(err)
	}
	return value
}

func nextTestRunID() string {
	testRunCounter++
	return "run_" + fmt.Sprintf("%032x", testRunCounter)
}

var testRunCounter int
