package faux

import (
	"context"
	"fmt"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/runtimetest/scenario"
)

const ScenarioLabel = "runtime.faux.scenario"

const (
	executionImplementation        = "runtime.runtimetest.faux"
	executionImplementationVersion = 1
)

type Driver struct {
	provider *Provider
}

func NewDriver(provider *Provider) (*Driver, error) {
	if provider == nil {
		return nil, fmt.Errorf("faux provider is required")
	}
	return &Driver{provider: provider}, nil
}

func (*Driver) ExecutionIdentity() model.DriverExecutionIdentity {
	return model.DriverExecutionIdentity{
		Driver:                model.DriverOpenAI,
		Implementation:        executionImplementation,
		ImplementationVersion: executionImplementationVersion,
	}
}

func (driver *Driver) Validate(model.Profile) error {
	return nil
}

func (driver *Driver) Stream(
	ctx context.Context,
	_ model.ResolvedModel,
	request contract.ModelRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	name := request.Trace.Labels[ScenarioLabel]
	if name == "" {
		return contract.ModelResult{}, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseRequest,
			Message: fmt.Sprintf("trace.labels[%q] is required", ScenarioLabel),
		}
	}
	var conversionError error
	result, runtimeErr := driver.provider.Stream(ctx, name, func(event scenario.Event) error {
		current, err := scenario.ToContractEvent(event)
		if err != nil {
			conversionError = err
			return err
		}
		if sink == nil {
			return nil
		}
		return sink(current)
	})
	if conversionError != nil {
		return contract.ModelResult{}, &contract.RuntimeError{
			Code: contract.ErrorInvalidProviderResponse, Phase: contract.PhaseProvider,
			Message: conversionError.Error(),
		}
	}
	if runtimeErr != nil {
		current, err := scenario.ToContractError(*runtimeErr)
		if err != nil {
			return contract.ModelResult{}, &contract.RuntimeError{
				Code: contract.ErrorInvalidProviderResponse, Phase: contract.PhaseProvider,
				Message: err.Error(),
			}
		}
		return contract.ModelResult{}, &current
	}
	current, err := scenario.ToContractResult(result)
	if err != nil {
		return contract.ModelResult{}, &contract.RuntimeError{
			Code: contract.ErrorInvalidProviderResponse, Phase: contract.PhaseProvider,
			Message: err.Error(),
		}
	}
	return current, nil
}
