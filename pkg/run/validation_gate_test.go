package run_test

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
)

// validationGateExecutor is an Executor that always claims completion, then
// lets the test control the CompletionValidator verdict to exercise the run
// service's validation gate without depending on real subprocess execution.
type validationGateExecutor struct {
	validation    runtime.ValidationResult
	validationErr error
}

func (*validationGateExecutor) Validate(runtime.Request) error { return nil }

func (*validationGateExecutor) Execute(
	context.Context,
	runtime.Record,
	contract.EventSink,
) runtime.ExecutionOutcome {
	return runtime.ExecutionOutcome{
		State:  runtime.StateCompleted,
		Result: json.RawMessage(`{"claim":"done"}`),
	}
}

func (executor *validationGateExecutor) ValidateCompletion(
	context.Context,
	runtime.Record,
	runtime.ExecutionOutcome,
) (runtime.ValidationResult, error) {
	return executor.validation, executor.validationErr
}

func submitAgentRun(t *testing.T, service *runtime.Service) runtime.Record {
	t.Helper()
	submitted, err := service.Submit(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "hello",
	})
	if err != nil {
		t.Fatalf("submit: %v", err)
	}
	return submitted
}

func TestCompletionValidationGateFailsOnUnmetCriteria(t *testing.T) {
	service := newRunService(t, &validationGateExecutor{
		validation: runtime.ValidationResult{Passed: false, Summary: "tests failed"},
	})
	submitted := submitAgentRun(t, service)

	completed, found, err := service.ExecuteNext(context.Background(), "worker-1")
	if !found {
		t.Fatalf("execute: found=%v err=%v", found, err)
	}
	// A validation failure surfaces as the run's terminal error.
	if err == nil {
		t.Fatalf("expected validation error from ExecuteNext, got nil")
	}
	if completed.ID != submitted.ID {
		t.Fatalf("completed run %q != submitted %q", completed.ID, submitted.ID)
	}
	if completed.State != runtime.StateFailed {
		t.Fatalf("unmet criteria must settle as failed, got %s", completed.State)
	}
	if completed.Error == nil ||
		completed.Error.Code != contract.ErrorValidationFailed {
		t.Fatalf("expected validation_failed error, got %+v", completed.Error)
	}
	if completed.Error.Message != "tests failed" {
		t.Fatalf("expected summary propagated, got %q", completed.Error.Message)
	}
}

func TestCompletionValidationGatePassesWhenCriteriaMet(t *testing.T) {
	service := newRunService(t, &validationGateExecutor{
		validation: runtime.ValidationResult{Passed: true},
	})
	submitted := submitAgentRun(t, service)

	completed, found, err := service.ExecuteNext(context.Background(), "worker-1")
	if err != nil || !found {
		t.Fatalf("execute: found=%v err=%v", found, err)
	}
	if completed.ID != submitted.ID {
		t.Fatalf("completed run %q != submitted %q", completed.ID, submitted.ID)
	}
	if completed.State != runtime.StateCompleted {
		t.Fatalf("met criteria must remain completed, got %s", completed.State)
	}
}

func TestCompletionValidationErrorEntersNeedsReconciliation(t *testing.T) {
	service := newRunService(t, &validationGateExecutor{
		validationErr: errors.New("validator infrastructure crashed"),
	})
	submitAgentRun(t, service)

	completed, found, err := service.ExecuteNext(context.Background(), "worker-1")
	if !found {
		t.Fatalf("execute: found=%v err=%v", found, err)
	}
	if err == nil {
		t.Fatalf("expected infra error from ExecuteNext, got nil")
	}
	if completed.State != runtime.StateNeedsReconciliation {
		t.Fatalf(
			"validator infra error must enter needs_reconciliation, got %s",
			completed.State,
		)
	}
}
