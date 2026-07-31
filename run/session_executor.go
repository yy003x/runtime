package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
	"github.com/yy003x/runtime/profile"
	"github.com/yy003x/runtime/session"
)

type SessionExecutor struct {
	Profiles *profile.Catalog
	Sessions *session.Service
}

func (executor *SessionExecutor) Prepare(
	_ context.Context,
	request Request,
) (Request, error) {
	if err := executor.Validate(request); err != nil {
		return Request{}, err
	}
	sessionRequest := session.RunRequest{
		SessionID: request.SessionID, TaskID: request.TaskID,
		ProfileID: request.ProfileID, Input: request.Input,
		Model: request.Model, Effort: request.Effort, CWD: request.CWD,
		InvocationBase: request.InvocationBase,
		Retention:      session.Retention(request.SessionRetention),
		ModelOptions:   request.ModelOptions,
	}
	if len(request.PrivateRequest) > 0 {
		var snapshot session.CLIExecutionSnapshot
		if err := decodePrivateSessionRequest(
			request.PrivateRequest, &snapshot,
		); err != nil {
			return Request{}, err
		}
		sessionRequest.Snapshot = &snapshot
	}
	prepared, runtimeErr := executor.Sessions.PrepareRunRequest(sessionRequest)
	if runtimeErr != nil {
		return Request{}, runtimeErr
	}
	request.CWD = prepared.CWD
	request.Model = prepared.Model
	request.Effort = prepared.Effort
	request.RequestDigest = prepared.SnapshotDigest()
	request.ConfigDigest = prepared.ConfigDigest()
	request.BasePromptDigest = prepared.BasePromptDigest()
	if prepared.Snapshot != nil {
		privateJSON, err := json.Marshal(prepared.Snapshot)
		if err != nil {
			return Request{}, fmt.Errorf("encode private Session request: %w", err)
		}
		if len(privateJSON) > MaxPrivateRequestBytes {
			return Request{}, &contract.RuntimeError{
				Code:  contract.ErrorContextOverflow,
				Phase: contract.PhaseRequest,
				Message: fmt.Sprintf(
					"private Session request exceeds %d bytes",
					MaxPrivateRequestBytes,
				),
			}
		}
		request.PrivateRequest = privateJSON
	}
	return request, nil
}

func (executor *SessionExecutor) Validate(request Request) error {
	if executor == nil || executor.Profiles == nil || executor.Sessions == nil {
		return fmt.Errorf("session executor is not configured")
	}
	if _, exists := executor.Profiles.Resolve(request.ProfileID); !exists {
		return fmt.Errorf("unknown profile %q", request.ProfileID)
	}
	if err := identity.Validate(request.SessionID, "session"); err != nil {
		return fmt.Errorf("session run requires session_id: %w", err)
	}
	if request.AgentBudget.MaxRounds != 0 ||
		request.AgentBudget.MaxToolCalls != 0 ||
		request.AgentBudget.MaxTotalTokens != 0 ||
		request.AgentBudget.MaxWallTime != 0 {
		return fmt.Errorf("agent_budget is invalid for session runs")
	}
	return nil
}

func (executor *SessionExecutor) Execute(
	ctx context.Context,
	record Record,
	_ contract.EventSink,
) ExecutionOutcome {
	sessionRequest := session.RunRequest{
		SessionID:    record.Request.SessionID,
		RunID:        record.ID,
		TaskID:       record.Request.TaskID,
		ProfileID:    record.Request.ProfileID,
		Input:        record.Request.Input,
		Model:        record.Request.Model,
		Effort:       record.Request.Effort,
		CWD:          record.Request.CWD,
		Retention:    session.Retention(record.Request.SessionRetention),
		ModelOptions: record.Request.ModelOptions,
	}
	if len(record.Request.PrivateRequest) > 0 {
		var snapshot session.CLIExecutionSnapshot
		if err := decodePrivateSessionRequest(
			record.Request.PrivateRequest, &snapshot,
		); err != nil {
			return ExecutionOutcome{
				State: StateFailed,
				Error: &contract.RuntimeError{
					Code: contract.ErrorInvalidRequest, Phase: contract.PhaseRun,
					Message: err.Error(),
				},
			}
		}
		sessionRequest.Snapshot = &snapshot
	}
	prepared, prepareErr := executor.Sessions.PrepareRunRequest(sessionRequest)
	if prepareErr != nil {
		return ExecutionOutcome{State: StateFailed, Error: prepareErr}
	}
	if prepared.SnapshotDigest() != record.Request.RequestDigest ||
		prepared.ConfigDigest() != record.Request.ConfigDigest ||
		prepared.BasePromptDigest() != record.Request.BasePromptDigest {
		return ExecutionOutcome{
			State: StateFailed,
			Error: &contract.RuntimeError{
				Code: contract.ErrorConflict, Phase: contract.PhaseRun,
				Message: "Session request or Profile changed after Run submission",
			},
		}
	}
	result, runtimeErr := executor.Sessions.Run(ctx, prepared)
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return ExecutionOutcome{
			State: StateFailed,
			Error: &contract.RuntimeError{
				Code: contract.ErrorInternal, Phase: contract.PhaseRun,
				Message: "encode Session result: " + err.Error(),
			},
		}
	}
	return sessionExecutionOutcome(result, resultJSON, runtimeErr)
}

