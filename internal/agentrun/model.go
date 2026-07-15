package agentrun

import "time"

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
	SchemaVersion           int            `json:"schema_version"`
	ContractVersion         int            `json:"contract_version"`
	RuntimeVersion          string         `json:"runtime_version"`
	ProjectID               string         `json:"project_id"`
	RunType                 string         `json:"run_type"`
	RunID                   string         `json:"run_id"`
	Caller                  string         `json:"caller"`
	ProviderProfile         string         `json:"provider_profile"`
	Provider                string         `json:"provider"`
	CWD                     string         `json:"cwd,omitempty"`
	PromptFile              string         `json:"prompt_file,omitempty"`
	RawCLIArgs              []string       `json:"raw_cli_args,omitempty"`
	DeadlineSeconds         int            `json:"deadline_seconds"`
	ResultFile              string         `json:"result_file"`
	ResultSchema            string         `json:"result_schema,omitempty"`
	ExecutionMode           string         `json:"execution_mode"`
	ModelOverride           string         `json:"model_override,omitempty"`
	ReasoningEffortOverride string         `json:"reasoning_effort_override,omitempty"`
	ProviderOverrides       map[string]any `json:"provider_overrides"`
	AllowedActions          []string       `json:"allowed_actions"`
	ForbiddenActions        []string       `json:"forbidden_actions"`
	CreatedAt               time.Time      `json:"created_at"`
	UpdatedAt               time.Time      `json:"updated_at"`
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
	UpdatedAt      time.Time      `json:"updated_at"`
}

type Result struct {
	SchemaVersion int              `json:"schema_version"`
	RunID         string           `json:"run_id"`
	Outcome       string           `json:"outcome"`
	Summary       string           `json:"summary"`
	Artifacts     []map[string]any `json:"artifacts"`
	Errors        []map[string]any `json:"errors"`
	Validation    Validation       `json:"validation"`
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
	RunType           string
	RunID             string
	Profile           string
	ProjectID         string
	Caller            string
	CWD               string
	Prompt            string
	PromptFile        string
	RawCLIArgs        []string
	DeadlineSeconds   int
	ResultSchema      string
	ExecutionMode     string
	ProviderOverrides map[string]any
	AllowedActions    []string
	ForbiddenActions  []string
	Force             bool
}

type RunSummary struct {
	RunID         string `json:"run_id"`
	ProjectID     string `json:"project_id"`
	RunType       string `json:"run_type"`
	State         string `json:"state"`
	FailureReason string `json:"failure_reason,omitempty"`
	ResultFile    string `json:"result_file"`
	RunDir        string `json:"run_dir"`
	Idempotent    bool   `json:"idempotent,omitempty"`
	FinalText     string `json:"-"`
}
