package model

import (
	"context"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/provider"
)

type AttemptNamespace string

const (
	AttemptNamespaceRequest AttemptNamespace = "call"
	AttemptNamespaceSession AttemptNamespace = "session"
	AttemptNamespaceAgent   AttemptNamespace = "agent"
)

// AttemptOrigin is trusted, process-local metadata. It does not enter the
// Provider-neutral request JSON, traces, snapshots, or durable stores.
type AttemptOrigin struct {
	Namespace AttemptNamespace
	Source    string
}

type Attempt struct {
	Origin    AttemptOrigin
	ProfileID string
	Wire      provider.Attempt
	Error     *contract.RuntimeError
}

// AttemptObserver receives a redacted snapshot after one real Provider call.
// It is diagnostic-only: its failure or panic must not affect model execution.
type AttemptObserver func(Attempt)

// CallObservation groups all started HTTP attempts of one logical model call.
// Attempts and Error are redacted diagnostics, not canonical execution state.
type CallObservation struct {
	Origin    AttemptOrigin
	ProfileID string
	Attempts  []Attempt
	Error     *contract.RuntimeError
}

// CallObserver runs once after a logical call that started at least one HTTP
// attempt. Like AttemptObserver, its failure or panic cannot affect execution.
type CallObserver func(CallObservation)

type callAttemptsKey struct{}

type callAttempts struct {
	values []Attempt
}

func pendingCallAttempts(ctx context.Context) *callAttempts {
	if ctx == nil {
		return nil
	}
	values, _ := ctx.Value(callAttemptsKey{}).(*callAttempts)
	return values
}

func notifyCallObserver(observer CallObserver, observation CallObservation) {
	if observer == nil || len(observation.Attempts) == 0 {
		return
	}
	// Call observations are built from already-redacted attempts and results.
	// Copy once more so the observer cannot mutate another diagnostic consumer.
	safe, err := cloneValue(observation)
	if err != nil {
		return
	}
	defer func() { _ = recover() }()
	observer(safe)
}

type attemptOriginKey struct{}
type attemptObserverKey struct{}

// WithAttemptObserver attaches a request-scoped diagnostic observer. It receives
// each started attempt (including retries) synchronously before Generate returns,
// in addition to the ServiceOptions observer, with an independent redacted copy.
// It replaces any observer on the parent context, not the service observer. A
// callback shared by concurrent calls must provide its own synchronization.
func WithAttemptObserver(ctx context.Context, observer AttemptObserver) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, attemptObserverKey{}, observer)
}

func attemptObserver(ctx context.Context) AttemptObserver {
	if ctx == nil {
		return nil
	}
	observer, _ := ctx.Value(attemptObserverKey{}).(AttemptObserver)
	return observer
}

func WithAttemptOrigin(ctx context.Context, origin AttemptOrigin) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	return context.WithValue(ctx, attemptOriginKey{}, origin)
}

func attemptOrigin(ctx context.Context) AttemptOrigin {
	if ctx != nil {
		if value, ok := ctx.Value(attemptOriginKey{}).(AttemptOrigin); ok {
			return value
		}
	}
	return AttemptOrigin{
		Namespace: AttemptNamespaceRequest,
		Source:    "model.Service",
	}
}
