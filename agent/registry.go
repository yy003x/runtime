package agent

import (
	"context"
	"fmt"
	"sort"
	"sync"

	"github.com/yy003x/runtime/contract"
)

type ToolHandler func(context.Context, ToolRequest) (ToolResult, error)

type RegisteredTool struct {
	Definition contract.ToolSpec
	Handler    ToolHandler
}

type Registry struct {
	mu    sync.RWMutex
	tools map[string]RegisteredTool
}

func NewRegistry(values ...RegisteredTool) (*Registry, error) {
	registry := &Registry{tools: make(map[string]RegisteredTool, len(values))}
	for _, value := range values {
		if err := registry.Register(value); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (registry *Registry) Register(value RegisteredTool) error {
	if registry == nil {
		return fmt.Errorf("tool registry is nil")
	}
	if err := value.Definition.Validate(); err != nil {
		return err
	}
	if value.Handler == nil {
		return fmt.Errorf("tool %q handler is required", value.Definition.Name)
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.tools[value.Definition.Name]; exists {
		return fmt.Errorf("tool %q is already registered", value.Definition.Name)
	}
	registry.tools[value.Definition.Name] = value
	return nil
}

func (registry *Registry) Definitions() []contract.ToolSpec {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	names := make([]string, 0, len(registry.tools))
	for name := range registry.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]contract.ToolSpec, 0, len(names))
	for _, name := range names {
		value := registry.tools[name].Definition
		value.InputSchema = append([]byte(nil), value.InputSchema...)
		values = append(values, value)
	}
	return values
}

func (registry *Registry) Execute(
	ctx context.Context,
	request ToolRequest,
) (ToolResult, error) {
	if registry == nil {
		return ToolResult{}, fmt.Errorf("tool registry is unavailable")
	}
	registry.mu.RLock()
	value, exists := registry.tools[request.Name]
	registry.mu.RUnlock()
	if !exists {
		return ToolResult{}, fmt.Errorf("unknown tool %q", request.Name)
	}
	return value.Handler(ctx, request)
}
