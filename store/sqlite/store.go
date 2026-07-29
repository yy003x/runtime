// Package sqlite implements the durable Runtime Run Store on SQLite WAL.
package sqlite

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"time"

	_ "modernc.org/sqlite"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
	runtime "github.com/yy003x/runtime/run"
)

const (
	schemaVersion   = 2
	maxPayloadBytes = 4 << 20
)

type Store struct {
	db  *sql.DB
	now func() time.Time
}

type Options struct {
	Now func() time.Time
}

func Open(path string, options Options) (*Store, error) {
	path = strings.TrimSpace(path)
	if path == "" {
		return nil, fmt.Errorf("SQLite path is required")
	}
	path = filepath.Clean(path)
	if err := prepareDatabasePath(path); err != nil {
		return nil, err
	}
	db, err := sql.Open("sqlite", path)
	if err != nil {
		return nil, fmt.Errorf("open SQLite: %w", err)
	}
	db.SetMaxOpenConns(1)
	db.SetMaxIdleConns(1)
	for _, statement := range []string{
		"PRAGMA foreign_keys = ON",
		"PRAGMA busy_timeout = 5000",
		"PRAGMA synchronous = FULL",
	} {
		if _, err := db.Exec(statement); err != nil {
			db.Close()
			return nil, fmt.Errorf("%s: %w", statement, err)
		}
	}
	var existingVersion int
	if err := db.QueryRow("PRAGMA user_version").Scan(&existingVersion); err != nil {
		db.Close()
		return nil, err
	}
	if existingVersion != 0 && existingVersion != schemaVersion {
		db.Close()
		return nil, fmt.Errorf(
			"unsupported Runtime database schema %d; expected %d",
			existingVersion, schemaVersion,
		)
	}
	if err := quickCheck(db); err != nil {
		db.Close()
		return nil, err
	}
	var journalMode string
	if err := db.QueryRow("PRAGMA journal_mode = WAL").Scan(&journalMode); err != nil {
		db.Close()
		return nil, fmt.Errorf("enable SQLite WAL: %w", err)
	}
	if !strings.EqualFold(journalMode, "wal") {
		db.Close()
		return nil, fmt.Errorf("SQLite journal_mode=%q, want wal", journalMode)
	}
	if err := migrate(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := validateRunRows(db); err != nil {
		db.Close()
		return nil, err
	}
	if err := os.Chmod(path, 0o600); err != nil {
		db.Close()
		return nil, fmt.Errorf("restrict SQLite permissions: %w", err)
	}
	now := options.Now
	if now == nil {
		now = time.Now
	}
	store := &Store{db: db, now: now}
	if err := store.Reconcile(context.Background()); err != nil {
		db.Close()
		return nil, fmt.Errorf("reconcile SQLite Run Store: %w", err)
	}
	return store, nil
}

func quickCheck(db *sql.DB) error {
	rows, err := db.Query("PRAGMA quick_check")
	if err != nil {
		return fmt.Errorf("check Runtime database integrity: %w", err)
	}
	defer rows.Close()
	var results []string
	for rows.Next() {
		var value string
		if err := rows.Scan(&value); err != nil {
			return fmt.Errorf("check Runtime database integrity: %w", err)
		}
		results = append(results, value)
	}
	if err := rows.Err(); err != nil {
		return fmt.Errorf("check Runtime database integrity: %w", err)
	}
	if len(results) != 1 || !strings.EqualFold(results[0], "ok") {
		return fmt.Errorf(
			"Runtime database integrity check failed: %s",
			strings.Join(results, "; "),
		)
	}
	return nil
}

func migrate(db *sql.DB) error {
	var version int
	if err := db.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return err
	}
	if version != 0 && version != schemaVersion {
		return fmt.Errorf(
			"unsupported Runtime database schema %d; expected %d",
			version, schemaVersion,
		)
	}
	if version == schemaVersion {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	statements := []string{
		`CREATE TABLE runs (
			run_id TEXT PRIMARY KEY,
			kind TEXT NOT NULL,
			session_id TEXT,
			state TEXT NOT NULL,
			request_json BLOB NOT NULL,
			private_request_json BLOB,
			result_json BLOB,
			error_json BLOB,
			pause_json BLOB,
			retry_of TEXT,
			cancel_requested INTEGER NOT NULL DEFAULT 0,
			settled_sequence INTEGER NOT NULL DEFAULT 0,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE INDEX runs_state_created ON runs(state, created_at)`,
		`CREATE UNIQUE INDEX runs_one_open_session
		   ON runs(session_id)
		 WHERE kind = 'session'
		   AND session_id IS NOT NULL
		   AND state IN ('queued', 'running', 'paused', 'needs_reconciliation')`,
		`CREATE TABLE events (
			run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
			sequence INTEGER NOT NULL,
			event_json BLOB NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (run_id, sequence)
		)`,
		`CREATE TABLE checkpoints (
			checkpoint_id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
			sequence INTEGER NOT NULL,
			state_json BLOB NOT NULL,
			created_at TEXT NOT NULL
		)`,
		`CREATE INDEX checkpoints_run_sequence ON checkpoints(run_id, sequence DESC)`,
		`CREATE TABLE model_calls (
			model_call_id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
			sequence INTEGER NOT NULL,
			request_digest TEXT NOT NULL,
			provider_request_id TEXT,
			state TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL
		)`,
		`CREATE TABLE tool_effects (
			run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
			call_id TEXT NOT NULL,
			idempotency_key TEXT NOT NULL,
			name TEXT NOT NULL,
			state TEXT NOT NULL,
			request_json BLOB NOT NULL,
			result_json BLOB,
			error_json BLOB,
			updated_at TEXT NOT NULL,
			PRIMARY KEY (run_id, call_id),
			UNIQUE (run_id, idempotency_key)
		)`,
		`CREATE TABLE queue (
			run_id TEXT PRIMARY KEY REFERENCES runs(run_id) ON DELETE CASCADE,
			available_at TEXT NOT NULL,
			claimed_by TEXT,
			claimed_at TEXT
		)`,
		fmt.Sprintf("PRAGMA user_version = %d", schemaVersion),
	}
	for _, statement := range statements {
		if _, err := tx.Exec(statement); err != nil {
			return fmt.Errorf("migrate Runtime database: %w", err)
		}
	}
	return tx.Commit()
}

func validateRunRows(db *sql.DB) error {
	rows, err := db.Query(
		`SELECT run_id, kind, session_id, state, request_json,
		        private_request_json, settled_sequence
		   FROM runs`,
	)
	if err != nil {
		return err
	}
	defer rows.Close()
	for rows.Next() {
		var runID string
		var kind runtime.Kind
		var sessionID sql.NullString
		var state runtime.State
		var requestJSON, privateJSON []byte
		var settledSequence uint64
		if err := rows.Scan(
			&runID, &kind, &sessionID, &state, &requestJSON,
			&privateJSON, &settledSequence,
		); err != nil {
			return err
		}
		if err := identity.Validate(runID, "run"); err != nil {
			return fmt.Errorf("Runtime database has invalid run_id: %w", err)
		}
		switch kind {
		case runtime.KindAgent, runtime.KindSession:
		default:
			return fmt.Errorf(
				"run %s has unsupported kind %q", runID, kind,
			)
		}
		switch state {
		case runtime.StateQueued, runtime.StateRunning, runtime.StatePaused,
			runtime.StateNeedsReconciliation:
			if settledSequence != 0 {
				return fmt.Errorf(
					"nonterminal run %s has settled_sequence=%d",
					runID, settledSequence,
				)
			}
		case runtime.StateCompleted, runtime.StateFailed, runtime.StateCancelled:
			if settledSequence == 0 {
				return fmt.Errorf(
					"terminal run %s has no settled_sequence", runID,
				)
			}
		default:
			return fmt.Errorf(
				"run %s has unsupported state %q", runID, state,
			)
		}
		var request runtime.Request
		if err := decodeStrict(requestJSON, &request); err != nil {
			return fmt.Errorf("run %s request: %w", runID, err)
		}
		if request.Kind != kind ||
			request.SessionID != sessionID.String {
			return fmt.Errorf(
				"run %s request identity does not match indexed columns",
				runID,
			)
		}
		if _, err := marshalPrivateRequest(privateJSON); err != nil {
			return fmt.Errorf("run %s private request: %w", runID, err)
		}
	}
	return rows.Err()
}

func (store *Store) Create(
	ctx context.Context,
	runID string,
	request runtime.Request,
) (runtime.Record, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return runtime.Record{}, err
	}
	requestJSON, err := marshalPayload(request)
	if err != nil {
		return runtime.Record{}, err
	}
	privateRequestJSON, err := marshalPrivateRequest(request.PrivateRequest)
	if err != nil {
		return runtime.Record{}, err
	}
	now := store.now().UTC()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return runtime.Record{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	if request.Kind == runtime.KindSession {
		var existing string
		err := tx.QueryRowContext(
			ctx,
			`SELECT run_id
			   FROM runs
			  WHERE kind = ? AND session_id = ?
			    AND state IN (?, ?, ?, ?)
			  LIMIT 1`,
			runtime.KindSession, request.SessionID,
			runtime.StateQueued, runtime.StateRunning, runtime.StatePaused,
			runtime.StateNeedsReconciliation,
		).Scan(&existing)
		if err == nil {
			return runtime.Record{}, fmt.Errorf(
				"%w: session %s is owned by run %s",
				runtime.ErrSessionRunOpen, request.SessionID, existing,
			)
		}
		if !errors.Is(err, sql.ErrNoRows) {
			return runtime.Record{}, err
		}
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO runs (
			run_id, kind, session_id, state, request_json,
			private_request_json, retry_of, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		runID, request.Kind, nullString(request.SessionID),
		runtime.StateQueued, requestJSON, nullableBytes(privateRequestJSON),
		nullString(request.RetryOf), formatTime(now), formatTime(now),
	); err != nil {
		if request.Kind == runtime.KindSession &&
			strings.Contains(strings.ToLower(err.Error()), "unique") {
			return runtime.Record{}, fmt.Errorf(
				"%w: session %s",
				runtime.ErrSessionRunOpen, request.SessionID,
			)
		}
		return runtime.Record{}, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO queue (run_id, available_at) VALUES (?, ?)`,
		runID, formatTime(now),
	); err != nil {
		return runtime.Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return runtime.Record{}, err
	}
	return store.Get(ctx, runID)
}

func (store *Store) Get(
	ctx context.Context,
	runID string,
) (runtime.Record, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return runtime.Record{}, err
	}
	return scanRecord(store.db.QueryRowContext(
		ctx,
		`SELECT run_id, state, request_json, result_json, error_json,
		        pause_json, retry_of, cancel_requested, settled_sequence,
		        created_at, updated_at
		   FROM runs WHERE run_id = ?`,
		runID,
	))
}

func (store *Store) PrivateRequest(
	ctx context.Context,
	runID string,
) (json.RawMessage, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return nil, err
	}
	var value []byte
	if err := store.db.QueryRowContext(
		ctx,
		`SELECT private_request_json FROM runs WHERE run_id = ?`,
		runID,
	).Scan(&value); err != nil {
		return nil, err
	}
	if _, err := marshalPrivateRequest(value); err != nil {
		return nil, err
	}
	return cloneBytes(value), nil
}

func (store *Store) List(
	ctx context.Context,
	filter runtime.ListFilter,
) ([]runtime.Record, error) {
	if filter.Limit <= 0 || filter.Limit > 1000 {
		filter.Limit = 100
	}
	query := `SELECT run_id, state, request_json, result_json, error_json,
	                 pause_json, retry_of, cancel_requested, settled_sequence,
	                 created_at, updated_at
	            FROM runs WHERE 1=1`
	var arguments []any
	if filter.State != "" {
		query += " AND state = ?"
		arguments = append(arguments, filter.State)
	}
	if filter.Kind != "" {
		query += " AND kind = ?"
		arguments = append(arguments, filter.Kind)
	}
	query += " ORDER BY created_at DESC LIMIT ?"
	arguments = append(arguments, filter.Limit)
	rows, err := store.db.QueryContext(ctx, query, arguments...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []runtime.Record
	for rows.Next() {
		value, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (store *Store) Start(
	ctx context.Context,
	runID string,
) (runtime.Record, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return runtime.Record{}, err
	}
	now := store.now().UTC()
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return runtime.Record{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	var state runtime.State
	if err := tx.QueryRowContext(
		ctx, `SELECT state FROM runs WHERE run_id = ?`, runID,
	).Scan(&state); err != nil {
		return runtime.Record{}, err
	}
	if state != runtime.StateQueued {
		return runtime.Record{}, fmt.Errorf("run %s is %s, not queued", runID, state)
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE runs SET state = ?, updated_at = ? WHERE run_id = ?`,
		runtime.StateRunning, formatTime(now), runID,
	); err != nil {
		return runtime.Record{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM queue WHERE run_id = ?`, runID); err != nil {
		return runtime.Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return runtime.Record{}, err
	}
	return store.Get(ctx, runID)
}

func (store *Store) Claim(
	ctx context.Context,
	workerID string,
) (runtime.Record, bool, error) {
	if strings.TrimSpace(workerID) == "" {
		return runtime.Record{}, false, fmt.Errorf("worker ID is required")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return runtime.Record{}, false, err
	}
	defer tx.Rollback() //nolint:errcheck
	var runID string
	err = tx.QueryRowContext(
		ctx,
		`SELECT q.run_id
		   FROM queue q
		   JOIN runs r ON r.run_id = q.run_id
		  WHERE r.state = ? AND q.available_at <= ?
		  ORDER BY q.available_at, r.created_at
		  LIMIT 1`,
		runtime.StateQueued, formatTime(store.now().UTC()),
	).Scan(&runID)
	if errors.Is(err, sql.ErrNoRows) {
		return runtime.Record{}, false, nil
	}
	if err != nil {
		return runtime.Record{}, false, err
	}
	now := formatTime(store.now().UTC())
	result, err := tx.ExecContext(
		ctx,
		`UPDATE runs SET state = ?, updated_at = ?
		  WHERE run_id = ? AND state = ?`,
		runtime.StateRunning, now, runID, runtime.StateQueued,
	)
	if err != nil {
		return runtime.Record{}, false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return runtime.Record{}, false, err
	}
	if affected != 1 {
		return runtime.Record{}, false, nil
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM queue WHERE run_id = ?`, runID); err != nil {
		return runtime.Record{}, false, err
	}
	if err := tx.Commit(); err != nil {
		return runtime.Record{}, false, err
	}
	value, err := store.Get(ctx, runID)
	return value, err == nil, err
}

func (store *Store) AppendEvent(
	ctx context.Context,
	runID string,
	event contract.Event,
) (contract.Event, error) {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return contract.Event{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	var state runtime.State
	var settled uint64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT state, settled_sequence FROM runs WHERE run_id = ?`, runID,
	).Scan(&state, &settled); err != nil {
		return contract.Event{}, err
	}
	if state.Terminal() || settled != 0 {
		return contract.Event{}, fmt.Errorf("run %s is settled", runID)
	}
	sequence, err := nextSequence(ctx, tx, runID)
	if err != nil {
		return contract.Event{}, err
	}
	if event.Sequence != 0 && event.Sequence != sequence {
		return contract.Event{}, fmt.Errorf(
			"event sequence=%d, next durable sequence=%d",
			event.Sequence, sequence,
		)
	}
	event.Sequence = sequence
	if event.Time == nil {
		now := store.now().UTC()
		event.Time = &now
	}
	if err := event.Validate(); err != nil {
		return contract.Event{}, err
	}
	if err := insertEvent(ctx, tx, runID, event); err != nil {
		return contract.Event{}, err
	}
	if _, err := tx.ExecContext(
		ctx, `UPDATE runs SET updated_at = ? WHERE run_id = ?`,
		formatTime(store.now().UTC()), runID,
	); err != nil {
		return contract.Event{}, err
	}
	if err := tx.Commit(); err != nil {
		return contract.Event{}, err
	}
	return event, nil
}

func (store *Store) Events(
	ctx context.Context,
	runID string,
	afterSequence uint64,
	limit int,
) ([]contract.Event, error) {
	if limit <= 0 || limit > 1000 {
		limit = 256
	}
	rows, err := store.db.QueryContext(
		ctx,
		`SELECT event_json FROM events
		  WHERE run_id = ? AND sequence > ?
		  ORDER BY sequence LIMIT ?`,
		runID, afterSequence, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	var values []contract.Event
	for rows.Next() {
		var data []byte
		if err := rows.Scan(&data); err != nil {
			return nil, err
		}
		var value contract.Event
		if err := decodeStrict(data, &value); err != nil {
			return nil, err
		}
		if err := value.Validate(); err != nil {
			return nil, err
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (store *Store) SaveCheckpoint(
	ctx context.Context,
	checkpoint runtime.Checkpoint,
) error {
	if err := identity.Validate(checkpoint.ID, "checkpoint"); err != nil {
		return err
	}
	if err := identity.Validate(checkpoint.RunID, "run"); err != nil {
		return err
	}
	if len(checkpoint.State) == 0 || len(checkpoint.State) > maxPayloadBytes ||
		!json.Valid(checkpoint.State) {
		return fmt.Errorf("checkpoint state is invalid or too large")
	}
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = store.now().UTC()
	}
	_, err := store.db.ExecContext(
		ctx,
		`INSERT INTO checkpoints (
			checkpoint_id, run_id, sequence, state_json, created_at
		) VALUES (?, ?, ?, ?, ?)`,
		checkpoint.ID, checkpoint.RunID, checkpoint.Sequence, checkpoint.State,
		formatTime(checkpoint.CreatedAt),
	)
	return err
}

func (store *Store) LatestCheckpoint(
	ctx context.Context,
	runID string,
) (runtime.Checkpoint, bool, error) {
	var value runtime.Checkpoint
	var created string
	err := store.db.QueryRowContext(
		ctx,
		`SELECT checkpoint_id, run_id, sequence, state_json, created_at
		   FROM checkpoints WHERE run_id = ?
		  ORDER BY sequence DESC, created_at DESC LIMIT 1`,
		runID,
	).Scan(&value.ID, &value.RunID, &value.Sequence, &value.State, &created)
	if errors.Is(err, sql.ErrNoRows) {
		return runtime.Checkpoint{}, false, nil
	}
	if err != nil {
		return runtime.Checkpoint{}, false, err
	}
	value.CreatedAt, err = parseTime(created)
	return value, err == nil, err
}

func (store *Store) StartModelCall(
	ctx context.Context,
	call runtime.ModelCall,
) error {
	if err := identity.Validate(call.ID, "model_call"); err != nil {
		return err
	}
	if call.Sequence <= 0 || call.RequestDigest == "" {
		return fmt.Errorf("model call sequence and request digest are required")
	}
	now := store.now().UTC()
	if call.CreatedAt.IsZero() {
		call.CreatedAt = now
	}
	_, err := store.db.ExecContext(
		ctx,
		`INSERT INTO model_calls (
			model_call_id, run_id, sequence, request_digest,
			provider_request_id, state, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, 'running', ?, ?)`,
		call.ID, call.RunID, call.Sequence, call.RequestDigest,
		nullString(call.ProviderRequestID), formatTime(call.CreatedAt),
		formatTime(now),
	)
	return err
}

func (store *Store) FinishModelCall(
	ctx context.Context,
	call runtime.ModelCall,
) error {
	if call.State != "completed" && call.State != "failed" &&
		call.State != "cancelled" {
		return fmt.Errorf("invalid model call terminal state %q", call.State)
	}
	result, err := store.db.ExecContext(
		ctx,
		`UPDATE model_calls
		    SET state = ?, provider_request_id = ?, updated_at = ?
		  WHERE model_call_id = ? AND run_id = ? AND state = 'running'`,
		call.State, nullString(call.ProviderRequestID),
		formatTime(store.now().UTC()), call.ID, call.RunID,
	)
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("model call %s has invalid state", call.ID)
	}
	return nil
}

func (store *Store) PrepareToolEffect(
	ctx context.Context,
	effect runtime.ToolEffect,
) error {
	requestJSON, err := marshalPayload(effect.Request)
	if err != nil {
		return err
	}
	_, err = store.db.ExecContext(
		ctx,
		`INSERT INTO tool_effects (
			run_id, call_id, idempotency_key, name, state,
			request_json, updated_at
		) VALUES (?, ?, ?, ?, 'prepared', ?, ?)`,
		effect.RunID, effect.CallID, effect.IdempotencyKey, effect.Name,
		requestJSON, formatTime(store.now().UTC()),
	)
	return err
}

func (store *Store) StartToolEffect(
	ctx context.Context,
	runID, callID string,
) error {
	return updateEffectState(
		ctx, store.db, runID, callID, "prepared", "started",
		formatTime(store.now().UTC()),
	)
}

func (store *Store) CompleteToolEffect(
	ctx context.Context,
	effect runtime.ToolEffect,
) error {
	resultJSON, err := marshalPayload(effect.Result)
	if err != nil {
		return err
	}
	result, err := store.db.ExecContext(
		ctx,
		`UPDATE tool_effects
		    SET state = 'completed', result_json = ?, error_json = NULL,
		        updated_at = ?
		  WHERE run_id = ? AND call_id = ? AND state = 'started'`,
		resultJSON, formatTime(store.now().UTC()), effect.RunID, effect.CallID,
	)
	return requireOneEffect(result, err, effect.RunID, effect.CallID)
}

func (store *Store) FailToolEffect(
	ctx context.Context,
	effect runtime.ToolEffect,
) error {
	errorJSON, err := marshalNullable(effect.Error)
	if err != nil {
		return err
	}
	result, err := store.db.ExecContext(
		ctx,
		`UPDATE tool_effects
		    SET state = 'failed', error_json = ?, updated_at = ?
		  WHERE run_id = ? AND call_id = ?
		    AND state IN ('prepared', 'started')`,
		errorJSON, formatTime(store.now().UTC()), effect.RunID, effect.CallID,
	)
	return requireOneEffect(result, err, effect.RunID, effect.CallID)
}

func (store *Store) Pause(
	ctx context.Context,
	runID string,
	pause json.RawMessage,
) (runtime.Record, error) {
	if len(pause) == 0 || len(pause) > maxPayloadBytes || !json.Valid(pause) {
		return runtime.Record{}, fmt.Errorf("pause payload is invalid or too large")
	}
	return store.transitionNonTerminal(ctx, runID, runtime.StatePaused, pause, nil)
}

func (store *Store) NeedsReconciliation(
	ctx context.Context,
	runID string,
	runtimeErr *contract.RuntimeError,
) (runtime.Record, error) {
	return store.transitionNonTerminal(
		ctx, runID, runtime.StateNeedsReconciliation, nil, runtimeErr,
	)
}

func (store *Store) transitionNonTerminal(
	ctx context.Context,
	runID string,
	state runtime.State,
	pause json.RawMessage,
	runtimeErr *contract.RuntimeError,
) (runtime.Record, error) {
	errorJSON, err := marshalNullable(runtimeErr)
	if err != nil {
		return runtime.Record{}, err
	}
	result, err := store.db.ExecContext(
		ctx,
		`UPDATE runs
		    SET state = ?, pause_json = ?, error_json = ?,
		        updated_at = ?
		  WHERE run_id = ? AND state = ?`,
		state, nullableBytes(pause), errorJSON, formatTime(store.now().UTC()),
		runID, runtime.StateRunning,
	)
	if err != nil {
		return runtime.Record{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return runtime.Record{}, err
	}
	if affected != 1 {
		return runtime.Record{}, fmt.Errorf("run %s is not running", runID)
	}
	return store.Get(ctx, runID)
}

func (store *Store) Settle(
	ctx context.Context,
	runID string,
	state runtime.State,
	resultJSON json.RawMessage,
	runtimeErr *contract.RuntimeError,
) (runtime.Record, error) {
	if !state.Terminal() {
		return runtime.Record{}, fmt.Errorf("state %q is not terminal", state)
	}
	if len(resultJSON) > maxPayloadBytes ||
		len(resultJSON) > 0 && !json.Valid(resultJSON) {
		return runtime.Record{}, fmt.Errorf("terminal result is invalid or too large")
	}
	errorJSON, err := marshalNullable(runtimeErr)
	if err != nil {
		return runtime.Record{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return runtime.Record{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	var current runtime.State
	var settled uint64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT state, settled_sequence FROM runs WHERE run_id = ?`, runID,
	).Scan(&current, &settled); err != nil {
		return runtime.Record{}, err
	}
	if current.Terminal() || settled != 0 {
		return runtime.Record{}, fmt.Errorf("run %s is already settled", runID)
	}
	if current != runtime.StateRunning && current != runtime.StateQueued &&
		current != runtime.StatePaused &&
		current != runtime.StateNeedsReconciliation {
		return runtime.Record{}, fmt.Errorf(
			"run %s in state %s cannot settle as %s", runID, current, state,
		)
	}
	sequence, err := nextSequence(ctx, tx, runID)
	if err != nil {
		return runtime.Record{}, err
	}
	terminalType := contract.EventRunCompleted
	if state == runtime.StateFailed {
		terminalType = contract.EventRunFailed
	}
	if state == runtime.StateCancelled {
		terminalType = contract.EventRunCancelled
	}
	now := store.now().UTC()
	terminal := contract.Event{
		Sequence: sequence, Time: &now, Type: terminalType,
		Run: &contract.RunEvent{RunID: runID, State: string(state)},
	}
	if state == runtime.StateFailed {
		terminal.Error = runtimeErr
		if terminal.Error == nil {
			terminal.Error = &contract.RuntimeError{
				Code: contract.ErrorInternal, Phase: contract.PhaseRun,
				Message: "run failed without a typed error",
			}
			errorJSON, err = marshalNullable(terminal.Error)
			if err != nil {
				return runtime.Record{}, err
			}
		}
	}
	if err := terminal.Validate(); err != nil {
		return runtime.Record{}, err
	}
	if err := insertEvent(ctx, tx, runID, terminal); err != nil {
		return runtime.Record{}, err
	}
	settledSequence := sequence + 1
	settledEvent := contract.Event{
		Sequence: settledSequence, Time: &now, Type: contract.EventRunSettled,
		Run: &contract.RunEvent{RunID: runID, State: string(state)},
	}
	if err := settledEvent.Validate(); err != nil {
		return runtime.Record{}, err
	}
	if err := insertEvent(ctx, tx, runID, settledEvent); err != nil {
		return runtime.Record{}, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE runs
		    SET state = ?, result_json = ?, error_json = ?, pause_json = NULL,
		        settled_sequence = ?, updated_at = ?
		  WHERE run_id = ?`,
		state, nullableBytes(resultJSON), errorJSON, settledSequence,
		formatTime(now), runID,
	); err != nil {
		return runtime.Record{}, err
	}
	if _, err := tx.ExecContext(ctx, `DELETE FROM queue WHERE run_id = ?`, runID); err != nil {
		return runtime.Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return runtime.Record{}, err
	}
	return store.Get(ctx, runID)
}

func (store *Store) RequestCancel(
	ctx context.Context,
	runID string,
) (runtime.Record, error) {
	value, err := store.Get(ctx, runID)
	if err != nil {
		return runtime.Record{}, err
	}
	switch value.State {
	case runtime.StateQueued, runtime.StatePaused:
		runtimeErr := &contract.RuntimeError{
			Code: contract.ErrorCancelled, Phase: contract.PhaseRun,
			Message: "run was cancelled",
		}
		return store.Settle(ctx, runID, runtime.StateCancelled, nil, runtimeErr)
	case runtime.StateRunning:
		_, err := store.db.ExecContext(
			ctx,
			`UPDATE runs SET cancel_requested = 1, updated_at = ?
			  WHERE run_id = ? AND state = ?`,
			formatTime(store.now().UTC()), runID, runtime.StateRunning,
		)
		if err != nil {
			return runtime.Record{}, err
		}
		return store.Get(ctx, runID)
	case runtime.StateNeedsReconciliation:
		return runtime.Record{}, fmt.Errorf(
			"run %s needs reconciliation and cannot be marked safely cancelled", runID,
		)
	default:
		return value, nil
	}
}

func (store *Store) Resume(
	ctx context.Context,
	runID string,
	input json.RawMessage,
) (runtime.Record, error) {
	if len(input) == 0 || len(input) > maxPayloadBytes || !json.Valid(input) {
		return runtime.Record{}, fmt.Errorf("resume input is invalid or too large")
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return runtime.Record{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	var state runtime.State
	var requestJSON []byte
	if err := tx.QueryRowContext(
		ctx,
		`SELECT state, request_json FROM runs WHERE run_id = ?`, runID,
	).Scan(&state, &requestJSON); err != nil {
		return runtime.Record{}, err
	}
	if state != runtime.StatePaused {
		return runtime.Record{}, fmt.Errorf("run %s is %s, not paused", runID, state)
	}
	var request runtime.Request
	if err := decodeStrict(requestJSON, &request); err != nil {
		return runtime.Record{}, err
	}
	request.Resume = append([]byte(nil), input...)
	requestJSON, err = marshalPayload(request)
	if err != nil {
		return runtime.Record{}, err
	}
	now := formatTime(store.now().UTC())
	if _, err := tx.ExecContext(
		ctx,
		`UPDATE runs
		    SET state = ?, request_json = ?, pause_json = NULL,
		        error_json = NULL, cancel_requested = 0, updated_at = ?
		  WHERE run_id = ?`,
		runtime.StateQueued, requestJSON, now, runID,
	); err != nil {
		return runtime.Record{}, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO queue (run_id, available_at) VALUES (?, ?)
		 ON CONFLICT(run_id) DO UPDATE SET
		   available_at = excluded.available_at,
		   claimed_by = NULL,
		   claimed_at = NULL`,
		runID, now,
	); err != nil {
		return runtime.Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return runtime.Record{}, err
	}
	return store.Get(ctx, runID)
}

func (store *Store) Reconcile(ctx context.Context) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	rows, err := tx.QueryContext(
		ctx, `SELECT run_id, kind FROM runs WHERE state = ?`, runtime.StateRunning,
	)
	if err != nil {
		return err
	}
	type runningRun struct {
		id   string
		kind runtime.Kind
	}
	var running []runningRun
	for rows.Next() {
		var value runningRun
		if err := rows.Scan(&value.id, &value.kind); err != nil {
			rows.Close()
			return err
		}
		running = append(running, value)
	}
	if err := rows.Close(); err != nil {
		return err
	}
	if err := rows.Err(); err != nil {
		return err
	}
	now := formatTime(store.now().UTC())
	for _, current := range running {
		runID := current.id
		if current.kind == runtime.KindSession {
			runtimeErr := &contract.RuntimeError{
				Code: contract.ErrorConflict, Phase: contract.PhaseRun,
				Message: "Session execution outcome is unknown after process restart",
			}
			errorJSON, err := marshalNullable(runtimeErr)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE runs
				    SET state = ?, error_json = ?, updated_at = ?
				  WHERE run_id = ?`,
				runtime.StateNeedsReconciliation, errorJSON, now, runID,
			); err != nil {
				return err
			}
			if _, err := tx.ExecContext(
				ctx, `DELETE FROM queue WHERE run_id = ?`, runID,
			); err != nil {
				return err
			}
			continue
		}
		var started int
		if err := tx.QueryRowContext(
			ctx,
			`SELECT COUNT(*) FROM tool_effects
			  WHERE run_id = ? AND state = 'started'`,
			runID,
		).Scan(&started); err != nil {
			return err
		}
		if started > 0 {
			runtimeErr := &contract.RuntimeError{
				Code: contract.ErrorConflict, Phase: contract.PhaseRun,
				Message: "tool effect outcome is unknown after process restart",
			}
			errorJSON, err := marshalNullable(runtimeErr)
			if err != nil {
				return err
			}
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE runs
				    SET state = ?, error_json = ?, updated_at = ?
				  WHERE run_id = ?`,
				runtime.StateNeedsReconciliation, errorJSON, now, runID,
			); err != nil {
				return err
			}
			continue
		}
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE runs SET state = ?, updated_at = ? WHERE run_id = ?`,
			runtime.StateQueued, now, runID,
		); err != nil {
			return err
		}
		if _, err := tx.ExecContext(
			ctx,
			`INSERT INTO queue (run_id, available_at) VALUES (?, ?)
			 ON CONFLICT(run_id) DO UPDATE SET
			   available_at = excluded.available_at,
			   claimed_by = NULL,
			   claimed_at = NULL`,
			runID, now,
		); err != nil {
			return err
		}
	}
	return tx.Commit()
}

func (store *Store) GC(
	ctx context.Context,
	options runtime.GCOptions,
) (runtime.GCResult, error) {
	if options.Before.IsZero() {
		return runtime.GCResult{}, fmt.Errorf("Run GC cutoff is required")
	}
	if options.Limit <= 0 || options.Limit > 1000 {
		options.Limit = 100
	}
	rows, err := store.db.QueryContext(
		ctx,
		`SELECT run_id
		   FROM runs
		  WHERE state IN (?, ?, ?) AND updated_at < ?
		  ORDER BY updated_at ASC, run_id ASC
		  LIMIT ?`,
		runtime.StateCompleted, runtime.StateFailed, runtime.StateCancelled,
		formatTime(options.Before.UTC()), options.Limit,
	)
	if err != nil {
		return runtime.GCResult{}, err
	}
	var candidates []string
	for rows.Next() {
		var runID string
		if err := rows.Scan(&runID); err != nil {
			rows.Close()
			return runtime.GCResult{}, err
		}
		candidates = append(candidates, runID)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return runtime.GCResult{}, err
	}
	if err := rows.Close(); err != nil {
		return runtime.GCResult{}, err
	}
	result := runtime.GCResult{
		Candidates: candidates, Applied: options.Apply,
	}
	if !options.Apply || len(candidates) == 0 {
		return result, nil
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return runtime.GCResult{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, runID := range candidates {
		deleted, err := tx.ExecContext(
			ctx,
			`DELETE FROM runs
			  WHERE run_id = ?
			    AND state IN (?, ?, ?)
			    AND updated_at < ?`,
			runID, runtime.StateCompleted, runtime.StateFailed,
			runtime.StateCancelled, formatTime(options.Before.UTC()),
		)
		if err != nil {
			return runtime.GCResult{}, err
		}
		count, err := deleted.RowsAffected()
		if err != nil {
			return runtime.GCResult{}, err
		}
		if count == 1 {
			result.Deleted = append(result.Deleted, runID)
		}
	}
	if err := tx.Commit(); err != nil {
		return runtime.GCResult{}, err
	}
	return result, nil
}

func (store *Store) Close() error {
	if store == nil || store.db == nil {
		return nil
	}
	return store.db.Close()
}

type scanner interface {
	Scan(...any) error
}

func scanRecord(row scanner) (runtime.Record, error) {
	var value runtime.Record
	var requestJSON []byte
	var resultJSON, errorJSON, pauseJSON []byte
	var retryOf sql.NullString
	var cancelRequested int
	var createdAt, updatedAt string
	err := row.Scan(
		&value.ID, &value.State, &requestJSON, &resultJSON, &errorJSON,
		&pauseJSON, &retryOf, &cancelRequested, &value.SettledSequence,
		&createdAt, &updatedAt,
	)
	if err != nil {
		return runtime.Record{}, err
	}
	if err := decodeStrict(requestJSON, &value.Request); err != nil {
		return runtime.Record{}, err
	}
	value.SchemaVersion = schemaVersion
	value.Result = cloneBytes(resultJSON)
	value.Pause = cloneBytes(pauseJSON)
	value.RetryOf = retryOf.String
	value.CancelRequested = cancelRequested != 0
	if len(errorJSON) > 0 {
		var runtimeErr contract.RuntimeError
		if err := decodeStrict(errorJSON, &runtimeErr); err != nil {
			return runtime.Record{}, err
		}
		value.Error = &runtimeErr
	}
	value.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return runtime.Record{}, err
	}
	value.UpdatedAt, err = parseTime(updatedAt)
	return value, err
}

func nextSequence(
	ctx context.Context,
	tx *sql.Tx,
	runID string,
) (uint64, error) {
	var maximum uint64
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COALESCE(MAX(sequence), 0) FROM events WHERE run_id = ?`,
		runID,
	).Scan(&maximum); err != nil {
		return 0, err
	}
	return maximum + 1, nil
}

func insertEvent(
	ctx context.Context,
	tx *sql.Tx,
	runID string,
	event contract.Event,
) error {
	data, err := marshalPayload(event)
	if err != nil {
		return err
	}
	_, err = tx.ExecContext(
		ctx,
		`INSERT INTO events (run_id, sequence, event_json, created_at)
		 VALUES (?, ?, ?, ?)`,
		runID, event.Sequence, data, formatTime(*event.Time),
	)
	return err
}

func updateEffectState(
	ctx context.Context,
	db *sql.DB,
	runID, callID, from, to, now string,
) error {
	result, err := db.ExecContext(
		ctx,
		`UPDATE tool_effects SET state = ?, updated_at = ?
		  WHERE run_id = ? AND call_id = ? AND state = ?`,
		to, now, runID, callID, from,
	)
	return requireOneEffect(result, err, runID, callID)
}

func requireOneEffect(
	result sql.Result,
	err error,
	runID, callID string,
) error {
	if err != nil {
		return err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return err
	}
	if affected != 1 {
		return fmt.Errorf("tool effect %s/%s has invalid state", runID, callID)
	}
	return nil
}

func marshalPayload(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	if len(data) > maxPayloadBytes {
		return nil, fmt.Errorf("payload exceeds %d bytes", maxPayloadBytes)
	}
	return data, nil
}

func marshalNullable(value any) ([]byte, error) {
	if value == nil {
		return nil, nil
	}
	return marshalPayload(value)
}

func marshalPrivateRequest(value json.RawMessage) ([]byte, error) {
	if len(value) == 0 {
		return nil, nil
	}
	if len(value) > runtime.MaxPrivateRequestBytes {
		return nil, fmt.Errorf(
			"private request exceeds %d bytes",
			runtime.MaxPrivateRequestBytes,
		)
	}
	if !json.Valid(value) {
		return nil, fmt.Errorf("private request is not valid JSON")
	}
	return cloneBytes(value), nil
}

func decodeStrict(data []byte, value any) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("unexpected trailing JSON")
		}
		return err
	}
	return nil
}

func prepareDatabasePath(path string) error {
	parent := filepath.Dir(path)
	info, err := os.Lstat(parent)
	if errors.Is(err, os.ErrNotExist) {
		if err := os.MkdirAll(parent, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(parent)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("database parent must be a directory, not a symlink")
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("database path must be a regular file, not a symlink")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	return nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func nullString(value string) any {
	if value == "" {
		return nil
	}
	return value
}

func nullableBytes(value []byte) any {
	if len(value) == 0 {
		return nil
	}
	return value
}

func cloneBytes(value []byte) []byte {
	if len(value) == 0 {
		return nil
	}
	return append([]byte(nil), value...)
}
