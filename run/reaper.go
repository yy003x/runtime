package run

import (
	"context"
	"time"

	"github.com/yy003x/runtime/contract"
)

// SweepReaper settles Runs stuck in paused or needs_reconciliation past their
// TTL. now is the reference instant and is injected so tests can fast-forward
// without sleeping. Runs with cancel_requested are skipped because their
// settlement is cancellation-owned. Returns the count of Runs settled.
//
// SweepReaper reuses the existing List(state) query and filters updated_at in
// memory; it deliberately avoids a schema bump (no new column or index), so it
// does not invalidate existing databases.
func (service *Service) SweepReaper(
	ctx context.Context,
	opts ReaperOptions,
	now time.Time,
) (int, error) {
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
		records, err := service.store.List(ctx, ListFilter{
			State: target.state, Limit: MaxListLimit,
		})
		if err != nil {
			return settled, err
		}
		for _, record := range records {
			if record.CancelRequested {
				continue
			}
			if !record.UpdatedAt.Before(cutoff) {
				continue
			}
			reaperErr := &contract.RuntimeError{
				Code: contract.ErrorTimeout, Phase: contract.PhaseRun,
				Message: "reaper: " + string(target.state) + " exceeded ttl",
			}
			if _, err := service.store.Settle(
				ctx, record.ID, StateFailed, nil, reaperErr,
			); err != nil {
				// Settle fails on a concurrent transition or cancellation
				// reservation; skip and let the next sweep retry.
				continue
			}
			settled++
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
			_, _ = service.SweepReaper(ctx, opts, time.Now())
		}
	}
}
