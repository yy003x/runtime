package model

import (
	"fmt"
	"sort"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/profileid"
)

type Catalog struct {
	profiles map[string]Profile
}

type ResolvedModel struct {
	ID       string
	Driver   DriverName
	Endpoint string
	Model    string
	Defaults Defaults
	Timeout  time.Duration
	headers  func() map[string]string
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

func (catalog *Catalog) resolve(
	id string,
	getenv func(string) (string, bool),
) (ResolvedModel, string, error) {
	profile, exists := catalog.Get(id)
	if !exists {
		return ResolvedModel{}, "", fmt.Errorf("model profile %q was not found", id)
	}
	secret, exists := getenv(profile.Auth.FromEnv)
	if !exists || secret == "" {
		return ResolvedModel{}, "", fmt.Errorf(
			"authentication environment variable %s is not set",
			profile.Auth.FromEnv,
		)
	}
	if !validSecretValue(secret) {
		return ResolvedModel{}, "", fmt.Errorf(
			"authentication environment variable %s contains invalid header characters",
			profile.Auth.FromEnv,
		)
	}
	endpoint, err := profile.ResolvedEndpoint()
	if err != nil {
		return ResolvedModel{}, "", err
	}
	headers := make(map[string]string, len(profile.Headers)+1)
	for name, value := range profile.Headers {
		headers[name] = value
	}
	authValue := secret
	if scheme := strings.TrimSpace(profile.Auth.Scheme); scheme != "" {
		authValue = scheme + " " + secret
	}
	headers[profile.Auth.Header] = authValue
	headerSource := func() map[string]string {
		values := make(map[string]string, len(headers))
		for name, value := range headers {
			values[name] = value
		}
		return values
	}
	return ResolvedModel{
		ID: id, Driver: profile.Driver, Endpoint: endpoint, Model: profile.Model,
		Defaults: profile.Defaults, Timeout: profile.TimeoutDuration(),
		headers: headerSource,
	}, secret, nil
}

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

func (resolved ResolvedModel) DefaultTokenLimit() *int64 {
	return resolved.Defaults.MaxTokens
}

func cloneProfile(profile Profile) Profile {
	result := profile
	if profile.Headers != nil {
		result.Headers = make(map[string]string, len(profile.Headers))
		for name, value := range profile.Headers {
			result.Headers[name] = value
		}
	}
	if profile.Defaults.MaxTokens != nil {
		value := *profile.Defaults.MaxTokens
		result.Defaults.MaxTokens = &value
	}
	if profile.Defaults.Temperature != nil {
		value := *profile.Defaults.Temperature
		result.Defaults.Temperature = &value
	}
	if profile.Defaults.TopP != nil {
		value := *profile.Defaults.TopP
		result.Defaults.TopP = &value
	}
	if profile.Defaults.StopSequences != nil {
		result.Defaults.StopSequences = append(
			[]string(nil), profile.Defaults.StopSequences...,
		)
	}
	if profile.Context.SummaryEnabled != nil {
		value := *profile.Context.SummaryEnabled
		result.Context.SummaryEnabled = &value
	}
	return result
}
