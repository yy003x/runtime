package http

import (
	"context"
	"encoding/json"
	"fmt"
	nethttp "net/http"
	"strconv"
	"strings"
	"time"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/strictjson"
	runtime "github.com/yy003x/runtime/run"
	"github.com/yy003x/runtime/session"
)

type RuntimeHandler struct {
	model            *Handler
	sessions         *session.Service
	runs             *runtime.Service
	agentBudget      agent.Budget
	settledRetention time.Duration
}

type RuntimeServices struct {
	Model            *Handler
	Sessions         *session.Service
	Runs             *runtime.Service
	AgentBudget      agent.Budget
	SettledRetention time.Duration
}

func NewRuntimeHandler(services RuntimeServices) (*RuntimeHandler, error) {
	if services.Model == nil || services.Sessions == nil || services.Runs == nil {
		return nil, fmt.Errorf("model, Session, and Run services are required")
	}
	if services.AgentBudget.MaxRounds == 0 &&
		services.AgentBudget.MaxToolCalls == 0 &&
		services.AgentBudget.MaxTotalTokens == 0 &&
		services.AgentBudget.MaxWallTime == 0 {
		services.AgentBudget = agent.DefaultBudget()
	}
	if services.SettledRetention <= 0 {
		services.SettledRetention = 168 * time.Hour
	}
	return &RuntimeHandler{
		model: services.Model, sessions: services.Sessions, runs: services.Runs,
		agentBudget:      services.AgentBudget,
		settledRetention: services.SettledRetention,
	}, nil
}

func (handler *RuntimeHandler) ServeHTTP(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
) {
	if request.URL.Path == "/v1/model/generate" {
		handler.model.ServeHTTP(writer, request)
		return
	}
	segments := splitPath(request.URL.Path)
	if len(segments) < 2 || segments[0] != "v1" {
		nethttp.NotFound(writer, request)
		return
	}
	switch segments[1] {
	case "sessions":
		handler.serveSessions(writer, request, segments[2:])
	case "agent":
		handler.serveAgent(writer, request, segments[2:])
	case "runs":
		handler.serveRuns(writer, request, segments[2:])
	default:
		nethttp.NotFound(writer, request)
	}
}

