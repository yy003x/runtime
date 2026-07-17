package transport

import (
	"crypto/subtle"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"path/filepath"
	"strconv"
	"strings"

	"agent-runtime/internal/agentrun"
	"agent-runtime/internal/provider"
)

type HTTPHandler struct {
	mux         *http.ServeMux
	svc         *agentrun.Service
	bearerToken string
	maxBody     int64
}

const defaultMaxBodyBytes int64 = 1 << 20

type HTTPOptions struct {
	BearerToken  string
	MaxBodyBytes int64
}

type RunRequest struct {
	RunID             string                    `json:"run_id,omitempty"`
	RunType           string                    `json:"run_type,omitempty"`
	Profile           string                    `json:"profile"`
	ProjectID         string                    `json:"project_id,omitempty"`
	CWD               string                    `json:"cwd,omitempty"`
	Prompt            string                    `json:"prompt"`
	PromptFile        string                    `json:"prompt_file,omitempty"`
	ExecutionMode     string                    `json:"execution_mode,omitempty"`
	DeadlineSeconds   int                       `json:"deadline_seconds,omitempty"`
	ProviderOverrides map[string]any            `json:"provider_overrides,omitempty"`
	AllowedActions    []string                  `json:"allowed_actions,omitempty"`
	ForbiddenActions  []string                  `json:"forbidden_actions,omitempty"`
	SessionID         string                    `json:"session_id,omitempty"`
	RecordMode        string                    `json:"record_mode,omitempty"`
	Retention         string                    `json:"retention,omitempty"`
	Memory            []provider.InjectedMemory `json:"memory,omitempty"`
}

type ControlRequest struct {
	Reason string                `json:"reason,omitempty"`
	Patch  *provider.NativePatch `json:"patch,omitempty"`
}

type SessionCreateRequest struct {
	SessionID  string   `json:"session_id,omitempty"`
	ProjectID  string   `json:"project_id,omitempty"`
	CWD        string   `json:"cwd,omitempty"`
	Title      string   `json:"title,omitempty"`
	Runtime    string   `json:"runtime,omitempty"`
	Profile    string   `json:"profile,omitempty"`
	RecordMode string   `json:"record_mode,omitempty"`
	Retention  string   `json:"retention,omitempty"`
	Tags       []string `json:"tags,omitempty"`
}

func NewHTTPHandler(service *agentrun.Service) *HTTPHandler {
	return NewHTTPHandlerWithOptions(service, HTTPOptions{})
}

func NewHTTPHandlerWithOptions(service *agentrun.Service, options HTTPOptions) *HTTPHandler {
	maxBody := options.MaxBodyBytes
	if maxBody <= 0 {
		maxBody = defaultMaxBodyBytes
	}
	handler := &HTTPHandler{
		mux: http.NewServeMux(), svc: service,
		bearerToken: strings.TrimSpace(options.BearerToken), maxBody: maxBody,
	}
	handler.routes()
	return handler
}

func (h *HTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	if h.bearerToken != "" && request.URL.Path != "/healthz" && !validBearerToken(request.Header.Get("Authorization"), h.bearerToken) {
		writer.Header().Set("WWW-Authenticate", `Bearer realm="sn-server"`)
		writeJSON(writer, http.StatusUnauthorized, errorResponse{Error: "unauthorized"})
		return
	}
	h.mux.ServeHTTP(writer, request)
}

func (h *HTTPHandler) routes() {
	h.mux.HandleFunc("GET /healthz", h.handleHealth)
	h.mux.HandleFunc("POST /v1/runs", h.handleRun)
	h.mux.HandleFunc("/v1/runs/", h.handleRunByID)
	h.mux.HandleFunc("/v1/sessions", h.handleSessions)
	h.mux.HandleFunc("/v1/sessions/", h.handleSessionByID)
}

