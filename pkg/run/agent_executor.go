package run

import (
	"context"
	"encoding/json"
	"fmt"
	"reflect"
	"time"

	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
	"github.com/yy003x/runtime/pkg/profile"
	"github.com/yy003x/runtime/pkg/session"
)

type AgentExecutor struct {
	Profiles *profile.Catalog
	Model    model.Generator
	Tools    agent.ToolExecutor
	Store    AgentStore
	Sessions *session.Service
	Now      func() time.Time
}

func (executor *AgentExecutor) Validate(request Request) error {
	if err := executor.validateAgentRequestShape(request, true); err != nil {
		return err
	}
	if len(request.PrivateRequest) == 0 {
		return fmt.Errorf("private Agent execution request is required")
	}
	_, _, err := decodeAgentExecutionSnapshot(request)
	return err
}

// ValidateResume checks the public resume envelope against the immutable pause
// snapshot without touching the Run Store or a bound Session.
func (executor *AgentExecutor) FinalizeCancellation(
	ctx context.Context,
	record Record,
) ExecutionOutcome {
	cancelled := &contract.RuntimeError{
		Code: contract.ErrorCancelled, Phase: contract.PhaseRun,
		Message: "run was cancelled",
	}
	cancelledOutcome := func() ExecutionOutcome {
		return ExecutionOutcome{
			State: StateCancelled, Error: cancelled,
		}
	}
	needsReconciliation := func(message string) ExecutionOutcome {
		return ExecutionOutcome{
			State: StateNeedsReconciliation,
			Error: agentRecoveryError(message),
		}
	}
	if !record.CancelRequested {
		return needsReconciliation(
			"Agent cancellation is not durably reserved",
		)
	}
	executionSnapshot, frozenValidator, snapshotErr :=
		executor.loadAgentExecutionSnapshot(ctx, &record)
	if snapshotErr != nil {
		return needsReconciliation(snapshotErr.Message)
	}
	boundTools := &snapshotBoundTools{
		validator: frozenValidator, executor: executor.Tools,
	}
	var turn session.AgentTurn
	found := false
	if record.Request.SessionID != "" {
		request := agentSessionRunRequest(record.Request, record.ID)
		var sessionErr *contract.RuntimeError
		turn, found, sessionErr = executor.Sessions.LookupAgent(request)
		if sessionErr != nil {
			return needsReconciliation(
				"lookup Agent Session cancellation projection: " +
					sessionErr.Message,
			)
		}
		if found {
			if runtimeErr := validateAgentSessionSnapshot(
				record, turn, executionSnapshot,
			); runtimeErr != nil {
				return needsReconciliation(runtimeErr.Message)
			}
		}
	}
	modelCalls, err := executor.Store.ModelCalls(ctx, record.ID)
	if err != nil {
		return needsReconciliation(
			"load Agent model evidence for cancellation: " + err.Error(),
		)
	}
	effects, err := executor.Store.ToolEffects(ctx, record.ID)
	if err != nil {
		return needsReconciliation(
			"load Agent tool evidence for cancellation: " + err.Error(),
		)
	}
	for _, effect := range effects {
		if effect.State == "started" {
			return needsReconciliation(
				"Agent tool effect outcome is unknown during cancellation",
			)
		}
	}
	checkpoint, checkpointExists, err := executor.Store.LatestCheckpoint(
		ctx, record.ID,
	)
	if err != nil {
		return needsReconciliation(
			"load Agent checkpoint for cancellation: " + err.Error(),
		)
	}
	hasEvidence := checkpointExists ||
		len(modelCalls) != 0 || len(effects) != 0
	var state agent.LoopState
	stateLoaded := false
	if checkpointExists || len(modelCalls) != 0 {
		initialMessages := []contract.Message(nil)
		initialBaseMessageCount := 1
		if found {
			initialMessages = turn.Messages
			initialBaseMessageCount = turn.BaseMessageCount
		}
		var runtimeErr *contract.RuntimeError
		state, runtimeErr = executor.loadState(
			ctx, record, initialMessages, initialBaseMessageCount,
			boundTools.Definitions(),
		)
		if runtimeErr != nil {
			return needsReconciliation(runtimeErr.Message)
		}
		stateLoaded = true
	}
	var terminalOutcome *agent.Outcome
	if stateLoaded && state.TerminalOutcome != nil {
		switch state.TerminalOutcome.State {
		case agent.StateCompleted, agent.StateFailed, agent.StateCancelled:
			current := *state.TerminalOutcome
			terminalOutcome = &current
		}
	}
	if terminalOutcome == nil && stateLoaded &&
		state.TerminalOutcome == nil &&
		state.PendingToolCursor < len(state.PendingToolCalls) &&
		state.RecoveredFromCheckpoint {
		cancelledContext, cancel := context.WithCancel(
			context.Background(),
		)
		cancel()
		currentModel := executor.Model
		if currentModel == nil {
			currentModel = unavailableAgentModel{}
		}
		kernel := agent.Kernel{
			Model: currentModel, Tools: boundTools,
			Effects: &durableEffects{store: executor.Store},
			Budget:  record.Request.AgentBudget, Now: executor.Now,
		}
		journalSink := func(event contract.Event) error {
			_, err := executor.Store.AppendEvent(
				context.WithoutCancel(ctx), record.ID, event,
			)
			return err
		}
		var outcome agent.Outcome
		var runtimeErr *contract.RuntimeError
		state, outcome, runtimeErr = kernel.Run(
			cancelledContext, state, journalSink,
		)
		switch outcome.State {
		case agent.StateFailed, agent.StateCancelled:
			current := outcome
			terminalOutcome = &current
		case agent.StateNeedsReconciliation:
			message := "Agent pending effect cannot be cancelled safely"
			if runtimeErr != nil {
				message = runtimeErr.Message
			}
			return needsReconciliation(message)
		default:
			return needsReconciliation(
				"Agent pending effect cancellation produced an unsupported outcome",
			)
		}
	}
	if found && turn.ExistingResult != nil {
		if terminalOutcome != nil {
			if runtimeErr := executor.validateExistingAgentSessionResult(
				record, turn, state,
			); runtimeErr != nil {
				return needsReconciliation(runtimeErr.Message)
			}
		} else {
			existing, err :=
				executor.validateExistingAgentSessionCancellation(
					record, turn,
				)
			if err != nil {
				return needsReconciliation(err.Error())
			}
			terminalOutcome = &existing
			if !stateLoaded {
				state = agent.LoopState{
					SchemaVersion: agent.LoopStateSchemaVersion,
					RunID:         record.ID, ModelProfile: record.Request.ProfileID,
					Messages:         turn.Messages,
					BaseMessageCount: turn.BaseMessageCount,
				}
				stateLoaded = true
			}
		}
	}
	if terminalOutcome != nil {
		if record.Request.SessionID != "" && !found {
			return needsReconciliation(
				"durable Agent terminal evidence has no matching Session Turn",
			)
		}
		checkpointTerminal :=
			state.TerminalOutcome != nil &&
				(state.TerminalOutcome.State == agent.StateCompleted ||
					state.TerminalOutcome.State == agent.StateFailed ||
					state.TerminalOutcome.State == agent.StateCancelled) &&
				reflect.DeepEqual(state.TerminalOutcome, terminalOutcome)
		if checkpointTerminal &&
			!checkpointMatchesLoopState(
				checkpoint, checkpointExists, state,
			) {
			if err := executor.saveState(
				context.WithoutCancel(ctx), state,
			); err != nil {
				return needsReconciliation(
					"persist recovered Agent terminal checkpoint: " +
						err.Error(),
				)
			}
		}
		var sessionResult *session.RunResult
		if found && turn.ExistingResult == nil {
			if runtimeErr := validateAgentSessionState(
				record, turn, state.Messages,
			); runtimeErr != nil {
				return needsReconciliation(runtimeErr.Message)
			}
			if runtimeErr := validateAgentSessionPrefixProjection(
				turn, state,
			); runtimeErr != nil {
				return needsReconciliation(runtimeErr.Message)
			}
			current, sessionErr := executor.Sessions.SettleAgent(
				turn, state.Messages, *terminalOutcome,
			)
			if sessionErr != nil {
				return needsReconciliation(
					"settle Agent Session terminal outcome: " +
						sessionErr.Message,
				)
			}
			sessionResult = &current
		} else if found {
			sessionResult = turn.ExistingResult
		}
		resultJSON, err := encodeAgentExecutionResult(
			state, *terminalOutcome, sessionResult,
		)
		if err != nil {
			return needsReconciliation(
				"encode recovered Agent terminal outcome: " + err.Error(),
			)
		}
		switch terminalOutcome.State {
		case agent.StateCompleted:
			return ExecutionOutcome{
				State: StateCompleted, Result: resultJSON,
			}
		case agent.StateFailed:
			return ExecutionOutcome{
				State: StateFailed, Result: resultJSON,
				Error: terminalOutcome.Error,
			}
		case agent.StateCancelled:
			return ExecutionOutcome{
				State: StateCancelled, Result: resultJSON,
				Error: terminalOutcome.Error,
			}
		}
	}
	if !found {
		if len(effects) != 0 && !stateLoaded {
			return needsReconciliation(
				"Agent tool evidence has no cancellation checkpoint",
			)
		}
		if record.Request.SessionID != "" && hasEvidence {
			return needsReconciliation(
				"durable Agent cancellation evidence has no matching Session Turn",
			)
		}
		if !stateLoaded {
			terminal := agent.Outcome{
				State: agent.StateCancelled, StopReason: "cancelled",
				Error: cancelled,
			}
			state = agent.LoopState{
				SchemaVersion: agent.LoopStateSchemaVersion,
				RunID:         record.ID,
				ModelProfile:  record.Request.ProfileID,
				Messages: []contract.Message{{
					Role: contract.RoleUser, Content: record.Request.Input,
				}},
				BaseMessageCount: 1,
				TerminalOutcome:  &terminal,
			}
			if err := executor.saveState(
				context.WithoutCancel(ctx), state,
			); err != nil {
				return needsReconciliation(
					"persist Agent cancellation checkpoint: " +
						err.Error(),
				)
			}
			resultJSON, err := encodeAgentExecutionResult(
				state, terminal, nil,
			)
			if err != nil {
				return needsReconciliation(
					"encode Agent cancellation checkpoint: " +
						err.Error(),
				)
			}
			return ExecutionOutcome{
				State: StateCancelled, Result: resultJSON,
				Error: cancelled,
			}
		}
		return cancelledOutcome()
	}
	messages := turn.Messages
	if stateLoaded {
		if runtimeErr := validateAgentSessionState(
			record, turn, state.Messages,
		); runtimeErr != nil {
			return needsReconciliation(runtimeErr.Message)
		}
		if runtimeErr := validateAgentSessionPrefixProjection(
			turn, state,
		); runtimeErr != nil {
			return needsReconciliation(runtimeErr.Message)
		}
		if turn.ProjectedPause != nil {
			if runtimeErr := validateProjectedAgentSessionPause(
				turn, state,
			); runtimeErr != nil {
				return needsReconciliation(runtimeErr.Message)
			}
		}
		messages = state.Messages
	} else if len(effects) != 0 {
		return needsReconciliation(
			"Agent tool evidence has no cancellation checkpoint",
		)
	}
	result, sessionErr := executor.Sessions.SettleAgent(
		turn, messages,
		agent.Outcome{
			State: agent.StateCancelled, StopReason: "cancelled",
			Error: cancelled,
		},
	)
	if sessionErr != nil {
		return needsReconciliation(
			"settle Agent Session cancellation: " + sessionErr.Message,
		)
	}
	if result.State != session.TurnCancelled ||
		result.Error == nil ||
		(result.Error.Code != contract.ErrorCancelled &&
			result.Error.Code != contract.ErrorTimeout) {
		return needsReconciliation(
			"Agent Session cancellation did not settle as cancelled",
		)
	}
	cancelledResult := agent.Outcome{
		State: agent.StateCancelled, StopReason: "cancelled",
		Error: result.Error,
	}
	if !stateLoaded {
		state = agent.LoopState{
			SchemaVersion: agent.LoopStateSchemaVersion,
			RunID:         record.ID, ModelProfile: record.Request.ProfileID,
			Messages: messages, BaseMessageCount: turn.BaseMessageCount,
		}
	}
	resultJSON, err := encodeAgentExecutionResult(
		state, cancelledResult, &result,
	)
	if err != nil {
		return needsReconciliation(
			"encode Agent Session cancellation: " + err.Error(),
		)
	}
	return ExecutionOutcome{
		State: StateCancelled, Result: resultJSON, Error: result.Error,
	}
}

