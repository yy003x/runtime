package model

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"reflect"
	"sort"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/provider"
)

type Driver interface {
	ExecutionIdentity() DriverExecutionIdentity
	Validate(Profile) error
	Stream(
		context.Context,
		ResolvedModel,
		contract.ModelRequest,
		contract.EventSink,
	) (contract.ModelResult, provider.Attempt, *contract.RuntimeError)
}

type Generator interface {
	Generate(context.Context, contract.GenerateRequest) (contract.ModelResult, *contract.RuntimeError)
	GenerateStream(
		context.Context,
		contract.GenerateRequest,
		contract.EventSink,
	) (contract.ModelResult, *contract.RuntimeError)
}

type Service struct {
	catalog         *Catalog
	drivers         map[DriverName]Driver
	getenv          func(string) (string, bool)
	attemptObserver AttemptObserver
}

type ServiceOptions struct {
	Getenv          func(string) (string, bool)
	AttemptObserver AttemptObserver
}

func NewService(
	catalog *Catalog,
	drivers map[DriverName]Driver,
	options ServiceOptions,
) (*Service, error) {
	if catalog == nil {
		return nil, fmt.Errorf("model catalog is required")
	}
	if len(drivers) == 0 {
		return nil, fmt.Errorf("at least one model driver is required")
	}
	values := make(map[DriverName]Driver, len(drivers))
	for name, driver := range drivers {
		if driver == nil {
			return nil, fmt.Errorf("model driver %q is nil", name)
		}
		identity := driver.ExecutionIdentity()
		if err := identity.Validate(); err != nil {
			return nil, fmt.Errorf("model driver %q: %w", name, err)
		}
		if identity.Driver != name {
			return nil, fmt.Errorf(
				"model driver %q execution identity declares driver %q",
				name,
				identity.Driver,
			)
		}
		values[name] = driver
	}
	for _, id := range catalog.IDs() {
		profile, _ := catalog.Get(id)
		driver, exists := values[profile.Driver]
		if !exists {
			return nil, fmt.Errorf("model profile %q references unregistered driver %q", id, profile.Driver)
		}
		if err := driver.Validate(profile); err != nil {
			return nil, fmt.Errorf("model profile %q: %w", id, err)
		}
	}
	getenv := options.Getenv
	if getenv == nil {
		getenv = os.LookupEnv
	}
	return &Service{
		catalog: catalog, drivers: values, getenv: getenv,
		attemptObserver: options.AttemptObserver,
	}, nil
}

// ExecutionSnapshot freezes the non-secret API Profile and the semantic
// identity of the concrete driver selected by this Service. Header ${VAR}
// references are kept unresolved so no secret value enters the snapshot.
func (service *Service) ExecutionSnapshot(
	profileID string,
) (ExecutionSnapshot, error) {
	if service == nil || service.catalog == nil {
		return ExecutionSnapshot{}, fmt.Errorf("model service is unavailable")
	}
	profile, exists := service.catalog.Get(profileID)
	if !exists {
		return ExecutionSnapshot{}, fmt.Errorf(
			"model profile %q was not found", profileID,
		)
	}
	driver, exists := service.drivers[profile.Driver]
	if !exists || driver == nil {
		return ExecutionSnapshot{}, fmt.Errorf(
			"model profile %q driver %q is unavailable",
			profileID,
			profile.Driver,
		)
	}
	identity := driver.ExecutionIdentity()
	if err := identity.Validate(); err != nil {
		return ExecutionSnapshot{}, fmt.Errorf(
			"model profile %q driver identity: %w", profileID, err,
		)
	}
	if identity.Driver != profile.Driver {
		return ExecutionSnapshot{}, fmt.Errorf(
			"model profile %q driver identity does not match Profile",
			profileID,
		)
	}
	return newExecutionSnapshot(profileID, profile, identity)
}

func (service *Service) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return service.GenerateStream(ctx, request, nil)
}

