package run

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
)

type Service struct {
	store     Store
	executors map[Kind]Executor
	now       func() time.Time

	activeMu sync.Mutex
	active   map[string]context.CancelFunc
}

type ServiceOptions struct {
	Store     Store
	Executors map[Kind]Executor
	Now       func() time.Time
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
	return &Service{
		store: options.Store, executors: executors, now: options.Now,
		active: make(map[string]context.CancelFunc),
	}, nil
}

func (service *Service) Submit(
	ctx context.Context,
	request Request,
) (Record, *contract.RuntimeError) {
	if runtimeErr := service.validateRequest(request); runtimeErr != nil {
		return Record{}, runtimeErr
	}
	runID, err := identity.New("run")
	if err != nil {
		return Record{}, runError(contract.ErrorInternal, err.Error())
	}
	value, err := service.store.Create(ctx, runID, cloneRequest(request))
	if err != nil {
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
	executor, exists := service.executors[record.Request.Kind]
	if !exists {
		runtimeErr := runError(
			contract.ErrorInvalidRequest,
			fmt.Sprintf("no executor is registered for run kind %q", record.Request.Kind),
		)
		value, _ := service.store.Settle(
			context.WithoutCancel(ctx), record.ID, StateFailed, nil, runtimeErr,
		)
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
	persistContext := context.WithoutCancel(ctx)
	switch outcome.State {
	case StatePaused:
		value, err := service.store.Pause(persistContext, record.ID, outcome.Pause)
		if err != nil {
			return Record{}, runError(contract.ErrorInternal, err.Error())
		}
		return value, nil
	case StateNeedsReconciliation:
		value, err := service.store.NeedsReconciliation(
			persistContext, record.ID, outcome.Error,
		)
		if err != nil {
			return Record{}, runError(contract.ErrorInternal, err.Error())
		}
		return value, outcome.Error
	case StateCompleted, StateFailed, StateCancelled:
		value, err := service.store.Settle(
			persistContext, record.ID, outcome.State, outcome.Result, outcome.Error,
		)
		if err != nil {
			return Record{}, runError(contract.ErrorInternal, err.Error())
		}
		return value, outcome.Error
	default:
		runtimeErr := runError(
			contract.ErrorInternal,
			fmt.Sprintf("executor returned invalid state %q", outcome.State),
		)
		value, err := service.store.Settle(
			persistContext, record.ID, StateFailed, nil, runtimeErr,
		)
		if err != nil {
			return Record{}, runError(contract.ErrorInternal, err.Error())
		}
		return value, runtimeErr
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
	return service.store.List(ctx, filter)
}

func (service *Service) Events(
	ctx context.Context,
	runID string,
	afterSequence uint64,
	limit int,
) ([]contract.Event, error) {
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
	service.activeMu.Lock()
	cancel := service.active[runID]
	service.activeMu.Unlock()
	if cancel != nil {
		cancel()
	}
	return value, nil
}

func (service *Service) Resume(
	ctx context.Context,
	runID string,
	input json.RawMessage,
) (Record, error) {
	return service.store.Resume(ctx, runID, append([]byte(nil), input...))
}

func (service *Service) Reconcile(ctx context.Context) error {
	return service.store.Reconcile(ctx)
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
	if len(request.Input) > 1<<20 {
		return runError(contract.ErrorInvalidRequest, "input exceeds 1048576 bytes")
	}
	if len(request.Labels) > 32 {
		return runError(contract.ErrorInvalidRequest, "labels exceed 32 items")
	}
	for key, value := range request.Labels {
		if key == "" || len(key) > 64 || len(value) > 512 {
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
	value.CommandArgs = append([]string(nil), value.CommandArgs...)
	value.Resume = append([]byte(nil), value.Resume...)
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
