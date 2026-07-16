package native

import (
	"context"
	"errors"
	"fmt"
	"time"
)

type State string

const (
	StateCreated      State = "created"
	StateRunning      State = "running"
	StateWaitingLLM   State = "waiting_llm"
	StateWaitingHuman State = "waiting_human"
	StateBlocked      State = "blocked"
	StateStopped      State = "stopped"
	StateCompleted    State = "completed"
	StateCancelled    State = "cancelled"
	StateFailed       State = "failed"
)

var (
	ErrInvalidTransition = errors.New("invalid agent state transition")
	ErrUpstream          = errors.New("agent llm upstream error")
)

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	Pinned     bool       `json:"pinned,omitempty"`
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

type Context struct {
	SystemInstructions []Message `json:"system_instructions"`
	Messages           []Message `json:"messages"`
}

type PatchOperation string

const (
	PatchAppend  PatchOperation = "append"
	PatchReplace PatchOperation = "replace"
)

type ContextPatch struct {
	Operation          PatchOperation `json:"operation"`
	SystemInstructions []Message      `json:"system_instructions,omitempty"`
	Messages           []Message      `json:"messages,omitempty"`
}

type Event struct {
	Sequence  int64     `json:"sequence"`
	Type      string    `json:"type"`
	Time      time.Time `json:"time"`
	FromState State     `json:"from_state"`
	ToState   State     `json:"to_state"`
	Round     int       `json:"round"`
	Message   string    `json:"message,omitempty"`
	Error     string    `json:"error,omitempty"`
}

type Snapshot struct {
	RunID            string    `json:"run_id"`
	State            State     `json:"state"`
	Round            int       `json:"round"`
	MaxRounds        int       `json:"max_rounds"`
	InputTokens      int       `json:"input_tokens,omitempty"`
	OutputTokens     int       `json:"output_tokens,omitempty"`
	LastFinishReason string    `json:"last_finish_reason,omitempty"`
	LastError        string    `json:"last_error,omitempty"`
	Context          Context   `json:"context"`
	Events           []Event   `json:"events"`
	CreatedAt        time.Time `json:"created_at"`
	UpdatedAt        time.Time `json:"updated_at"`
}

type Request struct {
	RunID    string
	Round    int
	Messages []Message
	Tools    []Tool
}

type Response struct {
	Message      Message
	ToolCalls    []ToolCall
	FinishReason string
	Done         bool
	InputTokens  int
	OutputTokens int
}

type Client interface {
	Generate(ctx context.Context, request Request) (Response, error)
}

type Config struct {
	MaxRounds   int
	TokenBudget int
	LLMTimeout  time.Duration
	Tools       []Tool
	Executor    ToolExecutor
}

type ToolExecutor interface {
	Execute(context.Context, ToolCall) (any, error)
}

type ToolExecutorFunc func(context.Context, ToolCall) (any, error)

func (function ToolExecutorFunc) Execute(ctx context.Context, call ToolCall) (any, error) {
	return function(ctx, call)
}

func CloneSnapshot(source Snapshot) Snapshot {
	clone := source
	clone.Context.SystemInstructions = cloneMessages(source.Context.SystemInstructions)
	clone.Context.Messages = cloneMessages(source.Context.Messages)
	clone.Events = append([]Event(nil), source.Events...)
	return clone
}

func cloneMessages(source []Message) []Message {
	cloned := append([]Message(nil), source...)
	for index := range cloned {
		cloned[index].ToolCalls = append([]ToolCall(nil), source[index].ToolCalls...)
		for callIndex := range cloned[index].ToolCalls {
			arguments := source[index].ToolCalls[callIndex].Arguments
			cloned[index].ToolCalls[callIndex].Arguments = cloneArguments(arguments)
		}
	}
	return cloned
}

func cloneArguments(source map[string]any) map[string]any {
	if source == nil {
		return nil
	}
	cloned := make(map[string]any, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

func ValidateTransition(from, to State) error {
	if from == to {
		return nil
	}
	allowed := false
	switch from {
	case StateCreated:
		allowed = to == StateRunning || to == StateCancelled
	case StateRunning:
		allowed = to == StateWaitingLLM || to == StateBlocked || to == StateStopped || to == StateCancelled || to == StateCompleted || to == StateFailed
	case StateWaitingLLM:
		allowed = to == StateRunning || to == StateWaitingHuman || to == StateBlocked || to == StateStopped || to == StateCancelled || to == StateCompleted || to == StateFailed
	case StateWaitingHuman:
		allowed = to == StateRunning || to == StateCancelled
	case StateBlocked, StateStopped:
		allowed = to == StateRunning || to == StateCancelled
	}
	if !allowed {
		return fmt.Errorf("%w: %s -> %s", ErrInvalidTransition, from, to)
	}
	return nil
}

func IsTerminal(state State) bool {
	return state == StateCompleted || state == StateCancelled || state == StateFailed
}
