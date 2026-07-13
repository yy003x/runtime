package agent

import "agent-arch/internal/persona"

type Session struct {
	ID       string
	Persona  persona.Persona
	Provider string
	Model    string
}

type CreateAgentRequest struct {
	PersonaID string `json:"persona_id"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
}

type CreateAgentResponse struct {
	SessionID string `json:"session_id"`
	PersonaID string `json:"persona_id"`
	Provider  string `json:"provider"`
	Model     string `json:"model"`
}

type ChatRequest struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}

type ChatResponse struct {
	SessionID string `json:"session_id"`
	Message   string `json:"message"`
}
