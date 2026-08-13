package sqlite

import (
	"context"
	"fmt"
	"time"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
)

func (store *Store) ListExpiredRuns(
	ctx context.Context,
	filter runtime.ExpiredRunFilter,
) ([]runtime.Record, error) {
	filter, err := runtime.NormalizeExpiredRunFilter(filter)
	if err != nil {
		return nil, err
	}
	query := `SELECT run_id, state, request_json, result_json, error_json,
	                 pause_json, resume_accepted_at, retry_of, cancel_requested,
	                 settled_sequence,
	                 created_at, updated_at
	            FROM runs
	           WHERE state = ?
	             AND cancel_requested = 0
	             AND updated_at < ?`
	arguments := []any{
		filter.State,
		formatTime(filter.UpdatedBefore),
	}
	if filter.After != nil {
		if err := identity.Validate(filter.After.RunID, "run"); err != nil {
			return nil, err
		}
		afterTime := formatTime(filter.After.UpdatedAt)
		query += `
	             AND (updated_at > ? OR (updated_at = ? AND run_id > ?))`
		arguments = append(
			arguments, afterTime, afterTime, filter.After.RunID,
		)
	}
	query += ` ORDER BY updated_at, run_id LIMIT ?`
	arguments = append(arguments, filter.Limit)
	rows, err := store.db.QueryContext(ctx, query, arguments...)
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
		values = append(values, value)
	}
	return values, rows.Err()
}

type settleExpectation struct {
	state     runtime.State
	updatedAt time.Time
}

// SettleExpiredRun conditionally settles one candidate selected by
// ListExpiredRuns. The no-op update reserves SQLite's writer slot while
// atomically checking the selected state, updated_at and cancellation flag, so
// a resumed or cancellation-owned Run cannot be reaped from a stale page.
func (store *Store) SettleExpiredRun(
	ctx context.Context,
	runID string,
	expectedState runtime.State,
	expectedUpdatedAt time.Time,
	runtimeErr *contract.RuntimeError,
) (runtime.Record, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return runtime.Record{}, err
	}
	switch expectedState {
	case runtime.StatePaused, runtime.StateNeedsReconciliation:
	default:
		return runtime.Record{}, fmt.Errorf(
			"expected state must be paused or needs_reconciliation",
		)
	}
	if expectedUpdatedAt.IsZero() {
		return runtime.Record{}, fmt.Errorf("expected updated_at is required")
	}
	return store.settle(
		ctx, runID, runtime.StateFailed, nil, runtimeErr, false,
		&settleExpectation{
			state: expectedState, updatedAt: expectedUpdatedAt.UTC(),
		},
	)
}
