package run

import (
	"encoding/json"

	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/session"
)

func agentReconciliationOutcome(
	runState State,
	agentState agent.State,
	stopReason, effectOutcome string,
	sessionResult *session.RunResult,
	runtimeErr *contract.RuntimeError,
) ExecutionOutcome {
	resultJSON, err := json.Marshal(map[string]any{
		"outcome": agent.Outcome{
			State: agentState, StopReason: stopReason, Error: runtimeErr,
		},
		"reconciliation": map[string]any{
			"acknowledged":   true,
			"effect_outcome": effectOutcome,
		},
		"session_result": sessionResult,
	})
	if err != nil {
		return pendingAgentReconciliation(
			"encode Agent reconciliation result: " + err.Error(),
		)
	}
	return ExecutionOutcome{
		State: runState, Result: resultJSON, Error: runtimeErr,
	}
}

func pendingAgentReconciliation(message string) ExecutionOutcome {
	return ExecutionOutcome{
		State: StateNeedsReconciliation,
		Error: &contract.RuntimeError{
			Code: contract.ErrorConflict, Phase: contract.PhaseRun,
			Message: message,
		},
	}
}

func reconciledAgentError(
	original *contract.RuntimeError,
) *contract.RuntimeError {
	if original == nil {
		return &contract.RuntimeError{
			Code: contract.ErrorConflict, Phase: contract.PhaseRun,
			Message: "Agent tool effect outcome remains unknown and was explicitly reconciled",
		}
	}
	result := *original
	result.Retryable = false
	result.RetryAfterMS = 0
	result.Message = "Agent tool effect outcome remains unknown and was explicitly reconciled: " +
		original.Message
	return &result
}
