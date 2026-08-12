package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
)

func TestOpenRejectsUnsupportedSchemaBeforeEnablingWAL(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	db, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := db.Exec("PRAGMA user_version = 999"); err != nil {
		db.Close()
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := Open(path, Options{}); err == nil ||
		!strings.Contains(err.Error(), "unsupported Runtime database schema 999") {
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

func TestLatestModelCallRejectsTamperedResultDigest(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	record := createTestRun(t, store)
	call := runtime.ModelCall{
		ID:    "model_call_99999999999999999999999999999999",
		RunID: record.ID, Sequence: 1,
		RequestDigest: "sha256:" + strings.Repeat("0", 64),
	}
	requestJSON, err := json.Marshal(contract.GenerateRequest{
		ModelProfile: "api",
		Input: contract.ModelRequest{
			Messages: []contract.Message{{
				Role: contract.RoleUser, Content: "start",
			}},
			Trace: contract.TraceContext{
				Labels: map[string]string{"run_id": record.ID},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	requestSum := sha256.Sum256(requestJSON)
	call.Request = requestJSON
	call.RequestDigest = "sha256:" + hex.EncodeToString(requestSum[:])
	if err := store.StartModelCall(ctx, call); err != nil {
		t.Fatal(err)
	}
	result := contract.ModelResult{
		Message: contract.Message{
			Role: contract.RoleAssistant, Content: "durable",
		},
		FinishReason: contract.FinishStop,
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(resultJSON)
	call.State = "completed"
	call.Result = resultJSON
	call.ResultDigest = "sha256:" + hex.EncodeToString(sum[:])
	if err := store.FinishModelCall(ctx, call); err != nil {
		t.Fatal(err)
	}
	if _, exists, err := store.LatestModelCall(
		ctx, record.ID,
	); err != nil || !exists {
		t.Fatalf("exists=%v error=%v", exists, err)
	}
	if _, err := store.db.ExecContext(
		ctx,
		`UPDATE model_calls
		    SET result_json = ?
		  WHERE model_call_id = ?`,
		[]byte(`{"message":{"role":"assistant","content":"forged"},"finish_reason":"stop"}`),
		call.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, _, err := store.LatestModelCall(
		ctx, record.ID,
	); err == nil || !strings.Contains(err.Error(), "digest") {
		t.Fatalf("tampered result error=%v", err)
	}
}

func TestEventsRejectsRowPayloadIdentityMismatch(t *testing.T) {
	ctx := context.Background()
	store := openTestStore(t)
	record := createTestRun(t, store)
	event, err := store.AppendEvent(ctx, record.ID, contract.Event{
		Type: contract.EventModelStarted,
	})
	if err != nil {
		t.Fatal(err)
	}
	event.Sequence++
	eventJSON, err := json.Marshal(event)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.db.ExecContext(
		ctx,
		`UPDATE events SET event_json = ?
		  WHERE run_id = ? AND sequence = 1`,
		eventJSON, record.ID,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Events(
		ctx, record.ID, 0, 10,
	); err == nil || !strings.Contains(err.Error(), "identity") {
		t.Fatalf("tampered event error=%v", err)
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

func TestMissingRunReturnsTypedNotFound(t *testing.T) {
	store := openTestStore(t)
	runID := "run_00000000000000000000000000000000"
	if _, err := store.Get(
		context.Background(), runID,
	); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Get error=%v", err)
	}
	if _, err := store.Resume(
		context.Background(), runID, json.RawMessage(`{}`),
		runtime.ResumeConstraint{
			Pause: json.RawMessage(`{"pause_id":"pause_missing"}`),
		},
	); !errors.Is(err, runtime.ErrNotFound) {
		t.Fatalf("Resume error=%v", err)
	}
}

func TestListUsesCanonicalFilterValidationAndDefault(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	for _, filter := range []runtime.ListFilter{
		{State: "future"},
		{Kind: "future"},
		{Limit: -1},
		{Limit: runtime.MaxListLimit + 1},
	} {
		if _, err := store.List(ctx, filter); err == nil {
			t.Fatalf("accepted invalid filter=%#v", filter)
		}
	}
	for index := 0; index <= runtime.DefaultListLimit; index++ {
		createTestRun(t, store)
	}
	values, err := store.List(ctx, runtime.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != runtime.DefaultListLimit {
		t.Fatalf(
			"default list returned %d records, want %d",
			len(values), runtime.DefaultListLimit,
		)
	}
	all, err := store.List(ctx, runtime.ListFilter{
		Limit: runtime.MaxListLimit,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(all) != runtime.DefaultListLimit+1 {
		t.Fatalf(
			"explicit list returned %d records, want %d",
			len(all), runtime.DefaultListLimit+1,
		)
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

func TestPrepareToolEffectCommitsCheckpointAndEffectAtomically(t *testing.T) {
	for _, testCase := range []struct {
		name       string
		failInsert bool
	}{
		{name: "commit"},
		{name: "rollback", failInsert: true},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			store := openTestStore(t)
			ctx := context.Background()
			record := createTestRun(t, store)
			if _, err := store.Start(ctx, record.ID); err != nil {
				t.Fatal(err)
			}
			if testCase.failInsert {
				if _, err := store.db.Exec(`
					CREATE TRIGGER fail_tool_effect_prepare
					BEFORE INSERT ON tool_effects
					BEGIN
					  SELECT RAISE(ABORT, 'injected tool effect failure');
					END
				`); err != nil {
					t.Fatal(err)
				}
			}
			request := agent.ToolRequest{
				RunID: record.ID, CallID: "call_atomic",
				IdempotencyKey: "idem_atomic", Name: "echo",
				Arguments: json.RawMessage(`{"value":"persisted"}`),
			}
			requestJSON, err := json.Marshal(request)
			if err != nil {
				t.Fatal(err)
			}
			stateJSON, err := json.Marshal(agent.LoopState{
				SchemaVersion: agent.LoopStateSchemaVersion,
				RunID:         record.ID, ModelProfile: "api", BaseMessageCount: 1,
				Messages: []contract.Message{{
					Role: contract.RoleUser, Content: "start",
				}},
				PendingToolCalls: []contract.ToolCall{{
					ID: request.CallID, Name: request.Name,
					Arguments: request.Arguments,
				}},
			})
			if err != nil {
				t.Fatal(err)
			}
			checkpoint := runtime.Checkpoint{
				ID:    "checkpoint_" + strings.Repeat("a", 32),
				RunID: record.ID, State: stateJSON,
			}
			err = store.PrepareToolEffect(ctx, runtime.ToolEffect{
				RunID: record.ID, CallID: request.CallID,
				IdempotencyKey: request.IdempotencyKey, Name: request.Name,
				Request: requestJSON,
			}, checkpoint)
			if testCase.failInsert {
				if err == nil ||
					!strings.Contains(err.Error(), "injected tool effect failure") {
					t.Fatalf("prepare error=%v", err)
				}
			} else if err != nil {
				t.Fatal(err)
			}
			_, checkpointExists, checkpointErr := store.LatestCheckpoint(
				ctx, record.ID,
			)
			if checkpointErr != nil {
				t.Fatal(checkpointErr)
			}
			effect, effectExists, effectErr := store.ToolEffect(
				ctx, record.ID, request.CallID,
			)
			if effectErr != nil {
				t.Fatal(effectErr)
			}
			if checkpointExists != !testCase.failInsert ||
				effectExists != !testCase.failInsert {
				t.Fatalf(
					"checkpointExists=%v effectExists=%v effect=%#v",
					checkpointExists, effectExists, effect,
				)
			}
			if effectExists &&
				(effect.State != "prepared" ||
					string(effect.Request) != string(requestJSON)) {
				t.Fatalf("effect=%#v", effect)
			}
		})
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
	}, runtime.Checkpoint{
		ID:    "checkpoint_" + strings.Repeat("1", 32),
		RunID: unknown.ID, State: json.RawMessage(`{"schema_version":2}`),
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

func TestOpenWithSkipReconcilePreservesRunningRun(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	store, err := Open(path, Options{})
	if err != nil {
		t.Fatal(err)
	}
	ctx := context.Background()
	record := createTestRun(t, store)
	if _, err := store.Start(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = Open(path, Options{SkipReconcile: true})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { store.Close() })
	current, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != runtime.StateRunning {
		t.Fatalf("state=%s", current.State)
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

func TestCancellationReservationOwnsTerminalAndReconciliationPublish(
	t *testing.T,
) {
	store := openTestStore(t)
	ctx := context.Background()

	terminal := createTestRun(t, store)
	if _, err := store.Start(ctx, terminal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestCancel(ctx, terminal.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Settle(
		ctx, terminal.ID, runtime.StateCompleted,
		json.RawMessage(`{"unexpected":true}`), nil,
	); !errors.Is(err, runtime.ErrCancelReserved) {
		t.Fatalf("ordinary terminal publish error=%v", err)
	}
	cancelled, err := store.SettleCancellation(
		ctx, terminal.ID, runtime.StateCancelled, nil,
		&contract.RuntimeError{
			Code: contract.ErrorCancelled, Phase: contract.PhaseRun,
			Message: "cancelled",
		},
	)
	if err != nil ||
		cancelled.State != runtime.StateCancelled ||
		!cancelled.CancelRequested ||
		cancelled.SettledSequence == 0 {
		t.Fatalf("cancelled=%#v error=%v", cancelled, err)
	}

	reconciliation := createTestRun(t, store)
	if _, err := store.Start(ctx, reconciliation.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestCancel(ctx, reconciliation.ID); err != nil {
		t.Fatal(err)
	}
	runtimeErr := &contract.RuntimeError{
		Code: contract.ErrorConflict, Phase: contract.PhaseRun,
		Message: "unknown effect",
	}
	if _, err := store.NeedsReconciliation(
		ctx, reconciliation.ID, runtimeErr,
	); !errors.Is(err, runtime.ErrCancelReserved) {
		t.Fatalf("ordinary reconciliation publish error=%v", err)
	}
	unknown, err := store.NeedsCancellationReconciliation(
		ctx, reconciliation.ID, runtimeErr,
	)
	if err != nil ||
		unknown.State != runtime.StateNeedsReconciliation ||
		!unknown.CancelRequested {
		t.Fatalf("unknown=%#v error=%v", unknown, err)
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
	queued := createTestRun(t, store)
	if _, err := store.Resume(
		ctx, queued.ID, json.RawMessage(`{"approved":true}`),
		runtime.ResumeConstraint{
			Pause: json.RawMessage(`{"pause_id":"pause_queued"}`),
		},
	); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("queued Resume error=%v", err)
	}
	value, err = store.Resume(
		ctx, paused.ID,
		json.RawMessage(`{"pause_id":"pause_1","input":{"approved":true}}`),
		runtime.ResumeConstraint{Pause: append([]byte(nil), value.Pause...)},
	)
	if err != nil {
		t.Fatal(err)
	}
	if value.State != runtime.StateQueued ||
		!strings.Contains(string(value.Request.Resume), "approved") {
		t.Fatalf("resumed=%#v", value)
	}
	cancelled, err := store.RequestCancel(ctx, queued.ID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != runtime.StateQueued ||
		!cancelled.CancelRequested ||
		cancelled.SettledSequence != 0 {
		t.Fatalf("cancelled=%#v", cancelled)
	}
	claimed, found, err := store.Claim(ctx, "worker")
	if err != nil || found && claimed.ID == queued.ID {
		t.Fatalf(
			"cancel-reserved queued run was claimable: run=%#v found=%v err=%v",
			claimed, found, err,
		)
	}
}

func TestResumePausedReservationCASPublishesExactlyOneJournalEntry(
	t *testing.T,
) {
	store := openTestStore(t)
	ctx := context.Background()
	record := createTestRun(t, store)
	if _, err := store.Start(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	paused, err := store.Pause(
		ctx, record.ID,
		json.RawMessage(`{"pause_id":"pause_race"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	constraint := runtime.ResumeConstraint{
		Pause: append([]byte(nil), paused.Pause...),
	}
	start := make(chan struct{})
	type result struct {
		value runtime.Record
		err   error
	}
	results := make(chan result, 2)
	for index := 0; index < 2; index++ {
		index := index
		go func() {
			<-start
			value, err := store.Resume(
				ctx, record.ID,
				json.RawMessage(fmt.Sprintf(
					`{"pause_id":"pause_race","input":{"choice":%d}}`,
					index,
				)),
				constraint,
			)
			results <- result{value: value, err: err}
		}()
	}
	close(start)
	successes := 0
	conflicts := 0
	for index := 0; index < 2; index++ {
		current := <-results
		switch {
		case current.err == nil:
			successes++
			if current.value.State != runtime.StateQueued {
				t.Fatalf("winner=%#v", current.value)
			}
		case errors.Is(current.err, runtime.ErrConflict):
			conflicts++
		default:
			t.Fatalf("resume error=%v", current.err)
		}
	}
	if successes != 1 || conflicts != 1 {
		t.Fatalf("successes=%d conflicts=%d", successes, conflicts)
	}
	resumes, err := store.Resumes(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumes) != 1 || resumes[0].Sequence != 1 {
		t.Fatalf("resume journal=%#v", resumes)
	}
}

func TestResumeReservationPreservesConflictDiagnostics(t *testing.T) {
	store := openTestStore(t)
	ctx := context.Background()
	input := json.RawMessage(`{"approved":true}`)

	queued := createTestRun(t, store)
	if _, err := store.Resume(
		ctx, queued.ID, input,
		runtime.ResumeConstraint{
			Pause: json.RawMessage(`{"pause_id":"pause_queued"}`),
		},
	); !errors.Is(err, runtime.ErrConflict) ||
		!strings.Contains(err.Error(), "is queued, not paused") {
		t.Fatalf("queued Resume error=%v", err)
	}

	cancelReserved := createTestRun(t, store)
	if _, err := store.Start(ctx, cancelReserved.ID); err != nil {
		t.Fatal(err)
	}
	cancelReserved, err := store.Pause(
		ctx, cancelReserved.ID,
		json.RawMessage(`{"pause_id":"pause_cancel_reserved"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.RequestCancel(ctx, cancelReserved.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resume(
		ctx, cancelReserved.ID, input,
		runtime.ResumeConstraint{
			Pause: append([]byte(nil), cancelReserved.Pause...),
		},
	); !errors.Is(err, runtime.ErrCancelReserved) {
		t.Fatalf("cancel-reserved Resume error=%v", err)
	}

	changed := createTestRun(t, store)
	if _, err := store.Start(ctx, changed.ID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Pause(
		ctx, changed.ID,
		json.RawMessage(`{"pause_id":"pause_current"}`),
	); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Resume(
		ctx, changed.ID, input,
		runtime.ResumeConstraint{
			Pause: json.RawMessage(`{"pause_id":"pause_stale"}`),
		},
	); !errors.Is(err, runtime.ErrConflict) ||
		!strings.Contains(err.Error(), "active pause changed before resume") {
		t.Fatalf("changed-pause Resume error=%v", err)
	}
}

func TestResumeSamplesAcceptanceAfterSQLiteWriterReservation(
	t *testing.T,
) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "runtime.db")
	var clockMu sync.Mutex
	current := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	sampled := make(chan struct{})
	var sampledOnce sync.Once
	monitorSampling := false
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		if monitorSampling {
			sampledOnce.Do(func() { close(sampled) })
		}
		return current
	}
	store, err := Open(
		databasePath, Options{Now: now, SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	record := createTestRun(t, store)
	if _, err := store.Start(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	pause := json.RawMessage(`{"pause_id":"pause_writer_lock"}`)
	paused, err := store.Pause(ctx, record.ID, pause)
	if err != nil {
		t.Fatal(err)
	}
	notAfter := current.Add(time.Second)

	blocker, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = blocker.Close() })
	connection, err := blocker.Conn(ctx)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = connection.Close() })
	if _, err := connection.ExecContext(
		ctx, "PRAGMA busy_timeout = 5000",
	); err != nil {
		t.Fatal(err)
	}
	if _, err := connection.ExecContext(ctx, "BEGIN IMMEDIATE"); err != nil {
		t.Fatal(err)
	}
	blockerOpen := true
	defer func() {
		if blockerOpen {
			_, _ = connection.ExecContext(
				context.Background(), "ROLLBACK",
			)
		}
	}()

	clockMu.Lock()
	monitorSampling = true
	clockMu.Unlock()
	type resumeResult struct {
		value runtime.Record
		err   error
	}
	finished := make(chan resumeResult, 1)
	go func() {
		value, err := store.Resume(
			ctx, record.ID,
			json.RawMessage(
				`{"pause_id":"pause_writer_lock","input":{"approved":true}}`,
			),
			runtime.ResumeConstraint{
				Pause: append([]byte(nil), paused.Pause...),
				NotAfter: func() *time.Time {
					value := notAfter
					return &value
				}(),
			},
		)
		finished <- resumeResult{value: value, err: err}
	}()
	select {
	case <-sampled:
		t.Fatal(
			"accepted_at was sampled before the SQLite writer reservation",
		)
	case <-time.After(250 * time.Millisecond):
	}
	clockMu.Lock()
	current = notAfter.Add(time.Second)
	clockMu.Unlock()
	if _, err := connection.ExecContext(ctx, "ROLLBACK"); err != nil {
		t.Fatal(err)
	}
	blockerOpen = false
	result := <-finished
	if !errors.Is(result.err, runtime.ErrConflict) ||
		!strings.Contains(result.err.Error(), "pause has expired") {
		t.Fatalf("resume=%#v err=%v", result.value, result.err)
	}
	select {
	case <-sampled:
	default:
		t.Fatal("accepted_at was not sampled after releasing the writer")
	}
	resumes, err := store.Resumes(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	currentRecord, err := store.Get(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumes) != 0 ||
		currentRecord.State != runtime.StatePaused ||
		!bytes.Equal(currentRecord.Pause, paused.Pause) ||
		len(currentRecord.Request.Resume) != 0 ||
		currentRecord.ResumeAcceptedAt != nil {
		t.Fatalf(
			"expired resume mutated durable state: resumes=%#v record=%#v",
			resumes, currentRecord,
		)
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
