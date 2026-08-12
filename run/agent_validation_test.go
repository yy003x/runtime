package run

import (
	"context"
	"strings"
	"testing"

	"github.com/yy003x/runtime/contract"
)

func TestValidateCompletionEmptyCriteriaPasses(t *testing.T) {
	executor := &AgentExecutor{}
	result, err := executor.ValidateCompletion(
		context.Background(), Record{}, ExecutionOutcome{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Passed {
		t.Fatalf("empty criteria should pass trivially, got %+v", result)
	}
}

func TestValidateCompletionCommandSuccess(t *testing.T) {
	executor := &AgentExecutor{}
	record := Record{Request: Request{CompletionCriteria: CompletionCriteria{
		Checks: []CompletionCheck{
			{Name: "ok", Type: "command", Command: []string{"true"}},
		},
	}}}
	result, err := executor.ValidateCompletion(
		context.Background(), record, ExecutionOutcome{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if !result.Passed {
		t.Fatalf("exit-0 command should pass, got %+v", result)
	}
	if len(result.Checks) != 1 || !result.Checks[0].Passed {
		t.Fatalf("expected one passing check, got %+v", result.Checks)
	}
}

func TestValidateCompletionCommandFailure(t *testing.T) {
	executor := &AgentExecutor{}
	record := Record{Request: Request{CompletionCriteria: CompletionCriteria{
		Checks: []CompletionCheck{
			{Name: "fails", Type: "command", Command: []string{"false"}},
		},
	}}}
	result, err := executor.ValidateCompletion(
		context.Background(), record, ExecutionOutcome{},
	)
	if err != nil {
		t.Fatalf("unexpected error: %v", err)
	}
	if result.Passed {
		t.Fatalf("non-zero exit should fail validation, got pass")
	}
	if len(result.Checks) != 1 || result.Checks[0].Passed {
		t.Fatalf("expected one failed check, got %+v", result.Checks)
	}
	if result.Checks[0].Detail == "" {
		t.Fatalf("failed check should carry evidence detail")
	}
	if !strings.Contains(result.Summary, "fails") {
		t.Fatalf("summary should name the failed check, got %q", result.Summary)
	}
}

func TestValidateCompletionUnsupportedType(t *testing.T) {
	executor := &AgentExecutor{}
	record := Record{Request: Request{CompletionCriteria: CompletionCriteria{
		Checks: []CompletionCheck{{Name: "x", Type: "magic"}},
	}}}
	if _, err := executor.ValidateCompletion(
		context.Background(), record, ExecutionOutcome{},
	); err == nil {
		t.Fatalf("unsupported check type should return an error")
	}
}

func TestValidationRuntimeErrorSummarizes(t *testing.T) {
	err := ValidationRuntimeError(ValidationResult{
		Passed:  false,
		Summary: "tests failed",
	})
	if err.Code != contract.ErrorValidationFailed {
		t.Fatalf("expected validation_failed code, got %q", err.Code)
	}
	if err.Message != "tests failed" {
		t.Fatalf("expected summary as message, got %q", err.Message)
	}
}

func TestValidationRuntimeErrorDefaultSummary(t *testing.T) {
	err := ValidationRuntimeError(ValidationResult{Passed: false})
	if err.Message == "" {
		t.Fatalf("expected non-empty default summary")
	}
}
