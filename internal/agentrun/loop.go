package agentrun

import (
	"context"
	"fmt"
	"time"
)

const (
	PhasePlanning  = "planning"
	PhaseExecuting = "executing"
	PhaseObserving = "observing"
	PhaseTerminal  = "terminal"
)

type Action struct {
	Type       string         `json:"type"`
	Content    string         `json:"content,omitempty"`
	Name       string         `json:"name,omitempty"`
	Arguments  map[string]any `json:"args,omitempty"`
	Request    map[string]any `json:"request,omitempty"`
	Capability string         `json:"capability,omitempty"`
}

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type ExecutionResult struct {
	Status string `json:"status"`
	Output any    `json:"output"`
}

type Planner interface {
	NextAction(context.Context, []Message) (Action, error)
}

type Executor interface {
	Execute(context.Context, Action) ExecutionResult
}

type LoopTransition struct {
	Phase   string    `json:"phase"`
	Outcome string    `json:"outcome"`
	At      time.Time `json:"at"`
}

type LoopResult struct {
	LoopState   string           `json:"loop_state"`
	Outcome     string           `json:"outcome"`
	Output      any              `json:"output"`
	Steps       int              `json:"steps"`
	Transitions []LoopTransition `json:"transitions"`
}

type LoopEngine struct {
	Planner  Planner
	Executor Executor
	System   string
	MaxSteps int
	OnEvent  func(string, map[string]any)
}

func (e LoopEngine) Run(ctx context.Context, userInput string) LoopResult {
	maxSteps := e.MaxSteps
	if maxSteps <= 0 {
		maxSteps = 10
	}
	result := LoopResult{LoopState: PhasePlanning}
	observations := []Message{}
	record := func(phase, outcome string) {
		result.LoopState = phase
		result.Transitions = append(result.Transitions, LoopTransition{Phase: phase, Outcome: outcome, At: time.Now().UTC()})
	}
	record(PhasePlanning, "")
	for {
		if ctx.Err() != nil {
			result.Outcome = OutcomeCancelled
			record(PhaseTerminal, result.Outcome)
			return result
		}
		if result.Steps >= maxSteps {
			result.Outcome = OutcomeFailed
			record(PhaseTerminal, result.Outcome)
			return result
		}
		record(PhasePlanning, "")
		messages := make([]Message, 0, len(observations)+2)
		if e.System != "" {
			messages = append(messages, Message{Role: "system", Content: e.System})
		}
		messages = append(messages, Message{Role: "user", Content: userInput})
		messages = append(messages, observations...)
		action, err := e.Planner.NextAction(ctx, messages)
		if err != nil || !validAction(action) {
			e.emit("planner.invalid", map[string]any{"error": fmt.Sprint(err)})
			result.Outcome = OutcomeFailed
			record(PhaseTerminal, result.Outcome)
			return result
		}
		e.emit("planner.action", map[string]any{"type": action.Type})
		if action.Type == "respond" {
			result.Output = action.Content
			result.Outcome = OutcomeSucceeded
			record(PhaseTerminal, result.Outcome)
			return result
		}
		record(PhaseExecuting, "")
		execution := e.Executor.Execute(ctx, action)
		record(PhaseObserving, "")
		kind := "progress"
		if execution.Status != "ok" {
			kind = "blocked"
		}
		observations = append(observations, Message{Role: "observation", Content: fmt.Sprint(execution.Output)})
		e.emit("observe", map[string]any{"kind": kind})
		if kind == "blocked" {
			result.Outcome = OutcomeBlocked
			record(PhaseTerminal, result.Outcome)
			return result
		}
		result.Steps++
	}
}

func (e LoopEngine) emit(event string, data map[string]any) {
	if e.OnEvent != nil {
		e.OnEvent(event, data)
	}
}

func validAction(action Action) bool {
	if action.Type == "respond" {
		return action.Content != ""
	}
	if action.Type == "tool" {
		return action.Name != ""
	}
	if action.Type == "run_agent" {
		return action.Request != nil
	}
	return false
}