func (h *HTTPHandler) handleSessions(writer http.ResponseWriter, request *http.Request) {
	manager := agentrun.NewSessionManager(h.svc)
	if request.Method == http.MethodGet {
		if _, err := h.svc.SessionList(request.Context()); err != nil {
			writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: err.Error()})
			return
		}
		limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
		values, err := manager.Store().List(agentrun.SessionFilter{
			ProjectID: request.URL.Query().Get("project_id"), State: request.URL.Query().Get("state"),
			Retention: request.URL.Query().Get("retention"), Tags: request.URL.Query()["tag"], Limit: limit,
		})
		if err != nil {
			writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: err.Error()})
			return
		}
		writeJSON(writer, http.StatusOK, map[string]any{"sessions": values})
		return
	}
	if request.Method != http.MethodPost {
		writeJSON(writer, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
		return
	}
	if !hasJSONContentType(request) {
		writeJSON(writer, http.StatusUnsupportedMediaType, errorResponse{Error: "Content-Type must be application/json"})
		return
	}
	var input SessionCreateRequest
	if err := h.decodeJSON(writer, request, &input, false); err != nil {
		handleDecodeError(writer, err)
		return
	}
	if input.CWD != "" && (!filepath.IsAbs(input.CWD) || strings.ContainsRune(input.CWD, '\x00')) {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "cwd must be an absolute path without NUL"})
		return
	}
	if err := agentrun.ValidateSessionTags(input.Tags); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	if input.Runtime != "" && input.Runtime != "api" && input.Runtime != "cli" && input.Runtime != "tmux" {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "runtime must be api|cli|tmux"})
		return
	}
	if input.Profile != "" {
		profiles, loadErr := h.svc.Profiles()
		profile, ok := provider.Resolve(profiles, input.Profile)
		if loadErr != nil || !ok {
			writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "unknown provider profile: " + input.Profile})
			return
		}
		expected := "api"
		if profile.Type == provider.TypeCLI {
			expected = "cli"
			if profile.Transport() == provider.ExecutorTmux {
				expected = "tmux"
			}
		}
		if input.Runtime == "" {
			input.Runtime = expected
		} else if input.Runtime != expected {
			writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "runtime does not match provider profile"})
			return
		}
	}
	decision, err := agentrun.DecideRecordPolicy("http", agentrun.RunTurn, agentrun.ExecutionAPI, input.SessionID, input.RecordMode, input.Retention)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	record, err := manager.EnsureSession(input.SessionID, input.ProjectID, input.CWD, input.Title, decision)
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	if input.Runtime != "" || input.Profile != "" {
		record, err = manager.Store().ConfigureSession(record.SessionID, input.Runtime, input.Profile)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
	}
	if len(input.Tags) > 0 {
		record, err = manager.Store().SetTags(record.SessionID, input.Tags)
		if err != nil {
			writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
			return
		}
	}
	writeJSON(writer, http.StatusCreated, record)
}

func (h *HTTPHandler) handleSessionByID(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(request.URL.Path, "/v1/sessions/"), "/"), "/")
	if len(parts) == 0 || parts[0] == "" {
		writeJSON(writer, http.StatusNotFound, errorResponse{Error: "session_id is required"})
		return
	}
	if len(parts) > 2 {
		writeJSON(writer, http.StatusNotFound, errorResponse{Error: "unknown session resource"})
		return
	}
	sessionID := parts[0]
	action := "show"
	if len(parts) > 1 {
		action = parts[1]
	}
	store := agentrun.NewSessionManager(h.svc).Store()
	if request.Method == http.MethodGet {
		after, _ := strconv.ParseInt(request.URL.Query().Get("after_seq"), 10, 64)
		limit, _ := strconv.Atoi(request.URL.Query().Get("limit"))
		switch action {
		case "show":
			if err := h.svc.ReconcileSession(request.Context(), sessionID); err != nil {
				writeJSON(writer, http.StatusInternalServerError, errorResponse{Error: err.Error()})
				return
			}
			value, err := store.View(sessionID)
			writeReadResult(writer, value, err)
		case "messages":
			value, err := store.Messages(sessionID, after, limit)
			writeReadResult(writer, map[string]any{"session_id": sessionID, "messages": value}, err)
		case "events", "watch":
			value, err := store.Events(sessionID, after, limit)
			writeReadResult(writer, map[string]any{"session_id": sessionID, "events": value}, err)
		default:
			writeJSON(writer, http.StatusNotFound, errorResponse{Error: "unknown session resource"})
		}
		return
	}
	if request.Method != http.MethodPost || action != "turns" {
		writeJSON(writer, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
		return
	}
	if !hasJSONContentType(request) {
		writeJSON(writer, http.StatusUnsupportedMediaType, errorResponse{Error: "Content-Type must be application/json"})
		return
	}
	var input RunRequest
	if err := h.decodeJSON(writer, request, &input, false); err != nil {
		handleDecodeError(writer, err)
		return
	}
	if err := validateRunRequest(input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	existing, err := store.Get(sessionID)
	if err != nil {
		writeJSON(writer, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	if input.ProjectID == "" {
		input.ProjectID = existing.ProjectID
	}
	if input.CWD == "" {
		input.CWD = existing.CWD
	}
	if input.Profile == "" {
		input.Profile = existing.Profile
	}
	if input.Profile == "" {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "profile is required because the Session has no default profile"})
		return
	}
	summary, err := h.svc.Run(request.Context(), agentrun.RunOptions{RunID: input.RunID, RunType: agentrun.RunTurn,
		Profile: input.Profile, ProjectID: input.ProjectID, CWD: input.CWD, Prompt: input.Prompt, PromptFile: input.PromptFile,
		ExecutionMode: input.ExecutionMode, DeadlineSeconds: input.DeadlineSeconds, ProviderOverrides: input.ProviderOverrides,
		AllowedActions: input.AllowedActions, ForbiddenActions: input.ForbiddenActions, SessionID: sessionID,
		RecordMode: input.RecordMode, Retention: input.Retention, InjectedMemory: input.Memory, Caller: "http"})
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": err.Error(), "run": summary})
		return
	}
	writeJSON(writer, http.StatusCreated, summary)
}

