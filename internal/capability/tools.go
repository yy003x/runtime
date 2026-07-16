package capability

import (
	"fmt"
	"math"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"gopkg.in/yaml.v3"
)

type Tool struct {
	Name        string         `json:"name" yaml:"name"`
	Description string         `json:"description" yaml:"description"`
	Schema      map[string]any `json:"parameters" yaml:"schema"`
	Kind        string         `json:"kind" yaml:"kind"`
	Capability  string         `json:"capability" yaml:"capability"`
	Dangerous   bool           `json:"dangerous" yaml:"dangerous"`
	Request     map[string]any `json:"-" yaml:"request"`
}

type ToolManager struct {
	tools  map[string]Tool
	errors []map[string]string
}

func NewToolManager() *ToolManager {
	manager := &ToolManager{tools: make(map[string]Tool)}
	manager.tools["echo"] = Tool{Name: "echo", Description: "返回传入参数,用于验证 tool 执行链路", Schema: map[string]any{"type": "object"}, Kind: "function"}
	manager.tools["require-capability"] = Tool{Name: "require-capability", Description: "需要 capability 的测试工具,用于验证 Guardrail", Schema: map[string]any{"type": "object"}, Kind: "function", Capability: "tool.execute"}
	manager.tools["run-agent"] = Tool{Name: "run-agent", Description: "声明一个外部 agent run", Schema: map[string]any{"type": "object"}, Kind: "external", Capability: "shell", Dangerous: true, Request: map[string]any{}}
	return manager
}

func (m *ToolManager) RegisterDir(path string) {
	info, err := os.Stat(path)
	if err != nil {
		m.errors = append(m.errors, map[string]string{"path": path, "error": "工具目录不存在"})
		return
	}
	var paths []string
	if !info.IsDir() {
		paths = []string{path}
	} else {
		first, _ := filepath.Glob(filepath.Join(path, "*.tool.yaml"))
		second, _ := filepath.Glob(filepath.Join(path, "*", "tool.yaml"))
		paths = append(first, second...)
		sort.Strings(paths)
	}
	for _, configPath := range paths {
		data, readErr := os.ReadFile(configPath)
		if readErr != nil {
			m.errors = append(m.errors, map[string]string{"path": configPath, "error": readErr.Error()})
			continue
		}
		var tool Tool
		if err := yaml.Unmarshal(data, &tool); err != nil || strings.TrimSpace(tool.Name) == "" {
			m.errors = append(m.errors, map[string]string{"path": configPath, "error": "tool.yaml 必须含 name"})
			continue
		}
		if tool.Kind == "" {
			tool.Kind = "external"
		}
		if tool.Kind != "external" {
			m.errors = append(m.errors, map[string]string{"path": configPath, "error": "tool.yaml 目前仅支持 kind: external"})
			continue
		}
		if tool.Dangerous && tool.Capability == "" {
			tool.Capability = "tool.execute"
		}
		m.tools[tool.Name] = tool
	}
}

func (m *ToolManager) Schemas() []Tool {
	names := make([]string, 0, len(m.tools))
	for name := range m.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]Tool, 0, len(names))
	for _, name := range names {
		out = append(out, m.tools[name])
	}
	return out
}

func (m *ToolManager) Doctor() map[string]any {
	return map[string]any{"ok": true, "loaded": len(m.tools), "errors": m.errors}
}

func (m *ToolManager) Call(name string, args map[string]any, capabilities, forbidden []string) (any, error) {
	tool, ok := m.tools[name]
	if !ok {
		return nil, fmt.Errorf("未注册工具: %s", name)
	}
	if err := validateSchema(tool.Schema, args, name+".args"); err != nil {
		return nil, err
	}
	if tool.Capability != "" {
		if contains(forbidden, tool.Capability) {
			return nil, fmt.Errorf("动作被 forbidden_actions 禁止: %s", tool.Capability)
		}
		if !contains(capabilities, tool.Capability) {
			return nil, fmt.Errorf("缺少 capability: %s", tool.Capability)
		}
	}
	if tool.Kind == "external" {
		return nil, fmt.Errorf("工具 %s 是 external，请使用 describe-external", name)
	}
	return args, nil
}