func (service *Service) GenerateStream(
	ctx context.Context,
	request contract.GenerateRequest,
	sink contract.EventSink,
) (result contract.ModelResult, resultError *contract.RuntimeError) {
	if err := request.Validate(); err != nil {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseRequest, err.Error(),
		)
	}
	profile, exists := service.catalog.Get(request.ModelProfile)
	if !exists {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInvalidRequest,
			contract.PhaseProfile,
			fmt.Sprintf("model profile %q was not found", request.ModelProfile),
		)
	}
	driver, exists := service.drivers[profile.Driver]
	if !exists {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInternal, contract.PhaseProfile, "model driver is unavailable",
		)
	}
	resolved, secrets, err := service.catalog.resolve(request.ModelProfile, service.getenv)
	if err != nil {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorAuthenticationFailed, contract.PhaseProfile, err.Error(),
		)
	}
	input, err := cloneValue(request.Input)
	if err != nil {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInternal, contract.PhaseRequest, "cannot clone model request",
		)
	}
	if tokenLimit := profile.DefaultTokenLimit(); input.Options.MaxOutputTokens == nil &&
		tokenLimit != nil {
		value := *tokenLimit
		input.Options.MaxOutputTokens = &value
	}
	if input.Options.Temperature == nil && profile.Parameters.Temperature != nil {
		value := *profile.Parameters.Temperature
		input.Options.Temperature = &value
	}
	if input.Options.TopP == nil && profile.Parameters.TopP != nil {
		value := *profile.Parameters.TopP
		input.Options.TopP = &value
	}
	if len(input.Options.StopSequences) == 0 &&
		len(profile.Parameters.StopSequences) > 0 {
		input.Options.StopSequences = append(
			[]string(nil), profile.Parameters.StopSequences...,
		)
	}
	callContext, cancel := context.WithTimeout(ctx, time.Duration(resolved.Timeout))
	defer cancel()

	state := streamState{
		sink: sink, secrets: secrets, expected: 1,
		toolCalls: make(map[string]string),
	}
	result, wireAttempt, callError := driver.Stream(
		callContext, resolved, input, state.accept,
	)
	if wireAttempt.Started && service.attemptObserver != nil {
		defer func() {
			attempt := Attempt{
				Origin: attemptOrigin(ctx), ProfileID: request.ModelProfile,
				Wire: wireAttempt, Error: resultError,
			}
			safeAttempt, redactErr := redactValue(attempt, secrets)
			if redactErr != nil {
				return
			}
			notifyAttemptObserver(service.attemptObserver, safeAttempt)
		}()
	}
	if state.failure != nil {
		return contract.ModelResult{}, state.failure
	}
	if state.sinkFailure != nil {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorCancelled, contract.PhaseConsumer, "event sink stopped",
		)
	}
	if callError != nil {
		return contract.ModelResult{}, redactRuntimeError(callError, secrets)
	}
	if callContext.Err() != nil {
		code := contract.ErrorCancelled
		if errors.Is(callContext.Err(), context.DeadlineExceeded) {
			code = contract.ErrorTimeout
		}
		return contract.ModelResult{}, runtimeError(code, contract.PhaseProvider, callContext.Err().Error())
	}
	result, err = redactValue(result, secrets)
	if err != nil {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInternal, contract.PhaseProvider, "cannot sanitize model result",
		)
	}
	if err := result.Validate(); err != nil {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInvalidProviderResponse, contract.PhaseProvider, err.Error(),
		)
	}
	if !state.started {
		return contract.ModelResult{}, runtimeError(
			contract.ErrorInvalidProviderResponse,
			contract.PhaseProvider,
			"driver did not emit model.started",
		)
	}
	if err := state.complete(result); err != nil {
		return contract.ModelResult{}, err
	}
	return result, nil
}

func notifyAttemptObserver(observer AttemptObserver, attempt Attempt) {
	defer func() { _ = recover() }()
	observer(attempt)
}

type streamState struct {
	sink             contract.EventSink
	secrets          []string
	expected         uint64
	started          bool
	pendingCompleted *contract.Event
	toolCalls        map[string]string
	failure          *contract.RuntimeError
	sinkFailure      error
}

func (state *streamState) accept(event contract.Event) error {
	if state.failure != nil || state.sinkFailure != nil {
		return fmt.Errorf("model stream is already failed")
	}
	event, err := redactValue(event, state.secrets)
	if err != nil {
		state.failure = runtimeError(
			contract.ErrorInternal, contract.PhaseProvider, "cannot sanitize model event",
		)
		return state.failure
	}
	if event.Sequence != state.expected {
		state.failure = runtimeError(
			contract.ErrorInvalidProviderResponse,
			contract.PhaseProvider,
			fmt.Sprintf("event sequence=%d, want %d", event.Sequence, state.expected),
		)
		return state.failure
	}
	state.expected++
	if event.Sequence == 1 {
		if event.Type != contract.EventModelStarted {
			state.failure = runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				"first model event must be model.started",
			)
			return state.failure
		}
		state.started = true
	} else if event.Type == contract.EventModelStarted {
		state.failure = runtimeError(
			contract.ErrorInvalidProviderResponse,
			contract.PhaseProvider,
			"model.started can only be the first event",
		)
		return state.failure
	}
	if state.pendingCompleted != nil {
		state.failure = runtimeError(
			contract.ErrorInvalidProviderResponse,
			contract.PhaseProvider,
			"event follows model.completed",
		)
		return state.failure
	}
	if event.Type == contract.EventModelCompleted {
		if event.Model != nil &&
			(event.Model.Text != "" || event.Model.ToolCallID != "" ||
				event.Model.ToolCall != nil) {
			state.failure = runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				"model.completed contains an invalid payload",
			)
			return state.failure
		}
		state.pendingCompleted = &event
		return nil
	}
	if err := event.Validate(); err != nil {
		state.failure = runtimeError(
			contract.ErrorInvalidProviderResponse, contract.PhaseProvider, err.Error(),
		)
		return state.failure
	}
	switch event.Type {
	case contract.EventToolCallStarted:
		call := event.Model.ToolCall
		if _, exists := state.toolCalls[call.ID]; exists {
			state.failure = runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				fmt.Sprintf("tool call id %q was started more than once", call.ID),
			)
			return state.failure
		}
		state.toolCalls[call.ID] = call.Name
	case contract.EventToolCallArgumentsDelta:
		if _, exists := state.toolCalls[event.Model.ToolCallID]; !exists {
			state.failure = runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				fmt.Sprintf(
					"tool arguments delta references unknown call id %q",
					event.Model.ToolCallID,
				),
			)
			return state.failure
		}
	}
	return state.emit(event)
}