func (h *HTTPHandler) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "runtime": h.svc.RuntimeVersion})
}

func (h *HTTPHandler) handleRun(writer http.ResponseWriter, request *http.Request) {
	if !hasJSONContentType(request) {
		writeJSON(writer, http.StatusUnsupportedMediaType, errorResponse{Error: "Content-Type must be application/json"})
		return
	}
	var input RunRequest
	if err := h.decodeJSON(writer, request, &input, false); err != nil {
		handleDecodeError(writer, err)
		return
	}
	if err := validateRunRequest(input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}
	summary, err := h.svc.Run(request.Context(), agentrun.RunOptions{
		RunID: input.RunID, RunType: input.RunType, Profile: input.Profile, ProjectID: input.ProjectID,
		CWD: input.CWD, Prompt: input.Prompt, PromptFile: input.PromptFile, ExecutionMode: input.ExecutionMode,
		DeadlineSeconds: input.DeadlineSeconds, ProviderOverrides: input.ProviderOverrides,
		AllowedActions: input.AllowedActions, ForbiddenActions: input.ForbiddenActions,
		SessionID: input.SessionID, RecordMode: input.RecordMode, Retention: input.Retention, InjectedMemory: input.Memory, Caller: "http",
	})
	if err != nil {
		writeJSON(writer, http.StatusBadRequest, map[string]any{"error": err.Error(), "run": summary})
		return
	}
	writeJSON(writer, http.StatusCreated, summary)
}

func (h *HTTPHandler) handleRunByID(writer http.ResponseWriter, request *http.Request) {
	parts := strings.Split(strings.Trim(strings.TrimPrefix(request.URL.Path, "/v1/runs/"), "/"), "/")
	if len(parts) < 2 || parts[0] == "" || parts[1] == "" {
		writeJSON(writer, http.StatusNotFound, errorResponse{Error: "run_type and run_id are required"})
		return
	}
	runType, runID := parts[0], parts[1]
	action := "status"
	if len(parts) > 2 {
		action = parts[2]
	}
	if request.Method == http.MethodGet {
		h.handleRead(writer, runType, runID, action)
		return
	}
	if request.Method != http.MethodPost {
		writeJSON(writer, http.StatusMethodNotAllowed, errorResponse{Error: "method not allowed"})
		return
	}
	var input ControlRequest
	if request.ContentLength != 0 {
		if !hasJSONContentType(request) {
			writeJSON(writer, http.StatusUnsupportedMediaType, errorResponse{Error: "Content-Type must be application/json"})
			return
		}
		if err := h.decodeJSON(writer, request, &input, true); err != nil {
			handleDecodeError(writer, err)
			return
		}
	}
	var summary agentrun.RunSummary
	var err error
	switch action {
	case "cancel":
		summary, err = h.svc.Cancel(runType, runID)
	case "block", "stop":
		summary, err = h.svc.ControlNative(runType, runID, action, input.Reason)
	case "continue":
		summary, err = h.svc.ResumeNative(request.Context(), runType, runID, nil)
	case "patch-resume":
		if input.Patch == nil {
			writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "patch is required"})
			return
		}
		summary, err = h.svc.ResumeNative(request.Context(), runType, runID, input.Patch)
	default:
		writeJSON(writer, http.StatusNotFound, errorResponse{Error: "unknown run action"})
		return
	}
	if err != nil {
		writeJSON(writer, http.StatusConflict, map[string]any{"error": err.Error(), "run": summary})
		return
	}
	writeJSON(writer, http.StatusOK, summary)
}

