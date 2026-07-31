package run

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"reflect"
	"sync"
	"time"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
	"github.com/yy003x/runtime/internal/strictjson"
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

func validateProjectedAgentSessionPause(
	turn session.AgentTurn,
	state agent.LoopState,
) *contract.RuntimeError {
	if turn.ProjectedPause == nil ||
		state.Pause == nil ||
		state.TerminalOutcome == nil ||
		state.TerminalOutcome.State != agent.StatePaused ||
		state.TerminalOutcome.Pause == nil ||
		!reflect.DeepEqual(turn.ProjectedPause, state.Pause) ||
		!reflect.DeepEqual(state.TerminalOutcome.Pause, state.Pause) {
		return agentRecoveryError(
			"Agent paused Session event does not match the durable checkpoint pause",
		)
	}
	safeEnd, openAssistant, completedCalls, err :=
		agentProviderSafePrefix(state.Messages, state.BaseMessageCount)
	if err != nil || openAssistant < 0 ||
		completedCalls >=
			len(state.Messages[openAssistant].ToolCalls) ||
		state.Messages[openAssistant].ToolCalls[completedCalls].ID !=
			state.Pause.ToolCallID {
		return agentRecoveryError(
			"Agent paused checkpoint has an invalid provider message boundary",
		)
	}
	if safeEnd != len(turn.Messages) ||
		!reflect.DeepEqual(turn.Messages, state.Messages[:safeEnd]) {
		return agentRecoveryError(
			"Agent paused Session messages are not the checkpoint provider-safe prefix",
		)
	}
	return nil
}

func validateAgentSessionSafeProjection(
	turn session.AgentTurn,
	state agent.LoopState,
) *contract.RuntimeError {
	safeEnd, _, _, err := agentProviderSafePrefix(
		state.Messages, state.BaseMessageCount,
	)
	if err != nil ||
		safeEnd != len(turn.Messages) ||
		!reflect.DeepEqual(turn.Messages, state.Messages[:safeEnd]) {
		return agentRecoveryError(
			"Agent Session messages are not the checkpoint provider-safe prefix",
		)
	}
	return nil
}

func validateAgentSessionPrefixProjection(
	turn session.AgentTurn,
	state agent.LoopState,
) *contract.RuntimeError {
	safeEnd, _, _, err := agentProviderSafePrefix(
		state.Messages, state.BaseMessageCount,
	)
	projectedSafeEnd, _, _, projectedErr := agentProviderSafePrefix(
		turn.Messages, turn.BaseMessageCount,
	)
	if err != nil || projectedErr != nil ||
		projectedSafeEnd != len(turn.Messages) ||
		len(turn.Messages) > safeEnd ||
		!reflect.DeepEqual(
			turn.Messages, state.Messages[:len(turn.Messages)],
		) {
		return agentRecoveryError(
			"Agent Session messages are not a prefix of the checkpoint provider-safe projection",
		)
	}
	return nil
}

func (executor *AgentExecutor) validateExistingAgentSessionCancellation(
	record Record,
	turn session.AgentTurn,
) (agent.Outcome, error) {
	result := turn.ExistingResult
	if result == nil ||
		result.SessionID != record.Request.SessionID ||
		result.RunID != record.ID ||
		result.TurnID != turn.TurnID ||
		result.ExecutionID != turn.ExecutionID ||
		result.State != session.TurnCancelled ||
		result.CaptureQuality != session.CaptureStructured ||
		result.Message != nil ||
		result.Error == nil ||
		(result.Error.Code != contract.ErrorCancelled &&
			result.Error.Code != contract.ErrorTimeout) {
		return agent.Outcome{}, agentRecoveryError(
			"terminal Agent Session cancellation does not match the durable Run",
		)
	}
	if err := result.Error.Validate(); err != nil {
		return agent.Outcome{}, agentRecoveryError(
			"terminal Agent Session cancellation error is invalid: " +
				err.Error(),
		)
	}
	execution, err := executor.Sessions.Execution(
		record.Request.SessionID, turn.ExecutionID,
	)
	if err != nil {
		return agent.Outcome{}, agentRecoveryError(
			"load terminal Agent Session cancellation: " + err.Error(),
		)
	}
	if execution.State != session.ExecutionSettled ||
		execution.Outcome != session.OutcomeCancelled ||
		execution.SessionID != record.Request.SessionID ||
		execution.RunID != record.ID ||
		execution.TurnID != turn.TurnID ||
		execution.ID != turn.ExecutionID ||
		execution.ProfileID != record.Request.ProfileID ||
		execution.ProfileKind != profile.KindModel ||
		execution.RequestDigest != turn.RequestDigest ||
		execution.ConfigDigest != turn.ConfigDigest ||
		execution.Error == nil ||
		!reflect.DeepEqual(execution.Error, result.Error) {
		return agent.Outcome{}, agentRecoveryError(
			"terminal Agent Session cancellation evidence is inconsistent",
		)
	}
	sessionValue, err := executor.Sessions.Get(record.Request.SessionID)
	if err != nil {
		return agent.Outcome{}, agentRecoveryError(
			"load cancelled Agent Session: " + err.Error(),
		)
	}
	if sessionValue.State != session.SessionIdle ||
		sessionValue.ActiveTurnID != "" {
		return agent.Outcome{}, agentRecoveryError(
			"cancelled Agent Session remains active or blocked",
		)
	}
	events, err := executor.Sessions.Events(
		record.Request.SessionID, 0,
	)
	if err != nil {
		return agent.Outcome{}, agentRecoveryError(
			"load cancelled Agent Session events: " + err.Error(),
		)
	}
	var terminal *session.EventRecord
	for index := range events {
		event := events[index]
		if event.TurnID == turn.TurnID &&
			event.RunID == record.ID &&
			event.ExecutionID == turn.ExecutionID {
			current := event
			terminal = &current
		}
	}
	if terminal == nil ||
		terminal.Type != "agent.cancelled" ||
		terminal.State != string(session.TurnCancelled) ||
		!reflect.DeepEqual(terminal.Error, result.Error) {
		return agent.Outcome{}, agentRecoveryError(
			"terminal Agent Session cancellation event is missing or inconsistent",
		)
	}
	var outcome agent.Outcome
	if err := strictjson.DecodeObject(
		bytes.NewReader(terminal.Detail), 1<<20, &outcome,
	); err != nil {
		return agent.Outcome{}, agentRecoveryError(
			"decode terminal Agent Session cancellation outcome: " +
				err.Error(),
		)
	}
	if outcome.State != agent.StateCancelled ||
		outcome.StopReason != "cancelled" ||
		outcome.Message != nil ||
		outcome.Pause != nil ||
		!reflect.DeepEqual(outcome.Error, result.Error) {
		return agent.Outcome{}, agentRecoveryError(
			"terminal Agent Session cancellation outcome is inconsistent",
		)
	}
	return outcome, nil
}

