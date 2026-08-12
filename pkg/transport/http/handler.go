// Package http provides the thin JSON/SSE adapters mounted by SN Runtime.
package http

import (
	"encoding/json"
	"fmt"
	"mime"
	nethttp "net/http"
	"strconv"
	"strings"

	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
)

const maxRequestBytes int64 = 1 << 20

type Handler struct {
	generator model.Generator
}

func NewHandler(generator model.Generator) *Handler {
	return &Handler{generator: generator}
}

func (handler *Handler) ServeHTTP(writer nethttp.ResponseWriter, request *nethttp.Request) {
	if request.URL.Path != "/v1/model/generate" {
		nethttp.NotFound(writer, request)
		return
	}
	if request.Method != nethttp.MethodPost {
		writer.Header().Set("Allow", nethttp.MethodPost)
		writeError(writer, nethttp.StatusMethodNotAllowed, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseTransport,
			Message: "method must be POST",
		})
		return
	}
	if handler.generator == nil {
		writeError(writer, nethttp.StatusServiceUnavailable, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseTransport,
			Message: "model generator is unavailable",
		})
		return
	}
	if !hasJSONContentType(request) {
		writeError(writer, nethttp.StatusUnsupportedMediaType, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseTransport,
			Message: "Content-Type must be application/json",
		})
		return
	}
	var input contract.GenerateRequest
	if err := strictjson.DecodeObject(
		request.Body, maxRequestBytes, &input,
	); err != nil {
		writeError(writer, nethttp.StatusBadRequest, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseTransport,
			Message: err.Error(),
		})
		return
	}
	if err := input.Validate(); err != nil {
		writeError(writer, nethttp.StatusBadRequest, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseRequest,
			Message: err.Error(),
		})
		return
	}
	if acceptsEventStream(request) {
		handler.stream(writer, request, input)
		return
	}
	modelContext := model.WithAttemptOrigin(request.Context(), model.AttemptOrigin{
		Namespace: model.AttemptNamespaceRequest,
		Source:    "POST /v1/model/generate",
	})
	result, runtimeErr := handler.generator.Generate(modelContext, input)
	if runtimeErr != nil {
		writeError(writer, statusForError(runtimeErr), runtimeErr)
		return
	}
	writeJSON(writer, nethttp.StatusOK, result)
}

func (handler *Handler) stream(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	input contract.GenerateRequest,
) {
	flusher, ok := writer.(nethttp.Flusher)
	if !ok {
		writeError(writer, nethttp.StatusInternalServerError, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseTransport,
			Message: "streaming is unsupported",
		})
		return
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	modelContext := model.WithAttemptOrigin(request.Context(), model.AttemptOrigin{
		Namespace: model.AttemptNamespaceRequest,
		Source:    "POST /v1/model/generate",
	})
	_, runtimeErr := handler.generator.GenerateStream(
		modelContext,
		input,
		func(event contract.Event) error {
			data, err := json.Marshal(event)
			if err != nil {
				return err
			}
			if _, err := fmt.Fprintf(
				writer,
				"id: %s\nevent: %s\ndata: %s\n\n",
				strconv.FormatUint(event.Sequence, 10),
				event.Type,
				data,
			); err != nil {
				return err
			}
			flusher.Flush()
			return nil
		},
	)
	if runtimeErr == nil {
		return
	}
	data, err := json.Marshal(runtimeErr)
	if err != nil {
		return
	}
	_, _ = fmt.Fprintf(writer, "event: error\ndata: %s\n\n", data)
	flusher.Flush()
}

func hasJSONContentType(request *nethttp.Request) bool {
	contentType := request.Header.Get("Content-Type")
	if contentType == "" {
		return false
	}
	mediaType, _, err := mime.ParseMediaType(contentType)
	return err == nil && mediaType == "application/json"
}

func acceptsEventStream(request *nethttp.Request) bool {
	for _, value := range strings.Split(request.Header.Get("Accept"), ",") {
		mediaType, _, err := mime.ParseMediaType(strings.TrimSpace(value))
		if err == nil && mediaType == "text/event-stream" {
			return true
		}
	}
	return false
}

func writeError(
	writer nethttp.ResponseWriter,
	status int,
	runtimeErr *contract.RuntimeError,
) {
	writeJSON(writer, status, struct {
		Error *contract.RuntimeError `json:"error"`
	}{Error: runtimeErr})
}

func writeJSON(writer nethttp.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.Header().Set("Cache-Control", "no-store")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}

func statusForError(runtimeErr *contract.RuntimeError) int {
	if runtimeErr == nil {
		return nethttp.StatusInternalServerError
	}
	if runtimeErr.HTTPStatus >= 400 && runtimeErr.HTTPStatus <= 599 {
		return runtimeErr.HTTPStatus
	}
	switch runtimeErr.Code {
	case contract.ErrorInvalidRequest:
		return nethttp.StatusBadRequest
	case contract.ErrorContextOverflow:
		return nethttp.StatusRequestEntityTooLarge
	case contract.ErrorAuthenticationFailed:
		return nethttp.StatusUnauthorized
	case contract.ErrorPermissionDenied:
		return nethttp.StatusForbidden
	case contract.ErrorRateLimited:
		return nethttp.StatusTooManyRequests
	case contract.ErrorTimeout:
		return nethttp.StatusGatewayTimeout
	case contract.ErrorProviderUnavailable:
		return nethttp.StatusServiceUnavailable
	case contract.ErrorInvalidProviderResponse, contract.ErrorProtocol:
		return nethttp.StatusBadGateway
	case contract.ErrorConflict:
		return nethttp.StatusConflict
	case contract.ErrorNotFound:
		return nethttp.StatusNotFound
	case contract.ErrorCancelled:
		return 499
	default:
		return nethttp.StatusInternalServerError
	}
}
