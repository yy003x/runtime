package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"
	"unicode/utf8"

	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/executionlog"
	"github.com/yy003x/runtime/internal/identity"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/profile"
)

// MaxInputBytes is the canonical upper bound shared by every Session ingress.
const MaxInputBytes = 1 << 20

type Service struct {
	store    *Store
	profiles *profile.Catalog
	models   model.Generator
	now      func() time.Time
	environ  []string
	logsDir  string
}

type ServiceOptions struct {
	Store    *Store
	Profiles *profile.Catalog
	Models   model.Generator
	Now      func() time.Time
	Environ  []string
	LogsDir  string
}

func NewService(options ServiceOptions) (*Service, error) {
	if options.Store == nil {
		return nil, fmt.Errorf("session store is required")
	}
	if options.Profiles == nil {
		return nil, fmt.Errorf("profile catalog is required")
	}
	if options.Models == nil {
		return nil, fmt.Errorf("model generator is required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	environ := options.Environ
	if environ == nil {
		environ = os.Environ()
	}
	service := &Service{
		store: options.Store, profiles: options.Profiles, models: options.Models,
		now: options.Now, environ: append([]string(nil), environ...),
		logsDir: options.LogsDir,
	}
	if err := service.cleanupInvocationManifests(); err != nil {
		return nil, fmt.Errorf("clean private invocation manifests: %w", err)
	}
	if err := service.reconcileStaleSessions(); err != nil {
		return nil, fmt.Errorf("reconcile Session store: %w", err)
	}
	return service, nil
}

func (service *Service) Run(
	ctx context.Context,
	request RunRequest,
) (RunResult, *contract.RuntimeError) {
	if service == nil || service.profiles == nil || service.models == nil {
		return RunResult{}, sessionRuntimeError(
			contract.ErrorInternal,
			"Session execution service is unavailable",
		)
	}
	entry, exists := service.profiles.Resolve(request.ProfileID)
	if !exists {
		return RunResult{}, sessionRuntimeError(
			contract.ErrorInvalidRequest,
			fmt.Sprintf("unknown profile %q", request.ProfileID),
		)
	}
	if err := validateRunRequest(request, entry); err != nil {
		return RunResult{}, sessionRuntimeError(contract.ErrorInvalidRequest, err.Error())
	}
	prepared, runtimeErr := service.prepareRunRequest(request, entry)
	if runtimeErr != nil {
		return RunResult{}, runtimeErr
	}
	request = prepared
	if request.RunID != "" && request.SessionID == "" {
		return RunResult{}, sessionRuntimeError(
			contract.ErrorInvalidRequest,
			"session_id is required when run_id is supplied",
		)
	}
	if request.RunID != "" {
		existing, found, runtimeErr := service.findExistingRun(
			request.SessionID, request.RunID, entry, request,
		)
		if runtimeErr != nil || found && existing.result.State != TurnRunning {
			return existing.result, runtimeErr
		}
		if found {
			if entry.Kind == profile.KindModel {
				return service.runModel(ctx, existing, request, entry)
			}
			return service.runCommand(ctx, existing, request, entry)
		}
	}
	ids, err := service.newExecutionIDs(request.SessionID, request.RunID)
	if err != nil {
		return RunResult{}, sessionRuntimeError(contract.ErrorInternal, err.Error())
	}
	started, runtimeErr := service.begin(ids, request, entry)
	if runtimeErr != nil {
		return started.result, runtimeErr
	}
	if entry.Kind == profile.KindModel {
		return service.runModel(ctx, started, request, entry)
	}
	return service.runCommand(ctx, started, request, entry)
}

type executionIDs struct {
	session   string
	turn      string
	run       string
	execution string
}

type startedExecution struct {
	ids        executionIDs
	projection projection
	result     RunResult
}

func (service *Service) newExecutionIDs(
	sessionID, runID string,
) (executionIDs, error) {
	var err error
	if sessionID == "" {
		sessionID, err = identity.New("session")
	} else {
		err = identity.Validate(sessionID, "session")
	}
	if err != nil {
		return executionIDs{}, err
	}
	turnID, err := identity.New("turn")
	if err != nil {
		return executionIDs{}, err
	}
	if runID == "" {
		runID, err = identity.New("run")
	} else {
		err = identity.Validate(runID, "run")
	}
	if err != nil {
		return executionIDs{}, err
	}
	executionID, err := identity.New("execution")
	if err != nil {
		return executionIDs{}, err
	}
	return executionIDs{
		session: sessionID, turn: turnID, run: runID, execution: executionID,
	}, nil
}

func NewID() (string, error) {
	return identity.New("session")
}

func (service *Service) findExistingRun(
	sessionID, runID string,
	entry profile.Entry,
	request RunRequest,
) (startedExecution, bool, *contract.RuntimeError) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return startedExecution{}, false, sessionRuntimeError(
			contract.ErrorInvalidRequest, err.Error(),
		)
	}
	if err := identity.Validate(runID, "run"); err != nil {
		return startedExecution{}, false, sessionRuntimeError(
			contract.ErrorInvalidRequest, err.Error(),
		)
	}
	var started startedExecution
	found := false
	var runtimeErr *contract.RuntimeError
	err := service.store.withLock(sessionID, func() error {
		entries, err := service.store.sessionDirectoryEntries(
			sessionID, "turns",
		)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, directory := range entries {
			if !directory.isDirectory() {
				continue
			}
			turn, err := service.store.loadTurn(sessionID, directory.name)
			if err != nil {
				return err
			}
			if turn.RunID != runID {
				continue
			}
			found = true
			if turn.ProfileID != entry.ID || turn.ProfileKind != entry.Kind {
				runtimeErr = sessionRuntimeError(
					contract.ErrorConflict,
					"run_id was already used with a different profile",
				)
				return nil
			}
			if turn.RequestDigest != requestDigest(request) ||
				turn.ConfigDigest != requestConfigDigest(request, entry) ||
				turn.BasePromptDigest != requestBasePromptDigest(request) ||
				turn.CWD != request.CWD {
				runtimeErr = sessionRuntimeError(
					contract.ErrorConflict,
					"run_id was already used with different effective request",
				)
				return nil
			}
			messages, err := service.store.messages(sessionID)
			if err != nil {
				return err
			}
			var assistant *contract.Message
			for _, message := range messages {
				if message.TurnID == turn.ID &&
					message.Message.Role == contract.RoleAssistant {
					current := message.Message
					assistant = &current
				}
			}
			started = startedExecution{
				ids: executionIDs{
					session: sessionID, turn: turn.ID, run: runID,
					execution: turn.ExecutionID,
				},
				result: service.resultFromTurn(turn, assistant),
			}
			execution, err := service.store.loadExecution(
				sessionID, turn.ExecutionID,
			)
			if err != nil {
				return err
			}
			started.result.ExitCode = execution.ExitCode
			if turn.State == TurnRunning {
				runtimeErr = sessionRuntimeError(
					contract.ErrorConflict,
					"run_id already has an active or unknown-outcome execution",
				)
			}
			return nil
		}
		return nil
	})
	if err != nil {
		return startedExecution{}, false, sessionRuntimeError(
			contract.ErrorInternal, err.Error(),
		)
	}
	return started, found, runtimeErr
}