func (state *streamState) complete(result contract.ModelResult) *contract.RuntimeError {
	resultCalls := make(map[string]string, len(result.Message.ToolCalls))
	for _, call := range result.Message.ToolCalls {
		startedName, exists := state.toolCalls[call.ID]
		if !exists {
			return runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				fmt.Sprintf("result contains tool call id %q without a start event", call.ID),
			)
		}
		if startedName != call.Name {
			return runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				fmt.Sprintf("tool call id %q changed name", call.ID),
			)
		}
		resultCalls[call.ID] = call.Name
	}
	for id := range state.toolCalls {
		if _, exists := resultCalls[id]; !exists {
			return runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				fmt.Sprintf("started tool call id %q is missing from the result", id),
			)
		}
	}
	var completed contract.Event
	if state.pendingCompleted == nil {
		completed = contract.Event{
			Sequence: state.expected,
			Type:     contract.EventModelCompleted,
			Model:    &contract.ModelEvent{Result: &result},
		}
		state.expected++
	} else {
		completed = *state.pendingCompleted
		if completed.Model == nil {
			completed.Model = &contract.ModelEvent{}
		}
		if completed.Model.Result != nil && !reflect.DeepEqual(*completed.Model.Result, result) {
			return runtimeError(
				contract.ErrorInvalidProviderResponse,
				contract.PhaseProvider,
				"model.completed result differs from driver result",
			)
		}
		completed.Model.Result = &result
	}
	if err := completed.Validate(); err != nil {
		return runtimeError(
			contract.ErrorInvalidProviderResponse, contract.PhaseProvider, err.Error(),
		)
	}
	if err := state.emit(completed); err != nil {
		if state.failure != nil {
			return state.failure
		}
		return runtimeError(
			contract.ErrorCancelled, contract.PhaseConsumer, "event sink stopped",
		)
	}
	return nil
}

func (state *streamState) emit(event contract.Event) error {
	if state.sink == nil {
		return nil
	}
	current, err := cloneValue(event)
	if err != nil {
		state.failure = runtimeError(
			contract.ErrorInternal, contract.PhaseConsumer, "cannot clone model event",
		)
		return state.failure
	}
	if err := state.sink(current); err != nil {
		state.sinkFailure = err
		return err
	}
	return nil
}

func runtimeError(
	code contract.ErrorCode,
	phase contract.ErrorPhase,
	message string,
) *contract.RuntimeError {
	return &contract.RuntimeError{
		Code: code, Phase: phase, Message: message, Retryable: false,
	}
}

func redactRuntimeError(value *contract.RuntimeError, secrets []string) *contract.RuntimeError {
	if value == nil {
		return nil
	}
	result, err := redactValue(*value, secrets)
	if err != nil {
		return runtimeError(
			contract.ErrorInternal, contract.PhaseProvider, "cannot sanitize provider error",
		)
	}
	if validationErr := result.Validate(); validationErr != nil {
		return runtimeError(
			contract.ErrorInvalidProviderResponse,
			contract.PhaseProvider,
			validationErr.Error(),
		)
	}
	return &result
}

func redactValue[T any](value T, secrets []string) (T, error) {
	if len(secrets) == 0 {
		return cloneValue(value)
	}
	data, err := json.Marshal(value)
	if err != nil {
		var zero T
		return zero, err
	}
	ordered := append([]string(nil), secrets...)
	sort.Slice(ordered, func(left, right int) bool {
		return len(ordered[left]) > len(ordered[right])
	})
	for _, secret := range ordered {
		if secret == "" {
			continue
		}
		encodedSecret, err := json.Marshal(secret)
		if err != nil {
			var zero T
			return zero, err
		}
		if len(encodedSecret) >= 2 {
			data = bytes.ReplaceAll(data, encodedSecret[1:len(encodedSecret)-1], []byte("[REDACTED]"))
		}
		data = bytes.ReplaceAll(data, []byte(secret), []byte("[REDACTED]"))
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		var zero T
		return zero, err
	}
	return result, nil
}

func cloneValue[T any](value T) (T, error) {
	data, err := json.Marshal(value)
	if err != nil {
		var zero T
		return zero, err
	}
	var result T
	if err := json.Unmarshal(data, &result); err != nil {
		var zero T
		return zero, err
	}
	return result, nil
}

func (resolved ResolvedModel) String() string {
	return fmt.Sprintf(
		"model=%s driver=%s endpoint=%s profile=%s headers=[REDACTED]",
		resolved.Model,
		resolved.Driver,
		resolved.Endpoint,
		resolved.ID,
	)
}
