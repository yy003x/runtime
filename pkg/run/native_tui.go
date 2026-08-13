package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"path/filepath"
	"reflect"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/pkg/contract"
)

const NativeTUIExecutionSchemaVersion = 1

const (
	NativeTUIExecutionRunning = "running"
	NativeTUIExecutionSettled = "settled"
	NativeTUICaptureOpaque    = "opaque"

	NativeTUIOutcomeCompleted = "completed"
	NativeTUIOutcomeFailed    = "failed"
	NativeTUIOutcomeCancelled = "cancelled"
	NativeTUIOutcomeUnknown   = "unknown"
)

type NativeTUIService struct {
	store NativeTUILifecycleStore
	now   func() time.Time
}

type NativeTUIServiceOptions struct {
	Store NativeTUILifecycleStore
	Now   func() time.Time
}

type NativeTUIBeginRequest struct {
	SessionID    string
	ExecutionID  string
	ProfileID    string
	Input        string
	TaskID       string
	CWD          string
	Model        string
	Effort       string
	ConfigDigest string
}

func NewNativeTUIService(options NativeTUIServiceOptions) (*NativeTUIService, error) {
	if options.Store == nil {
		return nil, fmt.Errorf("native_tui Run store is required")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &NativeTUIService{store: options.Store, now: options.Now}, nil
}

func (service *NativeTUIService) Begin(
	ctx context.Context,
	request NativeTUIBeginRequest,
) (Record, NativeTUIExecution, *contract.RuntimeError) {
	if runtimeErr := validateNativeTUIBegin(request); runtimeErr != nil {
		return Record{}, NativeTUIExecution{}, runtimeErr
	}
	runID, err := identity.New("run")
	if err != nil {
		return Record{}, NativeTUIExecution{}, nativeTUIRunError(
			contract.ErrorInternal, err.Error(),
		)
	}
	startedAt := service.now().UTC()
	record, err := service.store.CreateRunning(ctx, runID, Request{
		Kind: KindNativeTUI, ProfileID: request.ProfileID,
		Input: request.Input, SessionID: request.SessionID,
		ExecutionID: request.ExecutionID, TaskID: request.TaskID,
		Model: request.Model, Effort: request.Effort, CWD: request.CWD,
		ConfigDigest: request.ConfigDigest,
	})
	if err != nil {
		code := contract.ErrorInternal
		if errors.Is(err, ErrSessionRunOpen) {
			code = contract.ErrorConflict
		}
		return Record{}, NativeTUIExecution{}, nativeTUIRunError(code, err.Error())
	}
	return record, NativeTUIExecution{
		SchemaVersion: NativeTUIExecutionSchemaVersion,
		ID:            request.ExecutionID, RunID: record.ID,
		SessionID: request.SessionID, State: NativeTUIExecutionRunning,
		CaptureQuality: NativeTUICaptureOpaque, StartedAt: startedAt,
	}, nil
}

func (service *NativeTUIService) OpenForSession(
	ctx context.Context,
	sessionID string,
) (Record, bool, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return Record{}, false, err
	}
	return service.store.OpenSessionRun(ctx, sessionID, KindNativeTUI)
}

func (service *NativeTUIService) ForSession(
	ctx context.Context,
	sessionID string,
) (Record, bool, error) {
	if err := identity.Validate(sessionID, "session"); err != nil {
		return Record{}, false, err
	}
	return service.store.SessionRun(ctx, sessionID, KindNativeTUI)
}

func (service *NativeTUIService) Get(
	ctx context.Context,
	runID string,
) (Record, error) {
	if err := identity.Validate(runID, "run"); err != nil {
		return Record{}, err
	}
	return service.store.Get(ctx, runID)
}