func (service *Service) resultFromTurn(
	turn Turn,
	assistant *contract.Message,
) RunResult {
	result := RunResult{
		SessionID: turn.SessionID, TurnID: turn.ID, RunID: turn.RunID,
		ExecutionID: turn.ExecutionID, State: turn.State,
		CaptureQuality: turn.CaptureQuality, Error: turn.Error,
		PendingActions: append([]contract.ToolCall(nil), turn.PendingToolCalls...),
	}
	if assistant != nil &&
		!(turn.AgentOwned && turn.State != TurnCompleted) {
		current := cloneContractMessage(*assistant)
		result.Message = &current
	}
	return result
}

func cloneContractMessage(value contract.Message) contract.Message {
	value.ToolCalls = append([]contract.ToolCall(nil), value.ToolCalls...)
	for index := range value.ToolCalls {
		value.ToolCalls[index].Arguments = append(
			[]byte(nil), value.ToolCalls[index].Arguments...,
		)
	}
	return value
}

func (service *Service) begin(
	ids executionIDs,
	request RunRequest,
	entry profile.Entry,
) (startedExecution, *contract.RuntimeError) {
	result := RunResult{
		SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
		ExecutionID: ids.execution, State: TurnRunning,
	}
	var built projection
	var beginError *contract.RuntimeError
	err := service.store.withLock(ids.session, func() error {
		now := service.now().UTC()
		sessionValue, err := service.loadOrCreateSession(ids.session, request.Retention, now)
		if err != nil {
			return err
		}
		if sessionValue.State == SessionArchived {
			return fmt.Errorf("session %s is archived", ids.session)
		}
		if sessionValue.State == SessionActive {
			return fmt.Errorf("session %s already has active turn %s", ids.session, sessionValue.ActiveTurnID)
		}
		if sessionValue.State == SessionBlocked {
			return fmt.Errorf("session %s requires pending tool results", ids.session)
		}
		sessionValue.State = SessionActive
		sessionValue.ActiveTurnID = ids.turn
		sessionValue.LastProfileID = entry.ID
		sessionValue.LastProfileKind = entry.Kind
		sessionValue.UpdatedAt = now
		turn := Turn{
			SchemaVersion: SchemaVersion,
			ID:            ids.turn, SessionID: ids.session, RunID: ids.run,
			ExecutionID: ids.execution, TaskID: request.TaskID,
			ProfileID: entry.ID, ProfileKind: entry.Kind,
			RequestDigest:    requestDigest(request),
			ConfigDigest:     requestConfigDigest(request, entry),
			BasePromptDigest: requestBasePromptDigest(request),
			CWD:              request.CWD,
			State:            TurnRunning, CreatedAt: now, UpdatedAt: now,
			ToolResults: make(map[string]ToolResultReceipt),
		}
		ownerStartToken, err := processStartToken(os.Getpid())
		if err != nil {
			return fmt.Errorf("record Session owner identity: %w", err)
		}
		execution := Execution{
			SchemaVersion: SchemaVersion, ID: ids.execution,
			SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
			ProfileID: entry.ID, ProfileKind: entry.Kind,
			State:         ExecutionSpawnIntent,
			RequestDigest: turn.RequestDigest, ConfigDigest: turn.ConfigDigest,
			BasePromptDigest: turn.BasePromptDigest, CWD: turn.CWD,
			Process: &ProcessIdentity{
				OwnerPID: os.Getpid(), OwnerStartToken: ownerStartToken,
			},
			CreatedAt: now, UpdatedAt: now,
		}
		if err := service.store.writeSession(sessionValue); err != nil {
			return err
		}
		if err := service.store.writeTurn(turn); err != nil {
			return err
		}
		if err := service.store.writeExecution(execution); err != nil {
			return err
		}
		if err := service.store.appendMessage(&sessionValue, MessageRecord{
			Time: now, TurnID: ids.turn, RunID: ids.run,
			ExecutionID: ids.execution, ProfileID: entry.ID,
			Message: contract.Message{Role: contract.RoleUser, Content: request.Input},
		}); err != nil {
			return err
		}
		if err := service.store.appendEvent(&sessionValue, EventRecord{
			Time: now, Type: "turn.started", TurnID: ids.turn,
			RunID: ids.run, ExecutionID: ids.execution, State: string(TurnRunning),
		}); err != nil {
			return err
		}
		if err := service.store.writeSession(sessionValue); err != nil {
			return err
		}
		messages, err := service.store.messages(ids.session)
		if err != nil {
			return err
		}
		built, beginError = buildProjection(
			entry, ids.session, ids.turn, ids.run, ids.execution,
			request.TaskID, request, messages, now,
		)
		if beginError != nil {
			return service.failStarted(&sessionValue, &turn, beginError, now)
		}
		if err := service.store.writeManifest(built.manifest); err != nil {
			return err
		}
		return nil
	})
	if beginError != nil && err == nil {
		result.State = TurnFailed
		result.Error = beginError
		return startedExecution{ids: ids, result: result}, beginError
	}
	if err != nil {
		if beginError != nil {
			result.State = TurnFailed
			result.Error = beginError
			return startedExecution{ids: ids, result: result}, beginError
		}
		runtimeErr := sessionRuntimeError(contract.ErrorConflict, err.Error())
		result.State = TurnFailed
		result.Error = runtimeErr
		return startedExecution{ids: ids, result: result}, runtimeErr
	}
	if err := service.store.rebuildIndex(); err != nil {
		runtimeErr := sessionRuntimeError(
			contract.ErrorInternal,
			"Session turn was started but index rebuild failed: "+err.Error(),
		)
		result.Error = runtimeErr
		return startedExecution{
			ids: ids, projection: built, result: result,
		}, runtimeErr
	}
	return startedExecution{ids: ids, projection: built, result: result}, nil
}

