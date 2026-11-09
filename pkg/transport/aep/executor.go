package aep

import (
	"os"
	"path/filepath"
	"strings"

	runtimecommand "github.com/yy003x/sn-code/pkg/command"
	"github.com/yy003x/sn-code/pkg/profile"
)

// 执行器静态登记表。能力位按当前投影如实上报：streaming/structuredEvents
// 跟随 adapter 的 StreamDecoder 能力（codex/claude/pi 为 true），sessionResume
// 对已登记执行器为 true（同一 managed Session 新 Turn，不推断原生 resume
// identity）；acp 兼容 adapter 延后（registered=false）。
type executorDefinition struct {
	id         string
	name       string
	basename   string
	registered bool
}

var executorDefinitions = []executorDefinition{
	{id: "codex", name: "Codex CLI", basename: "codex", registered: true},
	{id: "claude-code", name: "Claude Code", basename: "claude", registered: true},
	{id: "pi", name: "Pi Coding Agent", basename: "pi", registered: true},
	{id: "acp", name: "ACP Compatibility", basename: "", registered: false},
}

// capabilitiesFor 返回当前 Runtime 投影层对该执行器实际交付的能力：流式
// 与结构化事件仅在 command adapter 实现 StreamDecoder 时为 true（codex/
// claude/pi 已落地），sessionResume 对已登记执行器为 true（resumeFromRunId
// 复用源 Run 的 managed Session 开新 Turn）。
// 参数 definition 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 Capabilities 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func capabilitiesFor(definition executorDefinition) Capabilities {
	streaming := false
	if definition.registered {
		if adapter, err := runtimecommand.Resolve(definition.basename); err == nil {
			_, streaming = adapter.(runtimecommand.StreamDecoder)
		}
	}
	return Capabilities{
		Streaming:        streaming,
		Cancellation:     true,
		StructuredEvents: streaming,
		SessionResume:    definition.registered,
		NativeResult:     true,
	}
}

// executorDefinitionByID 执行 executorDefinitionByID 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 id 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 executorDefinition、bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func executorDefinitionByID(id string) (executorDefinition, bool) {
	for _, definition := range executorDefinitions {
		if definition.id == id {
			return definition, true
		}
	}
	return executorDefinition{}, false
}

// executorForCommand 复用 pkg/command 的 basename 语义推导 Profile 对应的
// AEP 执行器；basename 不是已知执行器名时返回 false。
func executorForCommand(command string) (string, bool) {
	index := strings.LastIndex(command, string(filepath.Separator))
	rawBase := command[index+1:]
	base := filepath.Base(command)
	if rawBase == "" || rawBase != base {
		return "", false
	}
	for _, definition := range executorDefinitions {
		if definition.basename != "" && definition.basename == base {
			return definition.id, true
		}
	}
	return "", false
}

// probeCommand 复用执行面的 Profile 环境、cwd 和 PATH 解析规则。
// 参数 command、invocationBase 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func probeCommand(command runtimecommand.Profile, invocationBase string) error {
	_, err := runtimecommand.ResolveExecutable(command, invocationBase, os.Environ())
	return err
}

// executorDescriptor 组装单个执行器描述符。available 只在 adapter 已登记、
// 至少一个 CLI Profile 指向该执行器、且其 command 本地可解析时为 true；
// 不可用时在 metadata.reason 说明。
// 参数 definition、profiles、invocationBase 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 ExecutorDescriptor 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func executorDescriptor(
	definition executorDefinition,
	profiles *profile.Catalog,
	invocationBase string,
) ExecutorDescriptor {
	descriptor := ExecutorDescriptor{
		ID: definition.id, Name: definition.name,
		Available: false, Capabilities: capabilitiesFor(definition),
	}
	switch {
	case !definition.registered && definition.id == "acp":
		descriptor.Metadata = map[string]string{
			"reason": "acp compatibility adapter is deferred",
		}
	case !definition.registered:
		descriptor.Metadata = map[string]string{
			"reason": "executor command adapter is not registered yet",
		}
	default:
		usable := false
		probed := false
		for _, entry := range profiles.Entries() {
			if entry.Kind != profile.KindCommand || entry.Command == nil {
				continue
			}
			executorID, ok := executorForCommand(entry.Command.Command)
			if !ok || executorID != definition.id {
				continue
			}
			probed = true
			if probeCommand(*entry.Command, invocationBase) == nil {
				usable = true
				break
			}
		}
		if usable {
			descriptor.Available = true
			return descriptor
		}
		if probed {
			descriptor.Metadata = map[string]string{
				"reason": "EXECUTOR_NOT_INSTALLED",
			}
		} else {
			descriptor.Metadata = map[string]string{
				"reason": "no CLI Profile is configured for this executor",
			}
		}
	}
	return descriptor
}

// executorList 执行 executorList 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 profiles、invocationBase 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []ExecutorDescriptor 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func executorList(profiles *profile.Catalog, invocationBase string) []ExecutorDescriptor {
	values := make([]ExecutorDescriptor, 0, len(executorDefinitions))
	for _, definition := range executorDefinitions {
		values = append(values, executorDescriptor(definition, profiles, invocationBase))
	}
	return values
}
