package llm

type Message struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type Request struct {
	Model           string
	System          string
	Messages        []Message
	Temperature     float64
	MaxOutputTokens int
}

type Response struct {
	OutputText   string
	InputTokens  int
	OutputTokens int
}
