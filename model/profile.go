// Package model implements the Runtime vNext model profile, Driver SPI, and
// exactly-one-call Model Service.
package model

import (
	"fmt"
	"net/url"
	"strings"
	"time"
)

type DriverName string

const (
	DriverOpenAICompatible    DriverName = "openai-compatible"
	DriverAnthropicCompatible DriverName = "anthropic-compatible"
)

type Auth struct {
	Header  string `json:"header"`
	Scheme  string `json:"scheme"`
	FromEnv string `json:"from_env"`
}

type Defaults struct {
	MaxCompletionTokens *int64   `json:"max_completion_tokens,omitempty"`
	MaxTokens           *int64   `json:"max_tokens,omitempty"`
	Temperature         *float64 `json:"temperature,omitempty"`
}

type ContextPolicy struct {
	WindowTokens         int64 `json:"window_tokens,omitempty"`
	ReservedOutputTokens int64 `json:"reserved_output_tokens,omitempty"`
	KeepRecentTurns      int   `json:"keep_recent_turns,omitempty"`
	SummaryEnabled       *bool `json:"summary_enabled,omitempty"`
}

type Profile struct {
	Driver   DriverName        `json:"driver"`
	Endpoint string            `json:"endpoint"`
	Model    string            `json:"model"`
	Auth     Auth              `json:"auth"`
	Headers  map[string]string `json:"headers,omitempty"`
	Defaults Defaults          `json:"defaults,omitempty"`
	Timeout  string            `json:"timeout"`
	Context  ContextPolicy     `json:"context,omitempty"`
}

func (profile Profile) Validate() error {
	switch profile.Driver {
	case DriverOpenAICompatible, DriverAnthropicCompatible:
	default:
		return fmt.Errorf("unsupported driver %q", profile.Driver)
	}
	endpoint, err := url.Parse(profile.Endpoint)
	if err != nil {
		return fmt.Errorf("endpoint: %w", err)
	}
	if endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
		endpoint.Fragment != "" || !endpoint.IsAbs() ||
		endpoint.EscapedPath() == "" || endpoint.EscapedPath() == "/" {
		return fmt.Errorf("endpoint must be a complete HTTPS URL with an explicit path and without userinfo or fragment")
	}
	if strings.TrimSpace(profile.Model) == "" || len(profile.Model) > 1024 {
		return fmt.Errorf("model is required and must not exceed 1024 bytes")
	}
	if !validHeaderName(profile.Auth.Header) {
		return fmt.Errorf("auth.header is invalid")
	}
	if profile.Auth.Scheme != "" && !validHeaderName(profile.Auth.Scheme) {
		return fmt.Errorf("auth.scheme must be an HTTP authentication scheme token")
	}
	if !validEnvironmentName(profile.Auth.FromEnv) {
		return fmt.Errorf("auth.from_env must be an environment variable name")
	}
	if len(profile.Headers) > 128 {
		return fmt.Errorf("headers exceed 128 items")
	}
	for name, value := range profile.Headers {
		if !validHeaderName(name) {
			return fmt.Errorf("headers contains invalid name %q", name)
		}
		if secretHeader(name) || strings.EqualFold(name, profile.Auth.Header) {
			return fmt.Errorf("headers[%q] is reserved for secret authentication", name)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("headers[%q] must be a single-line value", name)
		}
		if strings.Contains(value, "${") {
			return fmt.Errorf("headers[%q] cannot contain environment references", name)
		}
		if len(value) > 8192 {
			return fmt.Errorf("headers[%q] exceeds 8192 bytes", name)
		}
	}
	if profile.Defaults.MaxCompletionTokens != nil &&
		*profile.Defaults.MaxCompletionTokens <= 0 {
		return fmt.Errorf("defaults.max_completion_tokens must be positive")
	}
	if profile.Defaults.MaxTokens != nil && *profile.Defaults.MaxTokens <= 0 {
		return fmt.Errorf("defaults.max_tokens must be positive")
	}
	switch profile.Driver {
	case DriverOpenAICompatible:
		if profile.Defaults.MaxTokens != nil {
			return fmt.Errorf(
				"openai-compatible defaults use max_completion_tokens, not max_tokens",
			)
		}
	case DriverAnthropicCompatible:
		if profile.Defaults.MaxCompletionTokens != nil {
			return fmt.Errorf(
				"anthropic-compatible defaults use max_tokens, not max_completion_tokens",
			)
		}
		if profile.Defaults.MaxTokens == nil {
			return fmt.Errorf("anthropic-compatible defaults.max_tokens is required")
		}
	}
	if profile.Defaults.Temperature != nil &&
		(*profile.Defaults.Temperature < 0 || *profile.Defaults.Temperature > 2) {
		return fmt.Errorf("defaults.temperature must be between 0 and 2")
	}
	if profile.Context.WindowTokens < 0 || profile.Context.ReservedOutputTokens < 0 ||
		profile.Context.KeepRecentTurns < 0 {
		return fmt.Errorf("context numeric values must not be negative")
	}
	if profile.Context.WindowTokens > 0 {
		_, _, inputBudget, _ := profile.EffectiveContextBudget()
		if inputBudget < 2 {
			return fmt.Errorf("context window must leave at least 2 input tokens")
		}
	}
	timeout, err := time.ParseDuration(profile.Timeout)
	if err != nil || timeout <= 0 || timeout > 24*time.Hour {
		return fmt.Errorf("timeout must be a positive duration no greater than 24h")
	}
	return nil
}

func (profile Profile) EffectiveContextBudget() (
	window, reserved, inputBudget int64,
	configured bool,
) {
	window = profile.Context.WindowTokens
	if window <= 0 {
		return 0, 0, 0, false
	}
	reserved = profile.Context.ReservedOutputTokens
	if reserved == 0 {
		reserved = 8192
	}
	if tokenLimit := profile.DefaultTokenLimit(); tokenLimit != nil &&
		*tokenLimit > reserved {
		reserved = *tokenLimit
	}
	return window, reserved, window - reserved, true
}

func (profile Profile) DefaultTokenLimit() *int64 {
	switch profile.Driver {
	case DriverOpenAICompatible:
		return profile.Defaults.MaxCompletionTokens
	case DriverAnthropicCompatible:
		return profile.Defaults.MaxTokens
	default:
		return nil
	}
}

func (profile Profile) TimeoutDuration() time.Duration {
	value, _ := time.ParseDuration(profile.Timeout)
	return value
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		if !headerTokenByte(value[index]) {
			return false
		}
	}
	return true
}

func headerTokenByte(value byte) bool {
	if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
		value >= '0' && value <= '9' {
		return true
	}
	return strings.ContainsRune("!#$%&'*+-.^_`|~", rune(value))
}

func validEnvironmentName(value string) bool {
	if value == "" || !asciiLetter(value[0]) && value[0] != '_' {
		return false
	}
	for index := 1; index < len(value); index++ {
		current := value[index]
		if !asciiLetter(current) && (current < '0' || current > '9') && current != '_' {
			return false
		}
	}
	return true
}

func asciiLetter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}

func secretHeader(value string) bool {
	switch strings.ToLower(value) {
	case "authorization", "proxy-authorization", "x-api-key", "api-key", "cookie", "set-cookie":
		return true
	default:
		return false
	}
}
