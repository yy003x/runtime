// Package session owns Runtime local conversations, turns, message history,
// execution projections, and context manifests. It does not execute tools or
// own durable Run state.
package session

import (
	"encoding/json"
	"time"

	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/profile"
)

const SchemaVersion = 2

type SessionState string

const (
	SessionIdle     SessionState = "idle"
	SessionActive   SessionState = "active"
	SessionBlocked  SessionState = "blocked"
	SessionArchived SessionState = "archived"
)

type TurnState string

const (
	TurnPending        TurnState = "pending"
	TurnRunning        TurnState = "running"
	TurnRequiresAction TurnState = "requires_action"
	TurnCompleted      TurnState = "completed"
	TurnFailed         TurnState = "failed"
	TurnCancelled      TurnState = "cancelled"
)

type Retention string

const (
	RetentionEphemeral Retention = "ephemeral"
	RetentionStandard  Retention = "standard"
	RetentionPinned    Retention = "pinned"
)

type CaptureQuality string

const (
	CaptureStructured CaptureQuality = "structured"
)

type ExecutionState string

const (
	ExecutionSpawnIntent ExecutionState = "spawn_intent"
	ExecutionRunning     ExecutionState = "running"
	ExecutionSettled     ExecutionState = "settled"
)

type ExecutionOutcome string

const (
	OutcomeCompleted ExecutionOutcome = "completed"
	OutcomeFailed    ExecutionOutcome = "failed"
	OutcomeCancelled ExecutionOutcome = "cancelled"
	OutcomeUnknown   ExecutionOutcome = "unknown"
)

type Session struct {
	SchemaVersion   int          `json:"schema_version"`
	ID              string       `json:"session_id"`
	State           SessionState `json:"state"`
	Retention       Retention    `json:"retention"`
	CreatedAt       time.Time    `json:"created_at"`
	UpdatedAt       time.Time    `json:"updated_at"`
	ActiveTurnID    string       `json:"active_turn_id,omitempty"`
	MessageCount    uint64       `json:"message_count"`
	EventCount      uint64       `json:"event_count"`
	LastProfileID   string       `json:"last_profile_id,omitempty"`
	LastProfileKind profile.Kind `json:"last_profile_kind,omitempty"`
}

type Turn struct {
	SchemaVersion    int                          `json:"schema_version"`
	ID               string                       `json:"turn_id"`
	SessionID        string                       `json:"session_id"`
	RunID            string                       `json:"run_id"`
	ExecutionID      string                       `json:"execution_id"`
	TaskID           string                       `json:"task_id,omitempty"`
	ProfileID        string                       `json:"profile_id"`
	ProfileKind      profile.Kind                 `json:"profile_kind"`
	State            TurnState                    `json:"state"`
	CaptureQuality   CaptureQuality               `json:"capture_quality,omitempty"`
	PendingToolCalls []contract.ToolCall          `json:"pending_tool_calls,omitempty"`
	ToolResults      map[string]ToolResultReceipt `json:"tool_results,omitempty"`
	Error            *contract.RuntimeError       `json:"error,omitempty"`
	RequestDigest    string                       `json:"request_digest"`
	ConfigDigest     string                       `json:"config_digest"`
	BasePromptDigest string                       `json:"base_prompt_digest,omitempty"`
	CWD              string                       `json:"cwd,omitempty"`
	CreatedAt        time.Time                    `json:"created_at"`
	UpdatedAt        time.Time                    `json:"updated_at"`
}

type MessageRecord struct {
	Sequence    uint64           `json:"sequence"`
	Time        time.Time        `json:"time"`
	TurnID      string           `json:"turn_id"`
	RunID       string           `json:"run_id"`
	ExecutionID string           `json:"execution_id"`
	ProfileID   string           `json:"profile_id"`
	Message     contract.Message `json:"message"`
	IsError     bool             `json:"is_error,omitempty"`
}

type EventRecord struct {
	Sequence    uint64                 `json:"sequence"`
	Time        time.Time              `json:"time"`
	Type        string                 `json:"type"`
	TurnID      string                 `json:"turn_id,omitempty"`
	RunID       string                 `json:"run_id,omitempty"`
	ExecutionID string                 `json:"execution_id,omitempty"`
	State       string                 `json:"state,omitempty"`
	Error       *contract.RuntimeError `json:"error,omitempty"`
	Detail      json.RawMessage        `json:"detail,omitempty"`
}

type Execution struct {
	SchemaVersion    int                    `json:"schema_version"`
	ID               string                 `json:"execution_id"`
	SessionID        string                 `json:"session_id"`
	TurnID           string                 `json:"turn_id"`
	RunID            string                 `json:"run_id"`
	ProfileID        string                 `json:"profile_id"`
	ProfileKind      profile.Kind           `json:"profile_kind"`
	State            ExecutionState         `json:"state"`
	Outcome          ExecutionOutcome       `json:"outcome,omitempty"`
	RequestDigest    string                 `json:"request_digest"`
	ConfigDigest     string                 `json:"config_digest"`
	BasePromptDigest string                 `json:"base_prompt_digest,omitempty"`
	CWD              string                 `json:"cwd,omitempty"`
	Process          *ProcessIdentity       `json:"process,omitempty"`
	ExitCode         *int                   `json:"exit_code,omitempty"`
	Signal           string                 `json:"signal,omitempty"`
	Stdout           StreamObservation      `json:"stdout"`
	Stderr           StreamObservation      `json:"stderr"`
	Error            *contract.RuntimeError `json:"error,omitempty"`
	CreatedAt        time.Time              `json:"created_at"`
	UpdatedAt        time.Time              `json:"updated_at"`
}

