// Package run owns durable execution identity, queueing, lifecycle control,
// checkpoints, events, and the terminal publish barrier. Business workflows
// and Session facts are outside this package.
package run

import (
	"context"
	"encoding/json"
	"errors"
	"time"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
)

var ErrSessionRunOpen = errors.New("session already has a nonterminal durable Run")

const MaxPrivateRequestBytes = 256 << 10

type Kind string

const (
	KindAgent   Kind = "agent"
	KindSession Kind = "session"
)

type State string

const (
	StateQueued              State = "queued"
	StateRunning             State = "running"
	StatePaused              State = "paused"
	StateNeedsReconciliation State = "needs_reconciliation"
	StateCompleted           State = "completed"
	StateFailed              State = "failed"
	StateCancelled           State = "cancelled"
)

func (state State) Terminal() bool {
	switch state {
	case StateCompleted, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}

type Request struct {
	Kind             Kind                     `json:"kind"`
	ProfileID        string                   `json:"profile_id"`
	Input            string                   `json:"input"`
	SessionID        string                   `json:"session_id,omitempty"`
	SessionRetention string                   `json:"session_retention,omitempty"`
	TaskID           string                   `json:"task_id,omitempty"`
	Model            string                   `json:"model,omitempty"`
	Effort           string                   `json:"effort,omitempty"`
	CWD              string                   `json:"cwd,omitempty"`
	ModelOptions     contract.GenerateOptions `json:"model_options,omitempty"`
	AgentBudget      agent.Budget             `json:"agent_budget,omitempty"`
	Labels           map[string]string        `json:"labels,omitempty"`
	RetryOf          string                   `json:"retry_of,omitempty"`
	Resume           json.RawMessage          `json:"resume,omitempty"`
	RequestDigest    string                   `json:"request_digest,omitempty"`
	ConfigDigest     string                   `json:"config_digest,omitempty"`
	BasePromptDigest string                   `json:"base_prompt_digest,omitempty"`

	// PrivateRequest is a store-only execution snapshot. It is persisted in the
	// dedicated private_request_json column and must never be serialized by a
	// public CLI/HTTP DTO, event, log, or error.
	PrivateRequest json.RawMessage `json:"-"`
	InvocationBase string          `json:"-"`
}

type Record struct {
	SchemaVersion   int                    `json:"schema_version"`
	ID              string                 `json:"run_id"`
	State           State                  `json:"state"`
	Request         Request                `json:"request"`
	Result          json.RawMessage        `json:"result,omitempty"`
	Error           *contract.RuntimeError `json:"error,omitempty"`
	Pause           json.RawMessage        `json:"pause,omitempty"`
	RetryOf         string                 `json:"retry_of,omitempty"`
	CancelRequested bool                   `json:"cancel_requested"`
	SettledSequence uint64                 `json:"settled_sequence,omitempty"`
	CreatedAt       time.Time              `json:"created_at"`
	UpdatedAt       time.Time              `json:"updated_at"`
}

type Checkpoint struct {
	ID        string          `json:"checkpoint_id"`
	RunID     string          `json:"run_id"`
	Sequence  uint64          `json:"sequence"`
	State     json.RawMessage `json:"state"`
	CreatedAt time.Time       `json:"created_at"`
}

type ToolEffect struct {
	RunID          string                 `json:"run_id"`
	CallID         string                 `json:"call_id"`
	IdempotencyKey string                 `json:"idempotency_key"`
	Name           string                 `json:"name"`
	State          string                 `json:"state"`
	Request        json.RawMessage        `json:"request"`
	Result         json.RawMessage        `json:"result,omitempty"`
	Error          *contract.RuntimeError `json:"error,omitempty"`
	UpdatedAt      time.Time              `json:"updated_at"`
}

type ModelCall struct {
	ID                string    `json:"model_call_id"`
	RunID             string    `json:"run_id"`
	Sequence          int       `json:"sequence"`
	RequestDigest     string    `json:"request_digest"`
	ProviderRequestID string    `json:"provider_request_id,omitempty"`
	State             string    `json:"state"`
	CreatedAt         time.Time `json:"created_at"`
	UpdatedAt         time.Time `json:"updated_at"`
}

type ListFilter struct {
	State State
	Kind  Kind
	Limit int
}

type GCOptions struct {
	Before time.Time
	Limit  int
	Apply  bool
}

type GCResult struct {
	Candidates []string `json:"candidates"`
	Deleted    []string `json:"deleted,omitempty"`
	Applied    bool     `json:"applied"`
}

type ExecutionOutcome struct {
	State  State
	Result json.RawMessage
	Pause  json.RawMessage
	Error  *contract.RuntimeError
}

type Executor interface {
	Validate(Request) error
	Execute(context.Context, Record, contract.EventSink) ExecutionOutcome
}

// RequestPreparer freezes executor-private inputs before a durable Run is
// created. The returned Request is what is validated and persisted.
type RequestPreparer interface {
	Prepare(context.Context, Request) (Request, error)
}

// ReconcileExecutor resolves an executor-owned needs_reconciliation record
// without replaying the original execution.
type ReconcileExecutor interface {
	Reconcile(context.Context, Record) ExecutionOutcome
}

type Store interface {
	Create(context.Context, string, Request) (Record, error)
	Get(context.Context, string) (Record, error)
	PrivateRequest(context.Context, string) (json.RawMessage, error)
	List(context.Context, ListFilter) ([]Record, error)
	Start(context.Context, string) (Record, error)
	Claim(context.Context, string) (Record, bool, error)
	AppendEvent(context.Context, string, contract.Event) (contract.Event, error)
	Events(context.Context, string, uint64, int) ([]contract.Event, error)
	SaveCheckpoint(context.Context, Checkpoint) error
	LatestCheckpoint(context.Context, string) (Checkpoint, bool, error)
	StartModelCall(context.Context, ModelCall) error
	FinishModelCall(context.Context, ModelCall) error
	PrepareToolEffect(context.Context, ToolEffect) error
	StartToolEffect(context.Context, string, string) error
	CompleteToolEffect(context.Context, ToolEffect) error
	FailToolEffect(context.Context, ToolEffect) error
	Pause(context.Context, string, json.RawMessage) (Record, error)
	NeedsReconciliation(context.Context, string, *contract.RuntimeError) (Record, error)
	Settle(
		context.Context,
		string,
		State,
		json.RawMessage,
		*contract.RuntimeError,
	) (Record, error)
	RequestCancel(context.Context, string) (Record, error)
	Resume(context.Context, string, json.RawMessage) (Record, error)
	Reconcile(context.Context) error
	GC(context.Context, GCOptions) (GCResult, error)
	Close() error
}
