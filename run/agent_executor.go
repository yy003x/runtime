package run

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"sync"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/profile"
	"github.com/yy003x/runtime/session"
)

type AgentExecutor struct {
	Profiles *profile.Catalog
	Model    model.Generator
	Tools    agent.ToolExecutor
	Store    Store
	Sessions *session.Service
}

func (executor *AgentExecutor) Validate(request Request) error {
	if executor == nil || executor.Profiles == nil || executor.Model == nil ||
		executor.Tools == nil || executor.Store == nil {
		return fmt.Errorf("agent executor is not configured")
	}
	entry, exists := executor.Profiles.Resolve(request.ProfileID)
	if !exists {
		return fmt.Errorf("unknown profile %q", request.ProfileID)
	}
	if entry.Kind != profile.KindModel {
		return fmt.Errorf(
			"agent requires an API model profile; %q is a command profile",
			request.ProfileID,
		)
	}
	if request.Model != "" || request.Effort != "" ||
		len(request.PrivateRequest) != 0 ||
		request.ModelOptions.MaxOutputTokens != nil ||
		request.ModelOptions.Temperature != nil {
		return fmt.Errorf(
			"model, effort, model_options, and private CLI request are invalid for agent runs",
		)
	}
	if request.SessionID != "" && executor.Sessions == nil {
		return fmt.Errorf("agent Session service is unavailable")
	}
	return nil
}

func (executor *AgentExecutor) Execute(
	ctx context.Context,
	record Record,
	sink contract.EventSink,
) ExecutionOutcome {
	var sessionTurn *session.AgentTurn
	var sessionResult *session.RunResult
	var initialMessages []contract.Message
	if record.Request.SessionID != "" {
		current, runtimeErr := executor.Sessions.PrepareAgent(session.RunRequest{
			SessionID: record.Request.SessionID, RunID: record.ID,
			TaskID: record.Request.TaskID, ProfileID: record.Request.ProfileID,
			Input: record.Request.Input,
		}, len(record.Request.Resume) > 0)
		if runtimeErr != nil {
			return ExecutionOutcome{State: StateFailed, Error: runtimeErr}
		}
		if current.ExistingResult != nil {
			resultJSON, _ := json.Marshal(current.ExistingResult)
			if current.ExistingResult.State == session.TurnFailed ||
				current.ExistingResult.State == session.TurnCancelled {
				return ExecutionOutcome{
					State: StateFailed, Result: resultJSON,
					Error: current.ExistingResult.Error,
				}
			}
			return ExecutionOutcome{State: StateCompleted, Result: resultJSON}
		}
		sessionTurn = &current
		initialMessages = current.Messages
	}
	state, runtimeErr := executor.loadState(ctx, record, initialMessages)
	if runtimeErr != nil {
		return ExecutionOutcome{State: StateFailed, Error: runtimeErr}
	}
	modelRecorder := &recordingModel{
		runID: record.ID, model: executor.Model, store: executor.Store,
	}
	effects := &durableEffects{store: executor.Store}
	kernel := agent.Kernel{
		Model: modelRecorder, Tools: executor.Tools, Effects: effects,
		Budget: record.Request.AgentBudget,
	}
	var outcome agent.Outcome
	if len(record.Request.Resume) > 0 {
		var input agent.ResumeInput
		if err := json.Unmarshal(record.Request.Resume, &input); err != nil {
			runtimeErr := &contract.RuntimeError{
				Code: contract.ErrorInvalidRequest, Phase: contract.PhaseRun,
				Message: "decode agent resume input: " + err.Error(),
			}
			return ExecutionOutcome{State: StateFailed, Error: runtimeErr}
		}
		state, outcome, runtimeErr = kernel.Resume(ctx, state, input, sink)
	} else {
		state, outcome, runtimeErr = kernel.Run(ctx, state, sink)
	}
	if saveErr := executor.saveState(
		context.WithoutCancel(ctx), state,
	); saveErr != nil {
		runtimeErr = &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseRun,
			Message: "save agent checkpoint: " + saveErr.Error(),
		}
		outcome.State = agent.StateNeedsReconciliation
		outcome.Error = runtimeErr
	}
	if sessionTurn != nil {
		currentSessionResult, sessionErr := executor.Sessions.SettleAgent(
			*sessionTurn, state.Messages, outcome,
		)
		sessionResult = &currentSessionResult
		if sessionErr != nil && outcome.State != agent.StateFailed &&
			outcome.State != agent.StateCancelled {
			runtimeErr = &contract.RuntimeError{
				Code: contract.ErrorConflict, Phase: contract.PhaseRun,
				Message: "persist agent Session projection: " + sessionErr.Message,
			}
			outcome.State = agent.StateNeedsReconciliation
			outcome.Error = runtimeErr
		}
	}
	resultJSON, err := json.Marshal(map[string]any{
		"outcome":        outcome,
		"session_result": sessionResult,
		"state": map[string]any{
			"round": state.Round, "tool_call_count": state.ToolCallCount,
			"total_tokens": state.TotalTokens,
		},
	})
	if err != nil {
		runtimeErr = &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseRun,
			Message: "encode agent outcome: " + err.Error(),
		}
		return ExecutionOutcome{State: StateFailed, Error: runtimeErr}
	}
	switch outcome.State {
	case agent.StateCompleted:
		return ExecutionOutcome{State: StateCompleted, Result: resultJSON}
	case agent.StatePaused:
		pauseJSON, _ := json.Marshal(outcome.Pause)
		return ExecutionOutcome{
			State: StatePaused, Result: resultJSON, Pause: pauseJSON,
		}
	case agent.StateNeedsReconciliation:
		return ExecutionOutcome{
			State: StateNeedsReconciliation, Result: resultJSON,
			Error: outcome.Error,
		}
	case agent.StateCancelled:
		return ExecutionOutcome{
			State: StateCancelled, Result: resultJSON, Error: outcome.Error,
		}
	default:
		if runtimeErr == nil {
			runtimeErr = outcome.Error
		}
		if runtimeErr == nil {
			runtimeErr = &contract.RuntimeError{
				Code: contract.ErrorInternal, Phase: contract.PhaseRun,
				Message: "agent failed without a typed error",
			}
		}
		return ExecutionOutcome{
			State: StateFailed, Result: resultJSON, Error: runtimeErr,
		}
	}
}

