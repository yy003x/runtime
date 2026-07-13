package memory

type Turn struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type SummaryBlock struct {
	CompressedTurns int      `json:"compressed_turns"`
	Bullets         []string `json:"bullets"`
}

type RetrievedMemory struct {
	Source  string `json:"source"`
	Content string `json:"content"`
}

type SessionMemory struct {
	SessionID string            `json:"session_id"`
	Turns     []Turn            `json:"turns"`
	Summary   *SummaryBlock     `json:"summary,omitempty"`
	Retrieved []RetrievedMemory `json:"retrieved"`
}
