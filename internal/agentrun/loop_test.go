package agentrun

import (
	"context"
	"testing"
)

type plannerFunc func(context.Context, []Message) (Action, error)

func (f plannerFunc) NextAction(ctx context.Context, messages []Message) (Action, error) {
	return f(ctx, messages)
}

type executorFunc func(context.Context, Action) ExecutionResult

func (f executorFunc) Execute(ctx context.Context, action Action) ExecutionResult {
	return f(ctx, action)
}

func TestLoopRespondsAfterOneToolStep(t *testing.T) {
	calls := 0
	engine := LoopEngine{
		Planner: plannerFunc(func(_ context.Context, _ []Message) (Action, error) {
			calls++
			if calls == 1 {
				return Action{Type: "tool", Name: "lookup"}, nil
			}
			return Action{Type: "respond", Content: "done"}, nil
		}),
		Executor: executorFunc(func(_ context.Context, _ Action) ExecutionResult {
			return ExecutionResult{Status: "ok", Output: "found"}
		}),
		MaxSteps: 3,
	}
	result := engine.Run(context.Background(), "question")
	if result.Outcome != OutcomeSucceeded || result.Output != "done" || result.Steps != 1 {
		t.Fatalf("result=%#v", result)
	}
}

func TestLoopBlocksAndEnforcesStepLimit(t *testing.T) {
	planner := plannerFunc(func(_ context.Context, _ []Message) (Action, error) {
		return Action{Type: "tool", Name: "work"}, nil
	})
	blocked := LoopEngine{Planner: planner, Executor: executorFunc(func(context.Context, Action) ExecutionResult {
		return ExecutionResult{Status: "blocked", Output: "denied"}
	})}.Run(context.Background(), "question")
	if blocked.Outcome != OutcomeBlocked {
		t.Fatalf("blocked=%#v", blocked)
	}
	limited := LoopEngine{Planner: planner, MaxSteps: 2, Executor: executorFunc(func(context.Context, Action) ExecutionResult {
		return ExecutionResult{Status: "ok", Output: "progress"}
	})}.Run(context.Background(), "question")
	if limited.Outcome != OutcomeFailed || limited.Steps != 2 {
		t.Fatalf("limited=%#v", limited)
	}
}