func (service *Service) loadOrCreateSession(
	sessionID string,
	retention Retention,
	now time.Time,
) (Session, error) {
	value, err := service.store.loadSession(sessionID)
	if err == nil {
		return value, nil
	}
	if !errors.Is(err, os.ErrNotExist) {
		return Session{}, err
	}
	if retention == "" {
		retention = RetentionStandard
	}
	value = Session{
		SchemaVersion: SchemaVersion, ID: sessionID, State: SessionIdle,
		Retention: retention, CreatedAt: now, UpdatedAt: now,
	}
	if err := service.store.writeSession(value); err != nil {
		return Session{}, err
	}
	return value, nil
}

func (service *Service) failStarted(
	sessionValue *Session,
	turn *Turn,
	runtimeErr *contract.RuntimeError,
	now time.Time,
) error {
	execution, err := service.store.loadExecution(
		sessionValue.ID, turn.ExecutionID,
	)
	if err != nil {
		return err
	}
	turn.State = TurnFailed
	turn.Error = runtimeErr
	turn.UpdatedAt = now
	sessionValue.State = SessionIdle
	sessionValue.ActiveTurnID = ""
	sessionValue.UpdatedAt = now
	execution.State = ExecutionSettled
	execution.Outcome = OutcomeFailed
	execution.Error = runtimeErr
	execution.UpdatedAt = now
	if err := service.store.appendEvent(sessionValue, EventRecord{
		Time: now, Type: "turn.failed", TurnID: turn.ID,
		RunID: turn.RunID, ExecutionID: turn.ExecutionID,
		State: string(TurnFailed), Error: runtimeErr,
	}); err != nil {
		return err
	}
	if err := service.store.writeExecution(execution); err != nil {
		return err
	}
	if err := service.store.writeTurn(*turn); err != nil {
		return err
	}
	return service.store.writeSession(*sessionValue)
}

