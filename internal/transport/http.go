package transport

import (
	"encoding/json"
	"net/http"
	"strings"

	"agent-runtime/internal/agentrun"
	"agent-runtime/internal/provider"
)

type HTTPHandler struct {
	mux *http.ServeMux
	svc *agentrun.Service
}

type RunRequest struct {
	RunID             string         `json:"run_id,omitempty"`
	RunType           string         `json:"run_type,omitempty"`
	Profile           string         `json:"profile"`
	ProjectID         string         `json:"project_id,omitempty"`
	CWD               string         `json:"cwd,omitempty"`
	Prompt            string         `json:"prompt"`
	PromptFile        string         `json:"prompt_file,omitempty"`
	ExecutionMode     string         `json:"execution_mode,omitempty"`
	DeadlineSeconds   int            `json:"deadline_seconds,omitempty"`
	ProviderOverrides map[string]any `json:"provider_overrides,omitempty"`
}

type ControlRequest struct {
	Reason string                `json:"reason,omitempty"`
	Patch  *provider.NativePatch `json:"patch,omitempty"`
}

func NewHTTPHandler(service *agentrun.Service) *HTTPHandler {
	handler := &HTTPHandler{mux: http.NewServeMux(), svc: service}
	handler.routes()
	return handler
}

func (h *HTTPHandler) ServeHTTP(writer http.ResponseWriter, request *http.Request) {
	h.mux.ServeHTTP(writer, request)
}

func (h *HTTPHandler) routes() {
	h.mux.HandleFunc("GET /healthz", h.handleHealth)
	h.mux.HandleFunc("POST /v1/runs", h.handleRun)
	h.mux.HandleFunc("/v1/runs/", h.handleRunByID)
}

func (h *HTTPHandler) handleHealth(writer http.ResponseWriter, _ *http.Request) {
	writeJSON(writer, http.StatusOK, map[string]any{"ok": true, "runtime": h.svc.RuntimeVersion})
}

func (h *HTTPHandler) handleRun(writer http.ResponseWriter, request *http.Request) {
	var input RunRequest
	if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
		writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "invalid JSON body"})
		return
	}
	summary, err := h.svc.Run(request.Context(), agentrun.RunOptions{
		RunID: input.RunID, RunType: input.RunType, Profile: input.Profile, ProjectID: input.ProjectID,
		CWD: input.CWD, Prompt: input.Prompt, PromptFile: input.PromptFile, ExecutionMode: input.ExecutionMode,
		DeadlineSeconds: input.DeadlineSeconds, ProviderOverrides: input.ProviderOverrides,
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
		if err := json.NewDecoder(request.Body).Decode(&input); err != nil {
			writeJSON(writer, http.StatusBadRequest, errorResponse{Error: "invalid JSON body"})
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