func (m *ToolManager) DescribeExternal(name string, args map[string]any, capabilities, forbidden []string) (any, error) {
	tool, ok := m.tools[name]
	if !ok {
		return nil, fmt.Errorf("未注册工具: %s", name)
	}
	if tool.Kind != "external" {
		return nil, fmt.Errorf("%s 不是 external 工具", name)
	}
	request := cloneMap(tool.Request)
	for key, value := range args {
		request[key] = value
	}
	if err := validateSchema(tool.Schema, request, name+".args"); err != nil {
		return nil, err
	}
	if tool.Capability != "" {
		if contains(forbidden, tool.Capability) {
			return nil, fmt.Errorf("动作被 forbidden_actions 禁止: %s", tool.Capability)
		}
		if !contains(capabilities, tool.Capability) {
			return nil, fmt.Errorf("缺少 capability: %s", tool.Capability)
		}
	}
	return map[string]any{"type": "run_agent", "tool": name, "capability": tool.Capability, "request": request}, nil
}

func validateSchema(schema map[string]any, value any, path string) error {
	if len(schema) == 0 {
		return nil
	}
	if expected, exists := schema["type"]; exists && !matchesSchemaType(expected, value) {
		return fmt.Errorf("%s: 类型不匹配,期望 %v", path, expected)
	}
	expected := fmt.Sprint(schema["type"])
	if expected == "object" || schema["properties"] != nil || schema["required"] != nil {
		object, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("%s: 必须是 object", path)
		}
		for _, key := range schemaStrings(schema["required"]) {
			if _, exists := object[fmt.Sprint(key)]; !exists {
				return fmt.Errorf("%s: 缺少必填参数 %s", path, key)
			}
		}
		properties, _ := schema["properties"].(map[string]any)
		for key, child := range properties {
			if childSchema, ok := child.(map[string]any); ok {
				if item, exists := object[key]; exists {
					if err := validateSchema(childSchema, item, path+"."+key); err != nil {
						return err
					}
				}
			}
		}
		if additional, ok := schema["additionalProperties"].(bool); ok && !additional {
			for key := range object {
				if _, allowed := properties[key]; !allowed {
					return fmt.Errorf("%s: 不允许额外参数 %s", path, key)
				}
			}
		}
	}
	if expected == "array" {
		items, _ := schema["items"].(map[string]any)
		array, _ := value.([]any)
		for index, item := range array {
			if err := validateSchema(items, item, fmt.Sprintf("%s[%d]", path, index)); err != nil {
				return err
			}
		}
	}
	return nil
}

func matchesSchemaType(expected any, value any) bool {
	if values, ok := expected.([]any); ok {
		for _, item := range values {
			if matchesSchemaType(item, value) {
				return true
			}
		}
		return false
	}
	switch fmt.Sprint(expected) {
	case "object":
		_, ok := value.(map[string]any)
		return ok
	case "array":
		_, ok := value.([]any)
		return ok
	case "string":
		_, ok := value.(string)
		return ok
	case "integer":
		switch typed := value.(type) {
		case int, int32, int64:
			return true
		case float64:
			return !math.IsNaN(typed) && !math.IsInf(typed, 0) && math.Trunc(typed) == typed
		case float32:
			return !float32IsSpecial(typed) && math.Trunc(float64(typed)) == float64(typed)
		}
		return false
	case "number":
		switch value.(type) {
		case int, int32, int64, float32, float64:
			return true
		}
		return false
	case "boolean":
		_, ok := value.(bool)
		return ok
	case "null":
		return value == nil
	default:
		return true
	}
}

func float32IsSpecial(value float32) bool {
	return math.IsNaN(float64(value)) || math.IsInf(float64(value), 0)
}

func schemaStrings(value any) []string {
	switch typed := value.(type) {
	case []string:
		return typed
	case []any:
		out := make([]string, 0, len(typed))
		for _, item := range typed {
			out = append(out, fmt.Sprint(item))
		}
		return out
	default:
		return nil
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
