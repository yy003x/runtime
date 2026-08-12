package model

import (
	"fmt"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/domain/profileid"
	"github.com/yy003x/runtime/internal/infrastructure/envref"
	"github.com/yy003x/runtime/pkg/provider"
)

const authorizationHeader = "Authorization"

type Catalog struct {
	profiles map[string]Profile
}

type ResolvedModel struct {
	ID         string
	Driver     DriverName
	Endpoint   string
	Model      string
	Parameters Parameters
	Timeout    time.Duration
	headers    func() map[string]string
	logHeaders func() map[string]string
}

func NewCatalog(values map[string]Profile, reservedIDs ...string) (*Catalog, error) {
	reserved := make(map[string]struct{}, len(reservedIDs))
	for _, id := range reservedIDs {
		reserved[id] = struct{}{}
	}
	profiles := make(map[string]Profile, len(values))
	for id, profile := range values {
		if err := profileid.Validate(id); err != nil {
			return nil, fmt.Errorf("model profile %q: %w", id, err)
		}
		if _, exists := reserved[id]; exists {
			return nil, fmt.Errorf("model profile %q conflicts with a reserved profile ID", id)
		}
		if err := profile.Validate(); err != nil {
			return nil, fmt.Errorf("model profile %q: %w", id, err)
		}
		profiles[id] = cloneProfile(profile)
	}
	return &Catalog{profiles: profiles}, nil
}

func (catalog *Catalog) Get(id string) (Profile, bool) {
	if catalog == nil {
		return Profile{}, false
	}
	profile, exists := catalog.profiles[id]
	if !exists {
		return Profile{}, false
	}
	return cloneProfile(profile), true
}

func (catalog *Catalog) IDs() []string {
	if catalog == nil {
		return nil
	}
	values := make([]string, 0, len(catalog.profiles))
	for id := range catalog.profiles {
		values = append(values, id)
	}
	sort.Strings(values)
	return values
}

// resolve expands ${VAR} references in every header, applies driver-specific
// authentication shaping, and returns the resolved model plus the secret
// values that must be redacted from streamed events, errors, and diagnostics.
// Every ${VAR} replacement and every literal sensitive-header value is secret.
func (catalog *Catalog) resolve(
	id string,
	getenv func(string) (string, bool),
) (ResolvedModel, []string, error) {
	profile, exists := catalog.Get(id)
	if !exists {
		return ResolvedModel{}, nil, fmt.Errorf("model profile %q was not found", id)
	}
	endpoint, err := profile.ResolvedEndpoint()
	if err != nil {
		return ResolvedModel{}, nil, err
	}
	headers := make(map[string]string, len(profile.Headers))
	logHeaders := make(map[string]string, len(profile.Headers))
	secrets := make([]string, 0, len(profile.Headers))
	for name, rawValue := range profile.Headers {
		var referencedSecrets []string
		value, expandErr := envref.Expand(rawValue, func(name string) (string, bool) {
			value, exists := getenv(name)
			if exists {
				referencedSecrets = append(referencedSecrets, value)
			}
			return value, exists
		})
		if expandErr != nil {
			return ResolvedModel{}, nil, fmt.Errorf("headers[%q]: %w", name, expandErr)
		}
		if value == "" {
			return ResolvedModel{}, nil, fmt.Errorf("headers[%q] resolves to an empty value", name)
		}
		if !validSecretValue(value) {
			return ResolvedModel{}, nil, fmt.Errorf(
				"headers[%q] contains invalid header characters", name,
			)
		}
		sensitive := strings.Contains(rawValue, "${")
		logValue := rawValue
		secrets = append(secrets, referencedSecrets...)
		if sensitive || provider.SensitiveHeader(name) {
			secrets = append(secrets, value)
		}
		if !sensitive && provider.SensitiveHeader(name) {
			logValue = "[REDACTED]"
		}
		if sensitive && strings.EqualFold(name, authorizationHeader) &&
			profile.Driver == DriverOpenAI && !hasAuthScheme(value) {
			value = bearerScheme + " " + value
			logValue = bearerScheme + " " + logValue
		}
		headers[name] = value
		logHeaders[http.CanonicalHeaderKey(name)] = logValue
	}
	headerSource := func() map[string]string {
		values := make(map[string]string, len(headers))
		for name, value := range headers {
			values[name] = value
		}
		return values
	}
	logHeaderSource := func() map[string]string {
		values := make(map[string]string, len(logHeaders))
		for name, value := range logHeaders {
			values[name] = value
		}
		return values
	}
	return ResolvedModel{
		ID: id, Driver: profile.Driver, Endpoint: endpoint, Model: profile.Model,
		Parameters: profile.Parameters, Timeout: profile.TimeoutDuration(),
		headers: headerSource, logHeaders: logHeaderSource,
	}, secrets, nil
}

// hasAuthScheme reports whether value already begins with an authentication
// scheme token such as "Bearer" or "Basic", detected by a separating space.
func hasAuthScheme(value string) bool {
	return strings.Contains(value, " ")
}

const bearerScheme = "Bearer"

func validSecretValue(value string) bool {
	for index := 0; index < len(value); index++ {
		if value[index] < 0x20 || value[index] == 0x7f {
			return false
		}
	}
	return true
}

func (resolved ResolvedModel) RequestHeaders() map[string]string {
	if resolved.headers == nil {
		return nil
	}
	return resolved.headers()
}

// LogRequestHeaders returns the non-secret Profile header view that matches
// driver authentication shaping. ${VAR} references remain unresolved.
func (resolved ResolvedModel) LogRequestHeaders() map[string]string {
	if resolved.logHeaders == nil {
		return nil
	}
	return resolved.logHeaders()
}

func (resolved ResolvedModel) DefaultTokenLimit() *int64 {
	return resolved.Parameters.MaxTokens
}

func cloneProfile(profile Profile) Profile {
	result := profile
	if profile.Headers != nil {
		result.Headers = make(map[string]string, len(profile.Headers))
		for name, value := range profile.Headers {
			result.Headers[name] = value
		}
	}
	if profile.Parameters.MaxTokens != nil {
		value := *profile.Parameters.MaxTokens
		result.Parameters.MaxTokens = &value
	}
	if profile.Parameters.Temperature != nil {
		value := *profile.Parameters.Temperature
		result.Parameters.Temperature = &value
	}
	if profile.Parameters.TopP != nil {
		value := *profile.Parameters.TopP
		result.Parameters.TopP = &value
	}
	if profile.Parameters.StopSequences != nil {
		result.Parameters.StopSequences = append(
			[]string(nil), profile.Parameters.StopSequences...,
		)
	}
	if profile.Context.SummaryEnabled != nil {
		value := *profile.Context.SummaryEnabled
		result.Context.SummaryEnabled = &value
	}
	return result
}