func (service *Service) runModel(
	ctx context.Context,
	started startedExecution,
	request RunRequest,
	entry profile.Entry,
) (RunResult, *contract.RuntimeError) {
	if err := service.markExecutionRunning(started.ids); err != nil {
		runtimeErr := sessionRuntimeError(contract.ErrorInternal, err.Error())
		return service.finishFailureResult(started.ids, runtimeErr)
	}
	generateRequest := contract.GenerateRequest{
		ModelProfile: entry.ID,
		Input: contract.ModelRequest{
			Messages: started.projection.modelMessages,
			Options:  request.ModelOptions,
			Trace: contract.TraceContext{Labels: boundedLabels(map[string]string{
				"session_id":   started.ids.session,
				"turn_id":      started.ids.turn,
				"run_id":       started.ids.run,
				"execution_id": started.ids.execution,
				"task_id":      request.TaskID,
			})},
		},
	}
	modelContext := model.WithAttemptOrigin(ctx, model.AttemptOrigin{
		Namespace: model.AttemptNamespaceSession,
		Source:    "session " + started.ids.session,
	})
	modelResult, runtimeErr := service.models.GenerateStream(
		modelContext, generateRequest,
		func(event contract.Event) error {
			return service.recordModelEvent(started.ids, event)
		},
	)
	if runtimeErr != nil {
		return service.finishFailureResult(started.ids, runtimeErr)
	}
	return service.finishModel(started.ids, modelResult)
}

func (service *Service) markExecutionRunning(ids executionIDs) error {
	return service.store.withLock(ids.session, func() error {
		execution, err := service.store.loadExecution(
			ids.session, ids.execution,
		)
		if err != nil {
			return err
		}
		execution.State = ExecutionRunning
		execution.UpdatedAt = service.now().UTC()
		return service.store.writeExecution(execution)
	})
}

func (service *Service) runCommand(
	ctx context.Context,
	started startedExecution,
	request RunRequest,
	_ profile.Entry,
) (RunResult, *contract.RuntimeError) {
	if request.Snapshot == nil {
		runtimeErr := sessionRuntimeError(
			contract.ErrorInternal, "CLI execution snapshot is missing",
		)
		return service.finishFailureResult(started.ids, runtimeErr)
	}
	prompt := joinPromptFragments(
		request.Snapshot.BasePrompt, started.projection.commandPrompt,
	)
	modelValue := request.Snapshot.Model
	effortValue := runtimecommand.Effort(request.Snapshot.Effort)
	cwdValue := request.Snapshot.CWD
	var modelOverride *string
	if modelValue != "" {
		modelOverride = &modelValue
	}
	var effortOverride *runtimecommand.Effort
	if effortValue != "" {
		effortOverride = &effortValue
	}
	invocation, err := runtimecommand.Build(runtimecommand.BuildRequest{
		Mode: runtimecommand.ModeExec, OutputProtocol: runtimecommand.OutputCanonical,
		Profile: request.Snapshot.Profile,
		Overrides: runtimecommand.Overrides{
			Model: modelOverride, Effort: effortOverride, CWD: &cwdValue,
		},
		ArgvPrompt: &prompt, InheritedEnvironment: service.environ,
		InvocationBase: request.Snapshot.CWD,
	})
	if err != nil {
		var runtimeErr *contract.RuntimeError
		if errors.As(err, &runtimeErr) {
			safe := *runtimeErr
			safe.Message = "build CLI canonical invocation failed"
			return service.finishFailureResult(started.ids, &safe)
		}
		runtimeErr = sessionRuntimeError(
			contract.ErrorInvalidRequest,
			"build CLI canonical invocation failed",
		)
		runtimeErr.Phase = contract.PhaseTransport
		return service.finishFailureResult(started.ids, runtimeErr)
	}
	var turn Turn
	if err := service.store.withLock(started.ids.session, func() error {
		var loadErr error
		_, turn, loadErr = service.loadActive(started.ids)
		return loadErr
	}); err != nil {
		runtimeErr := sessionRuntimeError(contract.ErrorInternal, err.Error())
		return service.finishFailureResult(started.ids, runtimeErr)
	}
	_ = executionlog.AppendCLI(service.logsDir, executionlog.CLIRecord{
		Time: service.now(), Namespace: executionlog.NamespaceSession, Profile: request.Snapshot.ProfileID,
		Source:  "session " + started.ids.session,
		Command: executionlog.FormatCommand(request.Snapshot.Profile.Env, invocation.CWD, invocation.Path, invocation.Argv),
	})
	executionResult := service.executeManagedCLI(
		ctx, started.ids, turn, invocation,
	)
	result, runtimeErr := service.finishManagedCommand(
		started.ids, executionResult,
	)
	if runtimeErr != nil {
		return result, runtimeErr
	}
	return result, nil
}

