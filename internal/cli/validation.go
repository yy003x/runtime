package cli

import (
	"errors"
	"fmt"
)

// cliValidationError 只标记由 CLI 参数解码和校验负责的错误。下游 Store、文件系统、
// 进程和输出错误不会因此被归类；除非领域显式返回 canonical RuntimeError，否则仍按
// internal 处理。
type cliValidationError struct {
	cause error
}

func (err *cliValidationError) Error() string {
	if err == nil || err.cause == nil {
		return ""
	}
	return err.cause.Error()
}

func (err *cliValidationError) Unwrap() error {
	if err == nil {
		return nil
	}
	return err.cause
}

func cliValidation(err error) error {
	if err == nil {
		return nil
	}
	var marked *cliValidationError
	if errors.As(err, &marked) {
		return err
	}
	return &cliValidationError{cause: err}
}

func cliValidationf(format string, args ...any) error {
	return cliValidation(fmt.Errorf(format, args...))
}
