package model

import (
	"context"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/provider"
)

type AttemptNamespace string

const (
	AttemptNamespaceRequest AttemptNamespace = "req"
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

type attemptOriginKey struct{}

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