func joinPromptFragments(values ...string) string {
	var fragments []string
	for _, value := range values {
		if value != "" {
			fragments = append(fragments, value)
		}
	}
	return strings.Join(fragments, "\n")
}

func (service *Service) recordModelEvent(ids executionIDs, event contract.Event) error {
	detail, err := json.Marshal(event)
	if err != nil {
		return err
	}
	return service.store.withLock(ids.session, func() error {
		value, err := service.store.loadSession(ids.session)
		if err != nil {
			return err
		}
		now := service.now().UTC()
		if err := service.store.appendEvent(&value, EventRecord{
			Time: now, Type: string(event.Type), TurnID: ids.turn,
			RunID: ids.run, ExecutionID: ids.execution, Detail: detail,
		}); err != nil {
			return err
		}
		value.UpdatedAt = now
		return service.store.writeSession(value)
	})
}

func (service *Service) finishModel(
	ids executionIDs,
	modelResult contract.ModelResult,
) (RunResult, *contract.RuntimeError) {
	return service.finishModelResult(ids, modelResult, true)
}

func (service *Service) finishModelResult(
	ids executionIDs,
	modelResult contract.ModelResult,
	appendAssistant bool,
) (RunResult, *contract.RuntimeError) {
	result := RunResult{
		SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
		ExecutionID: ids.execution, State: TurnCompleted,
		CaptureQuality: CaptureStructured, Message: &modelResult.Message,
	}
	err := service.store.withLock(ids.session, func() error {
		now := service.now().UTC()
		sessionValue, turn, err := service.loadActive(ids)
		if err != nil {
			return err
		}
		if appendAssistant {
			if err := service.store.appendMessage(&sessionValue, MessageRecord{
				Time: now, TurnID: ids.turn, RunID: ids.run,
				ExecutionID: ids.execution, ProfileID: turn.ProfileID,
				Message: modelResult.Message,
			}); err != nil {
				return err
			}
		}
		if modelResult.FinishReason == contract.FinishToolCall {
			turn.State = TurnRequiresAction
			turn.PendingToolCalls = append(
				[]contract.ToolCall(nil), modelResult.Message.ToolCalls...,
			)
			sessionValue.State = SessionBlocked
			result.State = TurnRequiresAction
			result.PendingActions = append(
				[]contract.ToolCall(nil), modelResult.Message.ToolCalls...,
			)
		} else {
			turn.State = TurnCompleted
			sessionValue.State = SessionIdle
		}
		turn.CaptureQuality = CaptureStructured
		turn.UpdatedAt = now
		sessionValue.ActiveTurnID = ""
		sessionValue.UpdatedAt = now
		execution, err := service.store.loadExecution(
			ids.session, ids.execution,
		)
		if err != nil {
			return err
		}
		execution.State = ExecutionSettled
		execution.Outcome = OutcomeCompleted
		execution.Error = nil
		execution.UpdatedAt = now
		if err := service.store.appendEvent(&sessionValue, EventRecord{
			Time: now, Type: "turn.settled", TurnID: ids.turn,
			RunID: ids.run, ExecutionID: ids.execution, State: string(turn.State),
		}); err != nil {
			return err
		}
		if err := service.store.writeExecution(execution); err != nil {
			return err
		}
		if err := service.store.writeTurn(turn); err != nil {
			return err
		}
		return service.store.writeSession(sessionValue)
	})
	if err != nil {
		recovered := service.recoverFinalizationFailure(ids, err)
		return recovered.result, recovered.runtimeErr
	}
	if err := service.store.rebuildIndex(); err != nil {
		runtimeErr := sessionRuntimeError(
			contract.ErrorInternal,
			"Session result was committed but index rebuild failed: "+
				err.Error(),
		)
		result.Error = runtimeErr
		return result, runtimeErr
	}
	return result, nil
}

type finalizationRecovery struct {
	result     RunResult
	runtimeErr *contract.RuntimeError
}

