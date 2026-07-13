package runtime

import (
	"context"
	"fmt"
	"sort"
)

type Provider interface {
	Run(ctx context.Context, profile Profile, req RunRequest) (ProviderResult, error)
}

type Registry struct {
	providers map[string]Provider
}

func DefaultRegistry() *Registry {
	registry := NewRegistry()
	registry.Register("fake", FakeProvider{})
	return registry
}

func NewRegistry() *Registry {
	return &Registry{providers: make(map[string]Provider)}
}

func (r *Registry) Register(providerType string, provider Provider) {
	r.providers[providerType] = provider
}

func (r *Registry) Resolve(providerType string) (Provider, error) {
	provider, ok := r.providers[providerType]
	if !ok {
		return nil, fmt.Errorf("provider type %q is not registered", providerType)
	}
	return provider, nil
}

func (r *Registry) Types() []string {
	types := make([]string, 0, len(r.providers))
	for providerType := range r.providers {
		types = append(types, providerType)
	}
	sort.Strings(types)
	return types
}

func ProviderTypes() []string {
	return DefaultRegistry().Types()
}

type FakeProvider struct{}

func (FakeProvider) Run(_ context.Context, profile Profile, req RunRequest) (ProviderResult, error) {
	finalText := profile.Provider.EchoPrefix + req.Prompt
	return ProviderResult{
		Stdout:    finalText + "\n",
		FinalText: finalText,
		ExitCode:  0,
	}, nil
}
