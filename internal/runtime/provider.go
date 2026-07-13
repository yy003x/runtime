package runtime

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

type Provider interface {
	Run(ctx context.Context, profile Profile, req RunRequest) (ProviderResult, error)
}

type Registry struct {
	providers map[string]Provider
}

func DefaultRegistry() *Registry {
	registry := NewRegistry()
	registry.Register("command", CommandProvider{})
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

// CommandProvider runs Codex-compatible command profiles. The configured args
// are preserved, request images are appended as --image arguments, and the
// resolved prompt is provided on stdin.
type CommandProvider struct{}

func (CommandProvider) Run(ctx context.Context, profile Profile, req RunRequest) (ProviderResult, error) {
	command := strings.TrimSpace(profile.Provider.Command)
	if command == "" {
		return ProviderResult{ExitCode: 1}, fmt.Errorf("provider.command is required for command provider")
	}

	args := append([]string(nil), profile.Provider.Args...)
	for _, image := range req.Images {
		args = append(args, "--image", image)
	}

	cmd := exec.CommandContext(ctx, command, args...)
	cmd.Dir = req.CWD
	cmd.Env = commandEnvironment(profile.Provider.Env)
	cmd.Stdin = strings.NewReader(req.Prompt)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr

	err := cmd.Run()
	result := ProviderResult{
		Stdout:    stdout.String(),
		Stderr:    stderr.String(),
		FinalText: strings.TrimSpace(stdout.String()),
		ExitCode:  0,
	}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	result.ExitCode = 1
	return result, fmt.Errorf("run provider command %q: %w", command, err)
}

func commandEnvironment(overrides map[string]string) []string {
	env := append([]string(nil), os.Environ()...)
	for key, value := range overrides {
		env = append(env, key+"="+os.ExpandEnv(value))
	}
	return env
}
