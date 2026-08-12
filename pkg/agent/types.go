// Package agent implements the only Runtime model/tool/tool-result loop. The
// kernel depends only on injected ports and never reads profiles or opens a
// database.
package agent

import (
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
)

type State string

const (
	StateRunning             State = "running"
	StatePaused              State = "paused"
	StateNeedsReconciliation State = "needs_reconciliation"
	StateCompleted           State = "completed"
	StateFailed              State = "failed"
	StateCancelled           State = "cancelled"
)

type Budget struct {
	MaxRounds      int           `json:"max_rounds"`
	MaxToolCalls   int           `json:"max_tool_calls"`
	MaxTotalTokens int64         `json:"max_total_tokens,omitempty"`
	MaxWallTime    time.Duration `json:"max_wall_time"`
}

func DefaultBudget() Budget {
	return Budget{
		MaxRounds: 16, MaxToolCalls: 64, MaxWallTime: 15 * time.Minute,
	}
}

// Effective fills only omitted zero-valued limits. Negative values remain
// intact so validation cannot accidentally turn an invalid budget into a
// defaulted one.
func (budget Budget) Effective() Budget {
	defaults := DefaultBudget()
	if budget.MaxRounds == 0 {
		budget.MaxRounds = defaults.MaxRounds
	}
	if budget.MaxToolCalls == 0 {
		budget.MaxToolCalls = defaults.MaxToolCalls
	}
	if budget.MaxWallTime == 0 {
		budget.MaxWallTime = defaults.MaxWallTime
	}
	return budget
}

func (budget Budget) Validate() error {
	if budget.MaxRounds < 1 || budget.MaxRounds > 128 {
		return fmt.Errorf("max_rounds must be between 1 and 128")
	}
	if budget.MaxToolCalls < 1 || budget.MaxToolCalls > 1024 {
		return fmt.Errorf("max_tool_calls must be between 1 and 1024")
	}
	if budget.MaxTotalTokens < 0 {
		return fmt.Errorf("max_total_tokens must not be negative")
	}
	if budget.MaxWallTime < time.Second || budget.MaxWallTime > 24*time.Hour {
		return fmt.Errorf("max_wall_time must be between 1s and 24h")
	}
	return nil
}

type Pause struct {
	ID          string          `json:"pause_id"`
	Kind        string          `json:"kind"`
	InputSchema json.RawMessage `json:"input_schema"`
	Prompt      string          `json:"prompt"`
	ExpiresAt   *time.Time      `json:"expires_at,omitempty"`
	ToolCallID  string          `json:"tool_call_id"`
}

// PauseKindUserConfirmation 标识高风险写副作用执行前的人工确认门禁。该 kind
// 的 Pause 不闭合 durable effect（保持 started），Resume 时携带 approval 重跑
// handler 才触发真正副作用；期间崩溃则 effect 仍是 started，按既有不变量进入
// needs_reconciliation（fail-closed：副作用未发生）。
const PauseKindUserConfirmation = "user_confirmation"

type LoopState struct {
	SchemaVersion             int                 `json:"schema_version"`
	RunID                     string              `json:"run_id"`
	ModelProfile              string              `json:"model_profile"`
	Messages                  []contract.Message  `json:"messages"`
	BaseMessageCount          int                 `json:"base_message_count"`
	Round                     int                 `json:"round"`
	ToolCallCount             int                 `json:"tool_call_count"`
	TotalTokens               int64               `json:"total_tokens"`
	NextEventSequence         uint64              `json:"next_event_sequence"`
	SeenToolCallIDs           []string            `json:"seen_tool_call_ids,omitempty"`
	Pause                     *Pause              `json:"pause,omitempty"`
	PendingToolCalls          []contract.ToolCall `json:"pending_tool_calls,omitempty"`
	PendingToolCursor         int                 `json:"pending_tool_cursor,omitempty"`
	TerminalOutcome           *Outcome            `json:"terminal_outcome,omitempty"`
	PendingEffectCheckpointID string              `json:"pending_effect_checkpoint_id,omitempty"`

	// The following values are derived from the durable checkpoint/event
	// journal when a process resumes. They prevent replay from forging
	// duplicate lifecycle events and are intentionally not checkpointed.
	PendingCheckpointID        string `json:"-"`
	PendingCheckpointCommitted bool   `json:"-"`
	PendingToolStarted         bool   `json:"-"`
	PendingToolTerminal        bool   `json:"-"`
	RecoveredFromCheckpoint    bool   `json:"-"`
}