func agentProviderSafePrefix(
	messages []contract.Message,
	baseMessageCount int,
) (safeEnd, openAssistant, completedCalls int, err error) {
	if baseMessageCount < 0 || baseMessageCount > len(messages) {
		return 0, -1, 0, fmt.Errorf(
			"agent base message boundary is invalid",
		)
	}
	safeEnd = baseMessageCount
	openAssistant = -1
	for index := baseMessageCount; index < len(messages); {
		assistant := messages[index]
		if assistant.Role != contract.RoleAssistant {
			return 0, -1, 0, fmt.Errorf(
				"agent message suffix contains a non-assistant round boundary",
			)
		}
		if len(assistant.ToolCalls) == 0 {
			safeEnd = index + 1
			index++
			if index != len(messages) {
				return 0, -1, 0, fmt.Errorf(
					"agent message suffix continues after a terminal assistant message",
				)
			}
			continue
		}
		cursor := index + 1
		for callIndex, call := range assistant.ToolCalls {
			if cursor >= len(messages) {
				return safeEnd, index, callIndex, nil
			}
			toolMessage := messages[cursor]
			if toolMessage.Role != contract.RoleTool ||
				toolMessage.ToolCallID != call.ID {
				return safeEnd, index, callIndex, nil
			}
			cursor++
		}
		safeEnd = cursor
		index = cursor
	}
	return safeEnd, openAssistant, completedCalls, nil
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

func (executor *AgentExecutor) hasRecoveryEvidence(
	ctx context.Context,
	runID string,
) (bool, *contract.RuntimeError) {
	_, checkpointExists, err := executor.Store.LatestCheckpoint(ctx, runID)
	if err != nil {
		return false, agentRecoveryError(
			"probe durable Agent checkpoint: " + err.Error(),
		)
	}
	_, modelCallExists, err := executor.Store.LatestModelCall(ctx, runID)
	if err != nil {
		return false, agentRecoveryError(
			"probe durable Agent model call: " + err.Error(),
		)
	}
	return checkpointExists || modelCallExists, nil
}

func validateAgentSessionState(
	record Record,
	turn session.AgentTurn,
	messages []contract.Message,
) *contract.RuntimeError {
	if turn.SessionID != record.Request.SessionID ||
		turn.RunID != record.ID ||
		turn.ProfileID != record.Request.ProfileID ||
		turn.ProfileKind != profile.KindModel ||
		!validSHA256Digest(turn.RequestDigest) ||
		!validSHA256Digest(turn.ConfigDigest) ||
		turn.TurnID == "" ||
		turn.ExecutionID == "" {
		return agentRecoveryError(
			"Agent Session correlation does not match the durable Run",
		)
	}
	if turn.BaseMessageCount < 0 ||
		turn.BaseMessageCount > len(turn.Messages) ||
		turn.BaseMessageCount > len(messages) {
		return agentRecoveryError(
			"Agent Session base message boundary is invalid",
		)
	}
	for index := 0; index < turn.BaseMessageCount; index++ {
		if !reflect.DeepEqual(turn.Messages[index], messages[index]) {
			return agentRecoveryError(
				"Agent checkpoint messages do not match the Session canonical prefix",
			)
		}
	}
	return nil
}

func (executor *AgentExecutor) validateExistingAgentSessionResult(
	record Record,
	turn session.AgentTurn,
	state agent.LoopState,
) *contract.RuntimeError {
	result := turn.ExistingResult
	if result == nil {
		return nil
	}
	if result.SessionID != record.Request.SessionID ||
		result.RunID != record.ID ||
		result.TurnID != turn.TurnID ||
		result.ExecutionID != turn.ExecutionID ||
		result.CaptureQuality != session.CaptureStructured {
		return agentRecoveryError(
			"terminal Agent Session result does not match the durable Run",
		)
	}
	safeEnd, _, _, err := agentProviderSafePrefix(
		state.Messages, state.BaseMessageCount,
	)
	if err != nil ||
		safeEnd != len(turn.Messages) ||
		!reflect.DeepEqual(
			turn.Messages, state.Messages[:safeEnd],
		) {
		return agentRecoveryError(
			"terminal Agent Session messages do not match the checkpoint provider-safe prefix",
		)
	}
	execution, err := executor.Sessions.Execution(
		record.Request.SessionID, turn.ExecutionID,
	)
	if err != nil {
		return agentRecoveryError(
			"load terminal Agent Session execution: " + err.Error(),
		)
	}
	if execution.State != session.ExecutionSettled ||
		execution.SessionID != record.Request.SessionID ||
		execution.RunID != record.ID ||
		execution.TurnID != turn.TurnID ||
		execution.ID != turn.ExecutionID ||
		execution.ProfileID != record.Request.ProfileID ||
		execution.ProfileKind != profile.KindModel ||
		execution.RequestDigest != turn.RequestDigest ||
		execution.ConfigDigest != turn.ConfigDigest {
		return agentRecoveryError(
			"terminal Agent Session execution does not match the durable Run",
		)
	}
	if state.TerminalOutcome == nil {
		return agentRecoveryError(
			"terminal Agent Session outcome does not match Agent checkpoint evidence",
		)
	}
	switch state.TerminalOutcome.State {
	case agent.StateCompleted:
		if safeEnd != len(state.Messages) ||
			result.State != session.TurnCompleted ||
			execution.Outcome != session.OutcomeCompleted ||
			result.Error != nil ||
			execution.Error != nil ||
			state.TerminalOutcome.Error != nil ||
			state.TerminalOutcome.Message == nil ||
			!reflect.DeepEqual(
				result.Message, state.TerminalOutcome.Message,
			) {
			return agentRecoveryError(
				"completed Agent Session outcome does not match checkpoint evidence",
			)
		}
	case agent.StateFailed:
		if result.State != session.TurnFailed ||
			execution.Outcome != session.OutcomeFailed ||
			result.Message != nil ||
			state.TerminalOutcome.Error == nil ||
			!reflect.DeepEqual(
				result.Error, state.TerminalOutcome.Error,
			) ||
			!reflect.DeepEqual(
				execution.Error, state.TerminalOutcome.Error,
			) {
			return agentRecoveryError(
				"failed Agent Session outcome does not match checkpoint evidence",
			)
		}
	case agent.StateCancelled:
		if result.State != session.TurnCancelled ||
			execution.Outcome != session.OutcomeCancelled ||
			result.Message != nil ||
			state.TerminalOutcome.Error == nil ||
			!reflect.DeepEqual(
				result.Error, state.TerminalOutcome.Error,
			) ||
			!reflect.DeepEqual(
				execution.Error, state.TerminalOutcome.Error,
			) {
			return agentRecoveryError(
				"cancelled Agent Session outcome does not match checkpoint evidence",
			)
		}
	default:
		return agentRecoveryError(
			"terminal Agent Session is not backed by a completed, failed, or cancelled checkpoint",
		)
	}
	return nil
}

func (executor *AgentExecutor) loadState(
	ctx context.Context,
	record Record,
	initialMessages []contract.Message,
	initialBaseMessageCount int,
	toolDefinitions []contract.ToolSpec,
) (agent.LoopState, *contract.RuntimeError) {
	modelCalls, err := executor.Store.ModelCalls(ctx, record.ID)
	if err != nil {
		return agent.LoopState{}, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseRun,
			Message: "load agent model call journal: " + err.Error(),
		}
	}
	modelCallExists := len(modelCalls) > 0
	var modelCall ModelCall
	if modelCallExists {
		modelCall = modelCalls[len(modelCalls)-1]
	}
	checkpoint, exists, err := executor.Store.LatestCheckpoint(ctx, record.ID)
	if err != nil {
		return agent.LoopState{}, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseRun,
			Message: "load agent checkpoint: " + err.Error(),
		}
	}
	if exists {
		var state agent.LoopState
		if err := strictjson.Decode(
			bytes.NewReader(checkpoint.State), 4<<20, &state,
		); err != nil {
			return agent.LoopState{}, &contract.RuntimeError{
				Code: contract.ErrorInternal, Phase: contract.PhaseRun,
				Message: "decode agent checkpoint: " + err.Error(),
			}
		}
		if checkpoint.RunID != record.ID ||
			state.RunID != record.ID ||
			state.ModelProfile != record.Request.ProfileID {
			return agent.LoopState{}, agentRecoveryError(
				"agent checkpoint correlation does not match the durable Run",
			)
		}
		if state.SchemaVersion != agent.LoopStateSchemaVersion {
			return agent.LoopState{}, agentRecoveryError(
				"agent checkpoint has an unsupported loop state schema",
			)
		}
		if state.BaseMessageCount != initialBaseMessageCount {
			return agent.LoopState{}, agentRecoveryError(
				"agent checkpoint base message boundary does not match its execution",
			)
		}
		if checkpoint.Sequence != state.NextEventSequence {
			return agent.LoopState{}, agentRecoveryError(
				"agent checkpoint sequence does not match loop state",
			)
		}
		state, runtimeErr := executor.recoverDurableModelTerminal(
			record, state, modelCalls, true,
			initialMessages, initialBaseMessageCount, toolDefinitions,
		)
		if runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if runtimeErr := validateRecoveredLoopState(state); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if runtimeErr := validateModelCallJournal(
			modelCalls, state,
		); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if runtimeErr := validateModelRequestJournal(
			record, state, modelCalls,
		); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if runtimeErr := validateRecoveredBudget(
			record.Request.AgentBudget, state, modelCalls,
		); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if modelCallExists {
			if modelCall.RunID != record.ID {
				return agent.LoopState{}, agentRecoveryError(
					"latest agent model call does not belong to the durable Run",
				)
			}
			if runtimeErr := validateModelResultEvidence(
				modelCall, state,
			); runtimeErr != nil {
				return agent.LoopState{}, runtimeErr
			}
		}
		latestSequence, err := executor.Store.LatestEventSequence(
			ctx, record.ID,
		)
		if err != nil {
			return agent.LoopState{}, &contract.RuntimeError{
				Code: contract.ErrorInternal, Phase: contract.PhaseRun,
				Message: "load durable agent event sequence: " + err.Error(),
			}
		}
		if latestSequence < checkpoint.Sequence {
			return agent.LoopState{}, agentRecoveryError(
				"agent checkpoint is ahead of durable events",
			)
		}
		state.NextEventSequence = latestSequence
		if runtimeErr := executor.validateDurableAgentEventJournal(
			ctx, record, state, modelCalls,
		); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if runtimeErr := executor.validateToolEffectJournal(
			ctx, record, state, modelCalls,
		); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
		if state.PendingToolCursor < len(state.PendingToolCalls) &&
			state.PendingEffectCheckpointID != "" {
			if runtimeErr := executor.recoverPendingToolEvidence(
				ctx, record, &state,
			); runtimeErr != nil {
				return agent.LoopState{}, runtimeErr
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
	if initialBaseMessageCount <= 0 ||
		initialBaseMessageCount > len(messages) {
		return agent.LoopState{}, agentRecoveryError(
			"initial Agent base message boundary is invalid",
		)
	}
	state := agent.LoopState{
		SchemaVersion: agent.LoopStateSchemaVersion,
		RunID:         record.ID, ModelProfile: record.Request.ProfileID,
		Messages: messages, BaseMessageCount: initialBaseMessageCount,
	}
	if !modelCallExists {
		return state, nil
	}
	state, runtimeErr := executor.recoverDurableModelTerminal(
		record, state, modelCalls, false,
		initialMessages, initialBaseMessageCount, toolDefinitions,
	)
	if runtimeErr != nil {
		return agent.LoopState{}, runtimeErr
	}
	if runtimeErr := validateRecoveredLoopState(state); runtimeErr != nil {
		return agent.LoopState{}, runtimeErr
	}
	for _, validator := range []func() *contract.RuntimeError{
		func() *contract.RuntimeError {
			return validateModelCallJournal(modelCalls, state)
		},
		func() *contract.RuntimeError {
			return validateModelRequestJournal(record, state, modelCalls)
		},
		func() *contract.RuntimeError {
			return validateRecoveredBudget(
				record.Request.AgentBudget, state, modelCalls,
			)
		},
	} {
		if runtimeErr := validator(); runtimeErr != nil {
			return agent.LoopState{}, runtimeErr
		}
	}
	if runtimeErr := validateModelResultEvidence(
		modelCall, state,
	); runtimeErr != nil {
		return agent.LoopState{}, runtimeErr
	}
	latestSequence, err := executor.Store.LatestEventSequence(ctx, record.ID)
	if err != nil {
		return agent.LoopState{}, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseRun,
			Message: "load durable agent event sequence: " + err.Error(),
		}
	}
	state.NextEventSequence = latestSequence
	if runtimeErr := executor.validateDurableAgentEventJournal(
		ctx, record, state, modelCalls,
	); runtimeErr != nil {
		return agent.LoopState{}, runtimeErr
	}
	if runtimeErr := executor.validateToolEffectJournal(
		ctx, record, state, modelCalls,
	); runtimeErr != nil {
		return agent.LoopState{}, runtimeErr
	}
	return state, nil
}

// recoverDurableModelTerminal closes only the one model crash window whose
// outcome is independently proven by the append-only model-call row. A
// running call or a completed call ahead of the checkpoint remains ambiguous.
func (executor *AgentExecutor) recoverDurableModelTerminal(
	record Record,
	state agent.LoopState,
	modelCalls []ModelCall,
	hasCheckpoint bool,
	initialMessages []contract.Message,
	initialBaseMessageCount int,
	toolDefinitions []contract.ToolSpec,
) (agent.LoopState, *contract.RuntimeError) {
	if len(modelCalls) == 0 {
		return state, nil
	}
	last := modelCalls[len(modelCalls)-1]
	if last.State != "failed" && last.State != "cancelled" {
		if !hasCheckpoint || len(modelCalls) > state.Round {
			return agent.LoopState{}, agentRecoveryError(
				fmt.Sprintf(
					"agent model call sequence=%d state=%s is ahead of a durable terminal checkpoint",
					last.Sequence, last.State,
				),
			)
		}
		return state, nil
	}
	if last.Sequence != state.Round+1 ||
		len(modelCalls) != last.Sequence ||
		last.Error == nil ||
		len(last.Result) != 0 ||
		last.ResultDigest != "" {
		return agent.LoopState{}, agentRecoveryError(
			"durable terminal model call is not the exact next checkpoint round",
		)
	}
	if err := last.Error.Validate(); err != nil {
		return agent.LoopState{}, agentRecoveryError(
			"durable terminal model call error is invalid: " + err.Error(),
		)
	}
	outcome := agent.Outcome{
		State: agent.StateFailed, StopReason: "model_failed",
		Error: cloneRunRuntimeError(last.Error),
	}
	if last.State == "cancelled" {
		if last.Error.Code != contract.ErrorCancelled &&
			last.Error.Code != contract.ErrorTimeout {
			return agent.LoopState{}, agentRecoveryError(
				"cancelled model call has a non-cancellation error",
			)
		}
		outcome.State = agent.StateCancelled
		outcome.StopReason = "cancelled"
	} else if last.Error.Code == contract.ErrorCancelled ||
		last.Error.Code == contract.ErrorTimeout {
		return agent.LoopState{}, agentRecoveryError(
			"failed model call contains a cancellation error",
		)
	}
	var frozen contract.GenerateRequest
	if err := strictjson.DecodeObject(
		bytes.NewReader(last.Request), 4<<20, &frozen,
	); err != nil {
		return agent.LoopState{}, agentRecoveryError(
			"decode terminal frozen model request: " + err.Error(),
		)
	}
	if err := frozen.Validate(); err != nil {
		return agent.LoopState{}, agentRecoveryError(
			"terminal frozen model request is invalid: " + err.Error(),
		)
	}
	requestJSON, err := json.Marshal(frozen)
	if err != nil {
		return agent.LoopState{}, agentRecoveryError(
			"encode terminal frozen model request: " + err.Error(),
		)
	}
	sum := sha256.Sum256(requestJSON)
	if last.RequestDigest !=
		"sha256:"+hex.EncodeToString(sum[:]) {
		return agent.LoopState{}, agentRecoveryError(
			"terminal frozen model request digest does not match",
		)
	}
	if frozen.ModelProfile != record.Request.ProfileID ||
		frozen.Input.System != "" ||
		!reflect.DeepEqual(
			frozen.Input.Options, contract.GenerateOptions{},
		) ||
		!reflect.DeepEqual(
			frozen.Input.Trace,
			contract.TraceContext{Labels: map[string]string{
				"run_id": record.ID,
			}},
		) ||
		!equalAgentToolDefinitions(
			frozen.Input.Tools, toolDefinitions,
		) {
		return agent.LoopState{}, agentRecoveryError(
			"terminal frozen model request does not match the Agent execution adapter",
		)
	}
	baseMessages := initialMessages
	if len(baseMessages) == 0 {
		baseMessages = []contract.Message{{
			Role: contract.RoleUser, Content: record.Request.Input,
		}}
	}
	if initialBaseMessageCount <= 0 ||
		initialBaseMessageCount > len(baseMessages) ||
		initialBaseMessageCount > len(frozen.Input.Messages) ||
		state.BaseMessageCount != initialBaseMessageCount {
		return agent.LoopState{}, agentRecoveryError(
			"terminal model request has an invalid base message boundary",
		)
	}
	for index := 0; index < initialBaseMessageCount; index++ {
		if !reflect.DeepEqual(
			baseMessages[index], frozen.Input.Messages[index],
		) {
			return agent.LoopState{}, agentRecoveryError(
				"terminal model request changed the Agent base messages",
			)
		}
	}
	if state.TerminalOutcome != nil &&
		state.TerminalOutcome.State != agent.StatePaused {
		if !reflect.DeepEqual(state.TerminalOutcome, &outcome) ||
			!reflect.DeepEqual(state.Messages, frozen.Input.Messages) {
			return agent.LoopState{}, agentRecoveryError(
				"terminal model call conflicts with the durable Agent checkpoint",
			)
		}
		return state, nil
	}
	state.Messages = append(
		[]contract.Message(nil), frozen.Input.Messages...,
	)
	state.Pause = nil
	state.PendingToolCalls = nil
	state.PendingToolCursor = 0
	state.PendingEffectCheckpointID = ""
	state.PendingCheckpointID = ""
	state.PendingCheckpointCommitted = false
	state.PendingToolStarted = false
	state.PendingToolTerminal = false
	state.RecoveredFromCheckpoint = false
	state.TerminalOutcome = &outcome
	return state, nil
}

func equalAgentToolDefinitions(
	left, right []contract.ToolSpec,
) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if !reflect.DeepEqual(left[index], right[index]) {
			return false
		}
	}
	return true
}