type ProcessIdentity struct {
	OwnerPID        int    `json:"owner_pid"`
	OwnerStartToken string `json:"owner_start_token"`
	HelperPID       int    `json:"helper_pid,omitempty"`
	PGID            int    `json:"pgid,omitempty"`
	StartToken      string `json:"start_token,omitempty"`
}

type StreamObservation struct {
	ObservedBytes int64  `json:"observed_bytes"`
	PrefixDigest  string `json:"observed_prefix_digest,omitempty"`
	Truncated     bool   `json:"truncated"`
	LimitExceeded bool   `json:"limit_exceeded"`
	Summary       string `json:"summary,omitempty"`
}

type ContextManifest struct {
	SchemaVersion         int          `json:"schema_version"`
	SessionID             string       `json:"session_id"`
	TurnID                string       `json:"turn_id"`
	RunID                 string       `json:"run_id"`
	ExecutionID           string       `json:"execution_id"`
	TaskID                string       `json:"task_id,omitempty"`
	ProfileID             string       `json:"profile_id"`
	ProfileKind           profile.Kind `json:"profile_kind"`
	ConfigDigest          string       `json:"config_digest"`
	RequestDigest         string       `json:"request_digest"`
	BasePromptDigest      string       `json:"base_prompt_digest,omitempty"`
	CWD                   string       `json:"cwd,omitempty"`
	MessageSequenceStart  uint64       `json:"message_sequence_start"`
	MessageSequenceEnd    uint64       `json:"message_sequence_end"`
	MessageDigest         string       `json:"message_digest"`
	ContextWindowTokens   int64        `json:"context_window_tokens,omitempty"`
	ReservedOutputTokens  int64        `json:"reserved_output_tokens,omitempty"`
	InputBudgetTokens     int64        `json:"input_budget_tokens,omitempty"`
	EstimatedTokens       int64        `json:"estimated_tokens"`
	EstimatorCompleteness string       `json:"estimator_completeness"`
	CapacitySource        string       `json:"capacity_source"`
	PressureState         string       `json:"pressure_state"`
	CheckpointRef         string       `json:"checkpoint_ref,omitempty"`
	CheckpointDigest      string       `json:"checkpoint_digest,omitempty"`
	CurrentInputDigest    string       `json:"current_input_digest"`
	CreatedAt             time.Time    `json:"created_at"`
}

type ToolResultInput struct {
	ToolCallID     string `json:"tool_call_id"`
	Content        string `json:"content"`
	IsError        bool   `json:"is_error"`
	IdempotencyKey string `json:"idempotency_key"`
}

type ToolResultReceipt struct {
	ToolCallID      string    `json:"tool_call_id"`
	IdempotencyKey  string    `json:"idempotency_key"`
	ContentDigest   string    `json:"content_digest"`
	IsError         bool      `json:"is_error"`
	MessageSequence uint64    `json:"message_sequence"`
	AcceptedAt      time.Time `json:"accepted_at"`
}

type RunRequest struct {
	SessionID      string
	RunID          string
	TaskID         string
	ProfileID      string
	Input          string
	Model          string
	Effort         string
	CWD            string
	InvocationBase string
	Retention      Retention
	ModelOptions   contract.GenerateOptions
	Snapshot       *CLIExecutionSnapshot

	preparedRequestDigest    string
	preparedConfigDigest     string
	preparedBasePromptDigest string
}

// CLIExecutionSnapshot is the non-secret, store-only CLI invocation input
// frozen before a durable Run is queued. It is never part of public Session or
// Run DTOs.
type CLIExecutionSnapshot struct {
	SchemaVersion    int                    `json:"schema_version"`
	ProfileID        string                 `json:"profile_id"`
	Profile          runtimecommand.Profile `json:"profile"`
	BasePrompt       string                 `json:"base_prompt,omitempty"`
	ConfigDigest     string                 `json:"config_digest"`
	BasePromptDigest string                 `json:"base_prompt_digest,omitempty"`
	RequestDigest    string                 `json:"request_digest"`
	CWD              string                 `json:"cwd"`
	Model            string                 `json:"model,omitempty"`
	Effort           string                 `json:"effort,omitempty"`
}

type RunResult struct {
	SessionID      string                 `json:"session_id"`
	TurnID         string                 `json:"turn_id"`
	RunID          string                 `json:"run_id"`
	ExecutionID    string                 `json:"execution_id"`
	State          TurnState              `json:"state"`
	CaptureQuality CaptureQuality         `json:"capture_quality"`
	Message        *contract.Message      `json:"message,omitempty"`
	PendingActions []contract.ToolCall    `json:"pending_actions,omitempty"`
	ExitCode       *int                   `json:"exit_code,omitempty"`
	Error          *contract.RuntimeError `json:"error,omitempty"`
}

type ListFilter struct {
	State SessionState
}

type GCOptions struct {
	OlderThan time.Duration
	Limit     int
	Apply     bool
}

type GCResult struct {
	DryRun     bool     `json:"dry_run"`
	Candidates []string `json:"candidates"`
	Moved      []string `json:"moved,omitempty"`
}
