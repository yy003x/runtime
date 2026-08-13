package http

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	nethttp "net/http"
	"net/url"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
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
	budget, err := resolveAgentBudget(services.AgentBudget, agent.DefaultBudget())
	if err != nil {
		return nil, fmt.Errorf("agent budget: %w", err)
	}
	services.AgentBudget = budget
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
			filter, err := parseSessionListFilter(request)
			if err != nil {
				writeBadRequest(writer, err)
				return
			}
			values, err := handler.sessions.List(filter)
			if err != nil {
				writeInternalError(writer, err)
				return
			}
			writeJSON(writer, nethttp.StatusOK, map[string]any{"sessions": values})
		case nethttp.MethodPost:
			var input struct {
				Retention session.Retention `json:"retention,omitempty"`
			}
			if !decodeJSONObjectRequest(writer, request, &input) {
				return
			}
			if !validSessionRetention(input.Retention) {
				writeBadRequest(writer, fmt.Errorf(
					"retention must be ephemeral, standard, or pinned",
				))
				return
			}
			value, err := handler.sessions.Create(input.Retention)
			if err != nil {
				writeInternalError(writer, err)
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
			OlderThanHours *int64 `json:"older_than_hours,omitempty"`
			Limit          *int   `json:"limit,omitempty"`
			Apply          bool   `json:"apply"`
		}
		if !decodeJSONObjectRequest(writer, request, &input) {
			return
		}
		var olderThan time.Duration
		if input.OlderThanHours != nil {
			const maxDurationHours = int64((1<<63 - 1) / int64(time.Hour))
			if *input.OlderThanHours <= 0 ||
				*input.OlderThanHours > maxDurationHours {
				writeBadRequest(writer, fmt.Errorf(
					"older_than_hours must be a positive integer that fits a duration",
				))
				return
			}
			olderThan = time.Duration(*input.OlderThanHours) * time.Hour
		}
		limit := 0
		if input.Limit != nil {
			limit = *input.Limit
		}
		if input.Limit != nil && (limit < 1 || limit > 1000) {
			writeBadRequest(writer, fmt.Errorf(
				"limit must be between 1 and 1000",
			))
			return
		}
		value, err := handler.sessions.GC(session.GCOptions{
			OlderThan: olderThan, Limit: limit, Apply: input.Apply,
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
			writeSessionLookupError(writer, sessionID, err)
			return
		}
		writeJSON(writer, nethttp.StatusOK, value)
		return
	}
	switch path[1] {
	case "messages":
		if len(path) != 2 {
			nethttp.NotFound(writer, request)
			return
		}
		if request.Method != nethttp.MethodGet {
			methodNotAllowed(writer, nethttp.MethodGet)
			return
		}
		after, ok := querySequence(writer, request, "after_seq")
		if !ok {
			return
		}
		if !handler.requireSession(writer, sessionID) {
			return
		}
		values, err := handler.sessions.Messages(sessionID, after)
		if err != nil {
			writeInternalError(writer, err)
			return
		}
		writeJSON(writer, nethttp.StatusOK, map[string]any{"messages": values})
	case "events":
		if len(path) != 2 {
			nethttp.NotFound(writer, request)
			return
		}
		if request.Method != nethttp.MethodGet {
			methodNotAllowed(writer, nethttp.MethodGet)
			return
		}
		after, ok := querySequence(writer, request, "after_seq")
		if !ok {
			return
		}
		if !handler.requireSession(writer, sessionID) {
			return
		}
		values, err := handler.sessions.Events(sessionID, after)
		if err != nil {
			writeInternalError(writer, err)
			return
		}
		writeJSON(writer, nethttp.StatusOK, map[string]any{"events": values})
	case "executions":
		if len(path) != 2 && len(path) != 3 {
			nethttp.NotFound(writer, request)
			return
		}
		if request.Method != nethttp.MethodGet {
			methodNotAllowed(writer, nethttp.MethodGet)
			return
		}
		if !handler.requireSession(writer, sessionID) {
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
			if err := identity.Validate(path[2], "execution"); err != nil {
				writeBadRequest(writer, err)
				return
			}
			value, err := handler.sessions.Execution(sessionID, path[2])
			if err != nil {
				if errors.Is(err, os.ErrNotExist) {
					writeNotFound(writer, "execution", path[2])
				} else {
					writeInternalError(writer, err)
				}
				return
			}
			writeJSON(writer, nethttp.StatusOK, value)
			return
		}
	case "watch":
		if len(path) != 2 {
			nethttp.NotFound(writer, request)
			return
		}
		handler.watchSession(writer, request, sessionID)
	case "turns":
		if len(path) == 2 {
			handler.createSessionTurn(writer, request, sessionID)
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
	if !decodeJSONObjectRequest(writer, request, &input) {
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
	if !handler.requireSession(writer, sessionID) {
		return
	}
	var input session.ReconcileOptions
	if !decodeJSONObjectRequest(writer, request, &input) {
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

func (handler *RuntimeHandler) watchSession(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	sessionID string,
) {
	if request.Method != nethttp.MethodGet {
		methodNotAllowed(writer, nethttp.MethodGet)
		return
	}
	if !handler.requireSession(writer, sessionID) {
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
			writeSSEError(
				writer, flusher, sessionWatchError(sessionID, err),
			)
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
	if !decodeJSONObjectRequest(writer, request, &input) {
		return
	}
	budget, err := resolveAgentBudget(input.Budget, handler.agentBudget)
	if err != nil {
		writeBadRequest(writer, fmt.Errorf("budget: %w", err))
		return
	}
	runRequest := runtime.Request{
		Kind: runtime.KindAgent, ProfileID: input.ProfileID,
		Input: input.Input, SessionID: input.SessionID,
		TaskID: input.TaskID, Labels: input.Labels, AgentBudget: budget,
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
			filter, err := parseRunListFilter(request)
			if err != nil {
				writeBadRequest(writer, err)
				return
			}
			values, err := handler.runs.List(request.Context(), filter)
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
			writeRunLookupError(writer, runID, err)
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
			if !handler.requireRun(writer, request, runID) {
				return
			}
			handler.watchRun(writer, request, runID)
			return
		}
		after, ok := querySequence(writer, request, "after_seq")
		if !ok {
			return
		}
		if !handler.requireRun(writer, request, runID) {
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
	if !decodeJSONObjectRequest(writer, request, &input) {
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
	if input.Kind == runtime.KindAgent {
		budget, err := resolveAgentBudget(input.Budget, handler.agentBudget)
		if err != nil {
			writeBadRequest(writer, fmt.Errorf("budget: %w", err))
			return
		}
		runRequest.AgentBudget = budget
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
	if !decodeEmptyJSONObjectRequest(writer, request) {
		return
	}
	if !handler.requireRun(writer, request, runID) {
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
		OlderThan *string `json:"older_than,omitempty"`
		Limit     *int    `json:"limit,omitempty"`
		Apply     bool    `json:"apply"`
	}
	if !decodeJSONObjectRequest(writer, request, &input) {
		return
	}
	retention := handler.settledRetention
	var err error
	if input.OlderThan != nil {
		retention, err = time.ParseDuration(*input.OlderThan)
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
	limit := 0
	if input.Limit != nil {
		limit = *input.Limit
	}
	if input.Limit != nil && (limit < 1 || limit > 1000) {
		writeBadRequest(writer, fmt.Errorf(
			"limit must be between 1 and 1000",
		))
		return
	}
	result, err := handler.runs.GC(
		request.Context(),
		runtime.GCOptions{
			Before: time.Now().UTC().Add(-retention),
			Limit:  limit, Apply: input.Apply,
		},
	)
	if err != nil {
		writeInternalError(writer, err)
		return
	}
	writeJSON(writer, nethttp.StatusOK, result)
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
	if !decodeEmptyJSONObjectRequest(writer, request) {
		return
	}
	if !handler.requireRun(writer, request, runID) {
		return
	}
	value, err := handler.runs.Cancel(request.Context(), runID)
	if err != nil {
		writeRunControlError(writer, runID, err)
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
	if !handler.requireRun(writer, request, runID) {
		return
	}
	var input json.RawMessage
	if !decodeJSONRequest(writer, request, &input) {
		return
	}
	value, err := handler.runs.Resume(request.Context(), runID, input)
	if err != nil {
		writeRunControlError(writer, runID, err)
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
		writeSSEError(writer, flusher, runWatchError(runID, err))
	}
}

func parseRunListFilter(request *nethttp.Request) (runtime.ListFilter, error) {
	query, err := parseExactQuery(request, "state", "kind", "limit")
	if err != nil {
		return runtime.ListFilter{}, err
	}
	state := runtime.State("")
	if values, exists := query["state"]; exists {
		if values[0] == "" {
			return runtime.ListFilter{}, fmt.Errorf(
				"state must be queued, running, paused, needs_reconciliation, completed, failed, or cancelled",
			)
		}
		state = runtime.State(values[0])
	}
	kind := runtime.Kind("")
	if values, exists := query["kind"]; exists {
		if values[0] == "" {
			return runtime.ListFilter{}, fmt.Errorf(
				"kind must be agent, session, or native_tui",
			)
		}
		kind = runtime.Kind(values[0])
	}
	limit := 0
	if values, exists := query["limit"]; exists {
		if values[0] == "" {
			return runtime.ListFilter{}, fmt.Errorf(
				"limit must be between 1 and %d", runtime.MaxListLimit,
			)
		}
		parsed, err := strconv.Atoi(values[0])
		if err != nil || parsed < 1 || parsed > runtime.MaxListLimit {
			return runtime.ListFilter{}, fmt.Errorf(
				"limit must be between 1 and %d", runtime.MaxListLimit,
			)
		}
		limit = parsed
	}
	return runtime.NormalizeListFilter(runtime.ListFilter{
		State: state, Kind: kind, Limit: limit,
	})
}

func parseSessionListFilter(
	request *nethttp.Request,
) (session.ListFilter, error) {
	query, err := parseExactQuery(request, "state")
	if err != nil {
		return session.ListFilter{}, err
	}
	state := session.SessionState("")
	if values, exists := query["state"]; exists {
		if values[0] == "" {
			return session.ListFilter{}, fmt.Errorf(
				"state must be idle, active, blocked, or archived",
			)
		}
		state = session.SessionState(values[0])
	}
	filter := session.ListFilter{State: state}
	if err := session.ValidateListFilter(filter); err != nil {
		return session.ListFilter{}, err
	}
	return filter, nil
}

func parseExactQuery(
	request *nethttp.Request,
	allowedNames ...string,
) (url.Values, error) {
	query, err := url.ParseQuery(request.URL.RawQuery)
	if err != nil {
		return nil, fmt.Errorf("invalid query: %w", err)
	}
	allowed := make(map[string]struct{}, len(allowedNames))
	for _, name := range allowedNames {
		allowed[name] = struct{}{}
	}
	for name, values := range query {
		if _, exists := allowed[name]; !exists {
			return nil, fmt.Errorf("unknown query parameter %q", name)
		}
		if len(values) != 1 {
			return nil, fmt.Errorf(
				"query parameter %q may only be specified once", name,
			)
		}
	}
	return query, nil
}

func validSessionRetention(retention session.Retention) bool {
	switch retention {
	case "", session.RetentionEphemeral, session.RetentionStandard,
		session.RetentionPinned:
		return true
	default:
		return false
	}
}

func resolveAgentBudget(
	value agent.Budget,
	defaults agent.Budget,
) (agent.Budget, error) {
	if value.MaxRounds == 0 {
		value.MaxRounds = defaults.MaxRounds
	}
	if value.MaxToolCalls == 0 {
		value.MaxToolCalls = defaults.MaxToolCalls
	}
	if value.MaxTotalTokens == 0 {
		value.MaxTotalTokens = defaults.MaxTotalTokens
	}
	if value.MaxWallTime == 0 {
		value.MaxWallTime = defaults.MaxWallTime
	}
	if err := value.Validate(); err != nil {
		return agent.Budget{}, err
	}
	return value, nil
}

func (handler *RuntimeHandler) requireSession(
	writer nethttp.ResponseWriter,
	sessionID string,
) bool {
	if err := identity.Validate(sessionID, "session"); err != nil {
		writeBadRequest(writer, err)
		return false
	}
	if _, err := handler.sessions.Get(sessionID); err != nil {
		writeSessionLookupError(writer, sessionID, err)
		return false
	}
	return true
}

func (handler *RuntimeHandler) requireRun(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
	runID string,
) bool {
	if err := identity.Validate(runID, "run"); err != nil {
		writeBadRequest(writer, err)
		return false
	}
	if _, err := handler.runs.Get(request.Context(), runID); err != nil {
		writeRunLookupError(writer, runID, err)
		return false
	}
	return true
}

func writeSessionLookupError(
	writer nethttp.ResponseWriter,
	sessionID string,
	err error,
) {
	if identity.Validate(sessionID, "session") == nil &&
		errors.Is(err, os.ErrNotExist) {
		writeNotFound(writer, "session", sessionID)
		return
	}
	if idErr := identity.Validate(sessionID, "session"); idErr != nil {
		writeBadRequest(writer, idErr)
		return
	}
	writeInternalError(writer, err)
}

func writeRunLookupError(
	writer nethttp.ResponseWriter,
	runID string,
	err error,
) {
	if idErr := identity.Validate(runID, "run"); idErr != nil {
		writeBadRequest(writer, idErr)
		return
	}
	if errors.Is(err, runtime.ErrNotFound) {
		writeNotFound(writer, "run", runID)
		return
	}
	writeInternalError(writer, err)
}

func writeRunControlError(
	writer nethttp.ResponseWriter,
	runID string,
	err error,
) {
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		writeNotFound(writer, "run", runID)
	case errors.Is(err, runtime.ErrConflict):
		writeError(writer, nethttp.StatusConflict, &contract.RuntimeError{
			Code: contract.ErrorConflict, Phase: contract.PhaseRun,
			Message: err.Error(),
		})
	default:
		writeInternalError(writer, err)
	}
}

func sessionWatchError(
	sessionID string,
	err error,
) *contract.RuntimeError {
	if errorTreeOnlyMatches(err, os.ErrNotExist) {
		return &contract.RuntimeError{
			Code: contract.ErrorNotFound, Phase: contract.PhaseRequest,
			Message: fmt.Sprintf("session %s was not found", sessionID),
		}
	}
	return &contract.RuntimeError{
		Code: contract.ErrorInternal, Phase: contract.PhaseTransport,
		Message: err.Error(),
	}
}

func runWatchError(runID string, err error) *contract.RuntimeError {
	if errorTreeOnlyMatches(err, runtime.ErrNotFound) {
		return &contract.RuntimeError{
			Code: contract.ErrorNotFound, Phase: contract.PhaseRequest,
			Message: fmt.Sprintf("run %s was not found", runID),
		}
	}
	return &contract.RuntimeError{
		Code: contract.ErrorInternal, Phase: contract.PhaseTransport,
		Message: err.Error(),
	}
}

func errorTreeOnlyMatches(err, target error) bool {
	if err == nil {
		return false
	}
	if joined, ok := err.(interface{ Unwrap() []error }); ok {
		children := joined.Unwrap()
		if len(children) == 0 {
			return false
		}
		for _, child := range children {
			if !errorTreeOnlyMatches(child, target) {
				return false
			}
		}
		return true
	}
	if wrapped, ok := err.(interface{ Unwrap() error }); ok {
		child := wrapped.Unwrap()
		if child != nil {
			return errorTreeOnlyMatches(child, target)
		}
	}
	return errors.Is(err, target)
}

func writeNotFound(
	writer nethttp.ResponseWriter,
	resource string,
	id string,
) {
	writeError(writer, nethttp.StatusNotFound, &contract.RuntimeError{
		Code: contract.ErrorNotFound, Phase: contract.PhaseRequest,
		Message: fmt.Sprintf("%s %s was not found", resource, id),
	})
}

func writeBadRequest(writer nethttp.ResponseWriter, err error) {
	writeError(writer, nethttp.StatusBadRequest, requestError(err))
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

func decodeJSONObjectRequest(
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
	if err := strictjson.DecodeObjectNoNulls(
		request.Body, maxRequestBytes, value,
	); err != nil {
		writeError(writer, nethttp.StatusBadRequest, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseTransport,
			Message: err.Error(),
		})
		return false
	}
	return true
}

func decodeEmptyJSONObjectRequest(
	writer nethttp.ResponseWriter,
	request *nethttp.Request,
) bool {
	var object map[string]json.RawMessage
	if !decodeJSONObjectRequest(writer, request, &object) {
		return false
	}
	if len(object) != 0 {
		writeError(writer, nethttp.StatusBadRequest, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseTransport,
			Message: "request body must be a non-null empty JSON object",
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