func (service *Service) recoverFinalizationFailure(
	ids executionIDs,
	persistErr error,
) finalizationRecovery {
	runtimeErr := sessionRuntimeError(
		contract.ErrorInternal,
		"persist Session finalization; explicit reconciliation required: "+
			persistErr.Error(),
	)
	recovery := finalizationRecovery{
		result: RunResult{
			SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
			ExecutionID: ids.execution, State: TurnRunning, Error: runtimeErr,
		},
		runtimeErr: runtimeErr,
	}
	err := service.store.withLock(ids.session, func() error {
		sessionValue, err := service.store.loadSession(ids.session)
		if err != nil {
			return err
		}
		turn, err := service.store.loadTurn(ids.session, ids.turn)
		if err != nil {
			return err
		}
		execution, err := service.store.loadExecution(
			ids.session, ids.execution,
		)
		if err != nil {
			return err
		}
		if err := validateFinalizationCorrelation(
			ids, sessionValue, turn, execution,
		); err != nil {
			return err
		}
		if finalizationFactsCommitted(sessionValue, turn, execution) {
			assistant, err := service.lastAssistantForTurn(
				ids.session, ids.turn,
			)
			if err != nil {
				return err
			}
			recovery.result = service.resultFromTurn(turn, assistant)
			recovery.result.ExitCode = execution.ExitCode
			recovery.runtimeErr = turn.Error
			return nil
		}
		if !finalizationFactsRunning(sessionValue, turn, execution) {
			return fmt.Errorf(
				"Session finalization facts are neither terminal nor the " +
					"expected running execution",
			)
		}
		now := service.now().UTC()
		sessionValue.State = SessionBlocked
		sessionValue.UpdatedAt = now
		turn.Error = runtimeErr
		turn.UpdatedAt = now
		if err := service.store.writeTurn(turn); err != nil {
			return err
		}
		if err := service.store.writeSession(sessionValue); err != nil {
			return err
		}
		recovery.result = service.resultFromTurn(turn, nil)
		recovery.result.ExitCode = execution.ExitCode
		return nil
	})
	if err != nil {
		recovery.runtimeErr = sessionRuntimeError(
			contract.ErrorInternal,
			fmt.Sprintf(
				"%s; recovery failed: %v", runtimeErr.Message, err,
			),
		)
		recovery.result = RunResult{
			SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
			ExecutionID: ids.execution, State: TurnRunning,
			Error: recovery.runtimeErr,
		}
		return recovery
	}
	if err := service.store.rebuildIndex(); err != nil {
		recovery.runtimeErr = sessionRuntimeError(
			contract.ErrorInternal,
			"Session recovery was committed but index rebuild failed: "+
				err.Error(),
		)
		recovery.result.Error = recovery.runtimeErr
	}
	return recovery
}

func validateFinalizationCorrelation(
	ids executionIDs,
	sessionValue Session,
	turn Turn,
	execution Execution,
) error {
	if sessionValue.ID != ids.session ||
		turn.ID != ids.turn ||
		turn.SessionID != ids.session ||
		turn.RunID != ids.run ||
		turn.ExecutionID != ids.execution ||
		execution.ID != ids.execution ||
		execution.SessionID != ids.session ||
		execution.TurnID != ids.turn ||
		execution.RunID != ids.run ||
		execution.ProfileID != turn.ProfileID ||
		execution.ProfileKind != turn.ProfileKind ||
		execution.RequestDigest != turn.RequestDigest ||
		execution.ConfigDigest != turn.ConfigDigest ||
		execution.BasePromptDigest != turn.BasePromptDigest ||
		execution.CWD != turn.CWD {
		return fmt.Errorf("Session finalization correlation does not match")
	}
	return nil
}

func finalizationFactsCommitted(
	sessionValue Session,
	turn Turn,
	execution Execution,
) bool {
	if sessionValue.ActiveTurnID != "" ||
		execution.State != ExecutionSettled {
		return false
	}
	switch turn.State {
	case TurnCompleted:
		return sessionValue.State == SessionIdle &&
			execution.Outcome == OutcomeCompleted
	case TurnRequiresAction:
		return sessionValue.State == SessionBlocked &&
			execution.Outcome == OutcomeCompleted
	case TurnFailed:
		return sessionValue.State == SessionIdle &&
			(execution.Outcome == OutcomeFailed ||
				execution.Outcome == OutcomeUnknown)
	case TurnCancelled:
		return sessionValue.State == SessionIdle &&
			execution.Outcome == OutcomeCancelled
	default:
		return false
	}
}

func finalizationFactsRunning(
	sessionValue Session,
	turn Turn,
	execution Execution,
) bool {
	return sessionValue.ActiveTurnID == turn.ID &&
		(sessionValue.State == SessionActive ||
			sessionValue.State == SessionBlocked) &&
		turn.State == TurnRunning &&
		(execution.State == ExecutionSpawnIntent ||
			execution.State == ExecutionRunning)
}

func (service *Service) lastAssistantForTurn(
	sessionID, turnID string,
) (*contract.Message, error) {
	messages, err := service.store.messages(sessionID)
	if err != nil {
		return nil, err
	}
	var assistant *contract.Message
	for _, record := range messages {
		if record.TurnID != turnID ||
			record.Message.Role != contract.RoleAssistant {
			continue
		}
		current := cloneContractMessage(record.Message)
		assistant = &current
	}
	return assistant, nil
}