func (executor *AgentExecutor) Execute(
	ctx context.Context,
	record Record,
	sink contract.EventSink,
) ExecutionOutcome {
	executionSnapshot, frozenValidator, snapshotErr :=
		executor.loadAgentExecutionSnapshot(ctx, &record)
	if snapshotErr != nil {
		return ExecutionOutcome{
			State: StateNeedsReconciliation, Error: snapshotErr,
		}
	}
	boundTools := &snapshotBoundTools{
		validator: frozenValidator, executor: executor.Tools,
	}
	hasRecoveryEvidence, evidenceErr := executor.hasRecoveryEvidence(
		ctx, record.ID,
	)
	if evidenceErr != nil {
		return ExecutionOutcome{
			State: StateNeedsReconciliation, Error: evidenceErr,
		}
	}
	var recoveredSessionTurn *session.AgentTurn
	if record.Request.SessionID != "" {
		if executor.Sessions == nil {
			return pendingAgentReconciliation(
				"agent Session service is unavailable",
			)
		}
		current, found, lookupErr := executor.Sessions.LookupAgent(
			agentSessionRunRequest(record.Request, record.ID),
		)
		if lookupErr != nil {
			return ExecutionOutcome{
				State: StateNeedsReconciliation, Error: lookupErr,
			}
		}
		if found {
			if runtimeErr := validateAgentSessionSnapshot(
				record, current, executionSnapshot,
			); runtimeErr != nil {
				return ExecutionOutcome{
					State: StateNeedsReconciliation, Error: runtimeErr,
				}
			}
			hasRecoveryEvidence = true
			recoveredSessionTurn = &current
		}
	}
	if !hasRecoveryEvidence {
		if runtimeErr := executor.currentAgentExecutionGate(
			ctx, record.Request, executionSnapshot,
		); runtimeErr != nil {
			return executionSnapshotChangedOutcome(runtimeErr)
		}
	}
	var sessionTurn *session.AgentTurn
	var sessionResult *session.RunResult
	var initialMessages []contract.Message
	initialBaseMessageCount := 1
	if record.Request.SessionID != "" {
		sessionRequest := agentSessionRunRequest(record.Request, record.ID)
		var current session.AgentTurn
		var runtimeErr *contract.RuntimeError
		if hasRecoveryEvidence {
			if recoveredSessionTurn != nil {
				current = *recoveredSessionTurn
			} else {
				current, runtimeErr = executor.Sessions.RecoverAgent(
					sessionRequest,
				)
			}
		} else {
			prepared, prepareErr :=
				executor.Sessions.PrepareRunRequest(sessionRequest)
			if prepareErr != nil {
				return ExecutionOutcome{
					State: StateFailed, Error: prepareErr,
				}
			}
			if prepared.SnapshotDigest() !=
				executionSnapshot.SessionRequestDigest ||
				prepared.ConfigDigest() !=
					executionSnapshot.SessionConfigDigest {
				return executionSnapshotChangedOutcome(
					&contract.RuntimeError{
						Code:    contract.ErrorConflict,
						Phase:   contract.PhaseProfile,
						Message: "Agent execution snapshot changed: Session execution",
					},
				)
			}
			current, runtimeErr =
				executor.Sessions.PrepareAgent(prepared)
		}
		if runtimeErr != nil {
			state := StateFailed
			if hasRecoveryEvidence {
				state = StateNeedsReconciliation
			}
			return ExecutionOutcome{State: state, Error: runtimeErr}
		}
		if current.ExistingResult != nil && !hasRecoveryEvidence {
			return pendingAgentReconciliation(
				"terminal Agent Session has no matching durable Run evidence",
			)
		}
		if runtimeErr := validateAgentSessionSnapshot(
			record, current, executionSnapshot,
		); runtimeErr != nil {
			return ExecutionOutcome{
				State: StateNeedsReconciliation, Error: runtimeErr,
			}
		}
		sessionTurn = &current
		initialMessages = current.Messages
		initialBaseMessageCount = current.BaseMessageCount
	}
	state, runtimeErr := executor.loadState(
		ctx, record, initialMessages, initialBaseMessageCount,
		boundTools.Definitions(),
	)
	if runtimeErr != nil {
		outcomeState := StateFailed
		if runtimeErr.Code == contract.ErrorConflict {
			outcomeState = StateNeedsReconciliation
		}
		return ExecutionOutcome{State: outcomeState, Error: runtimeErr}
	}
	resumeInput, resumeCurrentPause, resumeErr :=
		prepareDurableAgentResume(record, state)
	if resumeErr != nil {
		return ExecutionOutcome{
			State: StateNeedsReconciliation, Error: resumeErr,
		}
	}
	if sessionTurn != nil {
		if runtimeErr := validateAgentSessionState(
			record, *sessionTurn, state.Messages,
		); runtimeErr != nil {
			return ExecutionOutcome{
				State: StateNeedsReconciliation, Error: runtimeErr,
			}
		}
		if runtimeErr := validateAgentSessionPrefixProjection(
			*sessionTurn, state,
		); runtimeErr != nil {
			return ExecutionOutcome{
				State: StateNeedsReconciliation, Error: runtimeErr,
			}
		}
		if resumeCurrentPause {
			if runtimeErr := validateAgentSessionSafeProjection(
				*sessionTurn, state,
			); runtimeErr != nil {
				return ExecutionOutcome{
					State: StateNeedsReconciliation, Error: runtimeErr,
				}
			}
		}
		if sessionTurn.ProjectedPause != nil {
			if runtimeErr := validateProjectedAgentSessionPause(
				*sessionTurn, state,
			); runtimeErr != nil {
				return ExecutionOutcome{
					State: StateNeedsReconciliation, Error: runtimeErr,
				}
			}
		}
	}
	if resumeCurrentPause {
		if runtimeErr := executor.currentAgentExecutionGate(
			ctx, record.Request, executionSnapshot,
		); runtimeErr != nil {
			if sessionTurn != nil {
				if sessionStateErr := validateAgentSessionState(
					record, *sessionTurn, state.Messages,
				); sessionStateErr != nil {
					return ExecutionOutcome{
						State: StateNeedsReconciliation,
						Error: sessionStateErr,
					}
				}
				if state.Pause == nil {
					return pendingAgentReconciliation(
						"Agent execution snapshot changed but the Session pause cannot be restored",
					)
				}
				if restoreErr := executor.Sessions.RestoreAgentPause(
					*sessionTurn, *state.Pause,
				); restoreErr != nil {
					return ExecutionOutcome{
						State: StateNeedsReconciliation,
						Error: restoreErr,
					}
				}
			}
			return preservePausedAgentSnapshot(state)
		}
	}
	if sessionTurn != nil {
		if sessionTurn.ProjectedPause != nil {
			if resumeCurrentPause {
				if runtimeErr := executor.Sessions.ActivateAgentResume(
					*sessionTurn,
				); runtimeErr != nil {
					return ExecutionOutcome{
						State: StateNeedsReconciliation,
						Error: runtimeErr,
					}
				}
				sessionTurn.ProjectedPause = nil
			} else {
				pauseJSON, err := json.Marshal(state.Pause)
				if err != nil {
					return ExecutionOutcome{
						State: StateNeedsReconciliation,
						Error: agentRecoveryError(
							"encode recovered Agent pause: " + err.Error(),
						),
					}
				}
				return ExecutionOutcome{
					State: StatePaused, Pause: pauseJSON,
				}
			}
		}
		if sessionTurn.ExistingResult != nil {
			if runtimeErr := executor.validateExistingAgentSessionResult(
				record, *sessionTurn, state,
			); runtimeErr != nil {
				return ExecutionOutcome{
					State: StateNeedsReconciliation, Error: runtimeErr,
				}
			}
			resultJSON, err := json.Marshal(map[string]any{
				"outcome":        state.TerminalOutcome,
				"session_result": sessionTurn.ExistingResult,
				"state": map[string]any{
					"round":           state.Round,
					"tool_call_count": state.ToolCallCount,
					"total_tokens":    state.TotalTokens,
				},
			})
			if err != nil {
				return ExecutionOutcome{
					State: StateNeedsReconciliation,
					Error: agentRecoveryError(
						"encode recovered Agent outcome: " + err.Error(),
					),
				}
			}
			switch state.TerminalOutcome.State {
			case agent.StateCompleted:
				return ExecutionOutcome{
					State: StateCompleted, Result: resultJSON,
				}
			case agent.StateFailed:
				return ExecutionOutcome{
					State: StateFailed, Result: resultJSON,
					Error: state.TerminalOutcome.Error,
				}
			case agent.StateCancelled:
				return ExecutionOutcome{
					State: StateCancelled, Result: resultJSON,
					Error: state.TerminalOutcome.Error,
				}
			default:
				return pendingAgentReconciliation(
					"terminal Agent Session has an unsupported checkpoint outcome",
				)
			}
		}
	}
	currentGate := agent.PreEffectGate(func(
		gateContext context.Context,
	) *contract.RuntimeError {
		return executor.currentAgentExecutionGate(
			gateContext, record.Request, executionSnapshot,
		)
	})
	modelRecorder := &recordingModel{
		runID: record.ID, model: executor.Model, store: executor.Store,
		sequence: state.Round, beforeEffect: currentGate,
	}
	effects := &durableEffects{store: executor.Store}
	kernel := agent.Kernel{
		Model: modelRecorder, Tools: boundTools, Effects: effects,
		BeforeEffect: currentGate,
		Budget:       record.Request.AgentBudget, Now: executor.Now,
	}
	var outcome agent.Outcome
	if resumeCurrentPause {
		state, outcome, runtimeErr = kernel.Resume(
			ctx, state, resumeInput, sink,
		)
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
		if sessionStateErr := validateAgentSessionState(
			record, *sessionTurn, state.Messages,
		); sessionStateErr != nil {
			runtimeErr = sessionStateErr
			outcome.State = agent.StateNeedsReconciliation
			outcome.Error = runtimeErr
		} else {
			currentSessionResult, sessionErr := executor.Sessions.SettleAgent(
				*sessionTurn, state.Messages, outcome,
			)
			sessionResult = &currentSessionResult
			if sessionErr != nil {
				runtimeErr = &contract.RuntimeError{
					Code: contract.ErrorConflict, Phase: contract.PhaseRun,
					Message: "persist agent Session projection: " + sessionErr.Message,
				}
				outcome.State = agent.StateNeedsReconciliation
				outcome.Error = runtimeErr
			}
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

func (executor *AgentExecutor) now() time.Time {
	if executor != nil && executor.Now != nil {
		return executor.Now().UTC()
	}
	return time.Now().UTC()
}

func cloneTimePointer(value *time.Time) *time.Time {
	if value == nil {
		return nil
	}
	current := value.UTC()
	return &current
}

func (executor *AgentExecutor) Reconcile(
	ctx context.Context,
	record Record,
) ExecutionOutcome {
	executionSnapshot, _, snapshotErr :=
		executor.loadAgentExecutionSnapshot(ctx, &record)
	if snapshotErr != nil {
		return pendingAgentReconciliation(snapshotErr.Message)
	}
	reconciledErr := reconciledAgentError(record.Error)
	var sessionResult *session.RunResult
	if record.Request.SessionID != "" {
		if executor == nil || executor.Sessions == nil {
			return pendingAgentReconciliation(
				"agent Session service is unavailable",
			)
		}
		turn, found, lookupErr := executor.Sessions.LookupAgent(
			agentSessionRunRequest(record.Request, record.ID),
		)
		if lookupErr != nil {
			return ExecutionOutcome{
				State: StateNeedsReconciliation, Error: lookupErr,
			}
		}
		if !found {
			return pendingAgentReconciliation(
				"Agent Session reconciliation has no matching Turn",
			)
		}
		if runtimeErr := validateAgentSessionSnapshot(
			record, turn, executionSnapshot,
		); runtimeErr != nil {
			return ExecutionOutcome{
				State: StateNeedsReconciliation, Error: runtimeErr,
			}
		}
		reconciliation, runtimeErr := executor.Sessions.ReconcileAgent(
			ctx, record.Request.SessionID, record.ID, record.Error,
		)
		if runtimeErr != nil {
			return ExecutionOutcome{
				State: StateNeedsReconciliation, Error: runtimeErr,
			}
		}
		if !reconciliation.Resolved ||
			reconciliation.RunID != record.ID {
			return pendingAgentReconciliation(
				"Agent Session reconciliation did not resolve the matching Turn",
			)
		}
		result, found, err := executor.Sessions.ResultForRun(
			record.Request.SessionID, record.ID,
		)
		if err != nil {
			return pendingAgentReconciliation(
				"read Agent Session reconciliation result: " + err.Error(),
			)
		}
		if !found ||
			result.TurnID != reconciliation.TurnID ||
			result.ExecutionID != reconciliation.ExecutionID {
			return pendingAgentReconciliation(
				"Agent Session reconciliation result is missing or inconsistent",
			)
		}
		execution, err := executor.Sessions.Execution(
			record.Request.SessionID, result.ExecutionID,
		)
		if err != nil {
			return pendingAgentReconciliation(
				"read Agent Session execution evidence: " + err.Error(),
			)
		}
		if execution.State != session.ExecutionSettled ||
			execution.RunID != record.ID ||
			execution.SessionID != record.Request.SessionID ||
			execution.TurnID != result.TurnID ||
			execution.ProfileID != record.Request.ProfileID ||
			execution.RequestDigest !=
				executionSnapshot.SessionRequestDigest ||
			execution.ConfigDigest !=
				executionSnapshot.SessionConfigDigest {
			return pendingAgentReconciliation(
				"Agent Session execution evidence does not match the durable Run",
			)
		}
		sessionResult = &result
		if result.Error != nil {
			reconciledErr = result.Error
		}
		switch {
		case result.State == session.TurnCompleted &&
			execution.Outcome == session.OutcomeCompleted:
			return agentReconciliationOutcome(
				StateCompleted, agent.StateCompleted,
				"projection_reconciled", "known", sessionResult, nil,
			)
		case result.State == session.TurnCancelled &&
			execution.Outcome == session.OutcomeCancelled:
			return agentReconciliationOutcome(
				StateCancelled, agent.StateCancelled,
				"projection_reconciled", "known",
				sessionResult, result.Error,
			)
		case result.State == session.TurnFailed &&
			execution.Outcome == session.OutcomeFailed:
			return agentReconciliationOutcome(
				StateFailed, agent.StateFailed,
				"projection_reconciled", "known",
				sessionResult, result.Error,
			)
		case result.State != session.TurnFailed ||
			execution.Outcome != session.OutcomeUnknown:
			return pendingAgentReconciliation(
				"Agent Session terminal evidence is inconsistent",
			)
		}
	}
	return agentReconciliationOutcome(
		StateFailed, agent.StateNeedsReconciliation,
		"explicitly_reconciled", "unknown",
		sessionResult, reconciledErr,
	)
}
