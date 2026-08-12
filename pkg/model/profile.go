// Package model implements the SN Runtime model profile, Driver SPI, and
// exactly-one-call Model Service.
package model

import (
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
)

type DriverName string

const (
	DriverOpenAI    DriverName = "openai"
	DriverAnthropic DriverName = "anthropic"

	defaultContextWindowTokens  int64 = 32_768
	defaultReservedOutputTokens int64 = 8_192
)

// Parameters carries the model generation defaults merged into each request.
type Parameters struct {
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

// Profile is the API-only side of the unified Profile protocol. Authentication
// is configured directly under headers using ${VAR} references; the runtime
// expands references and, for the openai driver, prefixes a bare Authorization
// value with the Bearer scheme.
type Profile struct {
	Driver     DriverName        `json:"driver"`
	Endpoint   string            `json:"endpoint,omitempty"`
	BaseURL    string            `json:"base_url,omitempty"`
	Model      string            `json:"model"`
	Headers    map[string]string `json:"headers,omitempty"`
	Parameters Parameters        `json:"parameters,omitempty"`
	Timeout    string            `json:"timeout"`
	Context    ContextPolicy     `json:"context,omitempty"`
}

func (profile Profile) Validate() error {
	switch profile.Driver {
	case DriverOpenAI, DriverAnthropic:
	default:
		return fmt.Errorf("unsupported driver %q", profile.Driver)
	}
	if _, err := profile.ResolvedEndpoint(); err != nil {
		return err
	}
	if strings.TrimSpace(profile.Model) == "" || len(profile.Model) > 1024 {
		return fmt.Errorf("model is required and must not exceed 1024 bytes")
	}
	if len(profile.Headers) > 128 {
		return fmt.Errorf("headers exceed 128 items")
	}
	for name, value := range profile.Headers {
		if !validHeaderName(name) {
			return fmt.Errorf("headers contains invalid name %q", name)
		}
		if strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("headers[%q] must be a single-line value", name)
		}
		if len(value) > 8192 {
			return fmt.Errorf("headers[%q] exceeds 8192 bytes", name)
		}
	}
	if profile.Parameters.MaxTokens != nil && *profile.Parameters.MaxTokens <= 0 {
		return fmt.Errorf("parameters.max_tokens must be positive")
	}
	if profile.Driver == DriverAnthropic && profile.Parameters.MaxTokens == nil {
		return fmt.Errorf("anthropic parameters.max_tokens is required")
	}
	if profile.Parameters.Temperature != nil {
		if err := contract.ValidateTemperature(*profile.Parameters.Temperature); err != nil {
			return fmt.Errorf("parameters.temperature: %w", err)
		}
	}
	if profile.Parameters.TopP != nil {
		if err := contract.ValidateTopP(*profile.Parameters.TopP); err != nil {
			return fmt.Errorf("parameters.top_p: %w", err)
		}
	}
	if err := contract.ValidateStopSequences(profile.Parameters.StopSequences); err != nil {
		return fmt.Errorf("parameters.stop_sequences: %w", err)
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
	return profile.Parameters.MaxTokens
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
	case DriverOpenAI:
		return "v1/chat/completions", nil
	case DriverAnthropic:
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
