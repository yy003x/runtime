// Package contract defines the stable Provider-neutral Go and JSON contract
// shared by SN Runtime model services and transports.
package contract

import (
	"encoding/json"
	"fmt"
	"time"
)

type Role string

const (
	RoleUser      Role = "user"
	RoleAssistant Role = "assistant"
	RoleTool      Role = "tool"
)

type FinishReason string

const (
	FinishStop          FinishReason = "stop"
	FinishToolCall      FinishReason = "tool_call"
	FinishLength        FinishReason = "length"
	FinishContentFilter FinishReason = "content_filter"
	FinishCancelled     FinishReason = "cancelled"
)

type EventType string

const (
	EventModelStarted           EventType = "model.started"
	EventContentDelta           EventType = "content.delta"
	EventReasoningDelta         EventType = "reasoning.delta"
	EventToolCallStarted        EventType = "tool.call.started"
	EventToolCallArgumentsDelta EventType = "tool.call.arguments.delta"
	EventModelCompleted         EventType = "model.completed"
	EventToolStarted            EventType = "tool.started"
	EventToolCompleted          EventType = "tool.completed"
	EventToolFailed             EventType = "tool.failed"
	EventAgentPaused            EventType = "agent.paused"
	EventAgentCompleted         EventType = "agent.completed"
	EventCheckpointCommitted    EventType = "checkpoint.committed"
	EventRunCompleted           EventType = "run.completed"
	EventRunFailed              EventType = "run.failed"
	EventRunCancelled           EventType = "run.cancelled"
	EventRunSettled             EventType = "run.settled"
)

// ErrorCode is the provider-neutral error classification used across the
// Runtime contract. Provider drivers (provider/internal/httpx) map HTTP status
// codes and transport/context errors into these codes; the canonical mapping
// is documented on httpx.ProviderError and httpx.NetworkError.
type ErrorCode string

const (
	ErrorInvalidRequest          ErrorCode = "invalid_request"
	ErrorAuthenticationFailed    ErrorCode = "authentication_failed"
	ErrorPermissionDenied        ErrorCode = "permission_denied"
	ErrorRateLimited             ErrorCode = "rate_limited"
	ErrorTimeout                 ErrorCode = "timeout"
	ErrorProviderUnavailable     ErrorCode = "provider_unavailable"
	ErrorProtocol                ErrorCode = "protocol_error"
	ErrorInvalidProviderResponse ErrorCode = "invalid_provider_response"
	ErrorContextOverflow         ErrorCode = "context_overflow"
	ErrorToolFailed              ErrorCode = "tool_failed"
	ErrorCancelled               ErrorCode = "cancelled"
	ErrorConflict                ErrorCode = "conflict"
	ErrorNotFound                ErrorCode = "not_found"
	ErrorInternal                ErrorCode = "internal"
	ErrorValidationFailed        ErrorCode = "validation_failed"
)

type ErrorPhase string

const (
	PhaseRequest   ErrorPhase = "request"
	PhaseProfile   ErrorPhase = "profile"
	PhaseProvider  ErrorPhase = "provider"
	PhaseTransport ErrorPhase = "transport"
	PhaseConsumer  ErrorPhase = "consumer"
	PhaseRun       ErrorPhase = "run"
)

type UsageSource string

const (
	UsageSourceProvider  UsageSource = "provider"
	UsageSourceEstimated UsageSource = "estimated"
)

type UsageCompleteness string

const (
	UsageComplete UsageCompleteness = "complete"
	UsagePartial  UsageCompleteness = "partial"
)

type GenerateRequest struct {
	ModelProfile string       `json:"model_profile"`
	Input        ModelRequest `json:"input"`
}

type ModelRequest struct {
	System   string          `json:"system,omitempty"`
	Messages []Message       `json:"messages"`
	Tools    []ToolSpec      `json:"tools,omitempty"`
	Options  GenerateOptions `json:"options,omitempty"`
	Trace    TraceContext    `json:"trace,omitempty"`
}