type agentLifecycleEvidence struct {
	checkpoint map[string][]uint64
	started    map[string][]uint64
	completed  map[string][]uint64
	failed     map[string][]uint64
	paused     map[string][]uint64
}

type agentToolCallPosition struct {
	call       contract.ToolCall
	round      int
	callIndex  int
	roundCalls []contract.ToolCall
}

func (executor *AgentExecutor) validateToolEffectJournal(
	ctx context.Context,
	record Record,
	state agent.LoopState,
	modelCalls []ModelCall,
) *contract.RuntimeError {
	view, runtimeErr := partitionModelCallJournal(modelCalls, state)
	if runtimeErr != nil {
		return runtimeErr
	}
	callByID := make(map[string]agentToolCallPosition)
	for roundIndex, modelCall := range view.completed {
		result, runtimeErr := decodeDurableModelResult(modelCall)
		if runtimeErr != nil {
			return runtimeErr
		}
		for callIndex, call := range result.Message.ToolCalls {
			if _, exists := callByID[call.ID]; exists {
				isCurrentDuplicate :=
					state.TerminalOutcome != nil &&
						state.TerminalOutcome.State == agent.StateFailed &&
						state.TerminalOutcome.StopReason ==
							"duplicate_tool_call" &&
						roundIndex == len(view.completed)-1 &&
						len(state.PendingToolCalls) > 0 &&
						callIndex == state.PendingToolCursor &&
						state.PendingToolCalls[state.PendingToolCursor].ID ==
							call.ID
				if isCurrentDuplicate {
					continue
				}
				return agentRecoveryError(
					"Agent model results contain an unsupported duplicate tool call",
				)
			}
			callByID[call.ID] = agentToolCallPosition{
				call: call, round: roundIndex + 1, callIndex: callIndex,
				roundCalls: result.Message.ToolCalls,
			}
		}
	}
	effects, err := executor.Store.ToolEffects(ctx, record.ID)
	if err != nil {
		return agentRecoveryError(
			"load Agent tool effect journal: " + err.Error(),
		)
	}
	if len(effects) != len(state.SeenToolCallIDs) {
		return agentRecoveryError(
			"Agent tool effects do not match seen tool calls",
		)
	}
	effectByID := make(map[string]agent.EffectRecord, len(effects))
	checkpointByCallID := make(map[string]Checkpoint, len(effects))
	for _, indexed := range effects {
		position, seen := callByID[indexed.CallID]
		if !seen {
			return agentRecoveryError(
				"Agent tool effect is absent from model results",
			)
		}
		effect, exists, err := (&durableEffects{
			store: executor.Store,
		}).Lookup(ctx, record.ID, indexed.CallID)
		if err != nil {
			return agentRecoveryError(
				"decode Agent tool effect journal: " + err.Error(),
			)
		}
		if !exists {
			return agentRecoveryError(
				"Agent tool effect disappeared from its journal",
			)
		}
		call := position.call
		if effect.Request.RunID != record.ID ||
			effect.Request.CallID != call.ID ||
			effect.Request.Name != call.Name ||
			!bytes.Equal(effect.Request.Arguments, call.Arguments) ||
			effect.Request.IdempotencyKey !=
				agentToolIdempotencyKey(record.ID, call) {
			return agentRecoveryError(
				"Agent tool effect request does not match model result",
			)
		}
		if err := identity.Validate(
			effect.Request.CheckpointID, "checkpoint",
		); err != nil {
			return agentRecoveryError(
				"Agent tool effect has no valid preparation checkpoint",
			)
		}
		checkpoint, _, runtimeErr := executor.validateToolPreparationCheckpoint(
			ctx, record, state, position, effect.Request,
		)
		if runtimeErr != nil {
			return runtimeErr
		}
		effectByID[indexed.CallID] = effect
		checkpointByCallID[indexed.CallID] = checkpoint
	}
	for _, callID := range state.SeenToolCallIDs {
		if _, exists := effectByID[callID]; !exists {
			return agentRecoveryError(
				"seen Agent tool call has no durable effect",
			)
		}
	}
	evidence, runtimeErr := executor.loadAgentLifecycleEvidence(
		ctx, record.ID,
	)
	if runtimeErr != nil {
		return runtimeErr
	}
	checkpointOwner := make(map[string]string, len(effectByID))
	for callID, effect := range effectByID {
		checkpointID := effect.Request.CheckpointID
		if previous, exists := checkpointOwner[checkpointID]; exists &&
			previous != callID {
			return agentRecoveryError(
				"Agent tool effects reuse a preparation checkpoint",
			)
		}
		checkpointOwner[checkpointID] = callID
	}
	for checkpointID := range evidence.checkpoint {
		if _, exists := checkpointOwner[checkpointID]; !exists {
			return agentRecoveryError(
				"checkpoint.committed event has no matching Agent tool effect",
			)
		}
	}
	currentCallID := ""
	hasPendingEffect := false
	if state.PendingToolCursor < len(state.PendingToolCalls) {
		currentCallID =
			state.PendingToolCalls[state.PendingToolCursor].ID
		hasPendingEffect = state.PendingEffectCheckpointID != ""
	} else if state.TerminalOutcome != nil &&
		state.TerminalOutcome.State == agent.StatePaused &&
		state.Pause != nil {
		currentCallID = state.Pause.ToolCallID
	}
	for _, callID := range state.SeenToolCallIDs {
		effect := effectByID[callID]
		if callID == currentCallID &&
			hasPendingEffect {
			continue
		}
		if effect.State != "completed" ||
			effect.Result == nil || effect.Error != nil {
			return agentRecoveryError(
				"historical Agent tool call is not durably completed",
			)
		}
		checkpoints := evidence.checkpoint[effect.Request.CheckpointID]
		started := evidence.started[callID]
		checkpoint := checkpointByCallID[callID]
		if len(checkpoints) != 1 || len(started) != 1 ||
			checkpoints[0] != checkpoint.Sequence+1 ||
			checkpoints[0] >= started[0] {
			return agentRecoveryError(
				"historical Agent tool checkpoint/start evidence is inconsistent",
			)
		}
		if effect.Result.Pause == nil {
			completed := evidence.completed[callID]
			if len(completed) != 1 ||
				started[0] >= completed[0] ||
				len(evidence.failed[callID]) != 0 {
				return agentRecoveryError(
					"historical Agent tool completion evidence is inconsistent",
				)
			}
			continue
		}
		pause := effect.Result.Pause
		if pause.ToolCallID != callID {
			return agentRecoveryError(
				"historical Agent pause is not bound to its tool call",
			)
		}
		paused := evidence.paused[pause.ID]
		if len(paused) != 1 ||
			started[0] >= paused[0] ||
			len(evidence.completed[callID]) != 0 ||
			len(evidence.failed[callID]) != 0 {
			return agentRecoveryError(
				"historical Agent tool pause evidence is inconsistent",
			)
		}
	}
	if runtimeErr := validatePauseEventClosure(
		evidence, effectByID, currentCallID,
		hasPendingEffect,
	); runtimeErr != nil {
		return runtimeErr
	}
	resumes, err := executor.Store.Resumes(ctx, record.ID)
	if err != nil {
		return agentRecoveryError(
			"load Agent resume journal: " + err.Error(),
		)
	}
	if runtimeErr := validateAgentMessageJournal(
		record, state, view.completed, effectByID, resumes,
	); runtimeErr != nil {
		return runtimeErr
	}
	return nil
}

func (executor *AgentExecutor) validateToolPreparationCheckpoint(
	ctx context.Context,
	record Record,
	recoveredState agent.LoopState,
	position agentToolCallPosition,
	request agent.ToolRequest,
) (Checkpoint, agent.LoopState, *contract.RuntimeError) {
	checkpoint, exists, err := executor.Store.Checkpoint(
		ctx, request.CheckpointID,
	)
	if err != nil {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"load Agent tool preparation checkpoint: " + err.Error(),
		)
	}
	if !exists || checkpoint.ID != request.CheckpointID ||
		checkpoint.RunID != record.ID {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"Agent tool effect preparation checkpoint is missing or mismatched",
		)
	}
	var prepared agent.LoopState
	if err := strictjson.Decode(
		bytes.NewReader(checkpoint.State), 4<<20, &prepared,
	); err != nil {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"decode Agent tool preparation checkpoint: " + err.Error(),
		)
	}
	if prepared.SchemaVersion != agent.LoopStateSchemaVersion {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"Agent tool preparation checkpoint uses an unsupported schema",
		)
	}
	if runtimeErr := validateRecoveredLoopState(prepared); runtimeErr != nil {
		return Checkpoint{}, agent.LoopState{}, runtimeErr
	}
	if checkpoint.Sequence != prepared.NextEventSequence ||
		prepared.RunID != record.ID ||
		prepared.ModelProfile != record.Request.ProfileID ||
		prepared.BaseMessageCount != recoveredState.BaseMessageCount ||
		prepared.Round != position.round ||
		prepared.PendingToolCursor != position.callIndex ||
		prepared.PendingEffectCheckpointID != checkpoint.ID ||
		!reflect.DeepEqual(
			prepared.PendingToolCalls, position.roundCalls,
		) ||
		!reflect.DeepEqual(
			prepared.PendingToolCalls[position.callIndex],
			position.call,
		) {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"Agent tool preparation checkpoint does not match its model call",
		)
	}
	if len(prepared.SeenToolCallIDs) == 0 ||
		prepared.SeenToolCallIDs[len(prepared.SeenToolCallIDs)-1] !=
			position.call.ID ||
		len(prepared.SeenToolCallIDs) > len(recoveredState.SeenToolCallIDs) ||
		!equalStringSlice(
			prepared.SeenToolCallIDs,
			recoveredState.SeenToolCallIDs[:len(prepared.SeenToolCallIDs)],
		) {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"Agent tool preparation checkpoint seen calls are inconsistent",
		)
	}
	if len(prepared.Messages) > len(recoveredState.Messages) ||
		!reflect.DeepEqual(
			prepared.Messages,
			recoveredState.Messages[:len(prepared.Messages)],
		) {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"Agent tool preparation checkpoint is not a message prefix",
		)
	}
	assistantIndex := -1
	for index := len(prepared.Messages) - 1; index >= 0; index-- {
		if prepared.Messages[index].Role == contract.RoleAssistant {
			assistantIndex = index
			break
		}
	}
	if assistantIndex < prepared.BaseMessageCount ||
		!reflect.DeepEqual(
			prepared.Messages[assistantIndex].ToolCalls,
			position.roundCalls,
		) {
		return Checkpoint{}, agent.LoopState{}, agentRecoveryError(
			"Agent tool preparation checkpoint has no matching assistant call",
		)
	}
	return checkpoint, prepared, nil
}