func (handler *RuntimeHandler) serveSessions(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	path []string,
) {
	if len(path) == 0 {
		switch request.Method {
		case nethttp.MethodGet:
			state := session.SessionState(request.URL.Query().Get("state"))
			values, err := handler.sessions.List(session.ListFilter{State: state})
			if err != nil {
				writeInternalError(writer, err)
				return
			}
			writeJSON(writer, nethttp.StatusOK, map[string]any{"sessions": values})
		case nethttp.MethodPost:
			var input struct {
				Retention session.Retention `json:"retention,omitempty"`
			}
			if !decodeJSONRequest(writer, request, &input) {
				return
			}
			value, err := handler.sessions.Create(input.Retention)
			if err != nil {
				writeError(writer, nethttp.StatusBadRequest, requestError(err))
				return
			}
			writeJSON(writer, nethttp.StatusCreated, value)
		default:
			methodNotAllowed(writer, "GET, POST")
		}
		return
	}
	if len(path) == 1 && path[0] == "gc" {
		if request.Method != nethttp.MethodPost {
			methodNotAllowed(writer, nethttp.MethodPost)
			return
		}
		var input struct {
			OlderThanHours int  `json:"older_than_hours,omitempty"`
			Limit          int  `json:"limit,omitempty"`
			Apply          bool `json:"apply"`
		}
		if !decodeJSONRequest(writer, request, &input) {
			return
		}
		value, err := handler.sessions.GC(session.GCOptions{
			OlderThan: time.Duration(input.OlderThanHours) * time.Hour,
			Limit:     input.Limit, Apply: input.Apply,
		})
		if err != nil {
			writeInternalError(writer, err)
			return
		}
		writeJSON(writer, nethttp.StatusOK, value)
		return
	}
	sessionID := path[0]
	if len(path) == 1 && strings.HasSuffix(sessionID, ":reconcile") {
		handler.reconcileSession(
			writer, request, strings.TrimSuffix(sessionID, ":reconcile"),
		)
		return
	}
	if len(path) == 1 {
		if request.Method != nethttp.MethodGet {
			methodNotAllowed(writer, nethttp.MethodGet)
			return
		}
		value, err := handler.sessions.Get(sessionID)
		if err != nil {
			writeError(writer, nethttp.StatusNotFound, requestError(err))
			return
		}
		writeJSON(writer, nethttp.StatusOK, value)
		return
	}
	switch path[1] {
	case "messages":
		if request.Method != nethttp.MethodGet {
			methodNotAllowed(writer, nethttp.MethodGet)
			return
		}
		after, ok := querySequence(writer, request, "after_seq")
		if !ok {
			return
		}
		values, err := handler.sessions.Messages(sessionID, after)
		if err != nil {
			writeInternalError(writer, err)
			return
		}
		writeJSON(writer, nethttp.StatusOK, map[string]any{"messages": values})
	case "events":
		if request.Method != nethttp.MethodGet {
			methodNotAllowed(writer, nethttp.MethodGet)
			return
		}
		after, ok := querySequence(writer, request, "after_seq")
		if !ok {
			return
		}
		values, err := handler.sessions.Events(sessionID, after)
		if err != nil {
			writeInternalError(writer, err)
			return
		}
		writeJSON(writer, nethttp.StatusOK, map[string]any{"events": values})
	case "executions":
		if request.Method != nethttp.MethodGet {
			methodNotAllowed(writer, nethttp.MethodGet)
			return
		}
		if len(path) == 2 {
			values, err := handler.sessions.Executions(sessionID)
			if err != nil {
				writeInternalError(writer, err)
				return
			}
			writeJSON(
				writer, nethttp.StatusOK,
				map[string]any{"executions": values},
			)
			return
		}
		if len(path) == 3 {
			value, err := handler.sessions.Execution(sessionID, path[2])
			if err != nil {
				writeError(writer, nethttp.StatusNotFound, requestError(err))
				return
			}
			writeJSON(writer, nethttp.StatusOK, value)
			return
		}
		nethttp.NotFound(writer, request)
	case "watch":
		handler.watchSession(writer, request, sessionID)
	case "turns":
		if len(path) == 2 {
			handler.createSessionTurn(writer, request, sessionID)
			return
		}
		if len(path) == 4 && path[3] == "tool-results" {
			handler.submitToolResult(writer, request, sessionID, path[2])
			return
		}
		nethttp.NotFound(writer, request)
	default:
		nethttp.NotFound(writer, request)
	}
}

func (handler *RuntimeHandler) createSessionTurn(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	sessionID string,
) {
	if request.Method != nethttp.MethodPost {
		methodNotAllowed(writer, nethttp.MethodPost)
		return
	}
	var input struct {
		ProfileID    string                   `json:"profile_id"`
		Input        string                   `json:"input"`
		TaskID       string                   `json:"task_id,omitempty"`
		Model        string                   `json:"model,omitempty"`
		Effort       string                   `json:"effort,omitempty"`
		CWD          string                   `json:"cwd,omitempty"`
		ModelOptions contract.GenerateOptions `json:"model_options,omitempty"`
	}
	if !decodeJSONRequest(writer, request, &input) {
		return
	}
	result, runtimeErr := handler.sessions.Run(
		request.Context(),
		session.RunRequest{
			SessionID: sessionID, TaskID: input.TaskID,
			ProfileID: input.ProfileID, Input: input.Input,
			Model: input.Model, Effort: input.Effort, CWD: input.CWD,
			ModelOptions: input.ModelOptions,
		},
	)
	if runtimeErr != nil {
		writeError(writer, statusForError(runtimeErr), runtimeErr)
		return
	}
	writeJSON(writer, nethttp.StatusOK, result)
}

