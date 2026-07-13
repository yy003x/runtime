package transport

import (
	"encoding/json"
	"net/http"
	"strings"

	"agent-arch/internal/agent"
)

type HTTPHandler struct {
	mux *http.ServeMux
	svc *agent.Service
}

func NewHTTPHandler(svc *agent.Service) *HTTPHandler {
	h := &HTTPHandler{
		mux: http.NewServeMux(),
		svc: svc,
	}
	h.routes()
	return h
}

func (h *HTTPHandler) ServeHTTP(w http.ResponseWriter, r *http.Request) {
	h.mux.ServeHTTP(w, r)
}

func (h *HTTPHandler) routes() {
	h.mux.HandleFunc("POST /v1/agents", h.handleCreateAgent)
	h.mux.HandleFunc("POST /v1/chat", h.handleChat)
	h.mux.HandleFunc("GET /v1/sessions/", h.handleGetMemory)
}

func (h *HTTPHandler) handleCreateAgent(w http.ResponseWriter, r *http.Request) {
	var req agent.CreateAgentRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body"})
		return
	}

	resp, err := h.svc.CreateAgent(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusCreated, resp)
}

func (h *HTTPHandler) handleChat(w http.ResponseWriter, r *http.Request) {
	var req agent.ChatRequest
	if err := json.NewDecoder(r.Body).Decode(&req); err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "invalid JSON body"})
		return
	}

	resp, err := h.svc.Chat(r.Context(), req)
	if err != nil {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func (h *HTTPHandler) handleGetMemory(w http.ResponseWriter, r *http.Request) {
	path := strings.TrimPrefix(r.URL.Path, "/v1/sessions/")
	if !strings.HasSuffix(path, "/memory") {
		http.NotFound(w, r)
		return
	}

	sessionID := strings.TrimSuffix(path, "/memory")
	sessionID = strings.TrimSuffix(sessionID, "/")
	if sessionID == "" {
		writeJSON(w, http.StatusBadRequest, errorResponse{Error: "missing session_id"})
		return
	}

	resp, err := h.svc.GetMemory(r.Context(), sessionID)
	if err != nil {
		writeJSON(w, http.StatusNotFound, errorResponse{Error: err.Error()})
		return
	}

	writeJSON(w, http.StatusOK, resp)
}

func writeJSON(w http.ResponseWriter, status int, value any) {
	w.Header().Set("Content-Type", "application/json")
	w.WriteHeader(status)
	_ = json.NewEncoder(w).Encode(value)
}
