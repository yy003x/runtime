package command

import (
	"errors"

	"github.com/yy003x/runtime/contract"
)

type invocationLimitError struct {
	message string
}

func (err *invocationLimitError) Error() string {
	if err == nil {
		return ""
	}
	return err.message
}

func typedBuildError(err error) error {
	if err == nil {
		return nil
	}
	var runtimeErr *contract.RuntimeError
	if errors.As(err, &runtimeErr) {
		return err
	}
	var limitErr *invocationLimitError
	if errors.As(err, &limitErr) {
		return &contract.RuntimeError{
			Code: contract.ErrorContextOverflow, Phase: contract.PhaseRequest,
			Message: limitErr.Error(),
		}
	}
	return &contract.RuntimeError{
		Code: contract.ErrorInvalidRequest, Phase: contract.PhaseProfile,
		Message: err.Error(),
	}
}

func typedDecodeError(err error) error {
	if err == nil {
		return nil
	}
	var runtimeErr *contract.RuntimeError
	if errors.As(err, &runtimeErr) {
		return err
	}
	return &contract.RuntimeError{
		Code:    contract.ErrorInvalidProviderResponse,
		Phase:   contract.PhaseTransport,
		Message: err.Error(),
	}
}
