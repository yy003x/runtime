// Package runtimeapi 定义本地 Runtime SDK 与 HTTP client 共用的稳定请求/响应契约。
package runtimeapi

import (
	"context"
	"time"
)

const (
	ToolModeSchemaOnly     = "schema_only"
	ToolModeRuntimeExecute = "runtime_execute"

	EventRequestStarted    = "request.started"
	EventContextCompiled   = "context.compiled"
	EventProviderStarted   = "provider.started"
	EventOutputDelta       = "output.delta"
	EventToolCall          = "tool.call"
	EventToolStarted       = "tool.started"
	EventToolCompleted     = "tool.completed"
	EventResponseCompleted = "response.completed"
	EventError             = "error"
)

type AssetRef struct {
	URI    string `json:"uri,omitempty"`
	Inline string `json:"inline,omitempty"`
	SHA256 string `json:"sha256,omitempty"`
}

type SkillRef struct {
	AssetRef
	Name      string         `json:"name,omitempty"`
	Variables map[string]any `json:"variables,omitempty"`
}

type ContextAssets struct {
	Prompts []AssetRef    `json:"prompts,omitempty"`
	Skills  []SkillRef    `json:"skills,omitempty"`
	Memory  []AssetRef    `json:"memory,omitempty"`
	Recall  []MemoryQuery `json:"recall,omitempty"`
}

type MemoryQuery struct {
	Provider string `json:"provider"`
	Query    string `json:"query,omitempty"`
	TopK     int    `json:"top_k,omitempty"`
}

type MemoryItem struct {
	Content  string         `json:"content"`
	Source   string         `json:"source,omitempty"`
	Score    float64        `json:"score,omitempty"`
	Metadata map[string]any `json:"metadata,omitempty"`
}

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type ToolSelection struct {
	Inline     []Tool   `json:"inline,omitempty"`
	Registered []string `json:"registered,omitempty"`
	MCP        []string `json:"mcp,omitempty"`
}

type Request struct {
	Profile     string         `json:"profile"`
	System      string         `json:"system,omitempty"`
	Prompt      string         `json:"prompt,omitempty"`
	Messages    []Message      `json:"messages,omitempty"`
	Context     ContextAssets  `json:"context,omitempty"`
	Tools       ToolSelection  `json:"tools,omitempty"`
	ToolMode    string         `json:"tool_mode,omitempty"`
	MaxRounds   int            `json:"max_rounds,omitempty"`
	Model       string         `json:"model,omitempty"`
	Temperature float64        `json:"temperature,omitempty"`
	MaxTokens   int            `json:"max_tokens,omitempty"`
	Metadata    map[string]any `json:"metadata,omitempty"`
}

type Usage struct {
	InputTokens  int `json:"input_tokens,omitempty"`
	OutputTokens int `json:"output_tokens,omitempty"`
}

type Response struct {
	Message      Message        `json:"message"`
	ToolCalls    []ToolCall     `json:"tool_calls,omitempty"`
	FinishReason string         `json:"finish_reason,omitempty"`
	Done         bool           `json:"done"`
	Rounds       int            `json:"rounds"`
	Usage        Usage          `json:"usage,omitempty"`
	Metadata     map[string]any `json:"metadata,omitempty"`
}

type Event struct {
	Sequence   int64     `json:"sequence"`
	Time       time.Time `json:"time"`
	Type       string    `json:"type"`
	Round      int       `json:"round,omitempty"`
	Delta      string    `json:"delta,omitempty"`
	ToolCall   *ToolCall `json:"tool_call,omitempty"`
	ToolName   string    `json:"tool_name,omitempty"`
	ToolResult any       `json:"tool_result,omitempty"`
	Response   *Response `json:"response,omitempty"`
	Error      string    `json:"error,omitempty"`
}

type EventSink func(Event) error

type Client interface {
	Generate(context.Context, Request) (Response, error)
}

type StreamingClient interface {
	Client
	GenerateStream(context.Context, Request, EventSink) (Response, error)
}