func validatePauseEventClosure(
	evidence agentLifecycleEvidence,
	effects map[string]agent.EffectRecord,
	currentCallID string,
	hasPendingCall bool,
) *contract.RuntimeError {
	expected := make(map[string]string)
	for callID, effect := range effects {
		if effect.State != "completed" || effect.Result == nil ||
			effect.Result.Pause == nil {
			continue
		}
		pause := effect.Result.Pause
		if pause.ToolCallID != callID {
			return agentRecoveryError(
				"durable Agent pause is not bound to its tool call",
			)
		}
		if previous, exists := expected[pause.ID]; exists &&
			previous != callID {
			return agentRecoveryError(
				"durable Agent pauses reuse a pause_id",
			)
		}
		expected[pause.ID] = callID
	}
	for pauseID, sequences := range evidence.paused {
		if _, exists := expected[pauseID]; !exists ||
			len(sequences) != 1 {
			return agentRecoveryError(
				"agent.paused event has no unique durable pause",
			)
		}
	}
	for pauseID, callID := range expected {
		count := len(evidence.paused[pauseID])
		if callID == currentCallID && hasPendingCall {
			if count > 1 {
				return agentRecoveryError(
					"pending Agent pause has duplicate terminal events",
				)
			}
			continue
		}
		if count != 1 {
			return agentRecoveryError(
				"durable Agent pause has no terminal event",
			)
		}
	}
	return nil
}

func validateAgentMessageJournal(
	record Record,
	state agent.LoopState,
	modelCalls []ModelCall,
	effects map[string]agent.EffectRecord,
	resumes []ResumeRecord,
) *contract.RuntimeError {
	if state.BaseMessageCount <= 0 ||
		state.BaseMessageCount > len(state.Messages) {
		return agentRecoveryError(
			"Agent base message boundary is invalid",
		)
	}
	if record.Request.SessionID == "" {
		base := contract.Message{
			Role: contract.RoleUser, Content: record.Request.Input,
		}
		if state.BaseMessageCount != 1 ||
			!reflect.DeepEqual(state.Messages[0], base) {
			return agentRecoveryError(
				"Agent base messages do not match the Run input",
			)
		}
	}
	expected := append(
		[]contract.Message(nil),
		state.Messages[:state.BaseMessageCount]...,
	)
	currentCallID := ""
	if state.PendingToolCursor < len(state.PendingToolCalls) {
		currentCallID =
			state.PendingToolCalls[state.PendingToolCursor].ID
	} else if state.TerminalOutcome != nil &&
		state.TerminalOutcome.State == agent.StatePaused &&
		state.Pause != nil {
		currentCallID = state.Pause.ToolCallID
	}
	resumeIndex := 0
	pauseIDs := make(map[string]struct{})
	seenIndex := 0
	for _, modelCall := range modelCalls {
		result, runtimeErr := decodeDurableModelResult(modelCall)
		if runtimeErr != nil {
			return runtimeErr
		}
		expected = append(expected, result.Message)
		for _, call := range result.Message.ToolCalls {
			if seenIndex >= len(state.SeenToolCallIDs) ||
				state.SeenToolCallIDs[seenIndex] != call.ID {
				continue
			}
			effect, exists := effects[call.ID]
			if !exists {
				return agentRecoveryError(
					"Agent message journal tool call has no durable effect",
				)
			}
			isPending := len(state.PendingToolCalls) > 0 &&
				state.PendingEffectCheckpointID != "" &&
				call.ID == currentCallID
			isActivePause :=
				state.TerminalOutcome != nil &&
					state.TerminalOutcome.State == agent.StatePaused &&
					call.ID == currentCallID
			if effect.State == "completed" && effect.Result != nil {
				if effect.Result.Pause != nil {
					pause := effect.Result.Pause
					if pause.ToolCallID != call.ID {
						return agentRecoveryError(
							"Agent pause message is not bound to its tool call",
						)
					}
					if _, exists := pauseIDs[pause.ID]; exists {
						return agentRecoveryError(
							"durable Agent pauses reuse a pause_id",
						)
					}
					pauseIDs[pause.ID] = struct{}{}
					var resumeInput *agent.ResumeInput
					var resumeRecord *ResumeRecord
					if resumeIndex < len(resumes) {
						candidate, err := decodeAgentResumeInput(
							resumes[resumeIndex].Input,
						)
						if err != nil {
							return agentRecoveryError(
								"decode durable Agent resume journal: " + err.Error(),
							)
						}
						if candidate.PauseID == pause.ID {
							if err := agent.ValidateResumeInput(
								pause.InputSchema, candidate.Input,
							); err != nil {
								return agentRecoveryError(
									"Agent resume journal input is invalid: " + err.Error(),
								)
							}
							if pause.ExpiresAt != nil &&
								resumes[resumeIndex].AcceptedAt.After(
									*pause.ExpiresAt,
								) {
								return agentRecoveryError(
									"Agent resume journal was accepted after pause expiry",
								)
							}
							resumeInput = &candidate
							resumeRecord = &resumes[resumeIndex]
							resumeIndex++
						}
					}
					if !isPending && !isActivePause {
						if resumeInput == nil {
							return agentRecoveryError(
								"resumed Agent pause has no durable resume journal entry",
							)
						}
						if len(expected) >= len(state.Messages) {
							return agentRecoveryError(
								"resumed Agent pause has no tool message",
							)
						}
						message := state.Messages[len(expected)]
						if message.Role != contract.RoleTool ||
							message.ToolCallID != call.ID ||
							message.IsError ||
							len(message.ToolCalls) != 0 ||
							agent.ValidateResumeInput(
								pause.InputSchema,
								json.RawMessage(message.Content),
							) != nil {
							return agentRecoveryError(
								"resumed Agent pause tool message is invalid",
							)
						}
						if !bytes.Equal(
							resumeInput.Input,
							[]byte(message.Content),
						) {
							return agentRecoveryError(
								"Agent resume message does not match its journal input",
							)
						}
						expected = append(expected, message)
					} else if isActivePause && resumeRecord == nil {
						// The current pause has not been accepted yet.
					}
				} else {
					message := contract.Message{
						Role: contract.RoleTool, ToolCallID: call.ID,
						Content: effect.Result.Content,
						IsError: effect.Result.IsError,
					}
					if isPending {
						if len(expected) < len(state.Messages) &&
							reflect.DeepEqual(
								state.Messages[len(expected)], message,
							) {
							expected = append(expected, message)
						}
					} else {
						expected = append(expected, message)
					}
				}
			}
			seenIndex++
		}
	}
	if seenIndex != len(state.SeenToolCallIDs) ||
		!reflect.DeepEqual(expected, state.Messages) {
		return agentRecoveryError(
			"Agent checkpoint messages do not match the durable execution journal",
		)
	}
	if resumeIndex != len(resumes) {
		return agentRecoveryError(
			"Agent resume journal has no one-to-one durable pause mapping",
		)
	}
	if len(resumes) == 0 {
		if len(record.Request.Resume) != 0 ||
			record.ResumeAcceptedAt != nil {
			return agentRecoveryError(
				"Agent Run latest resume fields have no journal entry",
			)
		}
	} else {
		latest := resumes[len(resumes)-1]
		if !bytes.Equal(record.Request.Resume, latest.Input) ||
			record.ResumeAcceptedAt == nil ||
			!record.ResumeAcceptedAt.Equal(latest.AcceptedAt) {
			return agentRecoveryError(
				"Agent Run latest resume fields do not match the append-only journal",
			)
		}
	}
	return nil
}

func (executor *AgentExecutor) loadAgentLifecycleEvidence(
	ctx context.Context,
	runID string,
) (agentLifecycleEvidence, *contract.RuntimeError) {
	result := agentLifecycleEvidence{
		checkpoint: make(map[string][]uint64),
		started:    make(map[string][]uint64),
		completed:  make(map[string][]uint64),
		failed:     make(map[string][]uint64),
		paused:     make(map[string][]uint64),
	}
	var afterSequence uint64
	for {
		events, err := executor.Store.Events(
			ctx, runID, afterSequence, 1000,
		)
		if err != nil {
			return agentLifecycleEvidence{}, agentRecoveryError(
				"load Agent lifecycle evidence: " + err.Error(),
			)
		}
		for _, event := range events {
			switch event.Type {
			case contract.EventCheckpointCommitted:
				result.checkpoint[event.Checkpoint.CheckpointID] =
					append(
						result.checkpoint[event.Checkpoint.CheckpointID],
						event.Sequence,
					)
			case contract.EventToolStarted:
				result.started[event.Tool.CallID] = append(
					result.started[event.Tool.CallID], event.Sequence,
				)
			case contract.EventToolCompleted:
				result.completed[event.Tool.CallID] = append(
					result.completed[event.Tool.CallID], event.Sequence,
				)
			case contract.EventToolFailed:
				result.failed[event.Tool.CallID] = append(
					result.failed[event.Tool.CallID], event.Sequence,
				)
			case contract.EventAgentPaused:
				result.paused[event.Agent.PauseID] = append(
					result.paused[event.Agent.PauseID], event.Sequence,
				)
			}
		}
		if len(events) < 1000 {
			break
		}
		afterSequence = events[len(events)-1].Sequence
	}
	return result, nil
}

func (executor *AgentExecutor) validateDurableAgentEventJournal(
	ctx context.Context,
	record Record,
	state agent.LoopState,
	modelCalls []ModelCall,
) *contract.RuntimeError {
	view, runtimeErr := partitionModelCallJournal(modelCalls, state)
	if runtimeErr != nil {
		return runtimeErr
	}
	durableResults := make(
		[]contract.ModelResult, len(view.completed),
	)
	for index, call := range view.completed {
		result, runtimeErr := decodeDurableModelResult(call)
		if runtimeErr != nil {
			return runtimeErr
		}
		durableResults[index] = result
	}
	var modelStarted, modelCompleted int
	var agentCompleted int
	agentPaused := make(map[string]int)
	eventModelResults := make(
		[]contract.ModelResult, 0, len(view.completed),
	)
	modelActive := false
	var afterSequence uint64
	for {
		events, err := executor.Store.Events(
			ctx, record.ID, afterSequence, 1000,
		)
		if err != nil {
			return agentRecoveryError(
				"validate durable Agent event journal: " + err.Error(),
			)
		}
		for _, event := range events {
			switch event.Type {
			case contract.EventModelStarted:
				if modelActive || modelStarted != modelCompleted ||
					modelStarted >= len(modelCalls) {
					return agentRecoveryError(
						"model.started event is out of order",
					)
				}
				modelActive = true
				modelStarted++
			case contract.EventContentDelta,
				contract.EventReasoningDelta,
				contract.EventToolCallStarted,
				contract.EventToolCallArgumentsDelta:
				if !modelActive {
					return agentRecoveryError(
						"model streaming event is outside an active model round",
					)
				}
			case contract.EventModelCompleted:
				if !modelActive ||
					modelCompleted >= len(durableResults) {
					return agentRecoveryError(
						"model.completed event is out of order",
					)
				}
				modelCompleted++
				if event.Model != nil && event.Model.Result != nil {
					eventModelResults = append(
						eventModelResults, *event.Model.Result,
					)
				}
				modelActive = false
			case contract.EventCheckpointCommitted:
				if modelActive {
					return agentRecoveryError(
						"checkpoint event occurred during a model round",
					)
				}
				checkpoint, exists, err := executor.Store.Checkpoint(
					ctx, event.Checkpoint.CheckpointID,
				)
				if err != nil {
					return agentRecoveryError(
						"load event checkpoint evidence: " + err.Error(),
					)
				}
				if !exists || checkpoint.RunID != record.ID {
					return agentRecoveryError(
						"checkpoint.committed event has no matching checkpoint",
					)
				}
			case contract.EventToolStarted,
				contract.EventToolCompleted,
				contract.EventToolFailed:
				if modelActive {
					return agentRecoveryError(
						"tool event occurred during a model round",
					)
				}
				if runtimeErr := executor.validateDurableToolEvent(
					ctx, record.ID, event,
				); runtimeErr != nil {
					return runtimeErr
				}
			case contract.EventAgentCompleted:
				if modelActive {
					return agentRecoveryError(
						"agent.completed occurred during a model round",
					)
				}
				agentCompleted++
				if state.TerminalOutcome == nil ||
					state.TerminalOutcome.State != agent.StateCompleted ||
					event.Agent.StopReason !=
						state.TerminalOutcome.StopReason {
					return agentRecoveryError(
						"agent.completed event does not match checkpoint outcome",
					)
				}
			case contract.EventAgentPaused:
				if modelActive {
					return agentRecoveryError(
						"agent.paused occurred during a model round",
					)
				}
				agentPaused[event.Agent.PauseID]++
			default:
				return agentRecoveryError(
					"durable Agent journal contains an unsupported event type",
				)
			}
		}
		if len(events) < 1000 {
			break
		}
		afterSequence = events[len(events)-1].Sequence
	}
	modelLifecycleValid :=
		modelCompleted == len(view.completed) &&
			len(eventModelResults) == len(view.completed)
	if view.terminal == nil {
		modelLifecycleValid = modelLifecycleValid &&
			!modelActive && modelStarted == modelCompleted
	} else {
		modelLifecycleValid = modelLifecycleValid &&
			(modelStarted == modelCompleted && !modelActive ||
				modelStarted == modelCompleted+1 && modelActive)
	}
	if !modelLifecycleValid || agentCompleted > 1 {
		return agentRecoveryError(
			"durable Agent event journal lifecycle counts are inconsistent",
		)
	}
	for index := range durableResults {
		if !reflect.DeepEqual(
			eventModelResults[index], durableResults[index],
		) {
			return agentRecoveryError(
				"model.completed event does not match durable model result",
			)
		}
	}
	if state.TerminalOutcome != nil {
		switch state.TerminalOutcome.State {
		case agent.StateCompleted:
			if agentCompleted != 1 {
				return agentRecoveryError(
					"completed Agent checkpoint has no terminal event",
				)
			}
		case agent.StatePaused:
			if state.Pause == nil ||
				agentPaused[state.Pause.ID] != 1 {
				return agentRecoveryError(
					"paused Agent checkpoint has no terminal event",
				)
			}
		case agent.StateFailed, agent.StateCancelled:
			if agentCompleted != 0 {
				return agentRecoveryError(
					"failed or cancelled Agent checkpoint has a completed terminal event",
				)
			}
		}
	}
	return nil
}

