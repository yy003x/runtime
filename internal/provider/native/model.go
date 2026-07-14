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
	ErrInvalidTransition = errors.New("invalid native state transition")
	ErrUpstream          = errors.New("native llm upstream error")
)

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Pinned  bool   `json:"pinned,omitempty"`
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
	RunID     string    `json:"run_id"`
	State     State     `json:"state"`
	Round     int       `json:"round"`
	MaxRounds int       `json:"max_rounds"`
	LastError string    `json:"last_error,omitempty"`
	Context   Context   `json:"context"`
	Events    []Event   `json:"events"`
	CreatedAt time.Time `json:"created_at"`
	UpdatedAt time.Time `json:"updated_at"`
}

type Request struct {
	RunID    string
	Round    int
	Messages []Message
}

type Response struct {
	Message Message
	Done    bool
}

type Client interface {
	Generate(ctx context.Context, request Request) (Response, error)
}

type Config struct {
	MaxRounds   int
	TokenBudget int
	LLMTimeout  time.Duration
}

func CloneSnapshot(source Snapshot) Snapshot {
	clone := source
	clone.Context.SystemInstructions = append([]Message(nil), source.Context.SystemInstructions...)
	clone.Context.Messages = append([]Message(nil), source.Context.Messages...)
	clone.Events = append([]Event(nil), source.Events...)
	return clone
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
