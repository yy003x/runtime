package run

import (
	"context"
	"errors"
	"fmt"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
)

// SweepReaper settles Runs stuck in paused or needs_reconciliation past their
// TTL. now is the reference instant and is injected so tests can fast-forward
// without sleeping. Runs with cancel_requested are skipped because their
// settlement is cancellation-owned. Returns the count of Runs settled.
func (service *Service) SweepReaper(
	ctx context.Context,
	opts ReaperOptions,
	now time.Time,
) (int, error) {
	if opts.PausedTTL <= 0 && opts.NeedsReconciliationTTL <= 0 {
		return 0, nil
	}
	reaperStore, ok := service.store.(ReaperStore)
	if !ok {
		return 0, fmt.Errorf("Run Store does not support reaper maintenance")
	}
	settled := 0
	targets := []struct {
		state State
		ttl   time.Duration
	}{
		{StatePaused, opts.PausedTTL},
		{StateNeedsReconciliation, opts.NeedsReconciliationTTL},
	}
	for _, target := range targets {
		if target.ttl <= 0 {
			continue
		}
		cutoff := now.Add(-target.ttl)
		var after *ExpiredRunCursor
		for {
			records, err := reaperStore.ListExpiredRuns(
				ctx,
				ExpiredRunFilter{
					State: target.state, UpdatedBefore: cutoff,
					After: after, Limit: MaxListLimit,
				},
			)
			if err != nil {
				return settled, fmt.Errorf(
					"list expired %s Runs: %w", target.state, err,
				)
			}
			for _, record := range records {
				reaperErr := &contract.RuntimeError{
					Code: contract.ErrorTimeout, Phase: contract.PhaseRun,
					Message: "reaper: " + string(target.state) + " exceeded ttl",
				}
				if _, err := reaperStore.SettleExpiredRun(
					ctx, record.ID, target.state, record.UpdatedAt, reaperErr,
				); err != nil {
					// A state transition or cancellation reservation that won
					// after the page query owns this Run. Every other error is a
					// Store failure and must stop the sweep.
					if errors.Is(err, ErrConflict) ||
						errors.Is(err, ErrCancelReserved) {
						continue
					}
					return settled, fmt.Errorf(
						"settle expired Run %s: %w", record.ID, err,
					)
				}
				settled++
			}
			if len(records) < MaxListLimit {
				break
			}
			last := records[len(records)-1]
			after = &ExpiredRunCursor{
				UpdatedAt: last.UpdatedAt,
				RunID:     last.ID,
			}
		}
	}
	return settled, nil
}

// RunReaper runs SweepReaper on opts.Interval until ctx is cancelled. Intended
// for sn-server; single-command CLI modes never start it. A non-positive
// interval disables the loop and returns immediately.
func (service *Service) RunReaper(
	ctx context.Context,
	opts ReaperOptions,
) error {
	if opts.Interval <= 0 {
		return nil
	}
	ticker := time.NewTicker(opts.Interval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return ctx.Err()
		case <-ticker.C:
			if _, err := service.SweepReaper(ctx, opts, time.Now()); err != nil {
				return fmt.Errorf("run reaper sweep: %w", err)
			}
		}
	}
}