type Outcome struct {
	State      State                  `json:"state"`
	StopReason string                 `json:"stop_reason"`
	Message    *contract.Message      `json:"message,omitempty"`
	Pause      *Pause                 `json:"pause,omitempty"`
	Error      *contract.RuntimeError `json:"error,omitempty"`
}

type ToolRequest struct {
	RunID          string          `json:"run_id"`
	CallID         string          `json:"call_id"`
	IdempotencyKey string          `json:"idempotency_key"`
	Name           string          `json:"name"`
	Arguments      json.RawMessage `json:"arguments"`
	CheckpointID   string          `json:"checkpoint_id,omitempty"`

	// Approval 携带 user_confirmation pause 的 resume 输入，仅在 Resume 重跑
	// handler 的内存路径上设置；不持久化（json:"-"），不进入 checkpoint/effect
	// journal 或 digest。handler 据此判定是否执行真正副作用。
	Approval json.RawMessage `json:"-"`
}

type ToolResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
	Pause   *Pause `json:"pause,omitempty"`
}

const ToolExecutionSnapshotSchemaVersion = 1

// ToolExecutionIdentity identifies one tool executor implementation and its
// non-secret execution configuration. Version must change whenever behavior
// that is not otherwise represented by the snapshot changes.
type ToolExecutionIdentity struct {
	Implementation        string          `json:"implementation"`
	ImplementationVersion int             `json:"implementation_version"`
	Configuration         json.RawMessage `json:"configuration,omitempty"`
}

// ToolExecutionSnapshot is the canonical, non-secret description of the tool
// execution environment used by a run.
type ToolExecutionSnapshot struct {
	SchemaVersion int `json:"schema_version"`
	ToolExecutionIdentity
	Definitions []contract.ToolSpec `json:"definitions"`
}

// CanonicalJSON returns the stable JSON representation used for persistence
// and digest calculation.
func (snapshot ToolExecutionSnapshot) CanonicalJSON() ([]byte, error) {
	return canonicalToolExecutionSnapshot(snapshot)
}

// Digest returns the SHA-256 digest of CanonicalJSON.
func (snapshot ToolExecutionSnapshot) Digest() (string, error) {
	canonical, err := snapshot.CanonicalJSON()
	if err != nil {
		return "", err
	}
	sum := sha256.Sum256(canonical)
	return fmt.Sprintf("sha256:%x", sum[:]), nil
}

// ToolExecutionSnapshotter exposes only a read-only execution snapshot. The
// returned snapshot must not share mutable storage with the executor.
type ToolExecutionSnapshotter interface {
	ToolExecutionSnapshot() ToolExecutionSnapshot
}

// KnownFailure is the only handler error contract that proves a tool effect
// failed without leaving an unknown external side effect. Ordinary errors are
// treated as unknown after the durable effect has entered started.
type KnownFailure struct {
	RuntimeError *contract.RuntimeError
}

func (failure *KnownFailure) Error() string {
	if failure == nil || failure.RuntimeError == nil {
		return "known tool failure"
	}
	return failure.RuntimeError.Error()
}

type EffectRecord struct {
	State   string                 `json:"state"`
	Request ToolRequest            `json:"request"`
	Result  *ToolResult            `json:"result,omitempty"`
	Error   *contract.RuntimeError `json:"error,omitempty"`
}

type ToolExecutor interface {
	Definitions() []contract.ToolSpec
	Validate(ToolRequest) error
	Execute(context.Context, ToolRequest) (ToolResult, error)
}

// PreEffectGate runs immediately before a new model/tool side effect. Durable
// executors use it to prove that the current execution environment still
// matches the frozen Run snapshot.
type PreEffectGate func(context.Context) *contract.RuntimeError

type EffectRecorder interface {
	Lookup(context.Context, string, string) (EffectRecord, bool, error)
	Prepared(context.Context, *ToolRequest, *LoopState) (checkpointID string, err error)
	Started(context.Context, ToolRequest) error
	Completed(context.Context, ToolRequest, ToolResult) error
	Failed(context.Context, ToolRequest, *contract.RuntimeError) error
}

type Kernel struct {
	Model        model.Generator
	Tools        ToolExecutor
	Effects      EffectRecorder
	BeforeEffect PreEffectGate
	Budget       Budget
	Now          func() time.Time
}

type ResumeInput struct {
	PauseID string          `json:"pause_id"`
	Input   json.RawMessage `json:"input"`

	// AcceptedAt is supplied by a durable Run controller after it atomically
	// records acceptance. Standalone Kernel callers leave it nil and retain
	// execution-time expiry semantics.
	AcceptedAt *time.Time `json:"-"`
}