func (h *HTTPHandler) decodeJSON(writer http.ResponseWriter, request *http.Request, output any, allowEmpty bool) error {
	request.Body = http.MaxBytesReader(writer, request.Body, h.maxBody)
	decoder := json.NewDecoder(request.Body)
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(output); err != nil {
		if allowEmpty && errors.Is(err, io.EOF) {
			return nil
		}
		return err
	}
	if err := decoder.Decode(&struct{}{}); !errors.Is(err, io.EOF) {
		if err == nil {
			return fmt.Errorf("JSON body must contain one object")
		}
		return err
	}
	return nil
}

func handleDecodeError(writer http.ResponseWriter, err error) {
	var tooLarge *http.MaxBytesError
	if errors.As(err, &tooLarge) {
		writeJSON(writer, http.StatusRequestEntityTooLarge, errorResponse{Error: "request body too large"})
		return
	}
	writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "invalid JSON body"})
}

func hasJSONContentType(request *http.Request) bool {
	mediaType, _, err := mime.ParseMediaType(request.Header.Get("Content-Type"))
	return err == nil && mediaType == "application/json"
}

func validBearerToken(header, token string) bool {
	prefix, value, ok := strings.Cut(strings.TrimSpace(header), " ")
	if !ok || !strings.EqualFold(prefix, "Bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(strings.TrimSpace(value)), []byte(token)) == 1
}

func validateRunRequest(input RunRequest) error {
	if input.DeadlineSeconds < 0 || input.DeadlineSeconds > 24*60*60 {
		return fmt.Errorf("deadline_seconds must be between 0 and 86400")
	}
	if strings.ContainsRune(input.CWD, '\x00') || strings.ContainsRune(input.PromptFile, '\x00') {
		return fmt.Errorf("cwd and prompt_file must not contain NUL")
	}
	if err := validateActions(input.AllowedActions, "allowed_actions"); err != nil {
		return err
	}
	if err := validateActions(input.ForbiddenActions, "forbidden_actions"); err != nil {
		return err
	}
	if input.CWD != "" && !filepath.IsAbs(input.CWD) {
		return fmt.Errorf("cwd must be an absolute path")
	}
	if input.PromptFile != "" {
		if filepath.IsAbs(input.PromptFile) {
			return fmt.Errorf("prompt_file must be relative to cwd")
		}
		clean := filepath.Clean(input.PromptFile)
		if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(filepath.Separator)) || strings.Contains(input.PromptFile, `\\`) {
			return fmt.Errorf("prompt_file must stay within cwd")
		}
		cwd := input.CWD
		if cwd == "" {
			var err error
			cwd, err = filepath.Abs(".")
			if err != nil {
				return fmt.Errorf("resolve cwd: %w", err)
			}
		}
		resolvedCWD, err := filepath.EvalSymlinks(cwd)
		if err != nil {
			return fmt.Errorf("resolve cwd: %w", err)
		}
		resolvedPrompt, err := filepath.EvalSymlinks(filepath.Join(cwd, clean))
		if err != nil {
			return fmt.Errorf("resolve prompt_file: %w", err)
		}
		relative, err := filepath.Rel(resolvedCWD, resolvedPrompt)
		if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
			return fmt.Errorf("prompt_file must stay within cwd")
		}
	}
	return nil
}

func validateActions(values []string, field string) error {
	if len(values) > 256 {
		return fmt.Errorf("%s supports at most 256 entries", field)
	}
	seen := make(map[string]struct{}, len(values))
	for index, value := range values {
		if strings.TrimSpace(value) != value || value == "" || len(value) > 256 || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("%s[%d] is invalid", field, index)
		}
		if _, exists := seen[value]; exists {
			return fmt.Errorf("%s contains duplicate %q", field, value)
		}
		seen[value] = struct{}{}
	}
	return nil
}

func (h *HTTPHandler) handleRead(writer http.ResponseWriter, runType, runID, action string) {
	switch action {
	case "status":
		value, err := h.svc.Status(runType, runID)
		writeReadResult(writer, value, err)
	case "logs":
		value, err := h.svc.Logs(runType, runID, 120)
		writeReadResult(writer, value, err)
	case "result":
		value, err := h.svc.ReadResult(runType, runID)
		writeReadResult(writer, value, err)
	default:
		writeJSON(writer, http.StatusNotFound, errorResponse{Error: "unknown run resource"})
	}
}

func writeReadResult(writer http.ResponseWriter, value any, err error) {
	if err != nil {
		writeJSON(writer, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}
	writeJSON(writer, http.StatusOK, value)
}

func writeJSON(writer http.ResponseWriter, status int, value any) {
	writer.Header().Set("Content-Type", "application/json")
	writer.WriteHeader(status)
	_ = json.NewEncoder(writer).Encode(value)
}
