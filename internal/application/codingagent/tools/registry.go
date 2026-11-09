// Package tools 实现 sn-code 的内置编码工具集。参数 schema 与行为对齐
// pi 的 createCodingToolDefinitions（read/write/edit/bash/grep/find/ls），
// 工具统一注册进公开的 agent.Registry，复用其 schema 编译与调用前校验。
package tools

import (
	"context"
	"encoding/json"
	"fmt"

	"github.com/yy003x/sn-code/pkg/agent"
	"github.com/yy003x/sn-code/pkg/contract"
)

// Environment 是工具执行所需的运行上下文，由应用层放入 context。
// CWD 同时是相对路径的解析基准和 bash 的工作目录。
type Environment struct {
	CWD       string
	SessionID string
	Profile   string
	Model     string
}

type environmentKey struct{}

// WithEnvironment 把执行环境绑定到 context。
// 参数 environment 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 context.Context 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func WithEnvironment(ctx context.Context, environment Environment) context.Context {
	return context.WithValue(ctx, environmentKey{}, environment)
}

// EnvironmentFrom 取出执行环境；未绑定时返回零值（相对路径按进程 CWD 解析）。
func EnvironmentFrom(ctx context.Context) Environment {
	if environment, ok := ctx.Value(environmentKey{}).(Environment); ok {
		return environment
	}
	return Environment{}
}

// PromptSnippet 是系统提示词 tools 段使用的一行工具摘要。
type PromptSnippet struct {
	Name      string
	Snippet   string
	Guideline string
}

// PromptSnippets 返回与注册顺序一致的工具提示片段。
func PromptSnippets() []PromptSnippet {
	return []PromptSnippet{
		{Name: "read", Snippet: "Read file contents with line-based pagination.", Guideline: "Prefer read with offset/limit over bash cat for inspecting files."},
		{Name: "bash", Snippet: "Execute a shell command in the current working directory.", Guideline: "Use bash for running builds, tests, and git; avoid it for file inspection when read/grep/ls work."},
		{Name: "edit", Snippet: "Apply exact text replacements to an existing file.", Guideline: "edit requires each oldText to match exactly once in the original file; read the file first."},
		{Name: "write", Snippet: "Create or overwrite a file with full content.", Guideline: "Use write for new files; prefer edit for changing existing files."},
		{Name: "grep", Snippet: "Search file contents with a regular expression.", Guideline: "Use grep instead of bash grep/rg so ignore rules apply."},
		{Name: "find", Snippet: "Find files by glob pattern.", Guideline: "Use find instead of bash find/ls -R."},
		{Name: "ls", Snippet: "List directory entries.", Guideline: "Use ls for a quick directory overview including hidden entries."},
	}
}

// ToolPolicy 声明一个工具的副作用分级与风险。agent.Registry 的公开
// Definitions 不携带 effect/risk（它们是运行期元数据，不发给 Provider），
// 确认门从这里取策略。
type ToolPolicy struct {
	Effect contract.Effect
	Risk   contract.Risk
}

// Policy 返回工具名到确认门策略的映射，与 Registry() 的注册集一致。
func Policy() map[string]ToolPolicy {
	return map[string]ToolPolicy{
		"read":  {Effect: contract.EffectReadOnly, Risk: contract.RiskLow},
		"write": {Effect: contract.EffectWriteLocal, Risk: contract.RiskLow},
		"edit":  {Effect: contract.EffectWriteLocal, Risk: contract.RiskLow},
		"bash":  {Effect: contract.EffectWriteExternal, Risk: contract.RiskHigh},
		"grep":  {Effect: contract.EffectReadOnly, Risk: contract.RiskLow},
		"find":  {Effect: contract.EffectReadOnly, Risk: contract.RiskLow},
		"ls":    {Effect: contract.EffectReadOnly, Risk: contract.RiskLow},
	}
}

