package config

import (
	"context"
	"fmt"
	"os"

	"gopkg.in/yaml.v3"
)

type Config struct {
	Server    ServerConfig              `yaml:"server"`
	Agent     AgentConfig               `yaml:"agent"`
	Providers map[string]ProviderConfig `yaml:"providers"`
	Memory    MemoryConfig              `yaml:"memory"`
	Debug     DebugConfig               `yaml:"debug"`
	Storage   StorageConfig             `yaml:"storage"`
}

type ServerConfig struct {
	HTTPAddr string `yaml:"http_addr"`
}

type AgentConfig struct {
	DefaultPersona  string `yaml:"default_persona"`
	DefaultProvider string `yaml:"default_provider"`
	DefaultModel    string `yaml:"default_model"`
}

type ProviderConfig struct {
	Enabled        bool   `yaml:"enabled"`
	APIKey         string `yaml:"api_key"`
	BaseURL        string `yaml:"base_url"`
	Model          string `yaml:"model"`
	TimeoutSeconds int    `yaml:"timeout_seconds"`
}

type MemoryConfig struct {
	MaxContextTokens       int     `yaml:"max_context_tokens"`
	ResponseReservedTokens int     `yaml:"response_reserved_tokens"`
	SafetyBufferTokens     int     `yaml:"safety_buffer_tokens"`
	RecentTurnsReserved    int     `yaml:"recent_turns_reserved_tokens"`
	SummaryReservedTokens  int     `yaml:"summary_reserved_tokens"`
	RetrievedReserved      int     `yaml:"retrieved_reserved_tokens"`
	CompressionThreshold   float64 `yaml:"compression_threshold"`
	KeepRecentTurns        int     `yaml:"keep_recent_turns"`
}

type StorageConfig struct {
	SessionStore   string `yaml:"session_store"`
	SummaryStore   string `yaml:"summary_store"`
	RetrievalStore string `yaml:"retrieval_store"`
}

type DebugConfig struct {
	LLMTraceDir      string `yaml:"llm_trace_dir"`
	LLMRequestLogDir string `yaml:"llm_request_log_dir"`
}

func Load(_ context.Context, path string) (Config, error) {
	raw, err := os.ReadFile(path)
	if err != nil {
		return Config{}, fmt.Errorf("read config %q: %w", path, err)
	}

	expanded := os.ExpandEnv(string(raw))

	var cfg Config
	if err := yaml.Unmarshal([]byte(expanded), &cfg); err != nil {
		return Config{}, fmt.Errorf("unmarshal config %q: %w", path, err)
	}

	if cfg.Server.HTTPAddr == "" {
		cfg.Server.HTTPAddr = ":8080"
	}
	if cfg.Debug.LLMTraceDir == "" {
		cfg.Debug.LLMTraceDir = cfg.Debug.LLMRequestLogDir
	}
	if cfg.Debug.LLMTraceDir == "" {
		cfg.Debug.LLMTraceDir = "logs/llm_traces"
	}
	applyProviderDefaults(&cfg)

	return cfg, nil
}

func applyProviderDefaults(cfg *Config) {
	if cfg.Providers == nil {
		return
	}

	if openaiCfg, ok := cfg.Providers["openai"]; ok {
		if openaiCfg.BaseURL == "" {
			openaiCfg.BaseURL = "https://api.openai.com/v1"
		}
		if openaiCfg.APIKey == "" {
			openaiCfg.APIKey = os.Getenv("OPENAI_API_KEY")
		}
		cfg.Providers["openai"] = openaiCfg
	}

	if anthropicCfg, ok := cfg.Providers["anthropic"]; ok {
		if anthropicCfg.BaseURL == "" {
			anthropicCfg.BaseURL = os.Getenv("ANTHROPIC_BASE_URL")
		}
		if anthropicCfg.BaseURL == "" {
			anthropicCfg.BaseURL = "https://api.anthropic.com"
		}
		if anthropicCfg.APIKey == "" {
			anthropicCfg.APIKey = os.Getenv("ANTHROPIC_AUTH_TOKEN")
		}
		if anthropicCfg.APIKey == "" {
			anthropicCfg.APIKey = os.Getenv("ANTHROPIC_API_KEY")
		}
		cfg.Providers["anthropic"] = anthropicCfg
	}
}