type modelCallJournalView struct {
	completed []ModelCall
	terminal  *ModelCall
}

func partitionModelCallJournal(
	modelCalls []ModelCall,
	state agent.LoopState,
) (modelCallJournalView, *contract.RuntimeError) {
	view := modelCallJournalView{
		completed: make([]ModelCall, 0, len(modelCalls)),
	}
	for index := range modelCalls {
		call := modelCalls[index]
		if call.Sequence != index+1 {
			return modelCallJournalView{}, agentRecoveryError(
				"agent model call journal is not contiguous",
			)
		}
		switch call.State {
		case "completed":
			if view.terminal != nil {
				return modelCallJournalView{}, agentRecoveryError(
					"completed Agent model call follows a terminal model error",
				)
			}
			view.completed = append(view.completed, call)
		case "failed", "cancelled":
			if index != len(modelCalls)-1 || view.terminal != nil ||
				call.Error == nil || len(call.Result) != 0 ||
				call.ResultDigest != "" {
				return modelCallJournalView{}, agentRecoveryError(
					"agent model call terminal error evidence is inconsistent",
				)
			}
			if err := call.Error.Validate(); err != nil {
				return modelCallJournalView{}, agentRecoveryError(
					"agent model call terminal error is invalid: " + err.Error(),
				)
			}
			current := call
			view.terminal = &current
		default:
			return modelCallJournalView{}, agentRecoveryError(
				fmt.Sprintf(
					"agent model call sequence=%d state=%s cannot be replayed safely",
					call.Sequence, call.State,
				),
			)
		}
	}
	if len(view.completed) != state.Round {
		return modelCallJournalView{}, agentRecoveryError(
			"agent checkpoint round is not covered by completed model calls",
		)
	}
	if view.terminal == nil {
		if len(modelCalls) != state.Round {
			return modelCallJournalView{}, agentRecoveryError(
				"agent model call journal is ahead of its checkpoint",
			)
		}
		return view, nil
	}
	if len(modelCalls) != state.Round+1 ||
		state.TerminalOutcome == nil ||
		state.TerminalOutcome.Error == nil ||
		!reflect.DeepEqual(
			state.TerminalOutcome.Error, view.terminal.Error,
		) {
		return modelCallJournalView{}, agentRecoveryError(
			"terminal Agent model call does not match checkpoint outcome",
		)
	}
	switch view.terminal.State {
	case "failed":
		if state.TerminalOutcome.State != agent.StateFailed ||
			state.TerminalOutcome.StopReason != "model_failed" ||
			view.terminal.Error.Code == contract.ErrorCancelled ||
			view.terminal.Error.Code == contract.ErrorTimeout {
			return modelCallJournalView{}, agentRecoveryError(
				"failed Agent model call does not match failed checkpoint evidence",
			)
		}
	case "cancelled":
		if state.TerminalOutcome.State != agent.StateCancelled ||
			state.TerminalOutcome.StopReason != "cancelled" ||
			(view.terminal.Error.Code != contract.ErrorCancelled &&
				view.terminal.Error.Code != contract.ErrorTimeout) {
			return modelCallJournalView{}, agentRecoveryError(
				"cancelled Agent model call does not match cancelled checkpoint evidence",
			)
		}
	}
	return view, nil
}

func validateModelCallJournal(
	modelCalls []ModelCall,
	state agent.LoopState,
) *contract.RuntimeError {
	view, runtimeErr := partitionModelCallJournal(modelCalls, state)
	if runtimeErr != nil {
		return runtimeErr
	}
	assistantMessages := make(
		[]contract.Message, 0, len(state.Messages),
	)
	for _, message := range state.Messages {
		if message.Role == contract.RoleAssistant {
			assistantMessages = append(assistantMessages, message)
		}
	}
	if len(assistantMessages) < len(view.completed) {
		return agentRecoveryError(
			"agent checkpoint has fewer assistant messages than model calls",
		)
	}
	assistantMessages = assistantMessages[len(assistantMessages)-len(view.completed):]
	seenCallIDs := make(map[string]struct{})
	expectedSeen := make([]string, 0, state.ToolCallCount)
	for index, call := range view.completed {
		result, runtimeErr := decodeDurableModelResult(call)
		if runtimeErr != nil {
			return runtimeErr
		}
		if !reflect.DeepEqual(
			assistantMessages[index], result.Message,
		) {
			return agentRecoveryError(
				"agent model call result does not match checkpoint messages",
			)
		}
		if index < len(view.completed)-1 &&
			result.FinishReason != contract.FinishToolCall {
			return agentRecoveryError(
				"non-final Agent model call did not request a tool",
			)
		}
		seenLimit := len(result.Message.ToolCalls)
		if index == len(view.completed)-1 {
			switch {
			case len(state.PendingToolCalls) > 0:
				seenLimit = state.PendingToolCursor
				if state.PendingEffectCheckpointID != "" {
					seenLimit++
				}
			case state.TerminalOutcome != nil &&
				state.TerminalOutcome.State == agent.StatePaused &&
				state.Pause != nil:
				seenLimit = 0
				for callIndex, toolCall := range result.Message.ToolCalls {
					if toolCall.ID == state.Pause.ToolCallID {
						seenLimit = callIndex + 1
						break
					}
				}
				if seenLimit == 0 {
					return agentRecoveryError(
						"paused Agent call is absent from the model result",
					)
				}
			}
		}
		if seenLimit > len(result.Message.ToolCalls) {
			return agentRecoveryError(
				"agent checkpoint pending cursor exceeds model tool calls",
			)
		}
		for callIndex, toolCall := range result.Message.ToolCalls {
			if _, exists := seenCallIDs[toolCall.ID]; exists {
				isCurrentDuplicate :=
					state.TerminalOutcome != nil &&
						state.TerminalOutcome.State == agent.StateFailed &&
						state.TerminalOutcome.StopReason ==
							"duplicate_tool_call" &&
						index == len(view.completed)-1 &&
						len(state.PendingToolCalls) > 0 &&
						callIndex == state.PendingToolCursor &&
						state.PendingToolCalls[state.PendingToolCursor].ID ==
							toolCall.ID
				if isCurrentDuplicate {
					continue
				}
				return agentRecoveryError(
					"agent model call journal contains duplicate tool call IDs",
				)
			}
			seenCallIDs[toolCall.ID] = struct{}{}
			if callIndex < seenLimit {
				expectedSeen = append(expectedSeen, toolCall.ID)
			}
		}
	}
	if !equalStringSlice(expectedSeen, state.SeenToolCallIDs) ||
		len(expectedSeen) != state.ToolCallCount {
		return agentRecoveryError(
			"agent checkpoint seen tool calls do not match model results",
		)
	}
	return nil
}

func validateModelRequestJournal(
	record Record,
	state agent.LoopState,
	modelCalls []ModelCall,
) *contract.RuntimeError {
	view, runtimeErr := partitionModelCallJournal(modelCalls, state)
	if runtimeErr != nil {
		return runtimeErr
	}
	assistantPositions := make([]int, 0, len(view.completed))
	for index := state.BaseMessageCount; index < len(state.Messages); index++ {
		if state.Messages[index].Role == contract.RoleAssistant {
			assistantPositions = append(assistantPositions, index)
		}
	}
	if len(assistantPositions) != len(view.completed) {
		return agentRecoveryError(
			"Agent message journal does not have one exact assistant boundary per model call",
		)
	}
	var frozenTools []contract.ToolSpec
	for index, call := range modelCalls {
		if !validSHA256Digest(call.RequestDigest) {
			return agentRecoveryError(
				"agent model call request_digest is not a canonical sha256 digest",
			)
		}
		var frozen contract.GenerateRequest
		if err := strictjson.DecodeObject(
			bytes.NewReader(call.Request), 4<<20, &frozen,
		); err != nil {
			return agentRecoveryError(
				"decode frozen agent model request: " + err.Error(),
			)
		}
		if err := frozen.Validate(); err != nil {
			return agentRecoveryError(
				"frozen agent model request is invalid: " + err.Error(),
			)
		}
		var expectedMessages []contract.Message
		if index < len(view.completed) {
			result, runtimeErr := decodeDurableModelResult(call)
			if runtimeErr != nil {
				return runtimeErr
			}
			assistantIndex := assistantPositions[index]
			if !reflect.DeepEqual(
				state.Messages[assistantIndex], result.Message,
			) {
				return agentRecoveryError(
					"agent model call result does not match its exact message boundary",
				)
			}
			expectedMessages = state.Messages[:assistantIndex]
		} else {
			if view.terminal == nil {
				return agentRecoveryError(
					"Agent model request has no matching result or terminal error",
				)
			}
			expectedMessages = state.Messages
		}
		if index == 0 {
			frozenTools = append(
				[]contract.ToolSpec(nil), frozen.Input.Tools...,
			)
		} else if !reflect.DeepEqual(frozenTools, frozen.Input.Tools) {
			return agentRecoveryError(
				"Agent model request tool definitions changed within one run",
			)
		}
		expected := contract.GenerateRequest{
			ModelProfile: state.ModelProfile,
			Input: contract.ModelRequest{
				Messages: append(
					[]contract.Message(nil),
					expectedMessages...,
				),
				Tools: append(
					[]contract.ToolSpec(nil), frozenTools...,
				),
				Trace: contract.TraceContext{Labels: map[string]string{
					"run_id": record.ID,
				}},
			},
		}
		if !reflect.DeepEqual(frozen, expected) {
			return agentRecoveryError(
				fmt.Sprintf(
					"frozen agent model request sequence=%d does not match its exact profile/message/trace boundary",
					call.Sequence,
				),
			)
		}
		requestJSON, err := json.Marshal(frozen)
		if err != nil {
			return agentRecoveryError(
				"encode reconstructed agent model request: " + err.Error(),
			)
		}
		sum := sha256.Sum256(requestJSON)
		expectedDigest := "sha256:" + hex.EncodeToString(sum[:])
		if call.RequestDigest != expectedDigest {
			return agentRecoveryError(
				fmt.Sprintf(
					"agent model call sequence=%d request digest does not match reconstructed request",
					call.Sequence,
				),
			)
		}
	}
	return nil
}