func (service *Service) finishManagedCommand(
	ids executionIDs,
	executionResult managedResult,
) (RunResult, *contract.RuntimeError) {
	turnState := TurnCompleted
	if executionResult.outcome == OutcomeCancelled {
		turnState = TurnCancelled
	} else if executionResult.runtimeErr != nil {
		turnState = TurnFailed
	}
	result := RunResult{
		SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
		ExecutionID: ids.execution, State: turnState,
		CaptureQuality: CaptureStructured,
		ExitCode:       executionResult.exitCode, Error: executionResult.runtimeErr,
	}
	if turnState == TurnCompleted {
		message := contract.Message{
			Role:    contract.RoleAssistant,
			Content: executionResult.assistant,
		}
		result.Message = &message
	}
	err := service.store.withLock(ids.session, func() error {
		now := service.now().UTC()
		sessionValue, turn, err := service.loadActive(ids)
		if err != nil {
			return err
		}
		if result.Message != nil {
			if err := service.store.appendMessage(&sessionValue, MessageRecord{
				Time: now, TurnID: ids.turn, RunID: ids.run,
				ExecutionID: ids.execution, ProfileID: turn.ProfileID,
				Message: *result.Message,
			}); err != nil {
				return err
			}
		}
		turn.State = turnState
		turn.Error = executionResult.runtimeErr
		turn.CaptureQuality = CaptureStructured
		turn.UpdatedAt = now
		sessionValue.State = SessionIdle
		sessionValue.ActiveTurnID = ""
		sessionValue.UpdatedAt = now
		execution, err := service.store.loadExecution(
			ids.session, ids.execution,
		)
		if err != nil {
			return err
		}
		execution.State = ExecutionSettled
		execution.Outcome = executionResult.outcome
		execution.RequestDigest = turn.RequestDigest
		execution.ConfigDigest = turn.ConfigDigest
		execution.BasePromptDigest = turn.BasePromptDigest
		execution.CWD = turn.CWD
		execution.ExitCode = executionResult.exitCode
		execution.Signal = executionResult.signal
		execution.Stdout = executionResult.stdout
		execution.Stderr = executionResult.stderr
		execution.Error = executionResult.runtimeErr
		execution.UpdatedAt = now
		eventType := "turn.settled"
		if turnState == TurnFailed {
			eventType = "turn.failed"
		} else if turnState == TurnCancelled {
			eventType = "turn.cancelled"
		}
		if err := service.store.appendEvent(&sessionValue, EventRecord{
			Time: now, Type: eventType, TurnID: ids.turn,
			RunID: ids.run, ExecutionID: ids.execution,
			State: string(turnState), Error: executionResult.runtimeErr,
		}); err != nil {
			return err
		}
		if err := service.store.writeExecution(execution); err != nil {
			return err
		}
		if err := service.store.writeTurn(turn); err != nil {
			return err
		}
		return service.store.writeSession(sessionValue)
	})
	if err != nil {
		recovered := service.recoverFinalizationFailure(ids, err)
		return recovered.result, recovered.runtimeErr
	}
	if err := service.store.rebuildIndex(); err != nil {
		runtimeErr := sessionRuntimeError(
			contract.ErrorInternal,
			"Session result was committed but index rebuild failed: "+
				err.Error(),
		)
		result.Error = runtimeErr
		return result, runtimeErr
	}
	return result, executionResult.runtimeErr
}

func (service *Service) finishFailure(
	ids executionIDs,
	runtimeErr *contract.RuntimeError,
) RunResult {
	result := RunResult{
		SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
		ExecutionID: ids.execution, State: TurnFailed,
		CaptureQuality: CaptureStructured, Error: runtimeErr,
	}
	err := service.store.withLock(ids.session, func() error {
		now := service.now().UTC()
		sessionValue, turn, err := service.loadActive(ids)
		if err != nil {
			return err
		}
		turn.State = TurnFailed
		turn.Error = runtimeErr
		turn.CaptureQuality = CaptureStructured
		turn.UpdatedAt = now
		sessionValue.State = SessionIdle
		sessionValue.ActiveTurnID = ""
		sessionValue.UpdatedAt = now
		execution, err := service.store.loadExecution(
			ids.session, ids.execution,
		)
		if err != nil {
			return err
		}
		execution.State = ExecutionSettled
		execution.Outcome = OutcomeFailed
		execution.Error = runtimeErr
		execution.UpdatedAt = now
		if err := service.store.appendEvent(&sessionValue, EventRecord{
			Time: now, Type: "turn.failed", TurnID: ids.turn,
			RunID: ids.run, ExecutionID: ids.execution,
			State: string(TurnFailed), Error: runtimeErr,
		}); err != nil {
			return err
		}
		if err := service.store.writeExecution(execution); err != nil {
			return err
		}
		if err := service.store.writeTurn(turn); err != nil {
			return err
		}
		return service.store.writeSession(sessionValue)
	})
	if err != nil {
		result = service.recoverFinalizationFailure(ids, err).result
	}
	if indexErr := service.store.rebuildIndex(); indexErr != nil {
		result.Error = sessionRuntimeError(
			contract.ErrorInternal,
			"Session failure was committed but index rebuild failed: "+
				indexErr.Error(),
		)
	}
	return result
}