func (handler *RuntimeHandler) reconcileSession(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	sessionID string,
) {
	if request.Method != nethttp.MethodPost {
		methodNotAllowed(writer, nethttp.MethodPost)
		return
	}
	var input session.ReconcileOptions
	if !decodeJSONRequest(writer, request, &input) {
		return
	}
	value, runtimeErr := handler.sessions.Reconcile(
		request.Context(), sessionID, input,
	)
	if runtimeErr != nil {
		writeError(writer, statusForError(runtimeErr), runtimeErr)
		return
	}
	writeJSON(writer, nethttp.StatusOK, value)
}

func (handler *RuntimeHandler) submitToolResult(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	sessionID, turnID string,
) {
	if request.Method != nethttp.MethodPost {
		methodNotAllowed(writer, nethttp.MethodPost)
		return
	}
	var input session.ToolResultInput
	if !decodeJSONRequest(writer, request, &input) {
		return
	}
	result, runtimeErr := handler.sessions.SubmitToolResult(
		sessionID, turnID, input,
	)
	if runtimeErr != nil {
		writeError(writer, statusForError(runtimeErr), runtimeErr)
		return
	}
	writeJSON(writer, nethttp.StatusOK, result)
}

func (handler *RuntimeHandler) watchSession(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	sessionID string,
) {
	if request.Method != nethttp.MethodGet {
		methodNotAllowed(writer, nethttp.MethodGet)
		return
	}
	after, valid := resumeSequence(writer, request)
	if !valid {
		return
	}
	flusher, ok := prepareSSE(writer)
	if !ok {
		return
	}
	for {
		events, err := handler.sessions.Events(sessionID, after)
		if err != nil {
			writeSSEError(writer, flusher, requestError(err))
			return
		}
		for _, event := range events {
			data, _ := json.Marshal(event)
			if _, err := fmt.Fprintf(
				writer, "id: %d\nevent: %s\ndata: %s\n\n",
				event.Sequence, event.Type, data,
			); err != nil {
				return
			}
			flusher.Flush()
			after = event.Sequence
		}
		timer := time.NewTimer(250 * time.Millisecond)
		select {
		case <-request.Context().Done():
			timer.Stop()
			return
		case <-timer.C:
		}
	}
}

func (handler *RuntimeHandler) serveAgent(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	path []string,
) {
	if len(path) != 1 || path[0] != "run" {
		nethttp.NotFound(writer, request)
		return
	}
	if request.Method != nethttp.MethodPost {
		methodNotAllowed(writer, nethttp.MethodPost)
		return
	}
	var input struct {
		ProfileID string            `json:"profile_id"`
		Input     string            `json:"input"`
		SessionID string            `json:"session_id,omitempty"`
		TaskID    string            `json:"task_id,omitempty"`
		Labels    map[string]string `json:"labels,omitempty"`
		Budget    agent.Budget      `json:"budget,omitempty"`
	}
	if !decodeJSONRequest(writer, request, &input) {
		return
	}
	runRequest := runtime.Request{
		Kind: runtime.KindAgent, ProfileID: input.ProfileID,
		Input: input.Input, SessionID: input.SessionID,
		TaskID: input.TaskID, Labels: input.Labels, AgentBudget: input.Budget,
	}
	if emptyBudget(runRequest.AgentBudget) {
		runRequest.AgentBudget = handler.agentBudget
	}
	if !acceptsEventStream(request) {
		record, runtimeErr := handler.runs.RunNow(request.Context(), runRequest, nil)
		if runtimeErr != nil {
			writeError(writer, statusForError(runtimeErr), runtimeErr)
			return
		}
		writeJSON(writer, nethttp.StatusOK, record)
		return
	}
	flusher, ok := prepareSSE(writer)
	if !ok {
		return
	}
	var last uint64
	disconnected := false
	record, runtimeErr := handler.runs.RunNow(
		context.WithoutCancel(request.Context()), runRequest,
		func(event contract.Event) error {
			if disconnected {
				return nil
			}
			last = event.Sequence
			if err := writeSSEEvent(writer, flusher, event); err != nil {
				disconnected = true
			}
			return nil
		},
	)
	if record.ID != "" && !disconnected {
		events, _ := handler.runs.Events(
			context.WithoutCancel(request.Context()), record.ID, last, 16,
		)
		for _, event := range events {
			_ = writeSSEEvent(writer, flusher, event)
		}
	}
	if runtimeErr != nil {
		writeSSEError(writer, flusher, runtimeErr)
	}
}

