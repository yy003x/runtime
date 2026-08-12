// Package sqlite implements the durable Runtime Run Store on SQLite WAL.
package sqlite

import (
	"bytes"
	"context"
	"crypto/sha256"
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

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
)

const (
	schemaVersion   = 4
	maxPayloadBytes = 4 << 20
)

type Store struct {
	db  *sql.DB
	now func() time.Time
}

type Options struct {
	Now           func() time.Time
	SkipReconcile bool
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
	if err := initializeSchema(db); err != nil {
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
	if !options.SkipReconcile {
		if err := store.Reconcile(context.Background()); err != nil {
			db.Close()
			return nil, fmt.Errorf("reconcile SQLite Run Store: %w", err)
		}
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

func initializeSchema(db *sql.DB) error {
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
			resume_accepted_at TEXT,
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
			request_json BLOB NOT NULL,
			provider_request_id TEXT,
			result_json BLOB,
			result_digest TEXT,
			error_json BLOB,
			state TEXT NOT NULL,
			created_at TEXT NOT NULL,
			updated_at TEXT NOT NULL,
			UNIQUE (run_id, sequence)
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
		`CREATE TABLE resumes (
			run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
			sequence INTEGER NOT NULL,
			input_json BLOB NOT NULL,
			input_digest TEXT NOT NULL,
			accepted_at TEXT NOT NULL,
			PRIMARY KEY (run_id, sequence)
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
			return fmt.Errorf("initialize Runtime database schema: %w", err)
		}
	}
	return tx.Commit()
}

func validateRunRows(db *sql.DB) error {
	rows, err := db.Query(
		`SELECT run_id, kind, session_id, state, request_json,
		        private_request_json, resume_accepted_at, settled_sequence
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
		var resumeAcceptedAt sql.NullString
		var settledSequence uint64
		if err := rows.Scan(
			&runID, &kind, &sessionID, &state, &requestJSON,
			&privateJSON, &resumeAcceptedAt, &settledSequence,
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
		if resumeAcceptedAt.Valid {
			if len(request.Resume) == 0 {
				return fmt.Errorf(
					"run %s has resume_accepted_at without resume input",
					runID,
				)
			}
			if _, err := parseTime(resumeAcceptedAt.String); err != nil {
				return fmt.Errorf(
					"run %s resume_accepted_at: %w", runID, err,
				)
			}
		} else if len(request.Resume) != 0 {
			return fmt.Errorf(
				"run %s has resume input without resume_accepted_at",
				runID,
			)
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
	if len(request.Resume) != 0 {
		return runtime.Record{}, fmt.Errorf(
			"resume is invalid when creating a Run",
		)
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
	value, err := scanRecord(store.db.QueryRowContext(
		ctx,
		`SELECT run_id, state, request_json, result_json, error_json,
		        pause_json, resume_accepted_at, retry_of, cancel_requested,
		        settled_sequence,
		        created_at, updated_at
		   FROM runs WHERE run_id = ?`,
		runID,
	))
	if errors.Is(err, sql.ErrNoRows) {
		return runtime.Record{}, fmt.Errorf("%w: %s", runtime.ErrNotFound, runID)
	}
	return value, err
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
	filter, err := runtime.NormalizeListFilter(filter)
	if err != nil {
		return nil, err
	}
	query := `SELECT run_id, state, request_json, result_json, error_json,
	                 pause_json, resume_accepted_at, retry_of, cancel_requested,
	                 settled_sequence,
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

func (store *Store) CancellationReservations(
	ctx context.Context,
	afterRunID string,
	limit int,
) ([]runtime.Record, error) {
	if afterRunID != "" {
		if err := identity.Validate(afterRunID, "run"); err != nil {
			return nil, err
		}
	}
	if limit <= 0 || limit > 1000 {
		limit = 1000
	}
	rows, err := store.db.QueryContext(
		ctx,
		`SELECT run_id, state, request_json, result_json, error_json,
		        pause_json, resume_accepted_at, retry_of, cancel_requested,
		        settled_sequence, created_at, updated_at
		   FROM runs
		  WHERE cancel_requested = 1
		    AND state IN (?, ?)
		    AND run_id > ?
		  ORDER BY run_id
		  LIMIT ?`,
		runtime.StateQueued, runtime.StatePaused, afterRunID, limit,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]runtime.Record, 0)
	for rows.Next() {
		value, err := scanRecord(rows)
		if err != nil {
			return nil, err
		}
		if !value.CancelRequested ||
			value.State != runtime.StateQueued &&
				value.State != runtime.StatePaused {
			return nil, fmt.Errorf(
				"cancellation reservation query returned run %s in state %s",
				value.ID, value.State,
			)
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
	var cancelRequested int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT state, cancel_requested FROM runs WHERE run_id = ?`,
		runID,
	).Scan(&state, &cancelRequested); err != nil {
		return runtime.Record{}, err
	}
	if state != runtime.StateQueued {
		return runtime.Record{}, fmt.Errorf("run %s is %s, not queued", runID, state)
	}
	if cancelRequested != 0 {
		return runtime.Record{}, fmt.Errorf(
			"%w: run %s", runtime.ErrCancelReserved, runID,
		)
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
		  WHERE r.state = ? AND r.cancel_requested = 0
		    AND q.available_at <= ?
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
		`SELECT run_id, sequence, event_json FROM events
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
		var rowRunID string
		var rowSequence uint64
		var data []byte
		if err := rows.Scan(&rowRunID, &rowSequence, &data); err != nil {
			return nil, err
		}
		var value contract.Event
		if err := decodeStrict(data, &value); err != nil {
			return nil, err
		}
		if err := value.Validate(); err != nil {
			return nil, err
		}
		if rowRunID != runID || value.Sequence != rowSequence {
			return nil, fmt.Errorf(
				"event row identity does not match its JSON payload",
			)
		}
		switch {
		case value.Checkpoint != nil &&
			value.Checkpoint.RunID != rowRunID:
			return nil, fmt.Errorf(
				"checkpoint event run_id does not match its row",
			)
		case value.Agent != nil && value.Agent.RunID != rowRunID:
			return nil, fmt.Errorf(
				"agent event run_id does not match its row",
			)
		case value.Run != nil && value.Run.RunID != rowRunID:
			return nil, fmt.Errorf(
				"run event run_id does not match its row",
			)
		}
		values = append(values, value)
	}
	return values, rows.Err()
}

func (store *Store) LatestEventSequence(
	ctx context.Context,
	runID string,
) (uint64, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return 0, err
	}
	var sequence uint64
	if err := store.db.QueryRowContext(
		ctx,
		`SELECT COALESCE(MAX(sequence), 0) FROM events WHERE run_id = ?`,
		runID,
	).Scan(&sequence); err != nil {
		return 0, err
	}
	return sequence, nil
}

func (store *Store) SaveCheckpoint(
	ctx context.Context,
	checkpoint runtime.Checkpoint,
) error {
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = store.now().UTC()
	}
	return insertCheckpoint(ctx, store.db, checkpoint)
}

func validateCheckpoint(checkpoint runtime.Checkpoint) error {
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
	return nil
}

type sqlExecer interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}

func insertCheckpoint(
	ctx context.Context,
	execer sqlExecer,
	checkpoint runtime.Checkpoint,
) error {
	_, err := execer.ExecContext(
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
		  ORDER BY sequence DESC, created_at DESC, rowid DESC LIMIT 1`,
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

func (store *Store) Checkpoint(
	ctx context.Context,
	checkpointID string,
) (runtime.Checkpoint, bool, error) {
	var value runtime.Checkpoint
	var created string
	err := store.db.QueryRowContext(
		ctx,
		`SELECT checkpoint_id, run_id, sequence, state_json, created_at
		   FROM checkpoints WHERE checkpoint_id = ?`,
		checkpointID,
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
	if call.Sequence <= 0 {
		return fmt.Errorf("model call sequence and request digest are required")
	}
	requestJSON, requestDigest, err := canonicalModelRequest(call.Request)
	if err != nil {
		return fmt.Errorf("model call request: %w", err)
	}
	if call.RequestDigest != "" && call.RequestDigest != requestDigest {
		return fmt.Errorf("model call request digest does not match")
	}
	call.RequestDigest = requestDigest
	now := store.now().UTC()
	if call.CreatedAt.IsZero() {
		call.CreatedAt = now
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	var maximum int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COALESCE(MAX(sequence), 0)
		   FROM model_calls
		  WHERE run_id = ?`,
		call.RunID,
	).Scan(&maximum); err != nil {
		return err
	}
	if call.Sequence != maximum+1 {
		return fmt.Errorf(
			"model call sequence=%d, next durable sequence=%d",
			call.Sequence, maximum+1,
		)
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO model_calls (
			model_call_id, run_id, sequence, request_digest, request_json,
			provider_request_id, state, created_at, updated_at
		) VALUES (?, ?, ?, ?, ?, ?, 'running', ?, ?)`,
		call.ID, call.RunID, call.Sequence, call.RequestDigest, requestJSON,
		nullString(call.ProviderRequestID), formatTime(call.CreatedAt),
		formatTime(now),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *Store) FinishModelCall(
	ctx context.Context,
	call runtime.ModelCall,
) error {
	if call.State != "completed" && call.State != "failed" &&
		call.State != "cancelled" {
		return fmt.Errorf("invalid model call terminal state %q", call.State)
	}
	var resultJSON, errorJSON []byte
	switch call.State {
	case "completed":
		if len(call.Result) == 0 || !json.Valid(call.Result) {
			return fmt.Errorf("completed model call requires a valid result")
		}
		sum := sha256.Sum256(call.Result)
		wantDigest := fmt.Sprintf("sha256:%x", sum[:])
		if call.ResultDigest != wantDigest {
			return fmt.Errorf("completed model call result digest does not match")
		}
		if call.Error != nil {
			return fmt.Errorf("completed model call cannot contain an error")
		}
		resultJSON = cloneBytes(call.Result)
	case "failed", "cancelled":
		if len(call.Result) != 0 || call.ResultDigest != "" {
			return fmt.Errorf(
				"%s model call cannot contain a result",
				call.State,
			)
		}
		if call.Error == nil {
			return fmt.Errorf("%s model call requires an error", call.State)
		}
		if err := call.Error.Validate(); err != nil {
			return fmt.Errorf("model call error: %w", err)
		}
		var err error
		errorJSON, err = marshalPayload(call.Error)
		if err != nil {
			return err
		}
	}
	result, err := store.db.ExecContext(
		ctx,
		`UPDATE model_calls
		    SET state = ?, provider_request_id = ?,
		        result_json = ?, result_digest = ?, error_json = ?,
		        updated_at = ?
		  WHERE model_call_id = ? AND run_id = ? AND state = 'running'`,
		call.State, nullString(call.ProviderRequestID),
		nullableBytes(resultJSON), nullString(call.ResultDigest),
		nullableBytes(errorJSON), formatTime(store.now().UTC()),
		call.ID, call.RunID,
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

func (store *Store) LatestModelCall(
	ctx context.Context,
	runID string,
) (runtime.ModelCall, bool, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return runtime.ModelCall{}, false, err
	}
	row := store.db.QueryRowContext(
		ctx,
		`SELECT model_call_id, run_id, sequence, request_digest, request_json,
		        provider_request_id, result_json, result_digest, error_json,
		        state, created_at, updated_at
		   FROM model_calls
		  WHERE run_id = ?
		  ORDER BY sequence DESC
		  LIMIT 1`,
		runID,
	)
	value, err := scanModelCall(row)
	if errors.Is(err, sql.ErrNoRows) {
		return runtime.ModelCall{}, false, nil
	}
	if err != nil {
		return runtime.ModelCall{}, false, err
	}
	return value, true, nil
}

func (store *Store) ModelCalls(
	ctx context.Context,
	runID string,
) ([]runtime.ModelCall, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(
		ctx,
		`SELECT model_call_id, run_id, sequence, request_digest, request_json,
		        provider_request_id, result_json, result_digest, error_json,
		        state, created_at, updated_at
		   FROM model_calls
		  WHERE run_id = ?
		  ORDER BY sequence`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]runtime.ModelCall, 0)
	for rows.Next() {
		value, err := scanModelCall(rows)
		if err != nil {
			return nil, err
		}
		if value.RunID != runID ||
			value.Sequence != len(values)+1 ||
			len(values) >= 128 {
			return nil, fmt.Errorf(
				"model call journal for run %s is not contiguous and bounded",
				runID,
			)
		}
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

func (store *Store) Resumes(
	ctx context.Context,
	runID string,
) ([]runtime.ResumeRecord, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(
		ctx,
		`SELECT run_id, sequence, input_json, input_digest, accepted_at
		   FROM resumes
		  WHERE run_id = ?
		  ORDER BY sequence`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	values := make([]runtime.ResumeRecord, 0)
	for rows.Next() {
		var value runtime.ResumeRecord
		var inputJSON []byte
		var acceptedAt string
		if err := rows.Scan(
			&value.RunID, &value.Sequence, &inputJSON,
			&value.InputDigest, &acceptedAt,
		); err != nil {
			return nil, err
		}
		canonical, digest, err := canonicalJSONValue(inputJSON)
		if err != nil {
			return nil, fmt.Errorf(
				"resume sequence=%d input is invalid: %w",
				value.Sequence, err,
			)
		}
		if value.RunID != runID ||
			value.Sequence != len(values)+1 ||
			value.InputDigest != digest ||
			!validSHA256Digest(value.InputDigest) ||
			!bytes.Equal(inputJSON, canonical) {
			return nil, fmt.Errorf(
				"resume journal for run %s is not canonical and contiguous",
				runID,
			)
		}
		value.AcceptedAt, err = parseTime(acceptedAt)
		if err != nil {
			return nil, err
		}
		value.Input = canonical
		values = append(values, value)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return values, nil
}

type modelCallScanner interface {
	Scan(...any) error
}

func scanModelCall(
	scanner modelCallScanner,
) (runtime.ModelCall, error) {
	var value runtime.ModelCall
	var providerRequestID sql.NullString
	var requestJSON, resultJSON, errorJSON []byte
	var resultDigest sql.NullString
	var createdAt, updatedAt string
	err := scanner.Scan(
		&value.ID, &value.RunID, &value.Sequence, &value.RequestDigest,
		&requestJSON, &providerRequestID, &resultJSON, &resultDigest, &errorJSON,
		&value.State, &createdAt, &updatedAt,
	)
	if err != nil {
		return runtime.ModelCall{}, err
	}
	switch value.State {
	case "running", "completed", "failed", "cancelled":
	default:
		return runtime.ModelCall{}, fmt.Errorf(
			"model call %s has invalid state %q", value.ID, value.State,
		)
	}
	canonicalRequest, requestDigest, err := canonicalModelRequest(requestJSON)
	if err != nil {
		return runtime.ModelCall{}, fmt.Errorf(
			"model call %s request is invalid: %w", value.ID, err,
		)
	}
	if !validSHA256Digest(value.RequestDigest) ||
		value.RequestDigest != requestDigest {
		return runtime.ModelCall{}, fmt.Errorf(
			"model call %s request digest does not match",
			value.ID,
		)
	}
	if !bytes.Equal(requestJSON, canonicalRequest) {
		return runtime.ModelCall{}, fmt.Errorf(
			"model call %s request_json is not canonical",
			value.ID,
		)
	}
	value.Request = canonicalRequest
	value.ProviderRequestID = providerRequestID.String
	value.Result = cloneBytes(resultJSON)
	value.ResultDigest = resultDigest.String
	switch value.State {
	case "completed":
		if len(value.Result) == 0 || !json.Valid(value.Result) ||
			value.ResultDigest == "" || len(errorJSON) != 0 {
			return runtime.ModelCall{}, fmt.Errorf(
				"completed model call %s has invalid durable evidence",
				value.ID,
			)
		}
		sum := sha256.Sum256(value.Result)
		if value.ResultDigest != fmt.Sprintf("sha256:%x", sum[:]) {
			return runtime.ModelCall{}, fmt.Errorf(
				"model call %s result digest does not match",
				value.ID,
			)
		}
	case "failed", "cancelled":
		if len(value.Result) != 0 || value.ResultDigest != "" ||
			len(errorJSON) == 0 {
			return runtime.ModelCall{}, fmt.Errorf(
				"%s model call %s has invalid durable evidence",
				value.State, value.ID,
			)
		}
		var runtimeErr contract.RuntimeError
		if err := decodeStrict(errorJSON, &runtimeErr); err != nil {
			return runtime.ModelCall{}, err
		}
		if err := runtimeErr.Validate(); err != nil {
			return runtime.ModelCall{}, err
		}
		value.Error = &runtimeErr
	case "running":
		if len(value.Result) != 0 || value.ResultDigest != "" ||
			len(errorJSON) != 0 {
			return runtime.ModelCall{}, fmt.Errorf(
				"running model call %s has terminal evidence",
				value.ID,
			)
		}
	}
	value.CreatedAt, err = parseTime(createdAt)
	if err != nil {
		return runtime.ModelCall{}, err
	}
	value.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return runtime.ModelCall{}, err
	}
	return value, nil
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 ||
		!strings.HasPrefix(value, "sha256:") {
		return false
	}
	for _, current := range value[len("sha256:"):] {
		if current < '0' || current > '9' &&
			(current < 'a' || current > 'f') {
			return false
		}
	}
	return true
}

func canonicalModelRequest(
	value json.RawMessage,
) (json.RawMessage, string, error) {
	var request contract.GenerateRequest
	if err := strictjson.DecodeObject(
		bytes.NewReader(value), maxPayloadBytes, &request,
	); err != nil {
		return nil, "", err
	}
	if err := request.Validate(); err != nil {
		return nil, "", err
	}
	canonical, err := json.Marshal(request)
	if err != nil {
		return nil, "", err
	}
	sum := sha256.Sum256(canonical)
	return canonical, fmt.Sprintf("sha256:%x", sum[:]), nil
}

func (store *Store) PrepareToolEffect(
	ctx context.Context,
	effect runtime.ToolEffect,
	checkpoint runtime.Checkpoint,
) error {
	if err := validateCheckpoint(checkpoint); err != nil {
		return err
	}
	if checkpoint.RunID != effect.RunID {
		return fmt.Errorf("tool effect and checkpoint run_id do not match")
	}
	if strings.TrimSpace(effect.CallID) == "" ||
		strings.TrimSpace(effect.IdempotencyKey) == "" ||
		strings.TrimSpace(effect.Name) == "" {
		return fmt.Errorf(
			"tool effect call_id, idempotency_key, and name are required",
		)
	}
	requestJSON, err := marshalPayload(effect.Request)
	if err != nil {
		return err
	}
	now := store.now().UTC()
	if checkpoint.CreatedAt.IsZero() {
		checkpoint.CreatedAt = now
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	if err := insertCheckpoint(ctx, tx, checkpoint); err != nil {
		return err
	}
	if _, err = tx.ExecContext(
		ctx,
		`INSERT INTO tool_effects (
			run_id, call_id, idempotency_key, name, state,
			request_json, updated_at
		) VALUES (?, ?, ?, ?, 'prepared', ?, ?)`,
		effect.RunID, effect.CallID, effect.IdempotencyKey, effect.Name,
		requestJSON, formatTime(now),
	); err != nil {
		return err
	}
	return tx.Commit()
}

func (store *Store) ToolEffect(
	ctx context.Context,
	runID, callID string,
) (runtime.ToolEffect, bool, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return runtime.ToolEffect{}, false, err
	}
	if strings.TrimSpace(callID) == "" {
		return runtime.ToolEffect{}, false, fmt.Errorf("tool effect call_id is required")
	}
	var value runtime.ToolEffect
	var requestJSON, resultJSON, errorJSON []byte
	var updatedAt string
	err := store.db.QueryRowContext(
		ctx,
		`SELECT run_id, call_id, idempotency_key, name, state,
		        request_json, result_json, error_json, updated_at
		   FROM tool_effects
		  WHERE run_id = ? AND call_id = ?`,
		runID, callID,
	).Scan(
		&value.RunID, &value.CallID, &value.IdempotencyKey, &value.Name,
		&value.State, &requestJSON, &resultJSON, &errorJSON, &updatedAt,
	)
	if errors.Is(err, sql.ErrNoRows) {
		return runtime.ToolEffect{}, false, nil
	}
	if err != nil {
		return runtime.ToolEffect{}, false, err
	}
	switch value.State {
	case "prepared", "started", "completed", "failed":
	default:
		return runtime.ToolEffect{}, false, fmt.Errorf(
			"tool effect %s/%s has invalid state %q",
			runID, callID, value.State,
		)
	}
	value.Request = cloneBytes(requestJSON)
	value.Result = cloneBytes(resultJSON)
	if len(errorJSON) > 0 {
		var runtimeErr contract.RuntimeError
		if err := decodeStrict(errorJSON, &runtimeErr); err != nil {
			return runtime.ToolEffect{}, false, err
		}
		value.Error = &runtimeErr
	}
	switch value.State {
	case "prepared", "started":
		if len(value.Result) != 0 || value.Error != nil {
			return runtime.ToolEffect{}, false, fmt.Errorf(
				"%s tool effect %s/%s has terminal evidence",
				value.State, runID, callID,
			)
		}
	case "completed":
		if len(value.Result) == 0 || !json.Valid(value.Result) ||
			value.Error != nil {
			return runtime.ToolEffect{}, false, fmt.Errorf(
				"completed tool effect %s/%s has invalid result evidence",
				runID, callID,
			)
		}
	case "failed":
		if len(value.Result) != 0 || value.Error == nil {
			return runtime.ToolEffect{}, false, fmt.Errorf(
				"failed tool effect %s/%s has invalid error evidence",
				runID, callID,
			)
		}
	}
	value.UpdatedAt, err = parseTime(updatedAt)
	if err != nil {
		return runtime.ToolEffect{}, false, err
	}
	return value, true, nil
}

func (store *Store) ToolEffects(
	ctx context.Context,
	runID string,
) ([]runtime.ToolEffect, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return nil, err
	}
	rows, err := store.db.QueryContext(
		ctx,
		`SELECT call_id FROM tool_effects
		  WHERE run_id = ?
		  ORDER BY rowid`,
		runID,
	)
	if err != nil {
		return nil, err
	}
	var callIDs []string
	for rows.Next() {
		var callID string
		if err := rows.Scan(&callID); err != nil {
			rows.Close()
			return nil, err
		}
		if len(callIDs) >= 1024 {
			rows.Close()
			return nil, fmt.Errorf(
				"tool effect journal for run %s exceeds its bound",
				runID,
			)
		}
		callIDs = append(callIDs, callID)
	}
	if err := rows.Close(); err != nil {
		return nil, err
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	values := make([]runtime.ToolEffect, 0, len(callIDs))
	for _, callID := range callIDs {
		value, exists, err := store.ToolEffect(ctx, runID, callID)
		if err != nil {
			return nil, err
		}
		if !exists {
			return nil, fmt.Errorf(
				"tool effect %s/%s disappeared while reading journal",
				runID, callID,
			)
		}
		values = append(values, value)
	}
	return values, nil
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
	return store.needsReconciliation(ctx, runID, runtimeErr, false)
}

func (store *Store) NeedsCancellationReconciliation(
	ctx context.Context,
	runID string,
	runtimeErr *contract.RuntimeError,
) (runtime.Record, error) {
	return store.needsReconciliation(ctx, runID, runtimeErr, true)
}

func (store *Store) needsReconciliation(
	ctx context.Context,
	runID string,
	runtimeErr *contract.RuntimeError,
	cancellationReserved bool,
) (runtime.Record, error) {
	errorJSON, err := marshalNullable(runtimeErr)
	if err != nil {
		return runtime.Record{}, err
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return runtime.Record{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	cancelRequested := 0
	if cancellationReserved {
		cancelRequested = 1
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE runs
		    SET state = ?, pause_json = NULL, error_json = ?,
		        updated_at = ?
		  WHERE run_id = ?
		    AND state IN (?, ?, ?)
		    AND cancel_requested = ?`,
		runtime.StateNeedsReconciliation, errorJSON,
		formatTime(store.now().UTC()), runID,
		runtime.StateRunning, runtime.StateQueued, runtime.StatePaused,
		cancelRequested,
	)
	if err != nil {
		return runtime.Record{}, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return runtime.Record{}, err
	}
	if affected != 1 {
		var current runtime.State
		var reserved int
		if err := tx.QueryRowContext(
			ctx,
			`SELECT state, cancel_requested FROM runs WHERE run_id = ?`,
			runID,
		).Scan(&current, &reserved); err == nil &&
			!cancellationReserved && reserved != 0 {
			return runtime.Record{}, fmt.Errorf(
				"%w: run %s", runtime.ErrCancelReserved, runID,
			)
		}
		return runtime.Record{}, fmt.Errorf(
			"run %s cannot enter needs_reconciliation", runID,
		)
	}
	if _, err := tx.ExecContext(
		ctx, `DELETE FROM queue WHERE run_id = ?`, runID,
	); err != nil {
		return runtime.Record{}, err
	}
	if err := tx.Commit(); err != nil {
		return runtime.Record{}, err
	}
	return store.Get(ctx, runID)
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
		  WHERE run_id = ? AND state = ? AND cancel_requested = 0`,
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
		var cancelRequested int
		var current runtime.State
		if err := store.db.QueryRowContext(
			ctx,
			`SELECT state, cancel_requested FROM runs WHERE run_id = ?`,
			runID,
		).Scan(&current, &cancelRequested); err == nil &&
			current == runtime.StateRunning &&
			cancelRequested != 0 {
			return runtime.Record{}, fmt.Errorf(
				"%w: run %s", runtime.ErrCancelReserved, runID,
			)
		}
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
	return store.settle(
		ctx, runID, state, resultJSON, runtimeErr, false,
	)
}

func (store *Store) SettleCancellation(
	ctx context.Context,
	runID string,
	state runtime.State,
	resultJSON json.RawMessage,
	runtimeErr *contract.RuntimeError,
) (runtime.Record, error) {
	return store.settle(
		ctx, runID, state, resultJSON, runtimeErr, true,
	)
}

func (store *Store) settle(
	ctx context.Context,
	runID string,
	state runtime.State,
	resultJSON json.RawMessage,
	runtimeErr *contract.RuntimeError,
	cancellationReserved bool,
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
	var cancelRequested int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT state, settled_sequence, cancel_requested
		   FROM runs WHERE run_id = ?`,
		runID,
	).Scan(&current, &settled, &cancelRequested); err != nil {
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
	if !cancellationReserved && cancelRequested != 0 {
		return runtime.Record{}, fmt.Errorf(
			"%w: run %s", runtime.ErrCancelReserved, runID,
		)
	}
	if cancellationReserved && cancelRequested == 0 {
		return runtime.Record{}, fmt.Errorf(
			"run %s has no cancellation reservation", runID,
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
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return runtime.Record{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	var state runtime.State
	var cancelRequested int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT state, cancel_requested FROM runs WHERE run_id = ?`,
		runID,
	).Scan(&state, &cancelRequested); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtime.Record{}, fmt.Errorf(
				"%w: %s", runtime.ErrNotFound, runID,
			)
		}
		return runtime.Record{}, err
	}
	switch state {
	case runtime.StateQueued, runtime.StatePaused:
		if cancelRequested == 0 {
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE runs
				    SET cancel_requested = 1, updated_at = ?
				  WHERE run_id = ? AND state = ?`,
				formatTime(store.now().UTC()), runID, state,
			); err != nil {
				return runtime.Record{}, err
			}
		}
		if _, err := tx.ExecContext(
			ctx, `DELETE FROM queue WHERE run_id = ?`, runID,
		); err != nil {
			return runtime.Record{}, err
		}
	case runtime.StateRunning:
		if _, err := tx.ExecContext(
			ctx,
			`UPDATE runs SET cancel_requested = 1, updated_at = ?
			  WHERE run_id = ? AND state = ?`,
			formatTime(store.now().UTC()), runID, runtime.StateRunning,
		); err != nil {
			return runtime.Record{}, err
		}
	case runtime.StateNeedsReconciliation:
		return runtime.Record{}, fmt.Errorf(
			"%w: run %s needs reconciliation and cannot be marked safely cancelled",
			runtime.ErrConflict, runID,
		)
	default:
		// Terminal cancellation is already settled.
	}
	if err := tx.Commit(); err != nil {
		return runtime.Record{}, err
	}
	return store.Get(ctx, runID)
}

func (store *Store) Resume(
	ctx context.Context,
	runID string,
	input json.RawMessage,
	constraint runtime.ResumeConstraint,
) (runtime.Record, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return runtime.Record{}, err
	}
	canonicalInput, inputDigest, err := canonicalJSONValue(input)
	if err != nil {
		return runtime.Record{}, fmt.Errorf(
			"resume input is invalid or too large: %w", err,
		)
	}
	if len(constraint.Pause) == 0 ||
		len(constraint.Pause) > maxPayloadBytes ||
		!json.Valid(constraint.Pause) {
		return runtime.Record{}, fmt.Errorf(
			"resume pause constraint is invalid or too large",
		)
	}
	if constraint.NotAfter != nil && constraint.NotAfter.IsZero() {
		return runtime.Record{}, fmt.Errorf(
			"resume expiry constraint is invalid",
		)
	}
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return runtime.Record{}, err
	}
	defer tx.Rollback() //nolint:errcheck
	reservation, err := tx.ExecContext(
		ctx,
		`UPDATE runs
		    SET updated_at = updated_at
		  WHERE run_id = ? AND state = ? AND cancel_requested = 0
		    AND pause_json = ?`,
		runID, runtime.StatePaused, constraint.Pause,
	)
	if err != nil {
		return runtime.Record{}, err
	}
	affected, err := reservation.RowsAffected()
	if err != nil {
		return runtime.Record{}, err
	}
	if affected != 1 {
		var current runtime.State
		var currentCancel int
		var currentPause []byte
		if err := tx.QueryRowContext(
			ctx,
			`SELECT state, cancel_requested, pause_json
			   FROM runs WHERE run_id = ?`,
			runID,
		).Scan(
			&current, &currentCancel, &currentPause,
		); err != nil {
			if errors.Is(err, sql.ErrNoRows) {
				return runtime.Record{}, fmt.Errorf(
					"%w: %s", runtime.ErrNotFound, runID,
				)
			}
			return runtime.Record{}, err
		}
		if current != runtime.StatePaused {
			return runtime.Record{}, fmt.Errorf(
				"%w: run %s is %s, not paused",
				runtime.ErrConflict, runID, current,
			)
		}
		if currentCancel != 0 {
			return runtime.Record{}, fmt.Errorf(
				"%w: run %s", runtime.ErrCancelReserved, runID,
			)
		}
		if bytes.Equal(currentPause, constraint.Pause) {
			return runtime.Record{}, fmt.Errorf(
				"%w: run %s resume lost its paused reservation",
				runtime.ErrConflict, runID,
			)
		}
		return runtime.Record{}, fmt.Errorf(
			"%w: run %s active pause changed before resume",
			runtime.ErrConflict, runID,
		)
	}
	// This exact no-op write is the acceptance linearization reservation.
	// SQLite now holds the writer slot for the transaction, so acceptedAt
	// cannot be sampled before waiting behind another writer.
	var state runtime.State
	var cancelRequested int
	var requestJSON []byte
	var pauseJSON []byte
	if err := tx.QueryRowContext(
		ctx,
		`SELECT state, cancel_requested, request_json, pause_json
		   FROM runs WHERE run_id = ?`,
		runID,
	).Scan(
		&state, &cancelRequested, &requestJSON, &pauseJSON,
	); err != nil {
		if errors.Is(err, sql.ErrNoRows) {
			return runtime.Record{}, fmt.Errorf(
				"%w: %s", runtime.ErrNotFound, runID,
			)
		}
		return runtime.Record{}, err
	}
	if state != runtime.StatePaused {
		return runtime.Record{}, fmt.Errorf(
			"%w: run %s is %s, not paused",
			runtime.ErrConflict, runID, state,
		)
	}
	if cancelRequested != 0 {
		return runtime.Record{}, fmt.Errorf(
			"%w: run %s", runtime.ErrCancelReserved, runID,
		)
	}
	if !bytes.Equal(pauseJSON, constraint.Pause) {
		return runtime.Record{}, fmt.Errorf(
			"%w: run %s active pause changed before resume",
			runtime.ErrConflict, runID,
		)
	}
	acceptedAt := store.now().UTC()
	if constraint.NotAfter != nil &&
		acceptedAt.After(constraint.NotAfter.UTC()) {
		return runtime.Record{}, fmt.Errorf(
			"%w: run %s pause has expired",
			runtime.ErrConflict, runID,
		)
	}
	var request runtime.Request
	if err := decodeStrict(requestJSON, &request); err != nil {
		return runtime.Record{}, err
	}
	request.Resume = cloneBytes(canonicalInput)
	requestJSON, err = marshalPayload(request)
	if err != nil {
		return runtime.Record{}, err
	}
	now := formatTime(acceptedAt)
	var resumeSequence int
	if err := tx.QueryRowContext(
		ctx,
		`SELECT COALESCE(MAX(sequence), 0) + 1
		   FROM resumes WHERE run_id = ?`,
		runID,
	).Scan(&resumeSequence); err != nil {
		return runtime.Record{}, err
	}
	if _, err := tx.ExecContext(
		ctx,
		`INSERT INTO resumes (
			run_id, sequence, input_json, input_digest, accepted_at
		) VALUES (?, ?, ?, ?, ?)`,
		runID, resumeSequence, canonicalInput, inputDigest,
		now,
	); err != nil {
		return runtime.Record{}, err
	}
	result, err := tx.ExecContext(
		ctx,
		`UPDATE runs
		    SET state = ?, request_json = ?, pause_json = NULL,
		        error_json = NULL, cancel_requested = 0,
		        resume_accepted_at = ?, updated_at = ?
		  WHERE run_id = ? AND state = ? AND cancel_requested = 0
		    AND pause_json = ?`,
		runtime.StateQueued, requestJSON, now,
		now, runID, runtime.StatePaused, constraint.Pause,
	)
	if err != nil {
		return runtime.Record{}, err
	}
	affected, err = result.RowsAffected()
	if err != nil {
		return runtime.Record{}, err
	}
	if affected != 1 {
		return runtime.Record{}, fmt.Errorf(
			"%w: run %s resume lost its paused reservation",
			runtime.ErrConflict, runID,
		)
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

func canonicalJSONValue(
	value json.RawMessage,
) (json.RawMessage, string, error) {
	var raw json.RawMessage
	if err := strictjson.Decode(
		bytes.NewReader(value), maxPayloadBytes, &raw,
	); err != nil {
		return nil, "", err
	}
	var compact bytes.Buffer
	if err := json.Compact(&compact, raw); err != nil {
		return nil, "", err
	}
	canonical := compact.Bytes()
	sum := sha256.Sum256(canonical)
	return cloneBytes(canonical), fmt.Sprintf("sha256:%x", sum[:]), nil
}

func (store *Store) Reconcile(ctx context.Context) error {
	tx, err := store.db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	rows, err := tx.QueryContext(
		ctx,
		`SELECT run_id, kind, cancel_requested
		   FROM runs WHERE state = ?`,
		runtime.StateRunning,
	)
	if err != nil {
		return err
	}
	type runningRun struct {
		id              string
		kind            runtime.Kind
		cancelRequested bool
	}
	var running []runningRun
	for rows.Next() {
		var value runningRun
		var cancelRequested int
		if err := rows.Scan(
			&value.id, &value.kind, &cancelRequested,
		); err != nil {
			rows.Close()
			return err
		}
		value.cancelRequested = cancelRequested != 0
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
		if current.cancelRequested {
			if _, err := tx.ExecContext(
				ctx,
				`UPDATE runs SET state = ?, updated_at = ?
				  WHERE run_id = ?`,
				runtime.StateQueued, now, runID,
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
	var resumeAcceptedAt sql.NullString
	var retryOf sql.NullString
	var cancelRequested int
	var createdAt, updatedAt string
	err := row.Scan(
		&value.ID, &value.State, &requestJSON, &resultJSON, &errorJSON,
		&pauseJSON, &resumeAcceptedAt, &retryOf, &cancelRequested,
		&value.SettledSequence,
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
	if resumeAcceptedAt.Valid {
		acceptedAt, parseErr := parseTime(resumeAcceptedAt.String)
		if parseErr != nil {
			return runtime.Record{}, parseErr
		}
		value.ResumeAcceptedAt = &acceptedAt
	}
	if (len(value.Request.Resume) != 0) !=
		(value.ResumeAcceptedAt != nil) {
		return runtime.Record{}, fmt.Errorf(
			"run %s resume input and acceptance time are inconsistent",
			value.ID,
		)
	}
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

func marshalNullable(value *contract.RuntimeError) ([]byte, error) {
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
