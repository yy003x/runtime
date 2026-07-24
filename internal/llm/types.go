package llm

type Message struct {
	Role       string     `json:"role"`
	Content    string     `json:"content,omitempty"`
	ToolCallID string     `json:"tool_call_id,omitempty"`
	ToolCalls  []ToolCall `json:"tool_calls,omitempty"`
}

type Tool struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	Parameters  map[string]any `json:"parameters"`
}

type ToolCall struct {
	ID        string         `json:"id"`
	Name      string         `json:"name"`
	Arguments map[string]any `json:"arguments"`
}

type Request struct {
	Model       string
	System      string
	Messages    []Message
	Tools       []Tool
	Temperature float64
	MaxTokens   int
}

type Response struct {
	OutputText   string
	ToolCalls    []ToolCall
	FinishReason string
	Done         bool
	InputTokens  int
	OutputTokens int
}

type HTTPOptions struct {
	Headers    map[string]string
	AuthHeader string
	AuthPrefix string
}
