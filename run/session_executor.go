package run

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
	"github.com/yy003x/runtime/profile"
	"github.com/yy003x/runtime/session"
)

type SessionExecutor struct {
	Profiles *profile.Catalog
	Sessions *session.Service
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
	return nil
}

func (executor *SessionExecutor) Execute(
	ctx context.Context,
	record Record,
	_ contract.EventSink,
) ExecutionOutcome {
	result, runtimeErr := executor.Sessions.Run(ctx, session.RunRequest{
		SessionID:      record.Request.SessionID,
		RunID:          record.ID,
		TaskID:         record.Request.TaskID,
		ProfileID:      record.Request.ProfileID,
		Input:          record.Request.Input,
		CommandArgs:    append([]string(nil), record.Request.CommandArgs...),
		CWD:            record.Request.CWD,
		Retention:      session.Retention(record.Request.SessionRetention),
		ModelOptions:   record.Request.ModelOptions,
		TerminalDriver: record.Request.TerminalDriver,
	})
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
