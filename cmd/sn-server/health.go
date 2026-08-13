package main

import (
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"

	"github.com/yy003x/runtime/pkg/contract"
)

// readinessState is deliberately limited to a boolean. Probe responses must
// never expose worker identifiers, store errors, paths, or other diagnostics.
type readinessState struct {
	ready atomic.Bool
}

func (state *readinessState) Set(ready bool) {
	state.ready.Store(ready)
}

func (state *readinessState) Ready() bool {
	return state != nil && state.ready.Load()
}

// newServerHandler applies configured bearer authentication to every route,
// including health probes. Auth is evaluated before readiness so an
// unauthenticated caller cannot observe execution-plane state.
func newServerHandler(
	readiness *readinessState,
	bearerToken string,
	runtimeHandler http.Handler,
) http.Handler {
	handler := healthProbes(
		readiness,
		rejectRunSubmissionsWhenNotReady(readiness, runtimeHandler),
	)
	if bearerToken != "" {
		handler = bearerAuth(bearerToken, handler)
	}
	return handler
}

func healthProbes(
	readiness *readinessState,
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/healthz":
			writeProbe(writer, request, http.StatusOK, "ok")
			return
		case "/readyz":
			if readiness.Ready() {
				writeProbe(writer, request, http.StatusOK, "ready")
				return
			}
			writeProbe(
				writer, request, http.StatusServiceUnavailable, "not_ready",
			)
			return
		default:
			next.ServeHTTP(writer, request)
		}
	})
}

func writeProbe(
	writer http.ResponseWriter,
	request *http.Request,
	status int,
	value string,
) {
	if request.Method != http.MethodGet {
		writer.Header().Set("Allow", http.MethodGet)
		status = http.StatusMethodNotAllowed
		value = "method_not_allowed"
	}
	writeServerJSON(writer, status, map[string]string{"status": value})
}

func rejectRunSubmissionsWhenNotReady(
	readiness *readinessState,
	next http.Handler,
) http.Handler {
	return http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if isRunSubmission(request) && !readiness.Ready() {
			writeServerJSON(writer, http.StatusServiceUnavailable, map[string]any{
				"error": &contract.RuntimeError{
					Code:       contract.ErrorInternal,
					Phase:      contract.PhaseRun,
					Message:    "durable Run execution is unavailable",
					Retryable:  true,
					HTTPStatus: http.StatusServiceUnavailable,
				},
			})
			return
		}
		next.ServeHTTP(writer, request)
	})
}

func isRunSubmission(request *http.Request) bool {
	return request.Method == http.MethodPost &&
		strings.Trim(request.URL.Path, "/") == "v1/runs"
}

func writeServerJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