func validateRecoveredBudget(
	configured agent.Budget,
	state agent.LoopState,
	modelCalls []ModelCall,
) *contract.RuntimeError {
	view, runtimeErr := partitionModelCallJournal(modelCalls, state)
	if runtimeErr != nil {
		return runtimeErr
	}
	budget := configured.Effective()
	if err := budget.Validate(); err != nil {
		return agentRecoveryError(
			"durable Agent budget is invalid: " + err.Error(),
		)
	}
	var total int64
	invalidUsage := false
	for index, call := range view.completed {
		result, runtimeErr := decodeDurableModelResult(call)
		if runtimeErr != nil {
			return runtimeErr
		}
		current, err := modelUsageTotal(result.Usage)
		if err != nil {
			if index != len(view.completed)-1 ||
				state.TerminalOutcome == nil ||
				state.TerminalOutcome.State != agent.StateFailed ||
				state.TerminalOutcome.StopReason != "invalid_model_usage" {
				return agentRecoveryError(err.Error())
			}
			invalidUsage = true
			break
		}
		if current > math.MaxInt64-total {
			if index != len(view.completed)-1 ||
				state.TerminalOutcome == nil ||
				state.TerminalOutcome.State != agent.StateFailed ||
				state.TerminalOutcome.StopReason != "invalid_model_usage" {
				return agentRecoveryError(
					"durable Agent model usage overflows total_tokens",
				)
			}
			invalidUsage = true
			break
		}
		total += current
	}
	if state.TotalTokens != total {
		return agentRecoveryError(
			"agent checkpoint total_tokens does not match durable model usage",
		)
	}
	if invalidUsage {
		if state.TotalTokens != total {
			return agentRecoveryError(
				"invalid model usage checkpoint changed total_tokens",
			)
		}
	} else if budget.MaxTotalTokens > 0 && total > budget.MaxTotalTokens {
		if state.TerminalOutcome == nil ||
			state.TerminalOutcome.State != agent.StateFailed ||
			state.TerminalOutcome.StopReason != "token_budget" {
			return agentRecoveryError(
				"durable Agent token usage exceeds its configured budget",
			)
		}
	} else if state.TerminalOutcome != nil &&
		state.TerminalOutcome.StopReason == "token_budget" {
		return agentRecoveryError(
			"token-budget Agent failure is not backed by excess durable usage",
		)
	}
	if state.Round > budget.MaxRounds ||
		state.ToolCallCount > budget.MaxToolCalls {
		return agentRecoveryError(
			"durable Agent counters exceed their configured budget",
		)
	}
	return nil
}

func modelUsageTotal(usage contract.Usage) (int64, error) {
	if usage.TotalTokens != nil {
		return *usage.TotalTokens, nil
	}
	var total int64
	if usage.InputTokens != nil {
		total += *usage.InputTokens
	}
	if usage.OutputTokens != nil {
		if *usage.OutputTokens > math.MaxInt64-total {
			return 0, fmt.Errorf(
				"durable Agent model usage overflows total_tokens",
			)
		}
		total += *usage.OutputTokens
	}
	return total, nil
}

func validSHA256Digest(value string) bool {
	if len(value) != len("sha256:")+sha256.Size*2 ||
		value[:len("sha256:")] != "sha256:" {
		return false
	}
	for _, current := range value[len("sha256:"):] {
		if current < '0' || current > '9' &&
			(current < 'a' || current > 'f') {
			return false
		}
	}
	return true
}