func (executor *AgentExecutor) loadState(
	ctx context.Context,
	record Record,
	initialMessages []contract.Message,
) (agent.LoopState, *contract.RuntimeError) {
	checkpoint, exists, err := executor.Store.LatestCheckpoint(ctx, record.ID)
	if err != nil {
		return agent.LoopState{}, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseRun,
			Message: "load agent checkpoint: " + err.Error(),
		}
	}
	if exists {
		var state agent.LoopState
		if err := json.Unmarshal(checkpoint.State, &state); err != nil {
			return agent.LoopState{}, &contract.RuntimeError{
				Code: contract.ErrorInternal, Phase: contract.PhaseRun,
				Message: "decode agent checkpoint: " + err.Error(),
			}
		}
		return state, nil
	}
	messages := initialMessages
	if len(messages) == 0 {
		messages = []contract.Message{{
			Role: contract.RoleUser, Content: record.Request.Input,
		}}
	}
	return agent.LoopState{
		RunID: record.ID, ModelProfile: record.Request.ProfileID,
		Messages: messages,
	}, nil
}

func (executor *AgentExecutor) saveState(
	ctx context.Context,
	state agent.LoopState,
) error {
	checkpointID, err := identity.New("checkpoint")
	if err != nil {
		return err
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return err
	}
	return executor.Store.SaveCheckpoint(ctx, Checkpoint{
		ID: checkpointID, RunID: state.RunID,
		Sequence: state.NextEventSequence, State: stateJSON,
	})
}

type durableEffects struct {
	store Store
}

func (effects *durableEffects) Prepared(
	ctx context.Context,
	request agent.ToolRequest,
	state agent.LoopState,
) (string, error) {
	checkpointID, err := identity.New("checkpoint")
	if err != nil {
		return "", err
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return "", err
	}
	if err := effects.store.SaveCheckpoint(ctx, Checkpoint{
		ID: checkpointID, RunID: request.RunID,
		Sequence: state.NextEventSequence, State: stateJSON,
	}); err != nil {
		return "", err
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return "", err
	}
	if err := effects.store.PrepareToolEffect(ctx, ToolEffect{
		RunID: request.RunID, CallID: request.CallID,
		IdempotencyKey: request.IdempotencyKey, Name: request.Name,
		State: "prepared", Request: requestJSON,
	}); err != nil {
		return "", err
	}
	return checkpointID, nil
}

func (effects *durableEffects) Started(
	ctx context.Context,
	request agent.ToolRequest,
) error {
	return effects.store.StartToolEffect(ctx, request.RunID, request.CallID)
}

func (effects *durableEffects) Completed(
	ctx context.Context,
	request agent.ToolRequest,
	result agent.ToolResult,
) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return effects.store.CompleteToolEffect(ctx, ToolEffect{
		RunID: request.RunID, CallID: request.CallID,
		IdempotencyKey: request.IdempotencyKey, Name: request.Name,
		State: "completed", Result: resultJSON,
	})
}

func (effects *durableEffects) Failed(
	ctx context.Context,
	request agent.ToolRequest,
	runtimeErr *contract.RuntimeError,
) error {
	return effects.store.FailToolEffect(ctx, ToolEffect{
		RunID: request.RunID, CallID: request.CallID,
		IdempotencyKey: request.IdempotencyKey, Name: request.Name,
		State: "failed", Error: runtimeErr,
	})
}

type recordingModel struct {
	runID string
	model model.Generator
	store Store

	mu       sync.Mutex
	sequence int
}

func (recorder *recordingModel) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return recorder.GenerateStream(ctx, request, nil)
}

func (recorder *recordingModel) GenerateStream(
	ctx context.Context,
	request contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	recorder.mu.Lock()
	recorder.sequence++
	sequence := recorder.sequence
	recorder.mu.Unlock()
	callID, err := identity.New("model_call")
	if err != nil {
		return contract.ModelResult{}, runError(contract.ErrorInternal, err.Error())
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return contract.ModelResult{}, runError(contract.ErrorInternal, err.Error())
	}
	sum := sha256.Sum256(requestJSON)
	call := ModelCall{
		ID: callID, RunID: recorder.runID, Sequence: sequence,
		RequestDigest: "sha256:" + hex.EncodeToString(sum[:]),
	}
	if err := recorder.store.StartModelCall(ctx, call); err != nil {
		return contract.ModelResult{}, runError(contract.ErrorInternal, err.Error())
	}
	result, runtimeErr := recorder.model.GenerateStream(ctx, request, sink)
	call.State = "completed"
	if runtimeErr != nil {
		call.State = "failed"
		if runtimeErr.Code == contract.ErrorCancelled {
			call.State = "cancelled"
		}
	} else {
		call.ProviderRequestID = result.Provider.RequestID
	}
	if err := recorder.store.FinishModelCall(
		context.WithoutCancel(ctx), call,
	); err != nil {
		return contract.ModelResult{}, runError(
			contract.ErrorInternal, "record model call: "+err.Error(),
		)
	}
	return result, runtimeErr
}
