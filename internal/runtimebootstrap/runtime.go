package runtimebootstrap

import (
	"fmt"
	"os"
	"sort"
	"time"

	"github.com/yy003x/runtime/internal/agentrun"
	"github.com/yy003x/runtime/internal/provider"
	"github.com/yy003x/runtime/llmruntime"
)

func New(service *agentrun.Service) (*llmruntime.Runtime, error) {
	if service == nil {
		return nil, fmt.Errorf("agentrun service is required")
	}
	if _, err := service.Profiles(); err != nil {
		return nil, err
	}
	runtime, err := llmruntime.New(llmruntime.Options{
		ProfileDir: service.ConfigDir,
		AssetRoots: service.AssetRoots,
		HTTPClient: service.HTTPClient,
	})
	if err != nil {
		return nil, err
	}
	for _, server := range service.MCPServers {
		environment, err := mcpEnvironment(server)
		if err != nil {
			return nil, fmt.Errorf("MCP server %s: %w", server.Name, err)
		}
		if err := runtime.RegisterMCP(llmruntime.MCPConfig{
			Name: server.Name, Command: server.Command, Args: append([]string(nil), server.Args...),
			Dir: server.Dir, Env: environment, Timeout: time.Duration(server.TimeoutSeconds * float64(time.Second)),
		}); err != nil {
			return nil, err
		}
	}
	return runtime, nil
}

func mcpEnvironment(server agentrun.MCPServerSettings) ([]string, error) {
	values := make(map[string]string)
	for _, name := range []string{"PATH", "HOME", "TMPDIR", "LANG", "LC_ALL"} {
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	for _, name := range server.EnvPassthrough {
		if !agentrun.ValidEnvironmentName(name) {
			return nil, fmt.Errorf("invalid env_passthrough name %q", name)
		}
		if value, ok := os.LookupEnv(name); ok {
			values[name] = value
		}
	}
	for name, value := range server.Env {
		if !agentrun.ValidEnvironmentName(name) {
			return nil, fmt.Errorf("invalid env name %q", name)
		}
		resolved, err := provider.ResolveEnv(value)
		if err != nil {
			return nil, fmt.Errorf("env.%s: %w", name, err)
		}
		values[name] = resolved
	}
	names := make([]string, 0, len(values))
	for name := range values {
		names = append(names, name)
	}
	sort.Strings(names)
	environment := make([]string, 0, len(names))
	for _, name := range names {
		environment = append(environment, name+"="+values[name])
	}
	return environment, nil
}
