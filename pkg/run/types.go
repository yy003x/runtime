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

	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
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
	KindAgent     Kind = "agent"
	KindSession   Kind = "session"
	KindNativeTUI Kind = "native_tui"
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
	Kind               Kind                     `json:"kind"`
	ProfileID          string                   `json:"profile_id"`
	Input              string                   `json:"input"`
	SessionID          string                   `json:"session_id,omitempty"`
	ExecutionID        string                   `json:"execution_id,omitempty"`
	SessionRetention   string                   `json:"session_retention,omitempty"`
	TaskID             string                   `json:"task_id,omitempty"`
	Model              string                   `json:"model,omitempty"`
	Effort             string                   `json:"effort,omitempty"`
	CWD                string                   `json:"cwd,omitempty"`
	ModelOptions       contract.GenerateOptions `json:"model_options,omitempty"`
	AgentBudget        agent.Budget             `json:"agent_budget,omitempty"`
	CompletionCriteria CompletionCriteria       `json:"completion_criteria,omitempty"`
	Labels             map[string]string        `json:"labels,omitempty"`
	RetryOf            string                   `json:"retry_of,omitempty"`
	Resume             json.RawMessage          `json:"resume,omitempty"`
	RequestDigest      string                   `json:"request_digest,omitempty"`
	ConfigDigest       string                   `json:"config_digest,omitempty"`
	BasePromptDigest   string                   `json:"base_prompt_digest,omitempty"`

	// PrivateRequest is a store-only execution snapshot. It is persisted in the
	// dedicated private_request_json column and must never be serialized by a
	// public CLI/HTTP DTO, event, log, or error.
	PrivateRequest json.RawMessage `json:"-"`
	InvocationBase string          `json:"-"`
}

// NativeTUIExecution is the canonical lifecycle evidence for one provider
// process hosted by a native_tui Session. It deliberately records only opaque
// process/window facts: a successful process exit is not a canonical Turn or
// a claim that any interactive task completed.
type NativeTUIExecution struct {
	SchemaVersion    int                    `json:"schema_version"`
	ID               string                 `json:"execution_id"`
	RunID            string                 `json:"run_id"`
	SessionID        string                 `json:"session_id"`
	TmuxID           string                 `json:"tmux_id,omitempty"`
	State            string                 `json:"state"`
	Outcome          string                 `json:"outcome,omitempty"`
	CaptureQuality   string                 `json:"capture_quality"`
	ExitCode         *int                   `json:"exit_code,omitempty"`
	Signal           string                 `json:"signal,omitempty"`
	CompletionReason string                 `json:"completion_reason,omitempty"`
	Error            *contract.RuntimeError `json:"error,omitempty"`
	StartedAt        time.Time              `json:"started_at"`
	SettledAt        *time.Time             `json:"settled_at,omitempty"`
}

