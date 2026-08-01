package run

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
)

type Service struct {
	store              Store
	executors          map[Kind]Executor
	now                func() time.Time
	cancelPollInterval time.Duration

	activeMu sync.Mutex
	active   map[string]context.CancelFunc
}

type ServiceOptions struct {
	Store              Store
	Executors          map[Kind]Executor
	Now                func() time.Time
	CancelPollInterval time.Duration
}

func NewService(options ServiceOptions) (*Service, error) {
	if options.Store == nil {
		return nil, fmt.Errorf("run store is required")
	}
	if len(options.Executors) == 0 {
		return nil, fmt.Errorf("at least one run executor is required")
	}
	executors := make(map[Kind]Executor, len(options.Executors))
	for kind, executor := range options.Executors {
		if executor == nil {
			return nil, fmt.Errorf("run executor %q is nil", kind)
		}
		executors[kind] = executor
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	if options.CancelPollInterval <= 0 {
		options.CancelPollInterval = 100 * time.Millisecond
	}
	return &Service{
		store: options.Store, executors: executors, now: options.Now,
		cancelPollInterval: options.CancelPollInterval,
		active:             make(map[string]context.CancelFunc),
	}, nil
}

func (service *Service) Submit(
	ctx context.Context,
	request Request,
) (Record, *contract.RuntimeError) {
	if executor, exists := service.executors[request.Kind]; exists {
		if preparer, ok := executor.(RequestPreparer); ok {
			prepared, err := preparer.Prepare(ctx, cloneRequest(request))
			if err != nil {
				var runtimeErr *contract.RuntimeError
				if errors.As(err, &runtimeErr) {
					return Record{}, runtimeErr
				}
				return Record{}, runError(contract.ErrorInvalidRequest, err.Error())
			}
			request = prepared
		}
	}
	if runtimeErr := service.validateRequest(request); runtimeErr != nil {
		return Record{}, runtimeErr
	}
	runID, err := identity.New("run")
	if err != nil {
		return Record{}, runError(contract.ErrorInternal, err.Error())
	}
	value, err := service.store.Create(ctx, runID, cloneRequest(request))
	if err != nil {
		if errors.Is(err, ErrSessionRunOpen) {
			return Record{}, runError(contract.ErrorConflict, err.Error())
		}
		return Record{}, runError(contract.ErrorInternal, err.Error())
	}
	return value, nil
}

func (service *Service) RunNow(
	ctx context.Context,
	request Request,
	sink contract.EventSink,
) (Record, *contract.RuntimeError) {
	value, runtimeErr := service.Submit(ctx, request)
	if runtimeErr != nil {
		return Record{}, runtimeErr
	}
	started, err := service.store.Start(ctx, value.ID)
	if err != nil {
		return Record{}, runError(contract.ErrorConflict, err.Error())
	}
	return service.execute(ctx, started, sink)
}

func (service *Service) ExecuteNext(
	ctx context.Context,
	workerID string,
) (Record, bool, *contract.RuntimeError) {
	value, exists, err := service.store.Claim(ctx, workerID)
	if err != nil {
		return Record{}, false, runError(contract.ErrorInternal, err.Error())
	}
	if !exists {
		return Record{}, false, nil
	}
	result, runtimeErr := service.execute(ctx, value, nil)
	return result, true, runtimeErr
}

func (service *Service) Worker(
	ctx context.Context,
	workerID string,
	pollInterval time.Duration,
) error {
	if pollInterval <= 0 {
		pollInterval = 250 * time.Millisecond
	}
	for {
		_, found, runtimeErr := service.ExecuteNext(ctx, workerID)
		if runtimeErr != nil && !found {
			return runtimeErr
		}
		if found {
			continue
		}
		timer := time.NewTimer(pollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return ctx.Err()
		case <-timer.C:
		}
	}
}

func (service *Service) execute(
	ctx context.Context,
	record Record,
	sink contract.EventSink,
) (Record, *contract.RuntimeError) {
	privateRequest, err := service.store.PrivateRequest(ctx, record.ID)
	if err != nil {
		runtimeErr := runError(
			contract.ErrorInternal,
			"load private execution request: "+err.Error(),
		)
		value, settleErr := service.settleTerminalExactly(
			context.WithoutCancel(ctx), record.ID, StateFailed, nil, runtimeErr,
		)
		if settleErr != nil {
			return value, runError(
				contract.ErrorInternal,
				"settle Run after private request failure: "+settleErr.Error(),
			)
		}
		return value, runtimeErr
	}
	record.Request.PrivateRequest = privateRequest
	executor, exists := service.executors[record.Request.Kind]
	if !exists {
		runtimeErr := runError(
			contract.ErrorInvalidRequest,
			fmt.Sprintf("no executor is registered for run kind %q", record.Request.Kind),
		)
		value, settleErr := service.settleTerminalExactly(
			context.WithoutCancel(ctx), record.ID, StateFailed, nil, runtimeErr,
		)
		if settleErr != nil {
			return value, runError(
				contract.ErrorInternal,
				"settle Run after executor lookup failure: "+settleErr.Error(),
			)
		}
		return value, runtimeErr
	}
	runContext, cancel := context.WithCancel(ctx)
	service.activeMu.Lock()
	service.active[record.ID] = cancel
	service.activeMu.Unlock()
	defer func() {
		cancel()
		service.activeMu.Lock()
		delete(service.active, record.ID)
		service.activeMu.Unlock()
	}()
	current, err := service.store.Get(ctx, record.ID)
	if err != nil {
		return Record{}, runError(
			contract.ErrorInternal,
			"recheck claimed Run cancellation: "+err.Error(),
		)
	}
	if current.CancelRequested {
		return service.finalizeReservedCancellation(
			context.WithoutCancel(ctx), current,
		)
	}
	monitorContext, stopMonitor := context.WithCancel(
		context.WithoutCancel(ctx),
	)
	monitorDone := make(chan struct{})
	go func() {
		defer close(monitorDone)
		service.monitorCancellation(
			monitorContext, record.ID, cancel,
		)
	}()
	monitorStopped := false
	stopCancellationMonitor := func() {
		if monitorStopped {
			return
		}
		monitorStopped = true
		stopMonitor()
		<-monitorDone
	}
	defer stopCancellationMonitor()
	journalSink := func(event contract.Event) error {
		persisted, err := service.store.AppendEvent(runContext, record.ID, event)
		if err != nil {
			return err
		}
		if sink != nil {
			return sink(persisted)
		}
		return nil
	}
	outcome := executor.Execute(runContext, record, journalSink)
	stopCancellationMonitor()
	persistContext := context.WithoutCancel(ctx)
	current, err = service.store.Get(persistContext, record.ID)
	if err != nil {
		return Record{}, runError(
			contract.ErrorInternal,
			"recheck Run cancellation after execution: "+err.Error(),
		)
	}
	if current.CancelRequested {
		return service.finalizeReservedCancellation(
			persistContext, current,
		)
	}
	switch outcome.State {
	case StatePaused:
		value, err := service.store.Pause(persistContext, record.ID, outcome.Pause)
		if err != nil {
			if errors.Is(err, ErrCancelReserved) {
				reserved, getErr := service.store.Get(
					persistContext, record.ID,
				)
				if getErr != nil {
					return Record{}, runError(
						contract.ErrorInternal, getErr.Error(),
					)
				}
				return service.finalizeReservedCancellation(
					persistContext, reserved,
				)
			}
			return Record{}, runError(contract.ErrorInternal, err.Error())
		}
		return value, nil
	case StateNeedsReconciliation:
		value, err := service.store.NeedsReconciliation(
			persistContext, record.ID, outcome.Error,
		)
		if err != nil {
			if errors.Is(err, ErrCancelReserved) {
				reserved, getErr := service.store.Get(
					persistContext, record.ID,
				)
				if getErr != nil {
					return Record{}, runError(
						contract.ErrorInternal, getErr.Error(),
					)
				}
				return service.finalizeReservedCancellation(
					persistContext, reserved,
				)
			}
			return Record{}, runError(contract.ErrorInternal, err.Error())
		}
		return value, outcome.Error
	case StateCompleted, StateFailed, StateCancelled:
		value, err := service.settleTerminalExactly(
			persistContext, record.ID, outcome.State, outcome.Result, outcome.Error,
		)
		if err != nil {
			if errors.Is(err, ErrCancelReserved) {
				reserved, getErr := service.store.Get(
					persistContext, record.ID,
				)
				if getErr != nil {
					return Record{}, runError(
						contract.ErrorInternal, getErr.Error(),
					)
				}
				return service.finalizeReservedCancellation(
					persistContext, reserved,
				)
			}
			return value, runError(contract.ErrorInternal, err.Error())
		}
		return value, outcome.Error
	default:
		runtimeErr := runError(
			contract.ErrorInternal,
			fmt.Sprintf("executor returned invalid state %q", outcome.State),
		)
		value, err := service.settleTerminalExactly(
			persistContext, record.ID, StateFailed, nil, runtimeErr,
		)
		if err != nil {
			return value, runError(contract.ErrorInternal, err.Error())
		}
		return value, runtimeErr
	}
}

func (service *Service) monitorCancellation(
	ctx context.Context,
	runID string,
	cancel context.CancelFunc,
) {
	ticker := time.NewTicker(service.cancelPollInterval)
	defer ticker.Stop()
	for {
		select {
		case <-ctx.Done():
			return
		case <-ticker.C:
			current, err := service.store.Get(ctx, runID)
			if err != nil {
				// A transient SQLite read failure does not make execution
				// outcome knowable. Keep polling; execute performs a mandatory
				// durable recheck before publishing any outcome.
				continue
			}
			if current.CancelRequested {
				cancel()
				return
			}
			if current.State != StateRunning {
				return
			}
		}
	}
}

func (service *Service) Get(
	ctx context.Context,
	runID string,
) (Record, error) {
	return service.store.Get(ctx, runID)
}

func (service *Service) List(
	ctx context.Context,
	filter ListFilter,
) ([]Record, error) {
	normalized, err := NormalizeListFilter(filter)
	if err != nil {
		return nil, err
	}
	return service.store.List(ctx, normalized)
}

func (service *Service) Events(
	ctx context.Context,
	runID string,
	afterSequence uint64,
	limit int,
) ([]contract.Event, error) {
	if _, err := service.store.Get(ctx, runID); err != nil {
		return nil, err
	}
	return service.store.Events(ctx, runID, afterSequence, limit)
}

func (service *Service) Watch(
	ctx context.Context,
	runID string,
	afterSequence uint64,
	sink contract.EventSink,
) (Record, error) {
	if sink == nil {
		return Record{}, fmt.Errorf("event sink is required")
	}
	for {
		events, err := service.Events(ctx, runID, afterSequence, 256)
		if err != nil {
			return Record{}, err
		}
		for _, event := range events {
			if err := sink(event); err != nil {
				return Record{}, err
			}
			afterSequence = event.Sequence
		}
		value, err := service.Get(ctx, runID)
		if err != nil {
			return Record{}, err
		}
		if value.State.Terminal() && afterSequence >= value.SettledSequence {
			return value, nil
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return Record{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func (service *Service) Cancel(
	ctx context.Context,
	runID string,
) (Record, error) {
	value, err := service.store.RequestCancel(ctx, runID)
	if err != nil {
		return Record{}, err
	}
	switch value.State {
	case StateQueued, StatePaused:
		settled, runtimeErr := service.finalizeReservedCancellation(
			context.WithoutCancel(ctx), value,
		)
		if runtimeErr != nil {
			return value, runtimeErr
		}
		return settled, nil
	case StateRunning:
		service.activeMu.Lock()
		cancel := service.active[runID]
		service.activeMu.Unlock()
		if cancel != nil {
			cancel()
		}
	}
	return value, nil
}

func (service *Service) finalizeReservedCancellation(
	ctx context.Context,
	record Record,
) (Record, *contract.RuntimeError) {
	executor, exists := service.executors[record.Request.Kind]
	if !exists {
		return Record{}, runError(
			contract.ErrorInternal,
			fmt.Sprintf(
				"no executor is registered for run kind %q",
				record.Request.Kind,
			),
		)
	}
	terminalState := StateCancelled
	var terminalResult json.RawMessage
	terminalError := &contract.RuntimeError{
		Code: contract.ErrorCancelled, Phase: contract.PhaseRun,
		Message: "run was cancelled",
	}
	if finalizer, ok := executor.(CancellationFinalizer); ok {
		outcome := finalizer.FinalizeCancellation(ctx, record)
		switch outcome.State {
		case StateCompleted, StateFailed, StateCancelled:
			terminalState = outcome.State
			terminalResult = outcome.Result
			terminalError = outcome.Error
			if terminalState == StateCancelled && terminalError == nil {
				terminalError = &contract.RuntimeError{
					Code: contract.ErrorCancelled, Phase: contract.PhaseRun,
					Message: "run was cancelled",
				}
			}
		case StateNeedsReconciliation:
			value, err := service.store.NeedsCancellationReconciliation(
				ctx, record.ID, outcome.Error,
			)
			if err != nil {
				return Record{}, runError(
					contract.ErrorInternal,
					"publish cancellation reconciliation: "+err.Error(),
				)
			}
			return value, outcome.Error
		default:
			return Record{}, runError(
				contract.ErrorInternal,
				fmt.Sprintf(
					"cancellation finalizer returned invalid state %q",
					outcome.State,
				),
			)
		}
	}
	value, err := service.settleCancellationTerminalExactly(
		ctx, record.ID, terminalState, terminalResult, terminalError,
	)
	if err != nil {
		return value, runError(
			contract.ErrorInternal,
			"settle reserved cancellation: "+err.Error(),
		)
	}
	if terminalState == StateFailed {
		return value, terminalError
	}
	return value, nil
}

func (service *Service) settleTerminalExactly(
	ctx context.Context,
	runID string,
	state State,
	result json.RawMessage,
	runtimeErr *contract.RuntimeError,
) (Record, error) {
	return service.settleTerminalExactlyWithReservation(
		ctx, runID, state, result, runtimeErr, false,
	)
}

func (service *Service) settleCancellationTerminalExactly(
	ctx context.Context,
	runID string,
	state State,
	result json.RawMessage,
	runtimeErr *contract.RuntimeError,
) (Record, error) {
	return service.settleTerminalExactlyWithReservation(
		ctx, runID, state, result, runtimeErr, true,
	)
}

func (service *Service) settleTerminalExactlyWithReservation(
	ctx context.Context,
	runID string,
	state State,
	result json.RawMessage,
	runtimeErr *contract.RuntimeError,
	cancellationReserved bool,
) (Record, error) {
	var settleErr error
	var current Record
	for attempt := 0; attempt < 2; attempt++ {
		var value Record
		var err error
		if cancellationReserved {
			value, err = service.store.SettleCancellation(
				ctx, runID, state, result, runtimeErr,
			)
		} else {
			value, err = service.store.Settle(
				ctx, runID, state, result, runtimeErr,
			)
		}
		if err == nil {
			return value, nil
		}
		settleErr = err
		var getErr error
		current, getErr = service.store.Get(ctx, runID)
		if getErr == nil {
			if terminalRecordMatches(
				current, state, result, runtimeErr,
			) {
				return current, nil
			}
			if current.State.Terminal() {
				return Record{}, fmt.Errorf(
					"terminal Run settlement conflicts with durable state %s "+
						"(settled=%t result_match=%t error_match=%t "+
						"durable_error=%v expected_error=%v): %w",
					current.State, current.SettledSequence != 0,
					reflect.DeepEqual(current.Result, result),
					reflect.DeepEqual(current.Error, runtimeErr),
					current.Error, runtimeErr,
					settleErr,
				)
			}
		}
		if errors.Is(settleErr, ErrCancelReserved) {
			return current, settleErr
		}
	}
	return current, fmt.Errorf(
		"terminal Run settlement remains uncommitted in durable state %s "+
			"and requires retry or startup recovery: %w",
		current.State, settleErr,
	)
}

func terminalRecordMatches(
	record Record,
	state State,
	result json.RawMessage,
	runtimeErr *contract.RuntimeError,
) bool {
	return record.State == state &&
		record.SettledSequence != 0 &&
		reflect.DeepEqual(record.Result, result) &&
		reflect.DeepEqual(record.Error, runtimeErr)
}

func (service *Service) Resume(
	ctx context.Context,
	runID string,
	input json.RawMessage,
) (Record, error) {
	record, err := service.store.Get(ctx, runID)
	if err != nil {
		return Record{}, err
	}
	if record.State != StatePaused {
		return Record{}, fmt.Errorf(
			"%w: run %s is %s, not paused",
			ErrConflict, runID, record.State,
		)
	}
	executor, exists := service.executors[record.Request.Kind]
	if !exists {
		return Record{}, fmt.Errorf(
			"no executor is registered for run kind %q",
			record.Request.Kind,
		)
	}
	validator, ok := executor.(ResumeValidator)
	if !ok {
		return Record{}, fmt.Errorf(
			"%w: run kind %q does not support resume",
			ErrConflict, record.Request.Kind,
		)
	}
	constraint, err := validator.ValidateResume(
		ctx, record, append([]byte(nil), input...),
	)
	if err != nil {
		return Record{}, err
	}
	return service.store.Resume(
		ctx, runID, append([]byte(nil), input...), constraint,
	)
}

func (service *Service) Retry(
	ctx context.Context,
	runID string,
) (Record, *contract.RuntimeError) {
	previous, err := service.store.Get(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Record{}, runError(contract.ErrorNotFound, err.Error())
		}
		return Record{}, runError(contract.ErrorInternal, err.Error())
	}
	if !previous.State.Terminal() {
		return Record{}, runError(
			contract.ErrorConflict, "only terminal runs can be retried",
		)
	}
	privateRequest, err := service.store.PrivateRequest(ctx, runID)
	if err != nil {
		return Record{}, runError(contract.ErrorInternal, err.Error())
	}
	request := previous.Request
	request.PrivateRequest = privateRequest
	request.RetryOf = runID
	request.Resume = nil
	return service.Submit(ctx, request)
}

func (service *Service) Reconcile(ctx context.Context) error {
	if err := service.store.Reconcile(ctx); err != nil {
		return err
	}
	afterRunID := ""
	for {
		values, err := service.store.CancellationReservations(
			ctx, afterRunID, 1000,
		)
		if err != nil {
			return err
		}
		if len(values) == 0 {
			return nil
		}
		for _, value := range values {
			afterRunID = value.ID
			if _, runtimeErr := service.finalizeReservedCancellation(
				context.WithoutCancel(ctx), value,
			); runtimeErr != nil &&
				runtimeErr.Code == contract.ErrorInternal {
				return runtimeErr
			}
		}
	}
}

func (service *Service) ReconcileRun(
	ctx context.Context,
	runID string,
) (Record, *contract.RuntimeError) {
	record, err := service.store.Get(ctx, runID)
	if err != nil {
		if errors.Is(err, ErrNotFound) {
			return Record{}, runError(contract.ErrorNotFound, err.Error())
		}
		return Record{}, runError(contract.ErrorInternal, err.Error())
	}
	// Session reconciliation uses its immutable terminal fact as the idempotency
	// marker. Agent reconciliation writes an explicit result marker so an
	// unrelated terminal Agent Run is not misreported as reconciled.
	if record.State.Terminal() {
		if record.Request.Kind == KindSession ||
			agentReconciliationAcknowledged(record.Result) {
			return record, nil
		}
		return Record{}, runError(
			contract.ErrorConflict,
			fmt.Sprintf("run %s was not settled by reconciliation", runID),
		)
	}
	if record.State != StateNeedsReconciliation {
		return Record{}, runError(
			contract.ErrorConflict,
			fmt.Sprintf("run %s is %s, not needs_reconciliation", runID, record.State),
		)
	}
	executor, exists := service.executors[record.Request.Kind]
	if !exists {
		return Record{}, runError(
			contract.ErrorInvalidRequest,
			fmt.Sprintf("no executor is registered for run kind %q", record.Request.Kind),
		)
	}
	reconciler, ok := executor.(ReconcileExecutor)
	if !ok {
		return Record{}, runError(
			contract.ErrorConflict,
			fmt.Sprintf("run kind %q does not support explicit reconciliation", record.Request.Kind),
		)
	}
	outcome := reconciler.Reconcile(ctx, record)
	switch outcome.State {
	case StateCompleted, StateFailed, StateCancelled:
		var value Record
		var settleErr error
		if record.CancelRequested {
			value, settleErr = service.store.SettleCancellation(
				context.WithoutCancel(ctx), record.ID, outcome.State,
				outcome.Result, outcome.Error,
			)
		} else {
			value, settleErr = service.store.Settle(
				context.WithoutCancel(ctx), record.ID, outcome.State,
				outcome.Result, outcome.Error,
			)
		}
		if settleErr != nil {
			current, getErr := service.store.Get(
				context.WithoutCancel(ctx), record.ID,
			)
			if getErr == nil && current.State.Terminal() {
				return current, nil
			}
			return Record{}, runError(contract.ErrorInternal, settleErr.Error())
		}
		return value, nil
	case StateNeedsReconciliation:
		if outcome.Error != nil {
			return record, outcome.Error
		}
		return record, runError(
			contract.ErrorConflict,
			"executor outcome is still unknown",
		)
	default:
		return Record{}, runError(
			contract.ErrorInternal,
			fmt.Sprintf("reconciler returned invalid state %q", outcome.State),
		)
	}
}

func agentReconciliationAcknowledged(result json.RawMessage) bool {
	var envelope struct {
		Reconciliation *struct {
			Acknowledged bool `json:"acknowledged"`
		} `json:"reconciliation"`
	}
	return len(result) > 0 &&
		json.Unmarshal(result, &envelope) == nil &&
		envelope.Reconciliation != nil &&
		envelope.Reconciliation.Acknowledged
}

func (service *Service) GC(
	ctx context.Context,
	options GCOptions,
) (GCResult, error) {
	if options.Before.IsZero() {
		return GCResult{}, fmt.Errorf("Run GC cutoff is required")
	}
	if options.Limit <= 0 || options.Limit > 1000 {
		options.Limit = 100
	}
	return service.store.GC(ctx, options)
}

func (service *Service) Close() error {
	return service.store.Close()
}

func (service *Service) validateRequest(
	request Request,
) *contract.RuntimeError {
	if len(request.Resume) != 0 {
		return runError(
			contract.ErrorInvalidRequest,
			"resume is Store-owned and cannot be supplied at Run submission",
		)
	}
	executor, exists := service.executors[request.Kind]
	if !exists {
		return runError(
			contract.ErrorInvalidRequest,
			fmt.Sprintf("unsupported run kind %q", request.Kind),
		)
	}
	if request.ProfileID == "" || request.Input == "" {
		return runError(contract.ErrorInvalidRequest, "profile_id and input are required")
	}
	if len(request.Input) > MaxResumeInputBytes {
		return runError(contract.ErrorInvalidRequest, "input exceeds 1048576 bytes")
	}
	if !utf8.ValidString(request.Input) ||
		strings.ContainsRune(request.Input, '\x00') ||
		!utf8.ValidString(request.TaskID) ||
		strings.ContainsRune(request.TaskID, '\x00') {
		return runError(
			contract.ErrorInvalidRequest,
			"input and task_id must be UTF-8 without NUL",
		)
	}
	if len(request.Labels) > 32 {
		return runError(contract.ErrorInvalidRequest, "labels exceed 32 items")
	}
	for key, value := range request.Labels {
		if key == "" || len(key) > 64 || len(value) > 512 ||
			!utf8.ValidString(key) || strings.ContainsRune(key, '\x00') ||
			!utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
			return runError(contract.ErrorInvalidRequest, "labels exceed size limits")
		}
	}
	if request.RetryOf != "" {
		if err := identity.Validate(request.RetryOf, "run"); err != nil {
			return runError(contract.ErrorInvalidRequest, err.Error())
		}
	}
	if err := executor.Validate(request); err != nil {
		return runError(contract.ErrorInvalidRequest, err.Error())
	}
	return nil
}

func cloneRequest(value Request) Request {
	value.Resume = append([]byte(nil), value.Resume...)
	value.PrivateRequest = append([]byte(nil), value.PrivateRequest...)
	if value.Labels != nil {
		labels := make(map[string]string, len(value.Labels))
		for key, current := range value.Labels {
			labels[key] = current
		}
		value.Labels = labels
	}
	return value
}

func runError(
	code contract.ErrorCode,
	message string,
) *contract.RuntimeError {
	if message == "" {
		message = "run failed"
	}
	return &contract.RuntimeError{
		Code: code, Phase: contract.PhaseRun, Message: message,
	}
}
