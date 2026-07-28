package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/profile"
)

const maxSessionInputBytes = 1 << 20

type Service struct {
	store          *Store
	profiles       *profile.Catalog
	models         model.Generator
	commands       *runtimecommand.Runner
	now            func() time.Time
	terminalDriver string
}

type ServiceOptions struct {
	Store          *Store
	Profiles       *profile.Catalog
	Models         model.Generator
	Commands       *runtimecommand.Runner
	Now            func() time.Time
	TerminalDriver string
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
	if options.Commands == nil {
		options.Commands = runtimecommand.NewRunner()
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Service{
		store: options.Store, profiles: options.Profiles, models: options.Models,
		commands: options.Commands, now: options.Now,
		terminalDriver: options.TerminalDriver,
	}, nil
}

func (service *Service) Run(
	ctx context.Context,
	request RunRequest,
) (RunResult, *contract.RuntimeError) {
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
	if request.RunID != "" && request.SessionID == "" {
		return RunResult{}, sessionRuntimeError(
			contract.ErrorInvalidRequest,
			"session_id is required when run_id is supplied",
		)
	}
	if request.RunID != "" {
		existing, found, runtimeErr := service.findExistingRun(
			request.SessionID, request.RunID, entry, request.Input,
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
	input string,
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
	var recoveryTurn *Turn
	var recoveryAssistant *contract.Message
	err := service.store.withLock(sessionID, func() error {
		turnsDir := filepath.Join(service.store.sessionDir(sessionID), "turns")
		entries, err := os.ReadDir(turnsDir)
		if errors.Is(err, os.ErrNotExist) {
			return nil
		}
		if err != nil {
			return err
		}
		for _, directory := range entries {
			if !directory.IsDir() || directory.Type()&os.ModeSymlink != 0 {
				continue
			}
			turn, err := service.store.loadTurn(sessionID, directory.Name())
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
			if turn.State == TurnRunning && assistant != nil {
				currentTurn := turn
				currentAssistant := cloneContractMessage(*assistant)
				recoveryTurn = &currentTurn
				recoveryAssistant = &currentAssistant
				return nil
			}
			started = startedExecution{
				ids: executionIDs{
					session: sessionID, turn: turn.ID, run: runID,
					execution: turn.ExecutionID,
				},
				result: service.resultFromTurn(turn, assistant),
			}
			if turn.State == TurnRunning {
				built, projectionErr := buildProjection(
					entry, sessionID, turn.ID, runID, turn.ExecutionID,
					turn.TaskID, input, messages, service.now().UTC(),
				)
				if projectionErr != nil {
					runtimeErr = projectionErr
					return nil
				}
				started.projection = built
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
	if recoveryTurn != nil {
		recovered, recoverErr := service.recoverAssistant(
			recoveryTurn, recoveryAssistant, entry,
		)
		started.result = recovered
		return started, true, recoverErr
	}
	return started, found, runtimeErr
}

func (service *Service) recoverAssistant(
	turn *Turn,
	assistant *contract.Message,
	entry profile.Entry,
) (RunResult, *contract.RuntimeError) {
	ids := executionIDs{
		session: turn.SessionID, turn: turn.ID, run: turn.RunID,
		execution: turn.ExecutionID,
	}
	if entry.Kind == profile.KindModel {
		finishReason := contract.FinishStop
		if len(assistant.ToolCalls) > 0 {
			finishReason = contract.FinishToolCall
		}
		return service.finishModelResult(ids, contract.ModelResult{
			Message: *assistant, FinishReason: finishReason,
		}, false)
	}
	return service.finishCommandResult(
		ids,
		runtimecommand.ExecutionResult{
			State: "completed", Stdout: assistant.Content,
			CaptureQuality: "parsed",
		},
		CaptureParsed,
		entry,
		false,
	)
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
	if assistant != nil {
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
			State: TurnRunning, CreatedAt: now, UpdatedAt: now,
			ToolResults: make(map[string]ToolResultReceipt),
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
		if err := service.store.writeTurn(turn); err != nil {
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
			request.TaskID, request.Input, messages, now,
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
	_ = service.store.rebuildIndex()
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
	turn.State = TurnFailed
	turn.Error = runtimeErr
	turn.UpdatedAt = now
	sessionValue.State = SessionIdle
	sessionValue.ActiveTurnID = ""
	sessionValue.UpdatedAt = now
	if err := service.store.appendEvent(sessionValue, EventRecord{
		Time: now, Type: "turn.failed", TurnID: turn.ID,
		RunID: turn.RunID, ExecutionID: turn.ExecutionID,
		State: string(TurnFailed), Error: runtimeErr,
	}); err != nil {
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
	modelResult, runtimeErr := service.models.GenerateStream(
		ctx, generateRequest,
		func(event contract.Event) error {
			return service.recordModelEvent(started.ids, event)
		},
	)
	if runtimeErr != nil {
		result := service.finishFailure(started.ids, runtimeErr, CaptureStructured, "http")
		return result, runtimeErr
	}
	return service.finishModel(started.ids, modelResult)
}

func (service *Service) runCommand(
	ctx context.Context,
	started startedExecution,
	request RunRequest,
	entry profile.Entry,
) (RunResult, *contract.RuntimeError) {
	executionResult, err := service.commands.Execute(ctx, *entry.Command, runtimecommand.ExecutionRequest{
		Args: request.CommandArgs, Prompt: started.projection.commandPrompt,
		CWD: request.CWD, TerminalDriver: firstNonEmpty(
			request.TerminalDriver, service.terminalDriver,
		),
	})
	if err != nil {
		runtimeErr := sessionRuntimeError(contract.ErrorInternal, err.Error())
		runtimeErr.Phase = contract.PhaseTransport
		return service.finishFailure(
			started.ids, runtimeErr, CaptureTranscriptOnly, string(entry.Command.Transport),
		), runtimeErr
	}
	quality := CaptureParsed
	if executionResult.CaptureQuality == "transcript_only" {
		quality = CaptureTranscriptOnly
	}
	if executionResult.ExitCode != 0 {
		runtimeErr := sessionRuntimeError(
			contract.ErrorInternal,
			fmt.Sprintf("command exited with status %d", executionResult.ExitCode),
		)
		runtimeErr.Phase = contract.PhaseTransport
		result := service.finishFailure(
			started.ids, runtimeErr, quality, string(entry.Command.Transport),
		)
		result.ExitCode = executionResult.ExitCode
		return result, runtimeErr
	}
	return service.finishCommand(started.ids, executionResult, quality, entry)
}

func firstNonEmpty(values ...string) string {
	for _, value := range values {
		if strings.TrimSpace(value) != "" {
			return value
		}
	}
	return ""
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
		execution := Execution{
			SchemaVersion: SchemaVersion, ID: ids.execution,
			SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
			ProfileID: turn.ProfileID, ProfileKind: turn.ProfileKind,
			Transport: "http", State: turn.State,
			CaptureQuality: CaptureStructured, CreatedAt: turn.CreatedAt, UpdatedAt: now,
		}
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
		runtimeErr := sessionRuntimeError(contract.ErrorInternal, err.Error())
		result.State = TurnFailed
		result.Error = runtimeErr
		return result, runtimeErr
	}
	_ = service.store.rebuildIndex()
	return result, nil
}

func (service *Service) finishCommand(
	ids executionIDs,
	executionResult runtimecommand.ExecutionResult,
	quality CaptureQuality,
	entry profile.Entry,
) (RunResult, *contract.RuntimeError) {
	return service.finishCommandResult(ids, executionResult, quality, entry, true)
}

func (service *Service) finishCommandResult(
	ids executionIDs,
	executionResult runtimecommand.ExecutionResult,
	quality CaptureQuality,
	entry profile.Entry,
	appendAssistant bool,
) (RunResult, *contract.RuntimeError) {
	result := RunResult{
		SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
		ExecutionID: ids.execution, State: TurnCompleted,
		CaptureQuality: quality, LaunchHandle: executionResult.LaunchHandle,
		ExitCode: executionResult.ExitCode,
	}
	if executionResult.State == "submitted" {
		result.State = TurnSubmitted
	}
	if quality == CaptureParsed &&
		strings.TrimSpace(executionResult.Stdout) != "" {
		message := contract.Message{
			Role:    contract.RoleAssistant,
			Content: strings.TrimSpace(executionResult.Stdout),
		}
		result.Message = &message
	}
	err := service.store.withLock(ids.session, func() error {
		now := service.now().UTC()
		sessionValue, turn, err := service.loadActive(ids)
		if err != nil {
			return err
		}
		if appendAssistant && result.Message != nil {
			if err := service.store.appendMessage(&sessionValue, MessageRecord{
				Time: now, TurnID: ids.turn, RunID: ids.run,
				ExecutionID: ids.execution, ProfileID: turn.ProfileID,
				Message: *result.Message,
			}); err != nil {
				return err
			}
		}
		turn.State = result.State
		turn.CaptureQuality = quality
		turn.UpdatedAt = now
		sessionValue.State = SessionIdle
		sessionValue.ActiveTurnID = ""
		sessionValue.UpdatedAt = now
		execution := Execution{
			SchemaVersion: SchemaVersion, ID: ids.execution,
			SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
			ProfileID: entry.ID, ProfileKind: entry.Kind,
			Transport: string(entry.Command.Transport), State: result.State,
			CaptureQuality: quality, LaunchHandle: executionResult.LaunchHandle,
			ExitCode:  executionResult.ExitCode,
			CreatedAt: turn.CreatedAt, UpdatedAt: now,
		}
		if err := service.store.appendEvent(&sessionValue, EventRecord{
			Time: now, Type: "turn.settled", TurnID: ids.turn,
			RunID: ids.run, ExecutionID: ids.execution, State: string(result.State),
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
		runtimeErr := sessionRuntimeError(contract.ErrorInternal, err.Error())
		result.State = TurnFailed
		result.Error = runtimeErr
		return result, runtimeErr
	}
	_ = service.store.rebuildIndex()
	return result, nil
}

func (service *Service) finishFailure(
	ids executionIDs,
	runtimeErr *contract.RuntimeError,
	quality CaptureQuality,
	transport string,
) RunResult {
	result := RunResult{
		SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
		ExecutionID: ids.execution, State: TurnFailed,
		CaptureQuality: quality, Error: runtimeErr,
	}
	err := service.store.withLock(ids.session, func() error {
		now := service.now().UTC()
		sessionValue, turn, err := service.loadActive(ids)
		if err != nil {
			return err
		}
		turn.State = TurnFailed
		turn.Error = runtimeErr
		turn.CaptureQuality = quality
		turn.UpdatedAt = now
		sessionValue.State = SessionIdle
		sessionValue.ActiveTurnID = ""
		sessionValue.UpdatedAt = now
		execution := Execution{
			SchemaVersion: SchemaVersion, ID: ids.execution,
			SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
			ProfileID: turn.ProfileID, ProfileKind: turn.ProfileKind,
			Transport: transport, State: TurnFailed, CaptureQuality: quality,
			CreatedAt: turn.CreatedAt, UpdatedAt: now,
		}
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
		result.Error = sessionRuntimeError(
			contract.ErrorInternal,
			fmt.Sprintf("%s; persist failure: %v", runtimeErr.Message, err),
		)
	}
	_ = service.store.rebuildIndex()
	return result
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
	if len(request.Input) > maxSessionInputBytes {
		return fmt.Errorf("input exceeds %d bytes", maxSessionInputBytes)
	}
	if len(request.TaskID) > 512 {
		return fmt.Errorf("task_id exceeds 512 bytes")
	}
	switch request.Retention {
	case "", RetentionEphemeral, RetentionStandard, RetentionPinned:
	default:
		return fmt.Errorf("unsupported retention %q", request.Retention)
	}
	if entry.Kind == profile.KindCommand &&
		entry.Command.PromptDelivery == runtimecommand.PromptManual {
		return fmt.Errorf(
			"command profile %q uses manual prompt delivery and cannot accept session input",
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