type Message struct {
	Role       Role       `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	IsError    bool       `json:"is_error,omitempty"`
}

type ToolSpec struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

// Effect 分级声明一个 tool 对运行环境的副作用边界。read_only 不改变外部
// 状态；write_local 仅改变本地 workspace；write_external 可改变本地以外的
// 状态（进程、网络、远端系统）。effect 是 Provider-neutral 的运行期元数据，
// 不进入发给 Provider 的 ToolSpec，仅由 Runtime/Agent 用于风险分级与确认门禁。
type Effect string

const (
	EffectReadOnly      Effect = "read_only"
	EffectWriteLocal    Effect = "write_local"
	EffectWriteExternal Effect = "write_external"
)

// Risk 是 tool 的风险等级，配合 Effect 决定是否需要人工确认。high 风险的
// 写副作用必须经过 UserConfirmation 才能执行。
type Risk string

const (
	RiskLow  Risk = "low"
	RiskHigh Risk = "high"
)

type ToolCall struct {
	ID        string          `json:"id"`
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type GenerateOptions struct {
	MaxOutputTokens *int64   `json:"max_output_tokens,omitempty"`
	Temperature     *float64 `json:"temperature,omitempty"`
	TopP            *float64 `json:"top_p,omitempty"`
	StopSequences   []string `json:"stop_sequences,omitempty"`
}

type TraceContext struct {
	Labels map[string]string `json:"labels,omitempty"`
}

type Event struct {
	Sequence   uint64           `json:"sequence"`
	Time       *time.Time       `json:"time,omitempty"`
	Type       EventType        `json:"type"`
	Model      *ModelEvent      `json:"model,omitempty"`
	Tool       *ToolEvent       `json:"tool,omitempty"`
	Agent      *AgentEvent      `json:"agent,omitempty"`
	Checkpoint *CheckpointEvent `json:"checkpoint,omitempty"`
	Run        *RunEvent        `json:"run,omitempty"`
	Error      *RuntimeError    `json:"error,omitempty"`
}

type ModelEvent struct {
	Text       string       `json:"text,omitempty"`
	ToolCallID string       `json:"tool_call_id,omitempty"`
	ToolCall   *ToolCall    `json:"tool_call,omitempty"`
	Result     *ModelResult `json:"result,omitempty"`
}

type ToolEvent struct {
	CallID         string `json:"call_id"`
	Name           string `json:"name,omitempty"`
	IdempotencyKey string `json:"idempotency_key,omitempty"`
	Content        string `json:"content,omitempty"`
	IsError        bool   `json:"is_error,omitempty"`
}

type AgentEvent struct {
	RunID      string `json:"run_id"`
	State      string `json:"state"`
	StopReason string `json:"stop_reason,omitempty"`
	PauseID    string `json:"pause_id,omitempty"`
}

type CheckpointEvent struct {
	RunID        string `json:"run_id"`
	CheckpointID string `json:"checkpoint_id"`
}

type RunEvent struct {
	RunID  string          `json:"run_id"`
	State  string          `json:"state"`
	Result json.RawMessage `json:"result,omitempty"`
}

type ModelResult struct {
	Message      Message      `json:"message"`
	FinishReason FinishReason `json:"finish_reason"`
	Usage        Usage        `json:"usage,omitempty"`
	Provider     ProviderInfo `json:"provider,omitempty"`
}

type Usage struct {
	InputTokens      *int64            `json:"input_tokens,omitempty"`
	OutputTokens     *int64            `json:"output_tokens,omitempty"`
	ReasoningTokens  *int64            `json:"reasoning_tokens,omitempty"`
	CacheReadTokens  *int64            `json:"cache_read_tokens,omitempty"`
	CacheWriteTokens *int64            `json:"cache_write_tokens,omitempty"`
	TotalTokens      *int64            `json:"total_tokens,omitempty"`
	Source           UsageSource       `json:"source,omitempty"`
	Completeness     UsageCompleteness `json:"completeness,omitempty"`
}

type ProviderInfo struct {
	Name      string `json:"name,omitempty"`
	Model     string `json:"model,omitempty"`
	RequestID string `json:"request_id,omitempty"`
}

type RuntimeError struct {
	Code         ErrorCode  `json:"code"`
	Phase        ErrorPhase `json:"phase"`
	Message      string     `json:"message"`
	Retryable    bool       `json:"retryable"`
	RetryAfterMS int64      `json:"retry_after_ms,omitempty"`
	HTTPStatus   int        `json:"http_status,omitempty"`
	Provider     string     `json:"provider,omitempty"`
	RequestID    string     `json:"request_id,omitempty"`
}

func (e *RuntimeError) Error() string {
	if e == nil {
		return ""
	}
	return fmt.Sprintf("%s/%s: %s", e.Phase, e.Code, e.Message)
}

type EventSink func(Event) error