func equalStringSlice(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func decodeDurableModelResult(
	modelCall ModelCall,
) (contract.ModelResult, *contract.RuntimeError) {
	if len(modelCall.Result) == 0 || modelCall.ResultDigest == "" {
		return contract.ModelResult{}, agentRecoveryError(
			"completed agent model call has no durable result evidence",
		)
	}
	var result contract.ModelResult
	if err := strictjson.DecodeObject(
		bytes.NewReader(modelCall.Result), 4<<20, &result,
	); err != nil {
		return contract.ModelResult{}, agentRecoveryError(
			"decode durable agent model result: " + err.Error(),
		)
	}
	if err := result.Validate(); err != nil {
		return contract.ModelResult{}, agentRecoveryError(
			"durable agent model result is invalid: " + err.Error(),
		)
	}
	if result.Provider.RequestID != modelCall.ProviderRequestID {
		return contract.ModelResult{}, agentRecoveryError(
			"durable agent model result provider request ID does not match",
		)
	}
	return result, nil
}

func validateRecoveredLoopState(
	state agent.LoopState,
) *contract.RuntimeError {
	if len(state.Messages) == 0 {
		return agentRecoveryError("agent checkpoint messages are required")
	}
	if state.BaseMessageCount <= 0 ||
		state.BaseMessageCount > len(state.Messages) {
		return agentRecoveryError(
			"agent checkpoint base message boundary is invalid",
		)
	}
	seen := make(map[string]struct{}, len(state.SeenToolCallIDs))
	for _, callID := range state.SeenToolCallIDs {
		if callID == "" {
			return agentRecoveryError(
				"agent checkpoint contains an empty seen tool call ID",
			)
		}
		if _, exists := seen[callID]; exists {
			return agentRecoveryError(
				"agent checkpoint contains duplicate seen tool call IDs",
			)
		}
		seen[callID] = struct{}{}
	}
	if state.ToolCallCount != len(state.SeenToolCallIDs) {
		return agentRecoveryError(
			"agent checkpoint tool call count does not match seen IDs",
		)
	}
	if state.TerminalOutcome != nil {
		outcome := state.TerminalOutcome
		if outcome.StopReason == "" {
			return agentRecoveryError(
				"agent checkpoint terminal outcome has no stop reason",
			)
		}
		switch outcome.State {
		case agent.StateCompleted:
			if outcome.Message == nil || outcome.Pause != nil ||
				outcome.Error != nil || state.Pause != nil {
				return agentRecoveryError(
					"completed Agent outcome requires only a message",
				)
			}
		case agent.StatePaused:
			if outcome.Pause == nil || outcome.Message != nil ||
				outcome.Error != nil || state.Pause == nil ||
				!reflect.DeepEqual(*state.Pause, *outcome.Pause) {
				return agentRecoveryError(
					"paused Agent outcome does not match its durable pause",
				)
			}
		case agent.StateFailed, agent.StateCancelled:
			if outcome.Message != nil || outcome.Pause != nil ||
				outcome.Error == nil || state.Pause != nil {
				return agentRecoveryError(
					"failed or cancelled Agent outcome requires only an error",
				)
			}
			if err := outcome.Error.Validate(); err != nil {
				return agentRecoveryError(
					"Agent terminal error is invalid: " + err.Error(),
				)
			}
			if outcome.State == agent.StateCancelled &&
				outcome.Error.Code != contract.ErrorCancelled &&
				outcome.Error.Code != contract.ErrorTimeout {
				return agentRecoveryError(
					"cancelled Agent outcome requires cancelled or timeout error",
				)
			}
			if outcome.State == agent.StateFailed &&
				outcome.Error.Code == contract.ErrorCancelled {
				return agentRecoveryError(
					"failed Agent outcome contains a cancelled error",
				)
			}
		default:
			return agentRecoveryError(
				"agent checkpoint contains an unsupported terminal outcome",
			)
		}
	}
	if len(state.PendingToolCalls) > 0 {
		if state.PendingToolCursor < 0 ||
			state.PendingToolCursor >= len(state.PendingToolCalls) {
			return agentRecoveryError(
				"agent checkpoint pending tool state is inconsistent",
			)
		}
		for _, call := range state.PendingToolCalls {
			if err := call.Validate(); err != nil {
				return agentRecoveryError(
					"agent checkpoint contains an invalid pending tool call: " +
						err.Error(),
				)
			}
		}
		current := state.PendingToolCalls[state.PendingToolCursor]
		_, currentSeen := seen[current.ID]
		if state.TerminalOutcome == nil {
			if !currentSeen {
				return agentRecoveryError(
					"recovered pending tool call is absent from seen tool call IDs",
				)
			}
			if err := identity.Validate(
				state.PendingEffectCheckpointID, "checkpoint",
			); err != nil {
				return agentRecoveryError(
					"recovered pending tool call has no valid preparation checkpoint",
				)
			}
			return nil
		}
		outcome := state.TerminalOutcome
		switch outcome.State {
		case agent.StatePaused:
			if !currentSeen ||
				state.Pause == nil ||
				outcome.Pause == nil ||
				current.ID != state.Pause.ToolCallID ||
				!reflect.DeepEqual(outcome.Pause, state.Pause) {
				return agentRecoveryError(
					"paused Agent checkpoint does not match its pending tool cursor",
				)
			}
			if err := identity.Validate(
				state.PendingEffectCheckpointID, "checkpoint",
			); err != nil {
				return agentRecoveryError(
					"paused Agent checkpoint has no valid preparation checkpoint",
				)
			}
		case agent.StateFailed:
			switch outcome.StopReason {
			case "duplicate_tool_call":
				if !currentSeen ||
					state.PendingEffectCheckpointID != "" {
					return agentRecoveryError(
						"duplicate tool-call failure has invalid pre-effect evidence",
					)
				}
			case "unknown_tool", "tool_budget", "invalid_tool_arguments",
				"execution_snapshot_changed":
				if currentSeen ||
					state.PendingEffectCheckpointID != "" {
					if outcome.StopReason !=
						"execution_snapshot_changed" ||
						!currentSeen {
						return agentRecoveryError(
							"pre-effect Agent failure has unexpected effect evidence",
						)
					}
					if err := identity.Validate(
						state.PendingEffectCheckpointID, "checkpoint",
					); err != nil {
						return agentRecoveryError(
							"execution snapshot failure has invalid preparation evidence",
						)
					}
				}
				if outcome.StopReason == "execution_snapshot_changed" &&
					!isExecutionSnapshotChangedError(outcome.Error) {
					return agentRecoveryError(
						"execution snapshot failure has an invalid error",
					)
				}
			case "tool_failed":
				if !currentSeen {
					return agentRecoveryError(
						"known tool failure is absent from seen tool calls",
					)
				}
				if err := identity.Validate(
					state.PendingEffectCheckpointID, "checkpoint",
				); err != nil {
					return agentRecoveryError(
						"known tool failure has no valid preparation checkpoint",
					)
				}
			default:
				return agentRecoveryError(
					"pending failed Agent checkpoint has an unsupported stop reason",
				)
			}
		case agent.StateCancelled:
			if currentSeen || state.PendingEffectCheckpointID != "" ||
				outcome.StopReason != "cancelled" {
				return agentRecoveryError(
					"pending cancelled Agent checkpoint has unexpected effect evidence",
				)
			}
		default:
			return agentRecoveryError(
				"pending Agent checkpoint has an unsupported terminal outcome",
			)
		}
		return nil
	}
	if state.PendingToolCursor != 0 ||
		state.PendingEffectCheckpointID != "" ||
		state.TerminalOutcome == nil {
		return agentRecoveryError(
			"agent checkpoint does not prove a pending or terminal model outcome",
		)
	}
	switch state.TerminalOutcome.State {
	case agent.StateCompleted:
		if state.TerminalOutcome.Message == nil ||
			len(state.Messages) == 0 ||
			state.Messages[len(state.Messages)-1].Role !=
				contract.RoleAssistant ||
			!reflect.DeepEqual(
				state.Messages[len(state.Messages)-1],
				*state.TerminalOutcome.Message,
			) {
			return agentRecoveryError(
				"completed Agent outcome does not match the last assistant message",
			)
		}
	case agent.StatePaused:
		return agentRecoveryError(
			"paused Agent outcome requires a pending tool call",
		)
	case agent.StateFailed, agent.StateCancelled:
	}
	return nil
}

func validateModelResultEvidence(
	modelCall ModelCall,
	state agent.LoopState,
) *contract.RuntimeError {
	if modelCall.State == "failed" || modelCall.State == "cancelled" {
		if len(modelCall.Result) != 0 ||
			modelCall.ResultDigest != "" ||
			modelCall.Error == nil ||
			state.TerminalOutcome == nil ||
			state.TerminalOutcome.Error == nil ||
			!reflect.DeepEqual(
				modelCall.Error, state.TerminalOutcome.Error,
			) {
			return agentRecoveryError(
				"terminal Agent model error does not match checkpoint evidence",
			)
		}
		if modelCall.State == "failed" &&
			(state.TerminalOutcome.State != agent.StateFailed ||
				state.TerminalOutcome.StopReason != "model_failed") {
			return agentRecoveryError(
				"failed Agent model call does not match checkpoint state",
			)
		}
		if modelCall.State == "cancelled" &&
			(state.TerminalOutcome.State != agent.StateCancelled ||
				state.TerminalOutcome.StopReason != "cancelled") {
			return agentRecoveryError(
				"cancelled Agent model call does not match checkpoint state",
			)
		}
		return nil
	}
	if modelCall.State != "completed" {
		return agentRecoveryError(
			"latest Agent model call has no proven terminal result",
		)
	}
	if len(modelCall.Result) == 0 || modelCall.ResultDigest == "" {
		return agentRecoveryError(
			"completed agent model call has no durable result evidence",
		)
	}
	var result contract.ModelResult
	if err := strictjson.DecodeObject(
		bytes.NewReader(modelCall.Result), 4<<20, &result,
	); err != nil {
		return agentRecoveryError(
			"decode durable agent model result: " + err.Error(),
		)
	}
	if err := result.Validate(); err != nil {
		return agentRecoveryError(
			"durable agent model result is invalid: " + err.Error(),
		)
	}
	if result.Provider.RequestID != modelCall.ProviderRequestID {
		return agentRecoveryError(
			"durable agent model result provider request ID does not match",
		)
	}
	assistantIndex := -1
	for index := len(state.Messages) - 1; index >= 0; index-- {
		if state.Messages[index].Role == contract.RoleAssistant {
			assistantIndex = index
			break
		}
	}
	if assistantIndex < 0 ||
		!reflect.DeepEqual(state.Messages[assistantIndex], result.Message) {
		return agentRecoveryError(
			"durable agent model result does not match checkpoint messages",
		)
	}
	if len(state.PendingToolCalls) > 0 {
		if result.FinishReason != contract.FinishToolCall ||
			!reflect.DeepEqual(
				state.PendingToolCalls, result.Message.ToolCalls,
			) {
			return agentRecoveryError(
				"pending tool checkpoint does not match durable model result",
			)
		}
		return nil
	}
	switch state.TerminalOutcome.State {
	case agent.StateCompleted:
		if result.FinishReason == contract.FinishToolCall ||
			state.TerminalOutcome.StopReason != string(result.FinishReason) ||
			!reflect.DeepEqual(
				state.TerminalOutcome.Message, &result.Message,
			) {
			return agentRecoveryError(
				"completed Agent outcome does not match durable model result",
			)
		}
	case agent.StatePaused:
		if result.FinishReason != contract.FinishToolCall ||
			state.Pause == nil {
			return agentRecoveryError(
				"paused Agent outcome is not backed by a tool-call model result",
			)
		}
		found := false
		for _, call := range result.Message.ToolCalls {
			if call.ID == state.Pause.ToolCallID {
				found = true
				break
			}
		}
		if !found {
			return agentRecoveryError(
				"paused Agent outcome does not match a durable tool call",
			)
		}
	case agent.StateCancelled:
		if result.FinishReason == contract.FinishCancelled {
			expected := &contract.RuntimeError{
				Code: contract.ErrorCancelled, Phase: contract.PhaseRun,
				Message: "model generation was cancelled",
			}
			if state.TerminalOutcome.StopReason !=
				string(contract.FinishCancelled) ||
				!reflect.DeepEqual(
					state.TerminalOutcome.Error, expected,
				) {
				return agentRecoveryError(
					"FinishCancelled model result does not match canonical Agent cancellation",
				)
			}
		}
	case agent.StateFailed:
	}
	return nil
}

func (executor *AgentExecutor) recoverPendingToolEvidence(
	ctx context.Context,
	record Record,
	state *agent.LoopState,
) *contract.RuntimeError {
	checkpointID := state.PendingEffectCheckpointID
	call := state.PendingToolCalls[state.PendingToolCursor]
	effect, effectExists, err := (&durableEffects{
		store: executor.Store,
	}).Lookup(ctx, record.ID, call.ID)
	if err != nil {
		return agentRecoveryError(
			"load durable Agent tool effect: " + err.Error(),
		)
	}
	if !effectExists {
		return agentRecoveryError(
			"recovered pending tool call has no durable effect",
		)
	}
	request := effect.Request
	if request.RunID != record.ID ||
		request.CallID != call.ID ||
		request.Name != call.Name ||
		!bytes.Equal(request.Arguments, call.Arguments) ||
		request.CheckpointID != checkpointID ||
		request.IdempotencyKey != agentToolIdempotencyKey(record.ID, call) {
		return agentRecoveryError(
			"durable Agent tool effect does not match its preparation checkpoint",
		)
	}
	position := agentToolCallPosition{
		call: call, round: state.Round, callIndex: state.PendingToolCursor,
		roundCalls: state.PendingToolCalls,
	}
	original, preparedState, runtimeErr :=
		executor.validateToolPreparationCheckpoint(
			ctx, record, *state, position, request,
		)
	if runtimeErr != nil {
		return runtimeErr
	}
	var checkpointEvents, startedEvents, completedEvents int
	var failedEvents, pausedEvents int
	var checkpointSequence, startedSequence, terminalSequence uint64
	var afterSequence uint64
	for {
		events, err := executor.Store.Events(
			ctx, record.ID, afterSequence, 1000,
		)
		if err != nil {
			return agentRecoveryError(
				"load durable agent recovery events: " + err.Error(),
			)
		}
		for _, event := range events {
			if event.Tool != nil {
				if runtimeErr := executor.validateDurableToolEvent(
					ctx, record.ID, event,
				); runtimeErr != nil {
					return runtimeErr
				}
			}
			switch event.Type {
			case contract.EventCheckpointCommitted:
				if event.Checkpoint != nil &&
					event.Checkpoint.CheckpointID == checkpointID {
					checkpointEvents++
					checkpointSequence = event.Sequence
				}
			case contract.EventToolStarted:
				if event.Tool != nil && event.Tool.CallID == call.ID {
					startedEvents++
					startedSequence = event.Sequence
				}
			case contract.EventToolCompleted:
				if event.Tool != nil && event.Tool.CallID == call.ID {
					completedEvents++
					terminalSequence = event.Sequence
				}
			case contract.EventToolFailed:
				if event.Tool != nil && event.Tool.CallID == call.ID {
					failedEvents++
					terminalSequence = event.Sequence
				}
			case contract.EventAgentPaused:
				if event.Agent != nil &&
					event.Agent.RunID == record.ID &&
					event.Sequence > original.Sequence &&
					effect.Result != nil &&
					effect.Result.Pause != nil &&
					event.Agent.PauseID == effect.Result.Pause.ID &&
					effect.Result.Pause.ToolCallID == call.ID {
					pausedEvents++
					terminalSequence = event.Sequence
				} else if event.Agent != nil &&
					event.Agent.RunID == record.ID &&
					event.Sequence > original.Sequence {
					return agentRecoveryError(
						"agent.paused event does not match durable tool pause",
					)
				}
			}
		}
		if len(events) < 1000 {
			break
		}
		afterSequence = events[len(events)-1].Sequence
	}
	preStartSnapshotFailure :=
		effect.State == "failed" &&
			isExecutionSnapshotChangedError(effect.Error) &&
			startedEvents == 0
	if checkpointEvents > 1 || startedEvents > 1 ||
		completedEvents > 1 || failedEvents > 1 || pausedEvents > 1 ||
		completedEvents+failedEvents+pausedEvents > 1 ||
		checkpointEvents == 1 &&
			checkpointSequence != original.Sequence+1 ||
		startedEvents > 0 &&
			(checkpointEvents != 1 ||
				checkpointSequence >= startedSequence) ||
		terminalSequence > 0 &&
			(preStartSnapshotFailure &&
				(startedEvents != 0 ||
					checkpointSequence >= terminalSequence) ||
				!preStartSnapshotFailure &&
					(startedEvents != 1 ||
						startedSequence >= terminalSequence)) {
		return agentRecoveryError(
			"durable Agent tool lifecycle events are inconsistent",
		)
	}
	switch effect.State {
	case "prepared":
		if effect.Result != nil || effect.Error != nil ||
			startedEvents != 0 || completedEvents != 0 ||
			failedEvents != 0 {
			return agentRecoveryError(
				"prepared Agent tool effect has terminal evidence",
			)
		}
	case "started":
		if effect.Result != nil || effect.Error != nil ||
			checkpointEvents != 1 ||
			completedEvents != 0 || failedEvents != 0 ||
			pausedEvents != 0 {
			return agentRecoveryError(
				"started Agent tool effect has inconsistent terminal evidence",
			)
		}
	case "completed":
		if effect.Result == nil || effect.Error != nil ||
			checkpointEvents != 1 || startedEvents != 1 ||
			failedEvents != 0 {
			return agentRecoveryError(
				"completed Agent tool effect has inconsistent evidence",
			)
		}
		if effect.Result.Pause == nil && pausedEvents != 0 ||
			effect.Result.Pause != nil && completedEvents != 0 {
			return agentRecoveryError(
				"completed Agent tool effect terminal event type is inconsistent",
			)
		}
	case "failed":
		if effect.Result != nil || effect.Error == nil ||
			checkpointEvents != 1 ||
			completedEvents != 0 || pausedEvents != 0 ||
			failedEvents > 1 ||
			state.TerminalOutcome != nil && failedEvents != 1 ||
			preStartSnapshotFailure && startedEvents != 0 ||
			!preStartSnapshotFailure && startedEvents != 1 {
			return agentRecoveryError(
				"failed Agent tool effect has inconsistent evidence",
			)
		}
		if state.TerminalOutcome != nil {
			wantStopReason := "tool_failed"
			if preStartSnapshotFailure {
				wantStopReason = "execution_snapshot_changed"
			}
			if state.TerminalOutcome.State != agent.StateFailed ||
				state.TerminalOutcome.StopReason != wantStopReason ||
				!reflect.DeepEqual(
					state.TerminalOutcome.Error, effect.Error,
				) ||
				failedEvents != 1 {
				return agentRecoveryError(
					"known terminal tool failure is not backed by one exact tool.failed event",
				)
			}
		}
	default:
		return agentRecoveryError(
			"durable Agent tool effect has an unsupported state",
		)
	}
	if runtimeErr := executor.validateRecoveredPendingMessages(
		ctx, record, preparedState, *state, effect,
	); runtimeErr != nil {
		return runtimeErr
	}
	state.PendingCheckpointID = checkpointID
	state.PendingCheckpointCommitted = checkpointEvents == 1
	state.PendingToolStarted = startedEvents == 1
	state.PendingToolTerminal =
		completedEvents == 1 || failedEvents == 1 ||
			pausedEvents == 1
	state.RecoveredFromCheckpoint = true
	return nil
}

func (executor *AgentExecutor) validateDurableToolEvent(
	ctx context.Context,
	runID string,
	event contract.Event,
) *contract.RuntimeError {
	effect, exists, err := (&durableEffects{
		store: executor.Store,
	}).Lookup(ctx, runID, event.Tool.CallID)
	if err != nil {
		return agentRecoveryError(
			"load tool effect for durable event: " + err.Error(),
		)
	}
	if !exists {
		return agentRecoveryError(
			"durable tool event has no matching effect",
		)
	}
	request := effect.Request
	if event.Tool.Name != request.Name ||
		event.Tool.IdempotencyKey != request.IdempotencyKey {
		return agentRecoveryError(
			"durable tool event metadata does not match its effect",
		)
	}
	switch event.Type {
	case contract.EventToolStarted:
		if effect.State == "prepared" {
			return agentRecoveryError(
				"tool.started event is not backed by a started effect",
			)
		}
	case contract.EventToolCompleted:
		if effect.State != "completed" || effect.Result == nil ||
			effect.Result.Pause != nil ||
			event.Tool.Content != effect.Result.Content ||
			event.Tool.IsError != effect.Result.IsError ||
			effect.Error != nil {
			return agentRecoveryError(
				"tool.completed event does not match durable result",
			)
		}
	case contract.EventToolFailed:
		if effect.State != "failed" || effect.Error == nil ||
			!reflect.DeepEqual(event.Error, effect.Error) ||
			effect.Result != nil {
			return agentRecoveryError(
				"tool.failed event does not match durable error",
			)
		}
	default:
		return agentRecoveryError(
			"unexpected durable tool event type",
		)
	}
	return nil
}

func (executor *AgentExecutor) validateRecoveredPendingMessages(
	ctx context.Context,
	record Record,
	preparedState agent.LoopState,
	state agent.LoopState,
	currentEffect agent.EffectRecord,
) *contract.RuntimeError {
	resumes, err := executor.Store.Resumes(ctx, record.ID)
	if err != nil {
		return agentRecoveryError(
			"load Agent resume journal: " + err.Error(),
		)
	}
	assistantIndex := -1
	for index := len(preparedState.Messages) - 1; index >= 0; index-- {
		if preparedState.Messages[index].Role == contract.RoleAssistant {
			assistantIndex = index
			break
		}
	}
	if assistantIndex < 0 ||
		!reflect.DeepEqual(
			preparedState.Messages[assistantIndex].ToolCalls,
			preparedState.PendingToolCalls,
		) {
		return agentRecoveryError(
			"tool preparation checkpoint has no matching assistant tool calls",
		)
	}
	expectedPrepared := append(
		[]contract.Message(nil),
		preparedState.Messages[:assistantIndex+1]...,
	)
	for index := 0; index < preparedState.PendingToolCursor; index++ {
		call := preparedState.PendingToolCalls[index]
		effect, exists, err := (&durableEffects{
			store: executor.Store,
		}).Lookup(ctx, record.ID, call.ID)
		if err != nil {
			return agentRecoveryError(
				"load prior durable tool result: " + err.Error(),
			)
		}
		if !exists || effect.State != "completed" ||
			effect.Result == nil || effect.Error != nil ||
			effect.Request.RunID != record.ID ||
			effect.Request.CallID != call.ID ||
			effect.Request.Name != call.Name ||
			!bytes.Equal(effect.Request.Arguments, call.Arguments) ||
			effect.Request.IdempotencyKey !=
				agentToolIdempotencyKey(record.ID, call) {
			return agentRecoveryError(
				"prior tool message has no matching durable result",
			)
		}
		message := contract.Message{
			Role: contract.RoleTool, ToolCallID: call.ID,
			Content: effect.Result.Content,
			IsError: effect.Result.IsError,
		}
		if effect.Result.Pause != nil {
			resumeMessage, runtimeErr := recoveredPauseMessage(
				call, *effect.Result.Pause, resumes,
			)
			if runtimeErr != nil {
				return runtimeErr
			}
			message = resumeMessage
		} else if effect.Result.Content == "" {
			return agentRecoveryError(
				"prior tool message has an empty durable result",
			)
		}
		expectedPrepared = append(
			expectedPrepared,
			message,
		)
	}
	if !reflect.DeepEqual(expectedPrepared, preparedState.Messages) {
		return agentRecoveryError(
			"tool preparation checkpoint contains an unproven message suffix",
		)
	}
	call := state.PendingToolCalls[state.PendingToolCursor]
	expectedPause := (*agent.Pause)(nil)
	expectedLatest := expectedPrepared
	if currentEffect.State == "completed" &&
		currentEffect.Result != nil {
		if currentEffect.Result.Pause != nil {
			pause := *currentEffect.Result.Pause
			pause.ToolCallID = call.ID
			expectedPause = &pause
		} else {
			expectedLatest = append(
				append([]contract.Message(nil), expectedPrepared...),
				contract.Message{
					Role: contract.RoleTool, ToolCallID: call.ID,
					Content: currentEffect.Result.Content,
					IsError: currentEffect.Result.IsError,
				},
			)
		}
	}
	messagesMatch := reflect.DeepEqual(state.Messages, expectedPrepared)
	if !messagesMatch &&
		!reflect.DeepEqual(state.Messages, expectedLatest) {
		return agentRecoveryError(
			"recovered tool checkpoint contains an unproven message suffix",
		)
	}
	if expectedPause == nil {
		if state.Pause != nil {
			return agentRecoveryError(
				"recovered tool checkpoint contains an unproven pause",
			)
		}
	} else if state.Pause != nil &&
		!reflect.DeepEqual(state.Pause, expectedPause) {
		return agentRecoveryError(
			"recovered tool checkpoint pause does not match durable result",
		)
	}
	return nil
}

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

func agentToolIdempotencyKey(
	runID string,
	call contract.ToolCall,
) string {
	hash := sha256.New()
	hash.Write([]byte(runID))
	hash.Write([]byte{0})
	hash.Write([]byte(call.ID))
	hash.Write([]byte{0})
	hash.Write([]byte(call.Name))
	hash.Write([]byte{0})
	hash.Write(call.Arguments)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func agentRecoveryError(message string) *contract.RuntimeError {
	return &contract.RuntimeError{
		Code: contract.ErrorConflict, Phase: contract.PhaseRun,
		Message: message,
	}
}

func cloneRunRuntimeError(
	value *contract.RuntimeError,
) *contract.RuntimeError {
	if value == nil {
		return nil
	}
	current := *value
	return &current
}

func checkpointMatchesLoopState(
	checkpoint Checkpoint,
	exists bool,
	state agent.LoopState,
) bool {
	if !exists ||
		checkpoint.RunID != state.RunID ||
		checkpoint.Sequence != state.NextEventSequence {
		return false
	}
	stateJSON, err := json.Marshal(state)
	return err == nil && bytes.Equal(checkpoint.State, stateJSON)
}

func encodeAgentExecutionResult(
	state agent.LoopState,
	outcome agent.Outcome,
	sessionResult *session.RunResult,
) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"outcome":        outcome,
		"session_result": sessionResult,
		"state": map[string]any{
			"round":           state.Round,
			"tool_call_count": state.ToolCallCount,
			"total_tokens":    state.TotalTokens,
		},
	})
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
	checkpoint := Checkpoint{
		ID: checkpointID, RunID: state.RunID,
		Sequence: state.NextEventSequence, State: stateJSON,
	}
	if err := executor.Store.SaveCheckpoint(ctx, checkpoint); err != nil {
		latest, exists, lookupErr := executor.Store.LatestCheckpoint(
			context.WithoutCancel(ctx), state.RunID,
		)
		if lookupErr == nil &&
			checkpointMatchesLoopState(latest, exists, state) {
			return nil
		}
		if lookupErr != nil {
			return fmt.Errorf(
				"%w; verify durable checkpoint: %v", err, lookupErr,
			)
		}
		return err
	}
	return nil
}