func (service *NativeTUIService) Settle(
	ctx context.Context,
	execution NativeTUIExecution,
) (Record, *contract.RuntimeError) {
	if runtimeErr := validateNativeTUIExecution(execution, true); runtimeErr != nil {
		return Record{}, runtimeErr
	}
	resultJSON, err := json.Marshal(NativeTUIResult{Execution: execution})
	if err != nil {
		return Record{}, nativeTUIRunError(contract.ErrorInternal, err.Error())
	}
	state := StateCompleted
	if execution.Outcome == NativeTUIOutcomeFailed ||
		execution.Outcome == NativeTUIOutcomeUnknown {
		state = StateFailed
	}
	if execution.Outcome == NativeTUIOutcomeCancelled {
		state = StateCancelled
	}
	var value Record
	if state == StateCancelled {
		reserved, err := service.store.RequestCancel(ctx, execution.RunID)
		if err != nil {
			current, getErr := service.store.Get(ctx, execution.RunID)
			if getErr == nil && current.State.Terminal() {
				return current, nil
			}
			return Record{}, nativeTUIRunError(contract.ErrorInternal, err.Error())
		}
		if reserved.State.Terminal() {
			return reserved, nil
		}
		value, err = service.store.SettleCancellation(
			ctx, execution.RunID, state, resultJSON, execution.Error,
		)
		if err != nil {
			return Record{}, nativeTUIRunError(contract.ErrorInternal, err.Error())
		}
		return value, nil
	}
	value, err = service.store.Settle(
		ctx, execution.RunID, state, resultJSON, execution.Error,
	)
	if err == nil {
		return value, nil
	}
	current, getErr := service.store.Get(ctx, execution.RunID)
	if getErr == nil && current.State.Terminal() {
		return current, nil
	}
	return Record{}, nativeTUIRunError(contract.ErrorInternal, err.Error())
}

// NativeTUIExecutionFromRecord decodes and verifies the opaque lifecycle
// evidence carried by a terminal native_tui Run result.
func NativeTUIExecutionFromRecord(record Record) (NativeTUIExecution, error) {
	if record.Request.Kind != KindNativeTUI || !record.State.Terminal() ||
		len(record.Result) == 0 {
		return NativeTUIExecution{}, fmt.Errorf(
			"run %s has no terminal native_tui execution", record.ID,
		)
	}
	var result NativeTUIResult
	decoder := json.NewDecoder(bytes.NewReader(record.Result))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&result); err != nil {
		return NativeTUIExecution{}, fmt.Errorf(
			"decode native_tui result for run %s: %w", record.ID, err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return NativeTUIExecution{}, fmt.Errorf(
			"native_tui result for run %s has trailing JSON", record.ID,
		)
	}
	value := result.Execution
	if runtimeErr := validateNativeTUIExecution(value, true); runtimeErr != nil {
		return NativeTUIExecution{}, runtimeErr
	}
	if value.RunID != record.ID ||
		value.SessionID != record.Request.SessionID ||
		value.ID != record.Request.ExecutionID ||
		!reflect.DeepEqual(value.Error, record.Error) ||
		nativeTUIStateForOutcome(value.Outcome) != record.State {
		return NativeTUIExecution{}, fmt.Errorf(
			"native_tui execution does not match run %s", record.ID,
		)
	}
	return value, nil
}

// NativeTUIExecutionForRecord projects the canonical opaque lifecycle
// Execution from either a running or terminal native_tui Run. Running
// executions have no terminal outcome; terminal executions are verified from
// the Run result.
func NativeTUIExecutionForRecord(
	record Record,
	tmuxID string,
) (NativeTUIExecution, error) {
	if record.Request.Kind != KindNativeTUI {
		return NativeTUIExecution{}, fmt.Errorf(
			"run %s is not native_tui", record.ID,
		)
	}
	if record.State.Terminal() {
		return NativeTUIExecutionFromRecord(record)
	}
	if record.State != StateRunning {
		return NativeTUIExecution{}, fmt.Errorf(
			"native_tui run %s has unsupported state %s", record.ID, record.State,
		)
	}
	value := NativeTUIExecution{
		SchemaVersion:  NativeTUIExecutionSchemaVersion,
		ID:             record.Request.ExecutionID,
		RunID:          record.ID,
		SessionID:      record.Request.SessionID,
		TmuxID:         tmuxID,
		State:          NativeTUIExecutionRunning,
		CaptureQuality: NativeTUICaptureOpaque,
		StartedAt:      record.CreatedAt.UTC(),
	}
	if runtimeErr := validateNativeTUIExecution(value, false); runtimeErr != nil {
		return NativeTUIExecution{}, runtimeErr
	}
	return value, nil
}

func nativeTUIStateForOutcome(outcome string) State {
	switch outcome {
	case NativeTUIOutcomeCompleted:
		return StateCompleted
	case NativeTUIOutcomeCancelled:
		return StateCancelled
	default:
		return StateFailed
	}
}

