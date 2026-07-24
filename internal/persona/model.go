package persona

type Persona struct {
	ID             string         `yaml:"id"`
	Name           string         `yaml:"name"`
	Description    string         `yaml:"description"`
	SystemPrompt   string         `yaml:"system_prompt"`
	StyleRules     []string       `yaml:"style_rules"`
	ModelPolicy    ModelPolicy    `yaml:"model_policy"`
	MemoryPolicy   MemoryPolicy   `yaml:"memory_policy"`
	ResponsePolicy ResponsePolicy `yaml:"response_policy"`
}

type ModelPolicy struct {
	Provider    string  `yaml:"provider"`
	Model       string  `yaml:"model"`
	Temperature float64 `yaml:"temperature"`
	MaxTokens   int     `yaml:"max_tokens"`
}

type MemoryPolicy struct {
	MaxContextTokens int  `yaml:"max_context_tokens"`
	KeepRecentTurns  int  `yaml:"keep_recent_turns"`
	SummaryEnabled   bool `yaml:"summary_enabled"`
	RetrievalEnabled bool `yaml:"retrieval_enabled"`
}

type ResponsePolicy struct {
	Format    string `yaml:"format"`
	Verbosity string `yaml:"verbosity"`
}
