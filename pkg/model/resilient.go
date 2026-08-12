package model

import (
	"context"
	"math"
	"math/rand"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
)

// RetryPolicy bounds the transient-error retry that ResilientModel applies
// around Service.GenerateStream. Each retry is a fresh GenerateStream call;
// the inner driver still performs exactly one HTTP attempt. Non-retryable
// errors and request-validation errors are never retried.
type RetryPolicy struct {
	// MaxAttempts is the total number of attempts including the first.
	// Defaults to 3 when zero or negative.
	MaxAttempts int
	// BaseDelay is the backoff before the first retry. Defaults to 200ms.
	BaseDelay time.Duration
	// MaxDelay caps any single backoff, even when the provider advertises a
	// larger Retry-After, so a run cannot stall on a single model call.
	// Defaults to 5s.
	MaxDelay time.Duration
	// Jitter is the fraction (0..1) by which each backoff is randomly
	// reduced to avoid synchronized retry storms. Defaults to 0.2.
	Jitter float64
}

// DefaultRetryPolicy returns a conservative production policy: at most 3
// attempts, 200ms base backoff doubling per retry, 5s cap, 20% jitter.
func DefaultRetryPolicy() RetryPolicy {
	return RetryPolicy{
		MaxAttempts: 3,
		BaseDelay:   200 * time.Millisecond,
		MaxDelay:    5 * time.Second,
		Jitter:      0.2,
	}
}

func (policy RetryPolicy) normalized() RetryPolicy {
	defaults := DefaultRetryPolicy()
	if policy.MaxAttempts < 1 {
		policy.MaxAttempts = defaults.MaxAttempts
	}
	if policy.BaseDelay <= 0 {
		policy.BaseDelay = defaults.BaseDelay
	}
	if policy.MaxDelay <= 0 {
		policy.MaxDelay = defaults.MaxDelay
	}
	if policy.MaxDelay < policy.BaseDelay {
		policy.MaxDelay = policy.BaseDelay
	}
	if policy.Jitter < 0 {
		policy.Jitter = 0
	}
	if policy.Jitter > 1 {
		policy.Jitter = 1
	}
	return policy
}

// resilientInner is the union of generation and execution-snapshot capability
// that *Service satisfies. ResilientModel retries generation while forwarding
// execution snapshots unchanged, so it remains a drop-in replacement for
// *Service on both the Agent (which requires both interfaces on the same
// object) and the Session execution paths.
type resilientInner interface {
	Generator
	ExecutionSnapshotter
}

// ResilientModel wraps an inner service and retries Retryable transient errors
// with exponential backoff. Per-attempt events are captured internally and
// only replayed to the real sink on success, so a failed attempt never leaks a
// partial event stream (which would violate the single model.started invariant
// the downstream Agent kernel and event journal rely on).
//
// TODO(fallback): FallbackChain across multiple drivers/profiles is intentionally
// out of scope here; it requires profile schema changes to express a driver
// preference list. The retry boundary is single-profile only for now.
type ResilientModel struct {
	inner   resilientInner
	policy  RetryPolicy
	backoff func(attempt int, retryAfter time.Duration) time.Duration
	sleep   func(ctx context.Context, delay time.Duration) bool
}

// NewResilientModel wraps inner with policy. Pass DefaultRetryPolicy() for the
// production default. inner must be non-nil.
func NewResilientModel(inner *Service, policy RetryPolicy) *ResilientModel {
	model := &ResilientModel{inner: inner, policy: policy.normalized()}
	model.backoff = model.defaultBackoff
	model.sleep = defaultRetrySleep
	return model
}

// Generate runs a non-streaming inference with the same retry semantics as
// GenerateStream (events are captured but discarded since there is no sink).
func (model *ResilientModel) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return model.GenerateStream(ctx, request, nil)
}

func (model *ResilientModel) GenerateStream(
	ctx context.Context,
	request contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	policy := model.policy
	var lastError *contract.RuntimeError
	for attempt := 0; attempt < policy.MaxAttempts; attempt++ {
		captured := make([]contract.Event, 0, 16)
		captureSink := func(event contract.Event) error {
			captured = append(captured, event)
			return nil
		}
		result, err := model.inner.GenerateStream(ctx, request, captureSink)
		if err == nil {
			if sink != nil {
				for _, event := range captured {
					if sinkErr := sink(event); sinkErr != nil {
						return contract.ModelResult{}, runtimeError(
							contract.ErrorCancelled,
							contract.PhaseConsumer,
							"event sink stopped",
						)
					}
				}
			}
			return result, nil
		}
		lastError = err
		if !err.Retryable || attempt+1 >= policy.MaxAttempts {
			return contract.ModelResult{}, err
		}
		retryAfter := time.Duration(err.RetryAfterMS) * time.Millisecond
		if !model.sleep(ctx, model.backoff(attempt, retryAfter)) {
			return contract.ModelResult{}, model.contextError(ctx, err)
		}
	}
	return contract.ModelResult{}, lastError
}

// ExecutionSnapshot forwards to the inner service so the Agent executor can
// freeze and later re-verify the model execution identity through the same
// object that performs (resilient) generation.
func (model *ResilientModel) ExecutionSnapshot(
	profileID string,
) (ExecutionSnapshot, error) {
	return model.inner.ExecutionSnapshot(profileID)
}

func (model *ResilientModel) defaultBackoff(
	attempt int,
	retryAfter time.Duration,
) time.Duration {
	policy := model.policy
	backoff := float64(policy.BaseDelay) * math.Pow(2, float64(attempt))
	if retryAfter > 0 && float64(retryAfter) > backoff {
		backoff = float64(retryAfter)
	}
	if backoff > float64(policy.MaxDelay) {
		backoff = float64(policy.MaxDelay)
	}
	backoff *= 1 - rand.Float64()*policy.Jitter
	if backoff < 0 {
		backoff = 0
	}
	return time.Duration(backoff)
}

func (model *ResilientModel) contextError(
	ctx context.Context,
	fallback *contract.RuntimeError,
) *contract.RuntimeError {
	if ctxErr := ctx.Err(); ctxErr != nil {
		code := contract.ErrorCancelled
		if ctxErr == context.DeadlineExceeded {
			code = contract.ErrorTimeout
		}
		return runtimeError(code, contract.PhaseProvider, ctxErr.Error())
	}
	return fallback
}

func defaultRetrySleep(ctx context.Context, delay time.Duration) bool {
	if delay <= 0 {
		return true
	}
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-timer.C:
		return true
	case <-ctx.Done():
		return false
	}
}