func (service *NativeTUIService) Close() error {
	return service.store.Close()
}

func NewSettledNativeTUIExecution(
	record Record,
	tmuxID string,
	exitCode *int,
	signal string,
	outcome string,
	reason string,
	runtimeErr *contract.RuntimeError,
	now time.Time,
) NativeTUIExecution {
	startedAt := record.CreatedAt.UTC()
	settledAt := now.UTC()
	var code *int
	if exitCode != nil {
		current := *exitCode
		code = &current
	}
	return NativeTUIExecution{
		SchemaVersion: NativeTUIExecutionSchemaVersion,
		ID:            record.Request.ExecutionID, RunID: record.ID,
		SessionID: record.Request.SessionID, TmuxID: tmuxID,
		State: NativeTUIExecutionSettled, Outcome: outcome,
		CaptureQuality: NativeTUICaptureOpaque,
		ExitCode:       code, Signal: signal, CompletionReason: reason,
		Error: runtimeErr, StartedAt: startedAt, SettledAt: &settledAt,
	}
}

func validateNativeTUIBegin(
	request NativeTUIBeginRequest,
) *contract.RuntimeError {
	if err := identity.Validate(request.SessionID, "session"); err != nil {
		return nativeTUIRunError(contract.ErrorInvalidRequest, err.Error())
	}
	if err := identity.Validate(request.ExecutionID, "execution"); err != nil {
		return nativeTUIRunError(contract.ErrorInvalidRequest, err.Error())
	}
	if strings.TrimSpace(request.ProfileID) == "" ||
		request.ConfigDigest == "" || !filepath.IsAbs(request.CWD) {
		return nativeTUIRunError(
			contract.ErrorInvalidRequest,
			"profile_id, config_digest, and absolute cwd are required",
		)
	}
	for _, value := range []string{
		request.ProfileID, request.Input, request.TaskID, request.CWD,
		request.Model, request.Effort, request.ConfigDigest,
	} {
		if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
			return nativeTUIRunError(
				contract.ErrorInvalidRequest,
				"native_tui lifecycle fields must be UTF-8 without NUL",
			)
		}
	}
	if len(request.Input) > MaxResumeInputBytes {
		return nativeTUIRunError(
			contract.ErrorInvalidRequest, "input exceeds 1048576 bytes",
		)
	}
	return nil
}

func validateNativeTUIExecution(
	value NativeTUIExecution,
	terminal bool,
) *contract.RuntimeError {
	if value.SchemaVersion != NativeTUIExecutionSchemaVersion {
		return nativeTUIRunError(
			contract.ErrorInvalidRequest,
			"native_tui execution schema_version is invalid",
		)
	}
	for _, pair := range []struct{ value, prefix string }{
		{value.ID, "execution"}, {value.RunID, "run"},
		{value.SessionID, "session"},
	} {
		if err := identity.Validate(pair.value, pair.prefix); err != nil {
			return nativeTUIRunError(contract.ErrorInvalidRequest, err.Error())
		}
	}
	if value.CaptureQuality != NativeTUICaptureOpaque ||
		value.StartedAt.IsZero() {
		return nativeTUIRunError(
			contract.ErrorInvalidRequest,
			"native_tui execution capture_quality and started_at are required",
		)
	}
	if terminal {
		if value.State != NativeTUIExecutionSettled || value.SettledAt == nil ||
			value.SettledAt.IsZero() || value.CompletionReason == "" {
			return nativeTUIRunError(
				contract.ErrorInvalidRequest,
				"settled native_tui execution is incomplete",
			)
		}
		switch value.Outcome {
		case NativeTUIOutcomeCompleted, NativeTUIOutcomeFailed,
			NativeTUIOutcomeCancelled, NativeTUIOutcomeUnknown:
		default:
			return nativeTUIRunError(
				contract.ErrorInvalidRequest,
				"settled native_tui execution outcome is invalid",
			)
		}
	}
	return nil
}

func nativeTUIRunError(
	code contract.ErrorCode,
	message string,
) *contract.RuntimeError {
	return &contract.RuntimeError{
		Code: code, Phase: contract.PhaseRun, Message: message,
	}
}