func (handler *RuntimeHandler) serveRuns(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	path []string,
) {
	if len(path) == 0 {
		switch request.Method {
		case nethttp.MethodPost:
			handler.submitRun(writer, request)
		case nethttp.MethodGet:
			limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
			values, err := handler.runs.List(request.Context(), runtime.ListFilter{
				State: runtime.State(request.URL.Query().Get("state")),
				Kind:  runtime.Kind(request.URL.Query().Get("kind")),
				Limit: limit,
			})
			if err != nil {
				writeInternalError(writer, err)
				return
			}
			writeJSON(writer, nethttp.StatusOK, map[string]any{"runs": values})
		default:
			methodNotAllowed(writer, "GET, POST")
		}
		return
	}
	if len(path) == 1 && path[0] == "gc" {
		handler.gcRuns(writer, request)
		return
	}
	runID := path[0]
	if len(path) == 1 && strings.HasSuffix(runID, ":cancel") {
		handler.cancelRun(writer, request, strings.TrimSuffix(runID, ":cancel"))
		return
	}
	if len(path) == 1 && strings.HasSuffix(runID, ":resume") {
		handler.resumeRun(writer, request, strings.TrimSuffix(runID, ":resume"))
		return
	}
	if len(path) == 1 && strings.HasSuffix(runID, ":reconcile") {
		handler.reconcileRun(
			writer, request, strings.TrimSuffix(runID, ":reconcile"),
		)
		return
	}
	if len(path) == 1 {
		if request.Method != nethttp.MethodGet {
			methodNotAllowed(writer, nethttp.MethodGet)
			return
		}
		value, err := handler.runs.Get(request.Context(), runID)
		if err != nil {
			writeError(writer, nethttp.StatusNotFound, requestError(err))
			return
		}
		writeJSON(writer, nethttp.StatusOK, value)
		return
	}
	if len(path) == 2 && path[1] == "events" {
		if request.Method != nethttp.MethodGet {
			methodNotAllowed(writer, nethttp.MethodGet)
			return
		}
		if acceptsEventStream(request) {
			handler.watchRun(writer, request, runID)
			return
		}
		after, ok := querySequence(writer, request, "after_seq")
		if !ok {
			return
		}
		events, err := handler.runs.Events(
			request.Context(), runID, after, 1000,
		)
		if err != nil {
			writeInternalError(writer, err)
			return
		}
		writeJSON(writer, nethttp.StatusOK, map[string]any{"events": events})
		return
	}
	nethttp.NotFound(writer, request)
}

