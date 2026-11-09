package codingagent

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"github.com/yy003x/sn-code/internal/application/codingagent/tools"
)

// ContextFile 是一份已加载的项目上下文文件（AGENTS.md / CLAUDE.md 系）。
type ContextFile struct {
	Path    string
	Content string
}

// 每个目录依次探测的上下文文件名，顺序对齐 pi：override 优先于常规
// AGENTS，AGENTS 优先于 CLAUDE。
var contextFileCandidates = []string{
	"AGENTS.override.md", "AGENTS.md", "AGENTS.MD", "CLAUDE.md", "CLAUDE.MD",
}

// LoadContextFiles 加载项目上下文：全局 agentDir 下的上下文文件最先，
// 然后从 cwd 逐级向上到根，每个目录取第一个命中的候选文件，外层目录
// 在前。同一文件只保留一次。
// 参数 cwd、agentDir 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []ContextFile 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func LoadContextFiles(cwd, agentDir string) []ContextFile {
	var files []ContextFile
	seen := map[string]bool{}
	add := func(file ContextFile) {
		cleaned := filepath.Clean(file.Path)
		if seen[cleaned] {
			return
		}
		seen[cleaned] = true
		files = append(files, file)
	}
	if global := loadContextFileFromDir(agentDir); global != nil {
		add(*global)
	}
	// 自内向外收集再反转：全局 → 最外层祖先 → … → cwd 父目录 → cwd。
	var ancestors []ContextFile
	current := cwd
	for {
		if file := loadContextFileFromDir(current); file != nil {
			cleaned := filepath.Clean(file.Path)
			if !seen[cleaned] {
				seen[cleaned] = true
				ancestors = append(ancestors, *file)
			}
		}
		parent := filepath.Dir(current)
		if parent == current {
			break
		}
		current = parent
	}
	for index := len(ancestors) - 1; index >= 0; index-- {
		files = append(files, ancestors[index])
	}
	return files
}

// loadContextFileFromDir 执行 loadContextFileFromDir 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 dir 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *ContextFile 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func loadContextFileFromDir(dir string) *ContextFile {
	if dir == "" {
		return nil
	}
	for _, name := range contextFileCandidates {
		path := filepath.Join(dir, name)
		info, err := os.Stat(path)
		if err != nil || info.IsDir() {
			continue
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			// 不可读的候选文件跳过；上下文加载不阻塞会话。
			continue
		}
		return &ContextFile{Path: path, Content: strings.TrimPrefix(string(raw), "\uFEFF")}
	}
	return nil
}

// BuildSystemPrompt 按 pi 的 section 顺序组装系统提示词：preamble（无
// 标签）→ <tools> → <rules> → <project_context> → <cwd>。每个 section
// 用同名标签包裹，模型可以把后续更新对应到 section 上。
// 参数 cwd、snippets、contextFiles 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func BuildSystemPrompt(cwd string, snippets []tools.PromptSnippet, contextFiles []ContextFile) string {
	var sections []string

	sections = append(sections,
		"You are a coding assistant running inside sn-code, a coding agent harness. "+
			"Help users inspect files, run commands, and modify code.")

	if len(snippets) > 0 {
		lines := make([]string, 0, len(snippets))
		for _, snippet := range snippets {
			lines = append(lines, fmt.Sprintf("- %s: %s", snippet.Name, snippet.Snippet))
		}
		sections = append(sections, wrapSection("tools", strings.Join(lines, "\n")+
			"\n\nAdditional tools may be available."))
	}

	if rules := buildRules(snippets); rules != "" {
		sections = append(sections, wrapSection("rules", rules))
	}

	if len(contextFiles) > 0 {
		parts := make([]string, 0, len(contextFiles)+1)
		parts = append(parts, "Project-specific instructions and guidelines:")
		for _, file := range contextFiles {
			parts = append(parts, fmt.Sprintf(
				"<project_instructions path=%q>\n%s\n</project_instructions>",
				file.Path, file.Content,
			))
		}
		sections = append(sections, wrapSection("project_context", strings.Join(parts, "\n\n")))
	}

	sections = append(sections, wrapSection("cwd", strings.ReplaceAll(cwd, "\\", "/")))

	return strings.Join(sections, "\n\n") + "\n"
}

// wrapSection 执行 wrapSection 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 name、content 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func wrapSection(name, content string) string {
	return fmt.Sprintf("<%s>\n%s\n</%s>", name, content, name)
}

// buildRules 汇总工具使用准则并去重，追加通用行为规则。
// 参数 snippets 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func buildRules(snippets []tools.PromptSnippet) string {
	rules := make([]string, 0, len(snippets)+2)
	seen := map[string]bool{}
	add := func(rule string) {
		trimmed := strings.TrimSpace(rule)
		if trimmed == "" || seen[trimmed] {
			return
		}
		seen[trimmed] = true
		rules = append(rules, trimmed)
	}
	for _, snippet := range snippets {
		add(snippet.Guideline)
	}
	add("Be concise in your responses")
	add("Show file paths clearly when working with files")
	if len(rules) == 0 {
		return ""
	}
	lines := make([]string, 0, len(rules))
	for _, rule := range rules {
		lines = append(lines, "- "+rule)
	}
	return strings.Join(lines, "\n")
}
