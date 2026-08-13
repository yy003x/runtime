package sqlite

import (
	"database/sql"
	"fmt"
	"sort"
	"strings"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
)

// SchemaVersion is the canonical durable Run Store schema accepted by this
// binary and published in release compatibility metadata.
const SchemaVersion = 6

const reaperIndexName = "runs_reaper_state_updated"

type schemaObject struct {
	objectType string
	name       string
	tableName  string
	definition string
}

// schemaObjects is the single exact SQLite schema manifest. Both schema
// creation and validation consume it so a matching PRAGMA user_version cannot
// conceal missing, changed, or additional tables, indexes, views, or triggers.
var schemaObjects = []schemaObject{
	{
		objectType: "table", name: "runs", tableName: "runs",
		definition: `CREATE TABLE runs (
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
	},
	{
		objectType: "index", name: "runs_state_created", tableName: "runs",
		definition: `CREATE INDEX runs_state_created ON runs(state, created_at)`,
	},
	{
		objectType: "index", name: reaperIndexName, tableName: "runs",
		definition: `CREATE INDEX runs_reaper_state_updated
		   ON runs(state, cancel_requested, updated_at, run_id)`,
	},
	{
		objectType: "index", name: "runs_one_open_session", tableName: "runs",
		definition: `CREATE UNIQUE INDEX runs_one_open_session
		   ON runs(session_id)
		 WHERE kind IN ('session', 'native_tui')
		   AND session_id IS NOT NULL
		   AND state IN ('queued', 'running', 'paused', 'needs_reconciliation')`,
	},
	{
		objectType: "table", name: "events", tableName: "events",
		definition: `CREATE TABLE events (
			run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
			sequence INTEGER NOT NULL,
			event_json BLOB NOT NULL,
			created_at TEXT NOT NULL,
			PRIMARY KEY (run_id, sequence)
		)`,
	},
	{
		objectType: "table", name: "checkpoints", tableName: "checkpoints",
		definition: `CREATE TABLE checkpoints (
			checkpoint_id TEXT PRIMARY KEY,
			run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
			sequence INTEGER NOT NULL,
			state_json BLOB NOT NULL,
			created_at TEXT NOT NULL
		)`,
	},
	{
		objectType: "index", name: "checkpoints_run_sequence",
		tableName: "checkpoints",
		definition: `CREATE INDEX checkpoints_run_sequence
		   ON checkpoints(run_id, sequence DESC)`,
	},
	{
		objectType: "table", name: "model_calls", tableName: "model_calls",
		definition: `CREATE TABLE model_calls (
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
	},
	{
		objectType: "table", name: "tool_effects", tableName: "tool_effects",
		definition: `CREATE TABLE tool_effects (
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
	},
	{
		objectType: "table", name: "resumes", tableName: "resumes",
		definition: `CREATE TABLE resumes (
			run_id TEXT NOT NULL REFERENCES runs(run_id) ON DELETE CASCADE,
			sequence INTEGER NOT NULL,
			input_json BLOB NOT NULL,
			input_digest TEXT NOT NULL,
			accepted_at TEXT NOT NULL,
			PRIMARY KEY (run_id, sequence)
		)`,
	},
	{
		objectType: "table", name: "queue", tableName: "queue",
		definition: `CREATE TABLE queue (
			run_id TEXT PRIMARY KEY REFERENCES runs(run_id) ON DELETE CASCADE,
			available_at TEXT NOT NULL,
			claimed_by TEXT,
			claimed_at TEXT
		)`,
	},
}

var reaperIndexColumns = [...]string{
	"state", "cancel_requested", "updated_at", "run_id",
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
	if version != 0 && version != SchemaVersion {
		return fmt.Errorf(
			"unsupported Runtime database schema %d; expected %d",
			version, SchemaVersion,
		)
	}
	if version == SchemaVersion {
		return nil
	}
	tx, err := db.Begin()
	if err != nil {
		return err
	}
	defer tx.Rollback() //nolint:errcheck
	for _, object := range schemaObjects {
		if _, err := tx.Exec(object.definition); err != nil {
			return fmt.Errorf("initialize Runtime database schema: %w", err)
		}
	}
	if _, err := tx.Exec(
		fmt.Sprintf("PRAGMA user_version = %d", SchemaVersion),
	); err != nil {
		return fmt.Errorf("initialize Runtime database schema: %w", err)
	}
	return tx.Commit()
}

// ValidateSchema verifies the complete explicit schema owned by this package.
// SQLite-generated autoindexes are represented by the exact table definitions
// that create them and are excluded from the sqlite_master object set.
func ValidateSchema(db *sql.DB) error {
	if db == nil {
		return fmt.Errorf("SQLite database is required")
	}
	expected := make(map[string]schemaObject, len(schemaObjects))
	for _, object := range schemaObjects {
		expected[object.name] = object
	}
	rows, err := db.Query(
		`SELECT type, name, tbl_name, sql
		   FROM sqlite_master
		  WHERE type IN ('table', 'index', 'view', 'trigger')
		    AND name NOT GLOB 'sqlite_*'`,
	)
	if err != nil {
		return fmt.Errorf("inspect Runtime database schema: %w", err)
	}
	for rows.Next() {
		var objectType, name, tableName string
		var definition sql.NullString
		if err := rows.Scan(
			&objectType, &name, &tableName, &definition,
		); err != nil {
			rows.Close()
			return fmt.Errorf("inspect Runtime database schema: %w", err)
		}
		want, found := expected[name]
		if !found {
			rows.Close()
			return fmt.Errorf("unexpected SQLite schema object %s", name)
		}
		if objectType != want.objectType || tableName != want.tableName ||
			!definition.Valid ||
			normalizeSchemaSQL(definition.String) !=
				normalizeSchemaSQL(want.definition) {
			rows.Close()
			return fmt.Errorf(
				"SQLite schema object %s does not match the current contract",
				name,
			)
		}
		delete(expected, name)
	}
	if err := rows.Err(); err != nil {
		rows.Close()
		return fmt.Errorf("inspect Runtime database schema: %w", err)
	}
	if err := rows.Close(); err != nil {
		return fmt.Errorf("inspect Runtime database schema: %w", err)
	}
	if len(expected) != 0 {
		missing := make([]string, 0, len(expected))
		for name := range expected {
			missing = append(missing, name)
		}
		sort.Strings(missing)
		return fmt.Errorf(
			"required SQLite schema objects are missing: %s",
			strings.Join(missing, ", "),
		)
	}

	indexRows, err := db.Query(`PRAGMA index_list('runs')`)
	if err != nil {
		return fmt.Errorf("inspect runs indexes: %w", err)
	}
	found := false
	for indexRows.Next() {
		var sequence, unique, partial int
		var name, origin string
		if err := indexRows.Scan(
			&sequence, &name, &unique, &origin, &partial,
		); err != nil {
			indexRows.Close()
			return fmt.Errorf("inspect runs indexes: %w", err)
		}
		if name != reaperIndexName {
			continue
		}
		found = true
		if unique != 0 || partial != 0 {
			indexRows.Close()
			return fmt.Errorf(
				"required index %s must be non-unique and non-partial",
				reaperIndexName,
			)
		}
	}
	if err := indexRows.Err(); err != nil {
		indexRows.Close()
		return fmt.Errorf("inspect runs indexes: %w", err)
	}
	if err := indexRows.Close(); err != nil {
		return fmt.Errorf("inspect runs indexes: %w", err)
	}
	if !found {
		return fmt.Errorf(
			"required index %s is not attached to runs",
			reaperIndexName,
		)
	}

	columnRows, err := db.Query(
		fmt.Sprintf("PRAGMA index_info('%s')", reaperIndexName),
	)
	if err != nil {
		return fmt.Errorf("inspect required index %s columns: %w", reaperIndexName, err)
	}
	var columns []string
	for columnRows.Next() {
		var sequence, columnID int
		var name sql.NullString
		if err := columnRows.Scan(&sequence, &columnID, &name); err != nil {
			columnRows.Close()
			return fmt.Errorf(
				"inspect required index %s columns: %w",
				reaperIndexName, err,
			)
		}
		if sequence != len(columns) || !name.Valid {
			columnRows.Close()
			return fmt.Errorf(
				"required index %s has invalid column metadata",
				reaperIndexName,
			)
		}
		columns = append(columns, name.String)
	}
	if err := columnRows.Err(); err != nil {
		columnRows.Close()
		return fmt.Errorf(
			"inspect required index %s columns: %w",
			reaperIndexName, err,
		)
	}
	if err := columnRows.Close(); err != nil {
		return fmt.Errorf(
			"inspect required index %s columns: %w",
			reaperIndexName, err,
		)
	}
	if len(columns) != len(reaperIndexColumns) {
		return fmt.Errorf(
			"required index %s has columns %v; expected %v",
			reaperIndexName, columns, reaperIndexColumns,
		)
	}
	for index := range reaperIndexColumns {
		if columns[index] != reaperIndexColumns[index] {
			return fmt.Errorf(
				"required index %s has columns %v; expected %v",
				reaperIndexName, columns, reaperIndexColumns,
			)
		}
	}
	return nil
}

func normalizeSchemaSQL(value string) string {
	// Preserve identifier and string-literal case. Lowercasing the complete SQL
	// would incorrectly treat a forged partial-index literal such as 'SESSION'
	// as equivalent to the canonical state value 'session'.
	return strings.Join(strings.Fields(value), " ")
}

func validateRunRows(db *sql.DB) error {
	rows, err := db.Query(
		`SELECT run_id, kind, session_id, state, request_json,
		        private_request_json, result_json, error_json,
		        resume_accepted_at, settled_sequence, created_at, updated_at
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
		var requestJSON, privateJSON, resultJSON, errorJSON []byte
		var resumeAcceptedAt sql.NullString
		var settledSequence uint64
		var createdAt, updatedAt string
		if err := rows.Scan(
			&runID, &kind, &sessionID, &state, &requestJSON,
			&privateJSON, &resultJSON, &errorJSON,
			&resumeAcceptedAt, &settledSequence, &createdAt, &updatedAt,
		); err != nil {
			return err
		}
		if err := identity.Validate(runID, "run"); err != nil {
			return fmt.Errorf("Runtime database has invalid run_id: %w", err)
		}
		switch kind {
		case runtime.KindAgent, runtime.KindSession, runtime.KindNativeTUI:
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
		if kind == runtime.KindNativeTUI {
			if err := identity.Validate(request.SessionID, "session"); err != nil {
				return fmt.Errorf(
					"run %s has invalid native_tui session_id: %w", runID, err,
				)
			}
			if err := identity.Validate(request.ExecutionID, "execution"); err != nil {
				return fmt.Errorf(
					"run %s has invalid native_tui execution_id: %w", runID, err,
				)
			}
			if state.Terminal() {
				var runtimeErr *contract.RuntimeError
				if len(errorJSON) != 0 {
					var value contract.RuntimeError
					if err := decodeStrict(errorJSON, &value); err != nil {
						return fmt.Errorf(
							"run %s error: %w", runID, err,
						)
					}
					runtimeErr = &value
				}
				created, err := parseTime(createdAt)
				if err != nil {
					return fmt.Errorf("run %s created_at: %w", runID, err)
				}
				updated, err := parseTime(updatedAt)
				if err != nil {
					return fmt.Errorf("run %s updated_at: %w", runID, err)
				}
				if _, err := runtime.NativeTUIExecutionFromRecord(runtime.Record{
					SchemaVersion: SchemaVersion, ID: runID, State: state,
					Request: request, Result: resultJSON, Error: runtimeErr,
					SettledSequence: settledSequence,
					CreatedAt:       created, UpdatedAt: updated,
				}); err != nil {
					return err
				}
			}
		} else if request.ExecutionID != "" {
			return fmt.Errorf(
				"run %s has execution_id for kind %q", runID, kind,
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
