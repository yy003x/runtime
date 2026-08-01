// Package model implements the Runtime vNext model profile, Driver SPI, and
// exactly-one-call Model Service.
package model

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/yy003x/runtime/contract"
)

type DriverName string

const (
	DriverOpenAICompatible    DriverName = "openai-compatible"
	DriverAnthropicCompatible DriverName = "anthropic-compatible"

	defaultContextWindowTokens  int64 = 32_768
	defaultReservedOutputTokens int64 = 8_192
)

type Auth struct {
	Header  string `json:"header"`
	Scheme  string `json:"scheme"`
	FromEnv string `json:"from_env"`
}

type Defaults struct {
	MaxTokens     *int64   `json:"max_tokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	StopSequences []string `json:"stop_sequences,omitempty"`
}

type ContextPolicy struct {
	WindowTokens         int64 `json:"window_tokens,omitempty"`
	ReservedOutputTokens int64 `json:"reserved_output_tokens,omitempty"`
	KeepRecentTurns      int   `json:"keep_recent_turns,omitempty"`
	SummaryEnabled       *bool `json:"summary_enabled,omitempty"`
}

type Profile struct {
	Driver   DriverName        `json:"driver"`
	Endpoint string            `json:"endpoint,omitempty"`
	BaseURL  string            `json:"base_url,omitempty"`
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
	if _, err := profile.ResolvedEndpoint(); err != nil {
		return err
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
	if profile.Defaults.MaxTokens != nil && *profile.Defaults.MaxTokens <= 0 {
		return fmt.Errorf("defaults.max_tokens must be positive")
	}
	if profile.Driver == DriverAnthropicCompatible && profile.Defaults.MaxTokens == nil {
		return fmt.Errorf("anthropic-compatible defaults.max_tokens is required")
	}
	if profile.Defaults.Temperature != nil {
		if err := contract.ValidateTemperature(*profile.Defaults.Temperature); err != nil {
			return fmt.Errorf("defaults.temperature: %w", err)
		}
	}
	if profile.Defaults.TopP != nil {
		if err := contract.ValidateTopP(*profile.Defaults.TopP); err != nil {
			return fmt.Errorf("defaults.top_p: %w", err)
		}
	}
	if err := contract.ValidateStopSequences(profile.Defaults.StopSequences); err != nil {
		return fmt.Errorf("defaults.stop_sequences: %w", err)
	}
	if profile.Context.WindowTokens < 0 || profile.Context.ReservedOutputTokens < 0 ||
		profile.Context.KeepRecentTurns < 0 {
		return fmt.Errorf("context numeric values must not be negative")
	}
	_, _, inputBudget, _ := profile.EffectiveContextBudget()
	if inputBudget < 2 {
		return fmt.Errorf("context window must leave at least 2 input tokens")
	}
	timeout, err := time.ParseDuration(profile.Timeout)
	if err != nil || timeout <= 0 || timeout > 24*time.Hour {
		return fmt.Errorf("timeout must be a positive duration no greater than 24h")
	}
	return nil
}

func (profile Profile) EffectiveContextBudget() (
	window, reserved, inputBudget int64,
	explicit bool,
) {
	return profile.EffectiveContextBudgetForRequest(nil)
}

func (profile Profile) EffectiveContextBudgetForRequest(
	maxOutputTokens *int64,
) (
	window, reserved, inputBudget int64,
	explicit bool,
) {
	window = profile.Context.WindowTokens
	explicit = window > 0
	if !explicit {
		window = defaultContextWindowTokens
	}
	reserved = profile.Context.ReservedOutputTokens
	if reserved == 0 {
		reserved = defaultReservedOutputTokens
	}
	if tokenLimit := profile.DefaultTokenLimit(); tokenLimit != nil &&
		*tokenLimit > reserved {
		reserved = *tokenLimit
	}
	if maxOutputTokens != nil && *maxOutputTokens > reserved {
		reserved = *maxOutputTokens
	}
	return window, reserved, window - reserved, explicit
}

func (profile Profile) DefaultTokenLimit() *int64 {
	return profile.Defaults.MaxTokens
}

func (profile Profile) ResolvedEndpoint() (string, error) {
	hasEndpoint := profile.Endpoint != ""
	hasBaseURL := profile.BaseURL != ""
	if hasEndpoint == hasBaseURL {
		return "", fmt.Errorf("exactly one of endpoint or base_url is required")
	}
	if hasEndpoint {
		endpoint, err := url.Parse(profile.Endpoint)
		if err != nil {
			return "", fmt.Errorf("endpoint: %w", err)
		}
		if endpoint.Scheme != "https" || endpoint.Host == "" || endpoint.User != nil ||
			endpoint.Fragment != "" || !endpoint.IsAbs() ||
			endpoint.EscapedPath() == "" || endpoint.EscapedPath() == "/" {
			return "", fmt.Errorf("endpoint must be a complete HTTPS URL with an explicit path and without userinfo or fragment")
		}
		return endpoint.String(), nil
	}
	baseURL, err := url.Parse(profile.BaseURL)
	if err != nil {
		return "", fmt.Errorf("base_url: %w", err)
	}
	if baseURL.Scheme != "https" || baseURL.Host == "" || baseURL.User != nil ||
		baseURL.RawQuery != "" || baseURL.Fragment != "" || !baseURL.IsAbs() {
		return "", fmt.Errorf("base_url must be an absolute HTTPS URL without userinfo, query, or fragment")
	}
	path, err := defaultEndpointPath(profile.Driver)
	if err != nil {
		return "", err
	}
	resolved, err := url.JoinPath(profile.BaseURL, path)
	if err != nil {
		return "", fmt.Errorf("base_url: %w", err)
	}
	return resolved, nil
}

func defaultEndpointPath(driver DriverName) (string, error) {
	switch driver {
	case DriverOpenAICompatible:
		return "v1/chat/completions", nil
	case DriverAnthropicCompatible:
		return "v1/messages", nil
	default:
		return "", fmt.Errorf("unsupported driver %q", driver)
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