func sessionExecutionOutcome(
	result session.RunResult,
	resultJSON json.RawMessage,
	runtimeErr *contract.RuntimeError,
) ExecutionOutcome {
	if result.State == session.TurnRunning {
		if result.Error != nil {
			runtimeErr = result.Error
		} else if runtimeErr == nil {
			runtimeErr = &contract.RuntimeError{
				Code: contract.ErrorInternal, Phase: contract.PhaseRun,
				Message: "Session execution remains open and requires reconciliation",
			}
		}
		return ExecutionOutcome{
			State:  StateNeedsReconciliation,
			Result: resultJSON,
			Error:  runtimeErr,
		}
	}
	if runtimeErr != nil {
		state := StateFailed
		if runtimeErr.Code == contract.ErrorCancelled {
			state = StateCancelled
		}
		return ExecutionOutcome{
			State: state, Result: resultJSON, Error: runtimeErr,
		}
	}
	return ExecutionOutcome{State: StateCompleted, Result: resultJSON}
}

func (executor *SessionExecutor) Reconcile(
	_ context.Context,
	record Record,
) ExecutionOutcome {
	result, found, err := executor.Sessions.ResultForRun(
		record.Request.SessionID, record.ID,
	)
	if err != nil {
		return ExecutionOutcome{
			State: StateNeedsReconciliation,
			Error: &contract.RuntimeError{
				Code: contract.ErrorConflict, Phase: contract.PhaseRun,
				Message: err.Error(),
			},
		}
	}
	if !found || result.State == session.TurnRunning {
		return ExecutionOutcome{
			State: StateNeedsReconciliation,
			Error: &contract.RuntimeError{
				Code: contract.ErrorConflict, Phase: contract.PhaseRun,
				Message: "Session Turn has not been explicitly reconciled",
			},
		}
	}
	execution, err := executor.Sessions.Execution(
		record.Request.SessionID, result.ExecutionID,
	)
	if err != nil {
		return ExecutionOutcome{
			State: StateNeedsReconciliation,
			Error: &contract.RuntimeError{
				Code: contract.ErrorConflict, Phase: contract.PhaseRun,
				Message: "Session execution evidence is unavailable",
			},
		}
	}
	if execution.State != session.ExecutionSettled ||
		execution.RunID != record.ID ||
		execution.RequestDigest != record.Request.RequestDigest ||
		execution.ConfigDigest != record.Request.ConfigDigest ||
		execution.BasePromptDigest != record.Request.BasePromptDigest {
		return ExecutionOutcome{
			State: StateNeedsReconciliation,
			Error: &contract.RuntimeError{
				Code: contract.ErrorConflict, Phase: contract.PhaseRun,
				Message: "Session execution evidence does not match the durable Run",
			},
		}
	}
	resultJSON, marshalErr := json.Marshal(result)
	if marshalErr != nil {
		return ExecutionOutcome{
			State: StateFailed,
			Error: &contract.RuntimeError{
				Code: contract.ErrorInternal, Phase: contract.PhaseRun,
				Message: marshalErr.Error(),
			},
		}
	}
	switch result.State {
	case session.TurnCompleted, session.TurnRequiresAction:
		return ExecutionOutcome{State: StateCompleted, Result: resultJSON}
	case session.TurnCancelled:
		return ExecutionOutcome{
			State: StateCancelled, Result: resultJSON, Error: result.Error,
		}
	default:
		return ExecutionOutcome{
			State: StateFailed, Result: resultJSON, Error: result.Error,
		}
	}
}

func decodePrivateSessionRequest(
	data []byte,
	value *session.CLIExecutionSnapshot,
) error {
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(value); err != nil {
		return fmt.Errorf("decode private Session request: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("decode private Session request: trailing JSON")
		}
		return fmt.Errorf("decode private Session request: %w", err)
	}
	return nil
}