func (handler *RuntimeHandler) submitRun(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
) {
	var input struct {
		Kind         runtime.Kind             `json:"kind"`
		ProfileID    string                   `json:"profile_id"`
		Input        string                   `json:"input"`
		SessionID    string                   `json:"session_id,omitempty"`
		TaskID       string                   `json:"task_id,omitempty"`
		Model        string                   `json:"model,omitempty"`
		Effort       string                   `json:"effort,omitempty"`
		CWD          string                   `json:"cwd,omitempty"`
		ModelOptions contract.GenerateOptions `json:"model_options,omitempty"`
		Labels       map[string]string        `json:"labels,omitempty"`
		Budget       agent.Budget             `json:"budget,omitempty"`
	}
	if !decodeJSONRequest(writer, request, &input) {
		return
	}
	if input.Kind == runtime.KindSession && input.SessionID == "" {
		var err error
		input.SessionID, err = session.NewID()
		if err != nil {
			writeInternalError(writer, err)
			return
		}
	}
	runRequest := runtime.Request{
		Kind: input.Kind, ProfileID: input.ProfileID, Input: input.Input,
		SessionID: input.SessionID, TaskID: input.TaskID,
		Model: input.Model, Effort: input.Effort, CWD: input.CWD,
		ModelOptions: input.ModelOptions,
		Labels:       input.Labels, AgentBudget: input.Budget,
	}
	if input.Kind == runtime.KindAgent && emptyBudget(runRequest.AgentBudget) {
		runRequest.AgentBudget = handler.agentBudget
	}
	record, runtimeErr := handler.runs.Submit(request.Context(), runRequest)
	if runtimeErr != nil {
		writeError(writer, statusForError(runtimeErr), runtimeErr)
		return
	}
	writeJSON(writer, nethttp.StatusAccepted, record)
}

func (handler *RuntimeHandler) reconcileRun(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	runID string,
) {
	if request.Method != nethttp.MethodPost {
		methodNotAllowed(writer, nethttp.MethodPost)
		return
	}
	var input struct{}
	if !decodeJSONRequest(writer, request, &input) {
		return
	}
	value, runtimeErr := handler.runs.ReconcileRun(
		request.Context(), runID,
	)
	if runtimeErr != nil {
		writeError(writer, statusForError(runtimeErr), runtimeErr)
		return
	}
	writeJSON(writer, nethttp.StatusOK, value)
}

func (handler *RuntimeHandler) gcRuns(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
) {
	if request.Method != nethttp.MethodPost {
		methodNotAllowed(writer, nethttp.MethodPost)
		return
	}
	var input struct {
		OlderThan string `json:"older_than,omitempty"`
		Limit     int    `json:"limit,omitempty"`
		Apply     bool   `json:"apply"`
	}
	if !decodeJSONRequest(writer, request, &input) {
		return
	}
	retention := handler.settledRetention
	var err error
	if input.OlderThan != "" {
		retention, err = time.ParseDuration(input.OlderThan)
		if err != nil || retention < time.Hour {
			writeError(
				writer, nethttp.StatusBadRequest,
				requestError(fmt.Errorf(
					"older_than must be a duration of at least 1h",
				)),
			)
			return
		}
	}
	result, err := handler.runs.GC(
		request.Context(),
		runtime.GCOptions{
			Before: time.Now().UTC().Add(-retention),
			Limit:  input.Limit, Apply: input.Apply,
		},
	)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	writeJSON(writer, nethttp.StatusOK, result)
}

func emptyBudget(value agent.Budget) bool {
	return value.MaxRounds == 0 && value.MaxToolCalls == 0 &&
		value.MaxTotalTokens == 0 && value.MaxWallTime == 0
}

func (handler *RuntimeHandler) cancelRun(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	runID string,
) {
	if request.Method != nethttp.MethodPost {
		methodNotAllowed(writer, nethttp.MethodPost)
		return
	}
	value, err := handler.runs.Cancel(request.Context(), runID)
	if err != nil {
		writeError(writer, nethttp.StatusConflict, requestError(err))
		return
	}
	writeJSON(writer, nethttp.StatusOK, value)
}

func (handler *RuntimeHandler) resumeRun(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	runID string,
) {
	if request.Method != nethttp.MethodPost {
		methodNotAllowed(writer, nethttp.MethodPost)
		return
	}
	var input json.RawMessage
	if !decodeJSONRequest(writer, request, &input) {
		return
	}
	value, err := handler.runs.Resume(request.Context(), runID, input)
	if err != nil {
		writeError(writer, nethttp.StatusConflict, requestError(err))
		return
	}
	writeJSON(writer, nethttp.StatusOK, value)
}

