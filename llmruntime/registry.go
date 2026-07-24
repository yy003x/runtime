package llmruntime

import (
	"context"
	"fmt"
	"strings"
	"sync"
	"time"

	"github.com/yy003x/runtime/runtimeapi"
)

type ToolHandler func(context.Context, map[string]any) (any, error)

type MemoryProvider interface {
	Recall(context.Context, runtimeapi.MemoryQuery) ([]runtimeapi.MemoryItem, error)
}

type MemoryProviderFunc func(context.Context, runtimeapi.MemoryQuery) ([]runtimeapi.MemoryItem, error)

func (function MemoryProviderFunc) Recall(ctx context.Context, query runtimeapi.MemoryQuery) ([]runtimeapi.MemoryItem, error) {
	return function(ctx, query)
}

type MCPConfig struct {
	Name    string
	Command string
	Args    []string
	Dir     string
	Env     []string
	Timeout time.Duration
}

type registeredTool struct {
	schema  runtimeapi.Tool
	handler ToolHandler
}

type registry struct {
	mu     sync.RWMutex
	tools  map[string]registeredTool
	mcp    map[string]MCPConfig
	memory map[string]MemoryProvider
}

func newRegistry() *registry {
	return &registry{
		tools: make(map[string]registeredTool), mcp: make(map[string]MCPConfig),
		memory: make(map[string]MemoryProvider),
	}
}

func (r *registry) registerMemoryProvider(name string, provider MemoryProvider) error {
	name = strings.TrimSpace(name)
	if name == "" || provider == nil {
		return fmt.Errorf("memory provider name and implementation are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.memory[name]; exists {
		return fmt.Errorf("memory provider %s is already registered", name)
	}
	r.memory[name] = provider
	return nil
}

func (r *registry) memoryProvider(name string) (MemoryProvider, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	provider, ok := r.memory[name]
	if !ok {
		return nil, fmt.Errorf("unknown memory provider %s", name)
	}
	return provider, nil
}

func (r *registry) registerTool(schema runtimeapi.Tool, handler ToolHandler) error {
	schema.Name = strings.TrimSpace(schema.Name)
	if schema.Name == "" || handler == nil {
		return fmt.Errorf("tool name and handler are required")
	}
	if schema.Parameters == nil {
		return fmt.Errorf("tool %s parameters are required", schema.Name)
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.tools[schema.Name]; exists {
		return fmt.Errorf("tool %s is already registered", schema.Name)
	}
	r.tools[schema.Name] = registeredTool{schema: schema, handler: handler}
	return nil
}

func (r *registry) registerMCP(config MCPConfig) error {
	config.Name = strings.TrimSpace(config.Name)
	if config.Name == "" || strings.TrimSpace(config.Command) == "" {
		return fmt.Errorf("MCP name and command are required")
	}
	r.mu.Lock()
	defer r.mu.Unlock()
	if _, exists := r.mcp[config.Name]; exists {
		return fmt.Errorf("MCP server %s is already registered", config.Name)
	}
	r.mcp[config.Name] = config
	return nil
}

func (r *registry) selectedTools(names []string) ([]registeredTool, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	values := make([]registeredTool, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("duplicate registered tool %s", name)
		}
		value, ok := r.tools[name]
		if !ok {
			return nil, fmt.Errorf("unknown registered tool %s", name)
		}
		seen[name] = struct{}{}
		values = append(values, value)
	}
	return values, nil
}

func (r *registry) selectedMCP(names []string) ([]MCPConfig, error) {
	r.mu.RLock()
	defer r.mu.RUnlock()
	values := make([]MCPConfig, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if _, duplicate := seen[name]; duplicate {
			return nil, fmt.Errorf("duplicate MCP server %s", name)
		}
		value, ok := r.mcp[name]
		if !ok {
			return nil, fmt.Errorf("unknown MCP server %s", name)
		}
		seen[name] = struct{}{}
		values = append(values, value)
	}
	return values, nil
}
