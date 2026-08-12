package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
)

func (executor *AgentExecutor) ValidateResume(
	ctx context.Context,
	record Record,
	value json.RawMessage,
) (ResumeConstraint, error) {
	if record.Request.Kind != KindAgent {
		return ResumeConstraint{}, fmt.Errorf(
			"agent resume requires an agent Run",
		)
	}
	executionSnapshot, _, runtimeErr :=
		executor.loadAgentExecutionSnapshot(ctx, &record)
	if runtimeErr != nil {
		return ResumeConstraint{}, runtimeErr
	}
	if runtimeErr := executor.currentAgentExecutionGate(
		ctx, record.Request, executionSnapshot,
	); runtimeErr != nil {
		return ResumeConstraint{}, runtimeErr
	}
	input, err := decodeAgentResumeInput(value)
	if err != nil {
		return ResumeConstraint{}, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseRun,
			Message: "decode agent resume input: " + err.Error(),
		}
	}
	if input.PauseID == "" || len(input.Input) == 0 {
		return ResumeConstraint{}, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseRun,
			Message: "agent resume requires pause_id and input",
		}
	}
	var pause agent.Pause
	if err := strictjson.DecodeObjectWithNullPolicy(
		bytes.NewReader(record.Pause), MaxResumeInputBytes, &pause,
		func(path []string) bool {
			return len(path) > 1 && path[0] == "input_schema"
		},
	); err != nil {
		return ResumeConstraint{}, fmt.Errorf(
			"decode durable Agent pause: %w", err,
		)
	}
	if pause.ID == "" || pause.Kind == "" || pause.Prompt == "" ||
		pause.ToolCallID == "" || len(pause.InputSchema) == 0 {
		return ResumeConstraint{}, fmt.Errorf(
			"durable Agent pause is incomplete",
		)
	}
	if input.PauseID != pause.ID {
		return ResumeConstraint{}, fmt.Errorf(
			"%w: pause_id does not match active pause",
			ErrConflict,
		)
	}
	if err := agent.ValidateResumeInput(
		pause.InputSchema, input.Input,
	); err != nil {
		return ResumeConstraint{}, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseRun,
			Message: err.Error(),
		}
	}
	return ResumeConstraint{
		Pause:    append([]byte(nil), record.Pause...),
		NotAfter: cloneTimePointer(pause.ExpiresAt),
	}, nil
}

// FinalizeCancellation closes a bound Session projection without replaying a
// model or tool after the Run Store has durably reserved cancellation.
func prepareDurableAgentResume(
	record Record,
	state agent.LoopState,
) (agent.ResumeInput, bool, *contract.RuntimeError) {
	if len(record.Request.Resume) == 0 ||
		state.Pause == nil ||
		state.TerminalOutcome == nil ||
		state.TerminalOutcome.State != agent.StatePaused {
		return agent.ResumeInput{}, false, nil
	}
	input, err := decodeAgentResumeInput(record.Request.Resume)
	if err != nil {
		return agent.ResumeInput{}, false, agentRecoveryError(
			"decode durable agent resume input: " + err.Error(),
		)
	}
	if input.PauseID != state.Pause.ID {
		// The stored envelope belongs to an earlier pause in the same Turn.
		return agent.ResumeInput{}, false, nil
	}
	if record.ResumeAcceptedAt == nil ||
		record.ResumeAcceptedAt.IsZero() {
		return agent.ResumeInput{}, false, agentRecoveryError(
			"durable agent resume has no acceptance timestamp",
		)
	}
	acceptedAt := record.ResumeAcceptedAt.UTC()
	if state.Pause.ExpiresAt != nil &&
		acceptedAt.After(*state.Pause.ExpiresAt) {
		return agent.ResumeInput{}, false, agentRecoveryError(
			"durable agent resume was accepted after pause expiry",
		)
	}
	if err := agent.ValidateResumeInput(
		state.Pause.InputSchema, input.Input,
	); err != nil {
		return agent.ResumeInput{}, false, agentRecoveryError(
			"durable agent resume input is invalid: " + err.Error(),
		)
	}
	input.AcceptedAt = &acceptedAt
	return input, true, nil
}

func decodeAgentResumeInput(
	value json.RawMessage,
) (agent.ResumeInput, error) {
	var input agent.ResumeInput
	if err := strictjson.DecodeObjectWithNullPolicy(
		bytes.NewReader(value), MaxResumeInputBytes, &input,
		func(path []string) bool {
			return len(path) > 0 && path[0] == "input"
		},
	); err != nil {
		return agent.ResumeInput{}, err
	}
	return input, nil
}

// Reconcile explicitly acknowledges an Agent tool effect whose outcome cannot
// be proven. It never replays the Agent or mutates the durable tool-effect
// evidence. When a Session projection exists, that projection is closed first;
// retrying after either Store write is safe and converges on the same terminal
// result.
func recoveredPauseMessage(
	call contract.ToolCall,
	pause agent.Pause,
	resumes []ResumeRecord,
) (contract.Message, *contract.RuntimeError) {
	if pause.ToolCallID != call.ID {
		return contract.Message{}, agentRecoveryError(
			"prior Agent pause is not bound to its tool call",
		)
	}
	var matched *ResumeRecord
	var input agent.ResumeInput
	for index := range resumes {
		candidate, err := decodeAgentResumeInput(resumes[index].Input)
		if err != nil {
			return contract.Message{}, agentRecoveryError(
				"decode durable Agent resume journal: " + err.Error(),
			)
		}
		if candidate.PauseID != pause.ID {
			continue
		}
		if matched != nil {
			return contract.Message{}, agentRecoveryError(
				"prior Agent pause has duplicate resume journal entries",
			)
		}
		current := resumes[index]
		matched = &current
		input = candidate
	}
	if matched == nil {
		return contract.Message{}, agentRecoveryError(
			"prior Agent pause has no durable resume journal entry",
		)
	}
	if err := agent.ValidateResumeInput(pause.InputSchema, input.Input); err != nil {
		return contract.Message{}, agentRecoveryError(
			"prior Agent resume journal input is invalid: " + err.Error(),
		)
	}
	if matched.AcceptedAt.IsZero() ||
		pause.ExpiresAt != nil && matched.AcceptedAt.After(*pause.ExpiresAt) {
		return contract.Message{}, agentRecoveryError(
			"prior Agent resume journal acceptance time is invalid",
		)
	}
	return contract.Message{
		Role:       contract.RoleTool,
		ToolCallID: call.ID,
		Content:    string(input.Input),
	}, nil
}