func (handler *RuntimeHandler) watchRun(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	runID string,
) {
	after, valid := resumeSequence(writer, request)
	if !valid {
		return
	}
	flusher, ok := prepareSSE(writer)
	if !ok {
		return
	}
	_, err := handler.runs.Watch(
		request.Context(), runID, after,
		func(event contract.Event) error {
			return writeSSEEvent(writer, flusher, event)
		},
	)
	if err != nil && request.Context().Err() == nil {
		writeSSEError(writer, flusher, requestError(err))
	}
}

func decodeJSONRequest(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	value any,
) bool {
	if !hasJSONContentType(request) {
		writeError(writer, nethttp.StatusUnsupportedMediaType, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseTransport,
			Message: "Content-Type must be application/json",
		})
		return false
	}
	if err := strictjson.Decode(request.Body, maxRequestBytes, value); err != nil {
		writeError(writer, nethttp.StatusBadRequest, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseTransport,
			Message: err.Error(),
		})
		return false
	}
	return true
}

func prepareSSE(writer nethttp.ResponseWriter) (nethttp.Flusher, bool) {
	flusher, ok := writer.(nethttp.Flusher)
	if !ok {
		writeInternalError(writer, fmt.Errorf("streaming is unsupported"))
		return nil, false
	}
	writer.Header().Set("Content-Type", "text/event-stream")
	writer.Header().Set("Cache-Control", "no-cache")
	writer.Header().Set("X-Content-Type-Options", "nosniff")
	return flusher, true
}

func writeSSEEvent(
	writer nethttp.ResponseWriter,
	flusher nethttp.Flusher,
	event contract.Event,
) error {
	data, err := json.Marshal(event)
	if err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		writer, "id: %d\nevent: %s\ndata: %s\n\n",
		event.Sequence, event.Type, data,
	); err != nil {
		return err
	}
	flusher.Flush()
	return nil
}

func writeSSEError(
	writer nethttp.ResponseWriter,
	flusher nethttp.Flusher,
	runtimeErr *contract.RuntimeError,
) {
	data, _ := json.Marshal(runtimeErr)
	_, _ = fmt.Fprintf(writer, "event: error\ndata: %s\n\n", data)
	flusher.Flush()
}

func querySequence(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	name string,
) (uint64, bool) {
	value := request.URL.Query().Get(name)
	if value == "" {
		return 0, true
	}
	current, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		writeError(writer, nethttp.StatusBadRequest, requestError(err))
		return 0, false
	}
	return current, true
}

func resumeSequence(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
) (uint64, bool) {
	after, ok := querySequence(writer, request, "after_seq")
	if !ok || after != 0 {
		return after, ok
	}
	value := strings.TrimSpace(request.Header.Get("Last-Event-ID"))
	if value == "" {
		return 0, true
	}
	current, err := strconv.ParseUint(value, 10, 64)
	if err != nil {
		writeError(writer, nethttp.StatusBadRequest, requestError(err))
		return 0, false
	}
	return current, true
}

func methodNotAllowed(writer nethttp.ResponseWriter, allow string) {
	writer.Header().Set("Allow", allow)
	writeError(writer, nethttp.StatusMethodNotAllowed, &contract.RuntimeError{
		Code: contract.ErrorInvalidRequest, Phase: contract.PhaseTransport,
		Message: "method is not allowed",
	})
}

func requestError(err error) *contract.RuntimeError {
	return &contract.RuntimeError{
		Code: contract.ErrorInvalidRequest, Phase: contract.PhaseRequest,
		Message: err.Error(),
	}
}

func writeInternalError(writer nethttp.ResponseWriter, err error) {
	writeError(writer, nethttp.StatusInternalServerError, &contract.RuntimeError{
		Code: contract.ErrorInternal, Phase: contract.PhaseTransport,
		Message: err.Error(),
	})
}

func splitPath(path string) []string {
	value := strings.Trim(path, "/")
	if value == "" {
		return nil
	}
	return strings.Split(value, "/")
}