type NativeTUIResult struct {
	Execution NativeTUIExecution `json:"execution"`
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

// ExpiredRunCursor is the stable keyset position used while scanning Runs for
// reaping. UpdatedAt is ordered first and RunID breaks timestamp ties.
type ExpiredRunCursor struct {
	UpdatedAt time.Time
	RunID     string
}

// ExpiredRunFilter selects one page of non-cancelled Runs that have remained
// in a reaper-owned state before UpdatedBefore. After is an exclusive keyset
// cursor; nil starts at the oldest matching Run.
type ExpiredRunFilter struct {
	State         State
	UpdatedBefore time.Time
	After         *ExpiredRunCursor
	Limit         int
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
	case "", KindAgent, KindSession, KindNativeTUI:
	default:
		return ListFilter{}, fmt.Errorf(
			"kind must be agent, session, or native_tui",
		)
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

// NormalizeExpiredRunFilter validates the private maintenance query contract.
// The reaper uses the maximum page size explicitly; the default remains the
// same as the public List filter for defensive direct Store callers.
func NormalizeExpiredRunFilter(
	filter ExpiredRunFilter,
) (ExpiredRunFilter, error) {
	switch filter.State {
	case StatePaused, StateNeedsReconciliation:
	default:
		return ExpiredRunFilter{}, fmt.Errorf(
			"state must be paused or needs_reconciliation",
		)
	}
	if filter.UpdatedBefore.IsZero() {
		return ExpiredRunFilter{}, fmt.Errorf("updated_before is required")
	}
	if filter.After != nil {
		if filter.After.UpdatedAt.IsZero() || filter.After.RunID == "" {
			return ExpiredRunFilter{}, fmt.Errorf(
				"after cursor requires updated_at and run_id",
			)
		}
		cursor := *filter.After
		cursor.UpdatedAt = cursor.UpdatedAt.UTC()
		filter.After = &cursor
	}
	switch {
	case filter.Limit == 0:
		filter.Limit = DefaultListLimit
	case filter.Limit < 1 || filter.Limit > MaxListLimit:
		return ExpiredRunFilter{}, fmt.Errorf(
			"limit must be between 1 and %d", MaxListLimit,
		)
	}
	filter.UpdatedBefore = filter.UpdatedBefore.UTC()
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

// Trace is the aggregated, read-only evidence for a single Run: the durable
// record plus its event, model-call and tool-effect journals. It surfaces the
// structured trace that already exists across the four tables (correlated by
// run_id) for one Run, without introducing a parallel trace store or a separate
// trace_id column. model_calls.provider_request_id serves as the per-call span
// identifier when one is needed.
type Trace struct {
	Run         Record           `json:"run"`
	Events      []contract.Event `json:"events"`
	ModelCalls  []ModelCall      `json:"model_calls"`
	ToolEffects []ToolEffect     `json:"tool_effects"`
}

// ReaperOptions controls a background sweep that settles Runs stuck in
// non-terminal states (paused, needs_reconciliation) past their TTL. A zero TTL
// disables that state's sweep.
type ReaperOptions struct {
	Interval               time.Duration
	PausedTTL              time.Duration
	NeedsReconciliationTTL time.Duration
}

type ExecutionOutcome struct {
	State  State
	Result json.RawMessage
	Pause  json.RawMessage
	Error  *contract.RuntimeError
}

// CompletionCriteria declares how a Run's completion is verified against real
// environment evidence rather than the model's claim. Checks run after the
// executor returns StateCompleted and before the run settles.
type CompletionCriteria struct {
	Checks []CompletionCheck `json:"checks,omitempty"`
}

// CompletionCheck is a single verifiable completion assertion. Type "command"
// runs Command as a subprocess (exit 0 == pass) in the Run's CWD.
type CompletionCheck struct {
	Name    string   `json:"name"`
	Type    string   `json:"type"`
	Command []string `json:"command,omitempty"`
}

// ValidationResult is the outcome of a CompletionValidator pass.
type ValidationResult struct {
	Passed  bool          `json:"passed"`
	Checks  []CheckResult `json:"checks,omitempty"`
	Summary string        `json:"summary,omitempty"`
}

// CheckResult is the result of one CompletionCheck.
type CheckResult struct {
	Name   string `json:"name"`
	Passed bool   `json:"passed"`
	Detail string `json:"detail,omitempty"`
}

// CompletionValidator is an optional Executor capability. When an executor
// implements it, the run service validates a StateCompleted outcome before
// settling: a non-nil error means validation itself failed and the run cannot
// be judged complete (it enters needs_reconciliation); a ValidationResult with
// Passed=false means the run demonstrably failed its criteria and is settled as
// failed with the validation evidence. Executors that do not implement this
// interface are validated exactly as before (model/executor outcome is trusted).
type CompletionValidator interface {
	ValidateCompletion(
		ctx context.Context,
		record Record,
		outcome ExecutionOutcome,
	) (ValidationResult, error)
}

// ValidationRuntimeError translates a failed ValidationResult into a
// RuntimeError suitable for settling the Run as failed.
func ValidationRuntimeError(validation ValidationResult) *contract.RuntimeError {
	summary := validation.Summary
	if summary == "" {
		summary = "completion validation failed"
	}
	return &contract.RuntimeError{
		Code:    contract.ErrorValidationFailed,
		Phase:   contract.PhaseRun,
		Message: summary,
	}
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

// RunReader exposes the read-only Run record surface.
type RunReader interface {
	Get(context.Context, string) (Record, error)
	PrivateRequest(context.Context, string) (json.RawMessage, error)
	List(context.Context, ListFilter) ([]Record, error)
	Resumes(context.Context, string) ([]ResumeRecord, error)
}

// ReaperStore exposes the maintenance-only keyset scan and conditional
// terminal transition. SettleExpiredRun must return ErrConflict when the
// selected state or updated_at changed before it could reserve settlement, and
// ErrCancelReserved when cancellation won the race.
type ReaperStore interface {
	ListExpiredRuns(
		context.Context,
		ExpiredRunFilter,
	) ([]Record, error)
	SettleExpiredRun(
		context.Context,
		string,
		State,
		time.Time,
		*contract.RuntimeError,
	) (Record, error)
}

// LeaseManager owns queue claim and cancellation reservation bookkeeping.
type LeaseManager interface {
	CancellationReservations(
		context.Context,
		string,
		int,
	) ([]Record, error)
	Start(context.Context, string) (Record, error)
	Claim(context.Context, string) (Record, bool, error)
}

// EventStore appends and reads the durable Run event journal.
type EventStore interface {
	AppendEvent(context.Context, string, contract.Event) (contract.Event, error)
	Events(context.Context, string, uint64, int) ([]contract.Event, error)
	LatestEventSequence(context.Context, string) (uint64, error)
}

// CheckpointStore persists executor checkpoints for crash recovery.
type CheckpointStore interface {
	SaveCheckpoint(context.Context, Checkpoint) error
	Checkpoint(context.Context, string) (Checkpoint, bool, error)
	LatestCheckpoint(context.Context, string) (Checkpoint, bool, error)
}

// ModelEffectStore tracks durable model call side effects.
type ModelEffectStore interface {
	StartModelCall(context.Context, ModelCall) error
	FinishModelCall(context.Context, ModelCall) error
	LatestModelCall(context.Context, string) (ModelCall, bool, error)
	ModelCalls(context.Context, string) ([]ModelCall, error)
}

// ToolEffectStore tracks durable tool side effects.
type ToolEffectStore interface {
	PrepareToolEffect(context.Context, ToolEffect, Checkpoint) error
	ToolEffect(context.Context, string, string) (ToolEffect, bool, error)
	ToolEffects(context.Context, string) ([]ToolEffect, error)
	StartToolEffect(context.Context, string, string) error
	CompleteToolEffect(context.Context, ToolEffect) error
	FailToolEffect(context.Context, ToolEffect) error
}

// RunController owns Run state transitions (pause/resume/cancel/settle).
// Multi-step atomic transitions must occur within a single Store transaction.
type RunController interface {
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
}

// StoreMaintenance runs background reconciliation, GC and teardown.
type StoreMaintenance interface {
	Reconcile(context.Context) error
	GC(context.Context, GCOptions) (GCResult, error)
	Close() error
}

// Store is the full durable Run control plane: every sub-store plus Create.
// store/sqlite satisfies all sub-interfaces; consumers should depend on the
// narrowest sub-interface they need (e.g. AgentExecutor depends on AgentStore).
type Store interface {
	RunReader
	LeaseManager
	EventStore
	CheckpointStore
	ModelEffectStore
	ToolEffectStore
	RunController
	StoreMaintenance
	Create(context.Context, string, Request) (Record, error)
}

// NativeTUILifecycleStore is the narrow durable surface used by the
// native_tui composition. CreateRunning publishes an externally supervised
// Run without exposing it to the worker queue; OpenSessionRun resolves the
// unique nonterminal lifecycle owner for session close/recovery.
type NativeTUILifecycleStore interface {
	RunReader
	RunController
	CreateRunning(context.Context, string, Request) (Record, error)
	OpenSessionRun(context.Context, string, Kind) (Record, bool, error)
	Close() error
}

// AgentStore is the Store surface an AgentExecutor needs: read access, the
// event/checkpoint/effect journals it writes during execution, and the
// RunController transitions it drives (settle). It excludes queue lease,
// maintenance and creation.
type AgentStore interface {
	RunReader
	EventStore
	CheckpointStore
	ModelEffectStore
	ToolEffectStore
	RunController
}
