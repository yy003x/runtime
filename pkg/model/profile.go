// Package model implements the SN Runtime model profile, Driver SPI, and
// exactly-one-call Model Service.
package model

import (
	"fmt"
	"net"
	"net/url"
	"strings"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
)

type DriverName string

const (
	DriverOpenAIChatCompletions DriverName = "openai-chat-completions"
	DriverOpenAIResponses       DriverName = "openai-responses"
	DriverOpenAICodexResponses  DriverName = "openai-codex-responses"
	DriverAnthropicMessages     DriverName = "anthropic-messages"
	OpenAICodexEndpoint                    = "https://chatgpt.com/backend-api/codex/responses"

	defaultContextWindowTokens  int64 = 32_768
	defaultReservedOutputTokens int64 = 8_192
)

// Parameters carries generation defaults and the upstream response mode.
type Parameters struct {
	MaxTokens     *int64   `json:"max_tokens,omitempty"`
	Temperature   *float64 `json:"temperature,omitempty"`
	TopP          *float64 `json:"top_p,omitempty"`
	StopSequences []string `json:"stop_sequences,omitempty"`
	Stream        *bool    `json:"stream,omitempty"`
}

// StreamingEnabled selects Provider SSE delivery, independently of whether a
// caller consumes Runtime events. Omission preserves the default streaming mode.
func (parameters Parameters) StreamingEnabled() bool {
	return parameters.Stream == nil || *parameters.Stream
}

type ContextPolicy struct {
	WindowTokens         int64 `json:"window_tokens,omitempty"`
	ReservedOutputTokens int64 `json:"reserved_output_tokens,omitempty"`
	KeepRecentTurns      int   `json:"keep_recent_turns,omitempty"`
	SummaryEnabled       *bool `json:"summary_enabled,omitempty"`
}

// Profile is the API-only side of the unified Profile protocol. Authentication
// is configured directly under headers using ${VAR} references; the runtime
// expands references and, for OpenAI drivers, prefixes a bare Authorization
// value with the Bearer scheme. openai-codex-responses instead obtains its
// credentials from ServiceOptions.Credentials and accepts no custom headers.
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
	case DriverOpenAIChatCompletions, DriverOpenAIResponses, DriverOpenAICodexResponses, DriverAnthropicMessages:
	default:
		return fmt.Errorf("unsupported driver %q", profile.Driver)
	}
	if _, err := profile.ResolvedEndpoint(); err != nil {
		return err
	}
	if profile.Driver == DriverOpenAICodexResponses {
		if len(profile.Headers) != 0 {
			return fmt.Errorf("openai-codex-responses uses managed OAuth credentials; headers must be empty")
		}
		if !profile.Parameters.StreamingEnabled() {
			return fmt.Errorf("openai-codex-responses requires parameters.stream=true")
		}
		if profile.Parameters.MaxTokens != nil || profile.Parameters.TopP != nil || len(profile.Parameters.StopSequences) != 0 {
			return fmt.Errorf("openai-codex-responses does not support max_tokens, top_p, or stop_sequences")
		}
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
	if profile.Driver == DriverAnthropicMessages && profile.Parameters.MaxTokens == nil {
		return fmt.Errorf("anthropic-messages parameters.max_tokens is required")
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
	if profile.Driver == DriverOpenAIResponses && len(profile.Parameters.StopSequences) != 0 {
		return fmt.Errorf("openai-responses does not support parameters.stop_sequences")
	}
	if profile.Context.WindowTokens < 0 || profile.Context.ReservedOutputTokens < 0 ||
		profile.Context.KeepRecentTurns < 0 {
		return fmt.Errorf("context numeric values must not be negative")
	}
	if profile.Driver != DriverOpenAIChatCompletions && profile.Driver != DriverOpenAIResponses {
		if err := profile.validateContextBudget(); err != nil {
			return err
		}
	}
	timeout, err := time.ParseDuration(profile.Timeout)
	if err != nil || timeout <= 0 || timeout > 24*time.Hour {
		return fmt.Errorf("timeout must be a positive duration no greater than 24h")
	}
	return nil
}

// ValidateManagedExecution rejects adapters that cannot losslessly round-trip
// the canonical conversation used by Session and durable Agent execution.
func (profile Profile) ValidateManagedExecution() error {
	if profile.Driver == DriverOpenAIResponses || profile.Driver == DriverOpenAICodexResponses {
		return fmt.Errorf("%s supports single calls only; Session and Agent execution are not supported", profile.Driver)
	}
	return profile.validateContextBudget()
}

func (profile Profile) validateContextBudget() error {
	_, _, inputBudget, _ := profile.EffectiveContextBudget()
	if inputBudget < 2 {
		return fmt.Errorf("context window must leave at least 2 input tokens")
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
	if profile.Driver == DriverOpenAICodexResponses {
		if profile.Endpoint == OpenAICodexEndpoint && profile.BaseURL == "" ||
			profile.Endpoint == "" && profile.BaseURL == "https://chatgpt.com/backend-api" {
			return OpenAICodexEndpoint, nil
		}
		return "", fmt.Errorf("openai-codex-responses requires endpoint %s or base_url https://chatgpt.com/backend-api", OpenAICodexEndpoint)
	}
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
		if !profile.validTransportURL(endpoint) || endpoint.User != nil || endpoint.Fragment != "" ||
			profile.Driver == DriverAnthropicMessages && (endpoint.EscapedPath() == "" || endpoint.EscapedPath() == "/") {
			return "", fmt.Errorf("endpoint must be an absolute HTTPS URL (HTTP allowed on loopback for generic OpenAI drivers), without userinfo or fragment; anthropic-messages requires an explicit path")
		}
		return endpoint.String(), nil
	}
	baseURL, err := url.Parse(profile.BaseURL)
	if err != nil {
		return "", fmt.Errorf("base_url: %w", err)
	}
	if !profile.validTransportURL(baseURL) || baseURL.User != nil ||
		baseURL.RawQuery != "" || baseURL.Fragment != "" {
		return "", fmt.Errorf("base_url must be an absolute HTTPS URL (HTTP allowed on loopback for generic OpenAI drivers), without userinfo, query, or fragment")
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

func (profile Profile) validTransportURL(value *url.URL) bool {
	if !value.IsAbs() || value.Hostname() == "" || value.Opaque != "" {
		return false
	}
	if value.Scheme == "https" {
		return true
	}
	if value.Scheme != "http" || (profile.Driver != DriverOpenAIChatCompletions && profile.Driver != DriverOpenAIResponses) {
		return false
	}
	ip := net.ParseIP(value.Hostname())
	return strings.EqualFold(value.Hostname(), "localhost") || ip != nil && ip.IsLoopback()
}

func defaultEndpointPath(driver DriverName) (string, error) {
	switch driver {
	case DriverOpenAIChatCompletions:
		return "chat/completions", nil
	case DriverOpenAIResponses:
		return "responses", nil
	case DriverAnthropicMessages:
		return "messages", nil
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
