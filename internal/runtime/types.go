package runtime

import "time"

const (
	StatusSucceeded = "succeeded"
	StatusFailed    = "failed"
	StatusBlocked   = "blocked"
	StatusTimeout   = "timeout"
	StatusCanceled  = "canceled"
)

const (
	StateCreated           = "created"
	StateLoadingConfig     = "loading_config"
	StatePreparingContext  = "preparing_context"
	StateResolvingProvider = "resolving_provider"
	StateRunning           = "running"
	StateExtractingOutput  = "extracting_output"
	StateWritingArtifacts  = "writing_artifacts"
)

const DefaultArtifactsRoot = "runs/global/runtime"

type Profile struct {
	Name      string          `yaml:"name" json:"name"`
	Provider  ProviderConfig  `yaml:"provider" json:"provider"`
	Runtime   RuntimeConfig   `yaml:"runtime" json:"runtime"`
	Artifacts ArtifactsConfig `yaml:"artifacts" json:"artifacts"`
}

type ProviderConfig struct {
	Type       string            `yaml:"type" json:"type"`
	EchoPrefix string            `yaml:"echo_prefix" json:"echo_prefix,omitempty"`
	Command    string            `yaml:"command" json:"command,omitempty"`
	Args       []string          `yaml:"args" json:"args,omitempty"`
	Env        map[string]string `yaml:"env" json:"env,omitempty"`
}

type RuntimeConfig struct {
	TimeoutSeconds int `yaml:"timeout_seconds" json:"timeout_seconds"`
}

type ArtifactsConfig struct {
	Root string `yaml:"root" json:"root"`
}

type RunOptions struct {
	Profile   string
	Prompt    string
	CWD       string
	SessionID string
}

type RunRequest struct {
	RunID     string    `json:"run_id"`
	Profile   string    `json:"profile"`
	Prompt    string    `json:"prompt"`
	CWD       string    `json:"cwd"`
	SessionID string    `json:"session_id,omitempty"`
	StartedAt time.Time `json:"started_at"`
}

type RunResult struct {
	RunID       string            `json:"run_id"`
	Profile     string            `json:"profile"`
	Provider    string            `json:"provider"`
	Status      string            `json:"status"`
	FinalText   string            `json:"final_text"`
	ExitCode    int               `json:"exit_code"`
	Artifacts   map[string]string `json:"artifacts"`
	StartedAt   time.Time         `json:"started_at"`
	CompletedAt time.Time         `json:"completed_at"`
	DurationMS  int64             `json:"duration_ms"`
	Error       string            `json:"error,omitempty"`
}

type Event struct {
	At      time.Time `json:"at"`
	State   string    `json:"state"`
	Message string    `json:"message,omitempty"`
}

type ProviderResult struct {
	Stdout    string
	Stderr    string
	FinalText string
	ExitCode  int
}