type durableEffects struct {
	store Store
}

func (effects *durableEffects) Lookup(
	ctx context.Context,
	runID, callID string,
) (agent.EffectRecord, bool, error) {
	effect, exists, err := effects.store.ToolEffect(ctx, runID, callID)
	if err != nil || !exists {
		return agent.EffectRecord{}, exists, err
	}
	var request agent.ToolRequest
	if err := strictjson.Decode(
		bytes.NewReader(effect.Request), 4<<20, &request,
	); err != nil {
		return agent.EffectRecord{}, false, fmt.Errorf(
			"decode durable tool request: %w", err,
		)
	}
	if request.RunID != effect.RunID ||
		request.CallID != effect.CallID ||
		request.IdempotencyKey != effect.IdempotencyKey ||
		request.Name != effect.Name {
		return agent.EffectRecord{}, false, fmt.Errorf(
			"durable tool request identity does not match indexed effect",
		)
	}
	record := agent.EffectRecord{
		State: effect.State, Request: request, Error: effect.Error,
	}
	if len(effect.Result) > 0 {
		var result agent.ToolResult
		if err := strictjson.Decode(
			bytes.NewReader(effect.Result), 4<<20, &result,
		); err != nil {
			return agent.EffectRecord{}, false, fmt.Errorf(
				"decode durable tool result: %w", err,
			)
		}
		record.Result = &result
	}
	return record, true, nil
}

func (effects *durableEffects) Prepared(
	ctx context.Context,
	request *agent.ToolRequest,
	state *agent.LoopState,
) (string, error) {
	checkpointID, err := identity.New("checkpoint")
	if err != nil {
		return "", err
	}
	request.CheckpointID = checkpointID
	state.PendingEffectCheckpointID = checkpointID
	state.PendingCheckpointID = checkpointID
	stateJSON, err := json.Marshal(*state)
	if err != nil {
		return "", err
	}
	checkpoint := Checkpoint{
		ID: checkpointID, RunID: request.RunID,
		Sequence: state.NextEventSequence, State: stateJSON,
	}
	requestJSON, err := json.Marshal(*request)
	if err != nil {
		return "", err
	}
	if err := effects.store.PrepareToolEffect(ctx, ToolEffect{
		RunID: request.RunID, CallID: request.CallID,
		IdempotencyKey: request.IdempotencyKey, Name: request.Name,
		State: "prepared", Request: requestJSON,
	}, checkpoint); err != nil {
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
	runID        string
	model        model.Generator
	store        Store
	beforeEffect agent.PreEffectGate

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
	if recorder.beforeEffect != nil {
		if runtimeErr := recorder.beforeEffect(ctx); runtimeErr != nil {
			return contract.ModelResult{}, runtimeErr
		}
	}
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
		Request:       requestJSON,
	}
	if err := recorder.store.StartModelCall(ctx, call); err != nil {
		durableContext := context.WithoutCancel(ctx)
		committed, lookupErr := recorder.modelCallMatches(
			durableContext, call, "running",
		)
		if !committed {
			retryErr := recorder.store.StartModelCall(
				durableContext, call,
			)
			if retryErr == nil {
				committed = true
			} else {
				committed, lookupErr = recorder.modelCallMatches(
					durableContext, call, "running",
				)
			}
			if !committed {
				message := "record model call start outcome is unknown: " +
					err.Error()
				if retryErr != nil {
					message += "; retry: " + retryErr.Error()
				}
				if lookupErr != nil {
					message += "; verify: " + lookupErr.Error()
				}
				return contract.ModelResult{}, runError(
					contract.ErrorConflict, message,
				)
			}
		}
	}
	result, runtimeErr := recorder.model.GenerateStream(ctx, request, sink)
	call.State = "completed"
	if runtimeErr != nil {
		call.State = "failed"
		if runtimeErr.Code == contract.ErrorCancelled ||
			runtimeErr.Code == contract.ErrorTimeout {
			call.State = "cancelled"
		}
		call.Error = runtimeErr
	} else {
		call.ProviderRequestID = result.Provider.RequestID
		resultJSON, err := json.Marshal(result)
		if err != nil {
			return contract.ModelResult{}, runError(
				contract.ErrorConflict,
				"model call is running but its durable result cannot be encoded: "+
					err.Error(),
			)
		}
		sum := sha256.Sum256(resultJSON)
		call.Result = resultJSON
		call.ResultDigest = "sha256:" + hex.EncodeToString(sum[:])
	}
	if err := recorder.store.FinishModelCall(
		context.WithoutCancel(ctx), call,
	); err != nil {
		durableContext := context.WithoutCancel(ctx)
		committed, lookupErr := recorder.modelCallMatches(
			durableContext, call, call.State,
		)
		if !committed {
			retryErr := recorder.store.FinishModelCall(
				durableContext, call,
			)
			if retryErr == nil {
				committed = true
			} else {
				committed, lookupErr = recorder.modelCallMatches(
					durableContext, call, call.State,
				)
			}
			if !committed {
				message := "record model call terminal outcome is unknown: " +
					err.Error()
				if retryErr != nil {
					message += "; retry: " + retryErr.Error()
				}
				if lookupErr != nil {
					message += "; verify: " + lookupErr.Error()
				}
				return contract.ModelResult{}, runError(
					contract.ErrorConflict, message,
				)
			}
		}
	}
	return result, runtimeErr
}

func (recorder *recordingModel) modelCallMatches(
	ctx context.Context,
	expected ModelCall,
	expectedState string,
) (bool, error) {
	current, exists, err := recorder.store.LatestModelCall(
		ctx, recorder.runID,
	)
	if err != nil || !exists {
		return false, err
	}
	if current.ID != expected.ID ||
		current.RunID != expected.RunID ||
		current.Sequence != expected.Sequence ||
		current.RequestDigest != expected.RequestDigest ||
		!bytes.Equal(current.Request, expected.Request) ||
		current.ProviderRequestID != expected.ProviderRequestID ||
		current.State != expectedState {
		return false, nil
	}
	switch expectedState {
	case "running":
		return len(current.Result) == 0 &&
			current.ResultDigest == "" &&
			current.Error == nil, nil
	case "completed":
		return bytes.Equal(current.Result, expected.Result) &&
			current.ResultDigest == expected.ResultDigest &&
			current.Error == nil, nil
	case "failed", "cancelled":
		return len(current.Result) == 0 &&
			current.ResultDigest == "" &&
			reflect.DeepEqual(current.Error, expected.Error), nil
	default:
		return false, nil
	}
}