// Registry 构建 sn-code 工具注册表。workspace 是相对路径的默认基准。
// 返回 *agent.Registry、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func Registry() (*agent.Registry, error) {
	values := []agent.RegisteredTool{
		{
			Definition: contract.ToolSpec{
				Name:        "read",
				Description: "Read a text file. Line numbers are 1-indexed; offset and limit paginate the file. Output is truncated to 2000 lines or 50KB with a note when longer.",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"path": {"type": "string", "description": "File path, relative to the working directory or absolute."},
						"offset": {"type": "integer", "minimum": 1, "description": "Starting line number, 1-indexed."},
						"limit": {"type": "integer", "minimum": 1, "description": "Maximum number of lines to read."}
					},
					"required": ["path"],
					"additionalProperties": false
				}`),
			},
			Handler: handleRead,
			Effect:  contract.EffectReadOnly,
			Risk:    contract.RiskLow,
		},
		{
			Definition: contract.ToolSpec{
				Name:        "write",
				Description: "Create or overwrite a file with the given content. Parent directories are created automatically.",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"path": {"type": "string", "description": "File path, relative to the working directory or absolute."},
						"content": {"type": "string", "description": "Full file content to write."}
					},
					"required": ["path", "content"],
					"additionalProperties": false
				}`),
			},
			Handler: handleWrite,
			Effect:  contract.EffectWriteLocal,
			Risk:    contract.RiskLow,
		},
		{
			Definition: contract.ToolSpec{
				Name:        "edit",
				Description: "Apply exact text replacements to an existing file. Each oldText must match exactly once in the original file content; matches are computed against the original, not incrementally.",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"path": {"type": "string", "description": "File path, relative to the working directory or absolute."},
						"edits": {
							"type": "array",
							"minItems": 1,
							"items": {
								"type": "object",
								"properties": {
									"oldText": {"type": "string", "description": "Exact text to replace."},
									"newText": {"type": "string", "description": "Replacement text."}
								},
								"required": ["oldText", "newText"],
								"additionalProperties": false
							}
						}
					},
					"required": ["path", "edits"],
					"additionalProperties": false
				}`),
			},
			Handler: handleEdit,
			Effect:  contract.EffectWriteLocal,
			Risk:    contract.RiskLow,
		},
		{
			Definition: contract.ToolSpec{
				Name:        "bash",
				Description: "Run a shell command in the working directory. stdout and stderr are merged; output is tail-truncated to 2000 lines or 50KB with the full output written to a temporary file. timeout is seconds.",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"command": {"type": "string", "description": "Shell command to execute."},
						"timeout": {"type": "number", "minimum": 1, "maximum": 2147483, "description": "Timeout in seconds."}
					},
					"required": ["command"],
					"additionalProperties": false
				}`),
			},
			Handler: handleBash,
			Effect:  contract.EffectWriteExternal,
			Risk:    contract.RiskHigh,
		},
		{
			Definition: contract.ToolSpec{
				Name:        "grep",
				Description: "Search file contents with a regular expression, honoring the repository's ignore rules. Returns matching lines as path:line:text.",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"pattern": {"type": "string", "description": "Regular expression, or literal text when literal is true."},
						"path": {"type": "string", "description": "File or directory to search; defaults to the working directory."},
						"glob": {"type": "string", "description": "Glob filter such as *.go or src/**/*.ts."},
						"ignoreCase": {"type": "boolean"},
						"literal": {"type": "boolean"},
						"context": {"type": "integer", "minimum": 0, "description": "Context lines shown around each match."},
						"limit": {"type": "integer", "minimum": 1, "description": "Maximum number of matches, default 100."}
					},
					"required": ["pattern"],
					"additionalProperties": false
				}`),
			},
			Handler: handleGrep,
			Effect:  contract.EffectReadOnly,
			Risk:    contract.RiskLow,
		},
		{
			Definition: contract.ToolSpec{
				Name:        "find",
				Description: "Find files by glob pattern, honoring ignore rules. Returns paths relative to the search directory.",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"pattern": {"type": "string", "description": "Glob pattern such as *.go or **/*.json."},
						"path": {"type": "string", "description": "Directory to search; defaults to the working directory."},
						"limit": {"type": "integer", "minimum": 1, "description": "Maximum number of results, default 1000."}
					},
					"required": ["pattern"],
					"additionalProperties": false
				}`),
			},
			Handler: handleFind,
			Effect:  contract.EffectReadOnly,
			Risk:    contract.RiskLow,
		},
		{
			Definition: contract.ToolSpec{
				Name:        "ls",
				Description: "List directory entries including hidden ones, sorted case-insensitively. Directories get a trailing /.",
				InputSchema: json.RawMessage(`{
					"type": "object",
					"properties": {
						"path": {"type": "string", "description": "Directory to list; defaults to the working directory."},
						"limit": {"type": "integer", "minimum": 1, "description": "Maximum number of entries, default 500."}
					},
					"additionalProperties": false
				}`),
			},
			Handler: handleLs,
			Effect:  contract.EffectReadOnly,
			Risk:    contract.RiskLow,
		},
	}
	registry, err := agent.NewRegistryWithToolExecution(agent.ToolExecutionIdentity{
		Implementation:        "sn-code.coding-tools",
		ImplementationVersion: 1,
	}, values...)
	if err != nil {
		return nil, fmt.Errorf("build sn-code tool registry: %w", err)
	}
	return registry, nil
}
