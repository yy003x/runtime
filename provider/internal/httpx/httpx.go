package httpx

import (
	"context"
	"errors"
	"fmt"
	"io"
	"net"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yy003x/runtime/contract"
)

const (
	MaxResponseBytes = int64(8 << 20)
	MaxErrorBytes    = int64(64 << 10)
)

func ReadLimited(reader io.Reader, limit int64) ([]byte, error) {
	value, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return value, nil
}

func ProviderError(provider string, status int, header http.Header, body []byte) *contract.RuntimeError {
	code := contract.ErrorProtocol
	retryable := false
	switch status {
	case http.StatusUnauthorized:
		code = contract.ErrorAuthenticationFailed
	case http.StatusForbidden:
		code = contract.ErrorPermissionDenied
	case http.StatusTooManyRequests:
		code = contract.ErrorRateLimited
		retryable = true
	default:
		if status >= 500 {
			code = contract.ErrorProviderUnavailable
			retryable = true
		}
	}
	message := strings.TrimSpace(string(body))
	if message == "" {
		message = http.StatusText(status)
	}
	return &contract.RuntimeError{
		Code: code, Phase: contract.PhaseProvider,
		Message: message, Retryable: retryable, HTTPStatus: status,
		Provider: provider, RetryAfterMS: RetryAfterMilliseconds(header.Get("Retry-After")),
	}
}

func NetworkError(provider string, err error) *contract.RuntimeError {
	code := contract.ErrorProviderUnavailable
	if errors.Is(err, context.Canceled) {
		code = contract.ErrorCancelled
	} else if errors.Is(err, context.DeadlineExceeded) {
		code = contract.ErrorTimeout
	} else {
		var networkError net.Error
		if errors.As(err, &networkError) && networkError.Timeout() {
			code = contract.ErrorTimeout
		}
	}
	return &contract.RuntimeError{
		Code: code, Phase: contract.PhaseProvider,
		Message: err.Error(), Retryable: code == contract.ErrorProviderUnavailable || code == contract.ErrorTimeout,
		Provider: provider,
	}
}

func ProtocolError(provider, message string) *contract.RuntimeError {
	return &contract.RuntimeError{
		Code: contract.ErrorProtocol, Phase: contract.PhaseProvider,
		Message: message, Provider: provider,
	}
}

func RetryAfterMilliseconds(value string) int64 {
	value = strings.TrimSpace(value)
	if value == "" {
		return 0
	}
	if seconds, err := strconv.ParseInt(value, 10, 64); err == nil {
		if seconds < 0 {
			return 0
		}
		return seconds * 1000
	}
	when, err := http.ParseTime(value)
	if err != nil {
		return 0
	}
	wait := time.Until(when)
	if wait <= 0 {
		return 0
	}
	return wait.Milliseconds()
}
