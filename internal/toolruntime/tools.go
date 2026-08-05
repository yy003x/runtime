// Package toolruntime 把不同 owner 的工具实现组合为注入 Agent Kernel 的单一
// executor。
package toolruntime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sort"

	"github.com/yy003x/runtime/agent"
)

const (
	ExecutionImplementation        = "runtime.toolruntime"
	ExecutionImplementationVersion = 1
	configurationSchemaVersion     = 1
)

// Component 表示一个独立版本化的工具实现。Identity.Configuration 只能包含
// 非 secret 值或尚未解析的 secret 引用；Build 不解析环境变量。
type Component struct {
	Identity agent.ToolExecutionIdentity
	Tools    []agent.RegisteredTool
}

type executionConfiguration struct {
	SchemaVersion int                           `json:"schema_version"`
	Components    []agent.ToolExecutionSnapshot `json:"components"`
}

type frozenComponent struct {
	snapshot  agent.ToolExecutionSnapshot
	canonical []byte
	tools     []agent.RegisteredTool
}

// Build 独立校验每个 child，冻结其 identity、configuration 和 definitions，
// 再创建一个路由完整选中工具集的 registry。空 component 不影响执行，因此不写入
// snapshot。
func Build(components ...Component) (*agent.Registry, error) {
	frozen := make([]frozenComponent, 0, len(components))
	for index, component := range components {
		if len(component.Tools) == 0 {
			continue
		}
		registry, err := agent.NewRegistryWithToolExecution(
			component.Identity, component.Tools...,
		)
		if err != nil {
			return nil, fmt.Errorf("tool component %d: %w", index, err)
		}
		snapshot := registry.ToolExecutionSnapshot()
		canonical, err := snapshot.CanonicalJSON()
		if err != nil {
			return nil, fmt.Errorf("freeze tool component %d: %w", index, err)
		}
		if err := json.Unmarshal(canonical, &snapshot); err != nil {
			return nil, fmt.Errorf("decode tool component %d snapshot: %w", index, err)
		}
		frozen = append(frozen, frozenComponent{
			snapshot:  snapshot,
			canonical: append([]byte(nil), canonical...),
			tools:     append([]agent.RegisteredTool(nil), component.Tools...),
		})
	}
	sort.Slice(frozen, func(left, right int) bool {
		return bytes.Compare(frozen[left].canonical, frozen[right].canonical) < 0
	})
	configuration := executionConfiguration{
		SchemaVersion: configurationSchemaVersion,
		Components:    make([]agent.ToolExecutionSnapshot, 0, len(frozen)),
	}
	tools := make([]agent.RegisteredTool, 0)
	for _, component := range frozen {
		configuration.Components = append(
			configuration.Components, component.snapshot,
		)
		tools = append(tools, component.tools...)
	}
	rawConfiguration, err := json.Marshal(configuration)
	if err != nil {
		return nil, fmt.Errorf("encode composite tool configuration: %w", err)
	}
	registry, err := agent.NewRegistryWithToolExecution(
		agent.ToolExecutionIdentity{
			Implementation:        ExecutionImplementation,
			ImplementationVersion: ExecutionImplementationVersion,
			Configuration:         rawConfiguration,
		},
		tools...,
	)
	if err != nil {
		return nil, fmt.Errorf("build composite tool registry: %w", err)
	}
	return registry, nil
}