func (service *Service) finishFailureResult(
	ids executionIDs,
	runtimeErr *contract.RuntimeError,
) (RunResult, *contract.RuntimeError) {
	result := service.finishFailure(ids, runtimeErr)
	if result.State == TurnRunning && result.Error != nil {
		return result, result.Error
	}
	return result, runtimeErr
}

func (service *Service) loadActive(ids executionIDs) (Session, Turn, error) {
	sessionValue, err := service.store.loadSession(ids.session)
	if err != nil {
		return Session{}, Turn{}, err
	}
	turn, err := service.store.loadTurn(ids.session, ids.turn)
	if err != nil {
		return Session{}, Turn{}, err
	}
	if sessionValue.ActiveTurnID != ids.turn || turn.ExecutionID != ids.execution ||
		turn.RunID != ids.run || turn.State != TurnRunning {
		return Session{}, Turn{}, fmt.Errorf("execution correlation does not match active turn")
	}
	return sessionValue, turn, nil
}

func validateRunRequest(request RunRequest, entry profile.Entry) error {
	if strings.TrimSpace(request.ProfileID) == "" {
		return fmt.Errorf("profile_id is required")
	}
	if strings.TrimSpace(request.Input) == "" {
		return fmt.Errorf("input is required")
	}
	if len(request.Input) > MaxInputBytes {
		return fmt.Errorf("input exceeds %d bytes", MaxInputBytes)
	}
	if !utf8.ValidString(request.Input) ||
		strings.ContainsRune(request.Input, '\x00') {
		return fmt.Errorf("input must be UTF-8 without NUL")
	}
	if len(request.TaskID) > 512 {
		return fmt.Errorf("task_id exceeds 512 bytes")
	}
	if !utf8.ValidString(request.TaskID) ||
		strings.ContainsRune(request.TaskID, '\x00') {
		return fmt.Errorf("task_id must be UTF-8 without NUL")
	}
	switch request.Retention {
	case "", RetentionEphemeral, RetentionStandard, RetentionPinned:
	default:
		return fmt.Errorf("unsupported retention %q", request.Retention)
	}
	hasModelOptions := request.ModelOptions.MaxOutputTokens != nil ||
		request.ModelOptions.Temperature != nil ||
		request.ModelOptions.TopP != nil ||
		len(request.ModelOptions.StopSequences) > 0
	if request.ModelOptions.MaxOutputTokens != nil &&
		*request.ModelOptions.MaxOutputTokens <= 0 {
		return fmt.Errorf("model_options.max_output_tokens must be positive")
	}
	if request.ModelOptions.Temperature != nil {
		if err := contract.ValidateTemperature(
			*request.ModelOptions.Temperature,
		); err != nil {
			return fmt.Errorf("model_options.temperature: %w", err)
		}
	}
	if request.ModelOptions.TopP != nil {
		if err := contract.ValidateTopP(*request.ModelOptions.TopP); err != nil {
			return fmt.Errorf("model_options.top_p: %w", err)
		}
	}
	if err := contract.ValidateStopSequences(
		request.ModelOptions.StopSequences,
	); err != nil {
		return fmt.Errorf("model_options.stop_sequences: %w", err)
	}
	if entry.Kind == profile.KindModel && entry.Model != nil {
		_, _, inputBudget, _ := entry.Model.EffectiveContextBudgetForRequest(
			request.ModelOptions.MaxOutputTokens,
		)
		if inputBudget < 2 {
			return fmt.Errorf(
				"model context policy leaves fewer than 2 input tokens",
			)
		}
	}
	if entry.Kind == profile.KindCommand {
		if hasModelOptions {
			return fmt.Errorf(
				"API model options are invalid for CLI profile %q", entry.ID,
			)
		}
		if request.Effort != "" {
			if _, err := runtimecommand.ParseEffort(request.Effort); err != nil {
				return err
			}
		}
		return nil
	}
	if request.Model != "" || request.Effort != "" || request.CWD != "" ||
		request.Snapshot != nil {
		return fmt.Errorf(
			"model, effort, cwd, and CLI snapshot are invalid for API profile %q",
			entry.ID,
		)
	}
	return nil
}

func boundedLabels(values map[string]string) map[string]string {
	result := make(map[string]string, len(values))
	for key, value := range values {
		if value != "" {
			result[key] = value
		}
	}
	return result
}

func sessionRuntimeError(
	code contract.ErrorCode,
	message string,
) *contract.RuntimeError {
	return &contract.RuntimeError{
		Code: code, Phase: contract.PhaseRun, Message: message,
	}
}
