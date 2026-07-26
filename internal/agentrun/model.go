package agentrun

import (
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/provider"
)

const ContractVersion = 1

const (
	RunSession = "session"
	RunTurn    = "turn"
	RunTask    = "task"
	RunCommand = "command"
)

const (
	StatePending       = "pending"
	StateRunning       = "running"
	StateResultPending = "result_pending"
	StateDone          = "done"
	StateFailed        = "failed"
	StateBlocked       = "blocked"
	StateCancelled     = "cancelled"
)

const (
	ModeManaged = "managed"
	ModeCapture = "capture"
)

const (
	OutcomeSucceeded = "succeeded"
	OutcomeFailed    = "failed"
	OutcomeBlocked   = "blocked"
	OutcomePartial   = "partial"
	OutcomeCancelled = "cancelled"
)

type Request struct {
	SchemaVersion           int                 `json:"schema_version"`
	ContractVersion         int                 `json:"contract_version"`
	RuntimeVersion          string              `json:"runtime_version"`
	ProjectID               string              `json:"project_id"`
	RunType                 string              `json:"run_type"`
	RunID                   string              `json:"run_id"`
	Caller                  string              `json:"caller"`
	SessionID               string              `json:"session_id,omitempty"`
	TurnID                  string              `json:"turn_id,omitempty"`
	ExecutionID             string              `json:"execution_id,omitempty"`
	ExecutionKind           string              `json:"execution_kind,omitempty"`
	RecordMode              string              `json:"record_mode,omitempty"`
	Retention               string              `json:"retention,omitempty"`
	CaptureQuality          string              `json:"capture_quality,omitempty"`
	ProviderProfile         string              `json:"provider_profile"`
	Provider                string              `json:"provider"`
	CWD                     string              `json:"cwd,omitempty"`
	PromptFile              string              `json:"prompt_file,omitempty"`
	RawCLIArgs              []string            `json:"raw_cli_args,omitempty"`
	DeadlineSeconds         int                 `json:"deadline_seconds"`
	ResultFile              string              `json:"result_file"`
	ResultSchema            string              `json:"result_schema,omitempty"`
	ExecutionMode           string              `json:"execution_mode"`
	ModelOverride           string              `json:"model_override,omitempty"`
	ReasoningEffortOverride string              `json:"reasoning_effort_override,omitempty"`
	ProviderOverrides       map[string]any      `json:"provider_overrides"`
	AllowedActions          []string            `json:"allowed_actions"`
	ForbiddenActions        []string            `json:"forbidden_actions"`
	MemoryReads             []ContextMemoryRead `json:"memory_reads,omitempty"`
	RequestFingerprint      string              `json:"request_fingerprint,omitempty"`
	CreatedAt               time.Time           `json:"created_at"`
	UpdatedAt               time.Time           `json:"updated_at"`
}

type Status struct {
	SchemaVersion  int            `json:"schema_version"`
	RunID          string         `json:"run_id"`
	RunType        string         `json:"run_type"`
	ProjectID      string         `json:"project_id"`
	State          string         `json:"state"`
	FailureReason  string         `json:"failure_reason,omitempty"`
	Provider       string         `json:"provider"`
	ProviderStatus map[string]any `json:"provider_status"`
	Message        string         `json:"message,omitempty"`
	QueuedAt       time.Time      `json:"queued_at,omitzero"`
	StartedAt      time.Time      `json:"started_at,omitzero"`
	CompletedAt    time.Time      `json:"completed_at,omitzero"`
	QueuePosition  int            `json:"queue_position,omitempty"`
	Attempt        int            `json:"attempt,omitempty"`
	ErrorCode      string         `json:"error_code,omitempty"`
	Retryable      bool           `json:"retryable,omitempty"`
	UpdatedAt      time.Time      `json:"updated_at"`
}

