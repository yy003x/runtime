package httpx

import (
	"context"
	"errors"
	"net/http"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
)

type timeoutError struct{}

func (timeoutError) Error() string   { return "i/o timeout" }
func (timeoutError) Timeout() bool   { return true }
func (timeoutError) Temporary() bool { return true }

func TestProviderErrorMapping(t *testing.T) {
	cases := []struct {
		name          string
		status        int
		wantCode      contract.ErrorCode
		wantRetryable bool
	}{
		{"401 unauthorized", http.StatusUnauthorized, contract.ErrorAuthenticationFailed, false},
		{"403 forbidden", http.StatusForbidden, contract.ErrorPermissionDenied, false},
		{"429 too many requests", http.StatusTooManyRequests, contract.ErrorRateLimited, true},
		{"500 internal", 500, contract.ErrorProviderUnavailable, true},
		{"503 service unavailable", 503, contract.ErrorProviderUnavailable, true},
		{"400 bad request", http.StatusBadRequest, contract.ErrorProtocol, false},
		{"404 not found", http.StatusNotFound, contract.ErrorProtocol, false},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := ProviderError("prov", tc.status, http.Header{}, []byte("body"))
			if err.Code != tc.wantCode {
				t.Fatalf("code=%s want=%s", err.Code, tc.wantCode)
			}
			if err.Retryable != tc.wantRetryable {
				t.Fatalf("retryable=%v want=%v", err.Retryable, tc.wantRetryable)
			}
			if err.HTTPStatus != tc.status {
				t.Fatalf("http_status=%d want=%d", err.HTTPStatus, tc.status)
			}
			if err.Phase != contract.PhaseProvider {
				t.Fatalf("phase=%s want=%s", err.Phase, contract.PhaseProvider)
			}
		})
	}
}

func TestProviderErrorFallsBackToStatusText(t *testing.T) {
	err := ProviderError("prov", http.StatusTooManyRequests, http.Header{}, nil)
	if err.Message != http.StatusText(http.StatusTooManyRequests) {
		t.Fatalf("message=%q want=%q", err.Message, http.StatusText(http.StatusTooManyRequests))
	}
}

func TestNetworkErrorMapping(t *testing.T) {
	cases := []struct {
		name          string
		err           error
		wantCode      contract.ErrorCode
		wantRetryable bool
	}{
		{"canceled", context.Canceled, contract.ErrorCancelled, false},
		{"deadline exceeded", context.DeadlineExceeded, contract.ErrorTimeout, true},
		{"net timeout", timeoutError{}, contract.ErrorTimeout, true},
		{"generic network", errors.New("connection reset"), contract.ErrorProviderUnavailable, true},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			err := NetworkError("prov", tc.err)
			if err.Code != tc.wantCode {
				t.Fatalf("code=%s want=%s", err.Code, tc.wantCode)
			}
			if err.Retryable != tc.wantRetryable {
				t.Fatalf("retryable=%v want=%v", err.Retryable, tc.wantRetryable)
			}
			if err.Phase != contract.PhaseProvider {
				t.Fatalf("phase=%s want=%s", err.Phase, contract.PhaseProvider)
			}
		})
	}
}

func TestProtocolErrorMapping(t *testing.T) {
	err := ProtocolError("prov", "malformed response body")
	if err.Code != contract.ErrorProtocol {
		t.Fatalf("code=%s want=%s", err.Code, contract.ErrorProtocol)
	}
	if err.Retryable {
		t.Fatalf("retryable=true want=false")
	}
	if err.Phase != contract.PhaseProvider {
		t.Fatalf("phase=%s want=%s", err.Phase, contract.PhaseProvider)
	}
}
