// Package agent implements the only Runtime model/tool/tool-result loop. The
// kernel depends only on injected ports and never reads profiles or opens a
// database.
package agent

import (
	"context"
	"encoding/json"
	"time"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
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

type Pause struct {
	ID          string          `json:"pause_id"`
	Kind        string          `json:"kind"`
	InputSchema json.RawMessage `json:"input_schema"`
	Prompt      string          `json:"prompt"`
	ExpiresAt   *time.Time      `json:"expires_at,omitempty"`
	ToolCallID  string          `json:"tool_call_id"`
}

type LoopState struct {
	SchemaVersion     int                `json:"schema_version"`
	RunID             string             `json:"run_id"`
	ModelProfile      string             `json:"model_profile"`
	Messages          []contract.Message `json:"messages"`
	Round             int                `json:"round"`
	ToolCallCount     int                `json:"tool_call_count"`
	TotalTokens       int64              `json:"total_tokens"`
	NextEventSequence uint64             `json:"next_event_sequence"`
	SeenToolCallIDs   []string           `json:"seen_tool_call_ids,omitempty"`
	Pause             *Pause             `json:"pause,omitempty"`
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
}

type ToolResult struct {
	Content string `json:"content"`
	IsError bool   `json:"is_error,omitempty"`
	Pause   *Pause `json:"pause,omitempty"`
}

type ToolExecutor interface {
	Definitions() []contract.ToolSpec
	Execute(context.Context, ToolRequest) (ToolResult, error)
}

type EffectRecorder interface {
	Prepared(context.Context, ToolRequest, LoopState) (checkpointID string, err error)
	Started(context.Context, ToolRequest) error
	Completed(context.Context, ToolRequest, ToolResult) error
	Failed(context.Context, ToolRequest, *contract.RuntimeError) error
}

type Kernel struct {
	Model   model.Generator
	Tools   ToolExecutor
	Effects EffectRecorder
	Budget  Budget
	Now     func() time.Time
}

type ResumeInput struct {
	PauseID string          `json:"pause_id"`
	Input   json.RawMessage `json:"input"`
}