type Result struct {
	SchemaVersion    int              `json:"schema_version"`
	RunID            string           `json:"run_id"`
	Outcome          string           `json:"outcome"`
	AssistantMessage string           `json:"assistant_message,omitempty"`
	Summary          string           `json:"summary"`
	ResultKind       string           `json:"result_kind,omitempty"`
	CaptureQuality   string           `json:"capture_quality,omitempty"`
	Artifacts        []map[string]any `json:"artifacts"`
	Errors           []map[string]any `json:"errors"`
	Validation       Validation       `json:"validation"`
}

func (r Result) SessionMessage() string {
	if value := strings.TrimSpace(r.AssistantMessage); value != "" {
		return value
	}
	return strings.TrimSpace(r.Summary)
}

type Validation struct {
	Commands []string `json:"commands"`
	Passed   bool     `json:"passed"`
}

type Event struct {
	SchemaVersion int            `json:"schema_version"`
	EventID       string         `json:"event_id"`
	RunID         string         `json:"run_id"`
	RunType       string         `json:"run_type"`
	Type          string         `json:"type"`
	Timestamp     time.Time      `json:"ts"`
	Sequence      int            `json:"seq"`
	Data          map[string]any `json:"data"`
}

type RunOptions struct {
	RunType   string `json:"run_type"`
	RunID     string `json:"run_id"`
	Profile   string `json:"profile"`
	ProjectID string `json:"project_id,omitempty"`
	Caller    string `json:"caller,omitempty"`
	SessionID string `json:"session_id,omitempty"`
	// CreateSession 表示调用方显式要求为本次 Run 创建逻辑 Session。
	// 普通 task/profile/HTTP Run 默认只写 runs artifact，不隐式创建 Session。
	CreateSession     bool                      `json:"create_session,omitempty"`
	TurnID            string                    `json:"turn_id,omitempty"`
	ExecutionID       string                    `json:"execution_id,omitempty"`
	ExecutionKind     string                    `json:"execution_kind,omitempty"`
	RecordMode        string                    `json:"record_mode,omitempty"`
	Retention         string                    `json:"retention,omitempty"`
	CWD               string                    `json:"cwd,omitempty"`
	Prompt            string                    `json:"prompt,omitempty"`
	PromptFile        string                    `json:"prompt_file,omitempty"`
	RawCLIArgs        []string                  `json:"raw_cli_args,omitempty"`
	DeadlineSeconds   int                       `json:"deadline_seconds,omitempty"`
	QueueTimeout      int                       `json:"queue_timeout_seconds,omitempty"`
	ResultSchema      string                    `json:"result_schema,omitempty"`
	ExecutionMode     string                    `json:"execution_mode,omitempty"`
	ProviderOverrides map[string]any            `json:"provider_overrides,omitempty"`
	AllowedActions    []string                  `json:"allowed_actions,omitempty"`
	ForbiddenActions  []string                  `json:"forbidden_actions,omitempty"`
	InjectedMemory    []provider.InjectedMemory `json:"injected_memory,omitempty"`
	Force             bool                      `json:"force,omitempty"`
}

type RunSummary struct {
	RunID         string    `json:"run_id"`
	ProjectID     string    `json:"project_id"`
	RunType       string    `json:"run_type"`
	State         string    `json:"state"`
	FailureReason string    `json:"failure_reason,omitempty"`
	ResultFile    string    `json:"result_file"`
	RunDir        string    `json:"run_dir"`
	SessionID     string    `json:"session_id,omitempty"`
	TurnID        string    `json:"turn_id,omitempty"`
	ExecutionID   string    `json:"execution_id,omitempty"`
	QueuePosition int       `json:"queue_position,omitempty"`
	QueuedAt      time.Time `json:"queued_at,omitzero"`
	StartedAt     time.Time `json:"started_at,omitzero"`
	CompletedAt   time.Time `json:"completed_at,omitzero"`
	ErrorCode     string    `json:"error_code,omitempty"`
	Retryable     bool      `json:"retryable,omitempty"`
	Idempotent    bool      `json:"idempotent,omitempty"`
	FinalText     string    `json:"-"`
}
