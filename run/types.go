// Package run owns durable execution identity, queueing, lifecycle control,
// checkpoints, events, and the terminal publish barrier. Business workflows
// and Session facts are outside this package.
package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
)

var (
	ErrNotFound       = errors.New("run not found")
	ErrConflict       = errors.New("run state conflict")
	ErrSessionRunOpen = errors.New("session already has a nonterminal durable Run")
	ErrCancelReserved = errors.New("run cancellation is reserved")
)

const (
	MaxPrivateRequestBytes = 256 << 10
	MaxResumeInputBytes    = 1 << 20
)

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
	SchemaVersion    int                    `json:"schema_version"`
	ID               string                 `json:"run_id"`
	State            State                  `json:"state"`
	Request          Request                `json:"request"`
	Result           json.RawMessage        `json:"result,omitempty"`
	Error            *contract.RuntimeError `json:"error,omitempty"`
	Pause            json.RawMessage        `json:"pause,omitempty"`
	RetryOf          string                 `json:"retry_of,omitempty"`
	CancelRequested  bool                   `json:"cancel_requested"`
	ResumeAcceptedAt *time.Time             `json:"-"`
	SettledSequence  uint64                 `json:"settled_sequence,omitempty"`
	CreatedAt        time.Time              `json:"created_at"`
	UpdatedAt        time.Time              `json:"updated_at"`
}

// MarshalJSON keeps the durable latest resume envelope private. The Store and
// executor retain Request.Resume in memory for replay validation, but public
// Run DTOs never expose the accepted resume input.
func (record Record) MarshalJSON() ([]byte, error) {
	type publicRecord Record
	value := publicRecord(record)
	value.Request = clonePublicRequest(record.Request)
	return json.Marshal(value)
}

func clonePublicRequest(value Request) Request {
	value.Resume = nil
	value.PrivateRequest = nil
	value.InvocationBase = ""
	return value
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
	ID                string                 `json:"model_call_id"`
	RunID             string                 `json:"run_id"`
	Sequence          int                    `json:"sequence"`
	RequestDigest     string                 `json:"request_digest"`
	Request           json.RawMessage        `json:"-"`
	ProviderRequestID string                 `json:"provider_request_id,omitempty"`
	Result            json.RawMessage        `json:"result,omitempty"`
	ResultDigest      string                 `json:"result_digest,omitempty"`
	Error             *contract.RuntimeError `json:"error,omitempty"`
	State             string                 `json:"state"`
	CreatedAt         time.Time              `json:"created_at"`
	UpdatedAt         time.Time              `json:"updated_at"`
}

type ResumeRecord struct {
	RunID       string          `json:"run_id"`
	Sequence    int             `json:"sequence"`
	Input       json.RawMessage `json:"-"`
	InputDigest string          `json:"input_digest"`
	AcceptedAt  time.Time       `json:"accepted_at"`
}

// ResumeConstraint binds a validated resume envelope to the exact durable
// pause snapshot. Store.Resume rechecks it and samples acceptance time inside
// the transaction so expiry cannot race validation.
type ResumeConstraint struct {
	Pause    json.RawMessage
	NotAfter *time.Time
}

type ListFilter struct {
	State State
	Kind  Kind
	Limit int
}

const (
	DefaultListLimit = 100
	MaxListLimit     = 1000
)

// NormalizeListFilter applies the single Run list default and validates the
// canonical filter shared by CLI, HTTP, Service, and Store callers.
func NormalizeListFilter(filter ListFilter) (ListFilter, error) {
	switch filter.State {
	case "", StateQueued, StateRunning, StatePaused,
		StateNeedsReconciliation, StateCompleted, StateFailed, StateCancelled:
	default:
		return ListFilter{}, fmt.Errorf(
			"state must be queued, running, paused, needs_reconciliation, completed, failed, or cancelled",
		)
	}
	switch filter.Kind {
	case "", KindAgent, KindSession:
	default:
		return ListFilter{}, fmt.Errorf("kind must be agent or session")
	}
	switch {
	case filter.Limit == 0:
		filter.Limit = DefaultListLimit
	case filter.Limit < 1 || filter.Limit > MaxListLimit:
		return ListFilter{}, fmt.Errorf(
			"limit must be between 1 and %d", MaxListLimit,
		)
	}
	return filter, nil
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

// ResumeValidator performs executor-owned, read-only validation against the
// durable paused record before Store.Resume publishes a queued mutation. The
// Store still rechecks paused state transactionally to close the race between
// validation and publication.
type ResumeValidator interface {
	ValidateResume(
		context.Context,
		Record,
		json.RawMessage,
	) (ResumeConstraint, error)
}

// CancellationFinalizer closes executor-owned projections after the Store has
// durably reserved cancellation for a queued or paused Run.
type CancellationFinalizer interface {
	FinalizeCancellation(context.Context, Record) ExecutionOutcome
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
	CancellationReservations(
		context.Context,
		string,
		int,
	) ([]Record, error)
	Start(context.Context, string) (Record, error)
	Claim(context.Context, string) (Record, bool, error)
	AppendEvent(context.Context, string, contract.Event) (contract.Event, error)
	Events(context.Context, string, uint64, int) ([]contract.Event, error)
	LatestEventSequence(context.Context, string) (uint64, error)
	SaveCheckpoint(context.Context, Checkpoint) error
	Checkpoint(context.Context, string) (Checkpoint, bool, error)
	LatestCheckpoint(context.Context, string) (Checkpoint, bool, error)
	StartModelCall(context.Context, ModelCall) error
	FinishModelCall(context.Context, ModelCall) error
	LatestModelCall(context.Context, string) (ModelCall, bool, error)
	ModelCalls(context.Context, string) ([]ModelCall, error)
	Resumes(context.Context, string) ([]ResumeRecord, error)
	PrepareToolEffect(context.Context, ToolEffect, Checkpoint) error
	ToolEffect(context.Context, string, string) (ToolEffect, bool, error)
	ToolEffects(context.Context, string) ([]ToolEffect, error)
	StartToolEffect(context.Context, string, string) error
	CompleteToolEffect(context.Context, ToolEffect) error
	FailToolEffect(context.Context, ToolEffect) error
	Pause(context.Context, string, json.RawMessage) (Record, error)
	NeedsReconciliation(context.Context, string, *contract.RuntimeError) (Record, error)
	NeedsCancellationReconciliation(
		context.Context,
		string,
		*contract.RuntimeError,
	) (Record, error)
	Settle(
		context.Context,
		string,
		State,
		json.RawMessage,
		*contract.RuntimeError,
	) (Record, error)
	SettleCancellation(
		context.Context,
		string,
		State,
		json.RawMessage,
		*contract.RuntimeError,
	) (Record, error)
	RequestCancel(context.Context, string) (Record, error)
	Resume(
		context.Context,
		string,
		json.RawMessage,
		ResumeConstraint,
	) (Record, error)
	Reconcile(context.Context) error
	GC(context.Context, GCOptions) (GCResult, error)
	Close() error
}
