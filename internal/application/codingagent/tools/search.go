package tools

import (
	"bufio"
	"context"
	"fmt"
	"os"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/yy003x/sn-code/pkg/agent"
)

// ignoreRules 是根目录 .gitignore 的简化解析结果，仅支持常见模式：
// 空行/注释、! 取反、尾部 / 目录限定、前导 / 锚定、* 单段通配、** 跨段。
type ignoreRules struct {
	base     string
	patterns []ignorePattern
}

type ignorePattern struct {
	negative bool
	dirOnly  bool
	anchored bool
	segments []string
}

// loadIgnoreRules 执行 loadIgnoreRules 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 root 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *ignoreRules 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func loadIgnoreRules(root string) *ignoreRules {
	raw, err := os.ReadFile(filepath.Join(root, ".gitignore"))
	if err != nil {
		return &ignoreRules{base: root}
	}
	rules := &ignoreRules{base: root}
	for line := range strings.SplitSeq(string(raw), "\n") {
		line = strings.TrimRight(line, "\r")
		trimmed := strings.TrimSpace(line)
		if trimmed == "" || strings.HasPrefix(trimmed, "#") {
			continue
		}
		pattern := ignorePattern{}
		if strings.HasPrefix(trimmed, "!") {
			pattern.negative = true
			trimmed = trimmed[1:]
		}
		if strings.HasSuffix(trimmed, "/") {
			pattern.dirOnly = true
			trimmed = strings.TrimSuffix(trimmed, "/")
		}
		if strings.HasPrefix(trimmed, "/") {
			pattern.anchored = true
			trimmed = trimmed[1:]
		} else if !strings.Contains(trimmed, "/") {
			// 无斜杠的模式匹配任意层级的文件名或目录名。
			pattern.segments = []string{"**", trimmed}
			rules.patterns = append(rules.patterns, pattern)
			continue
		}
		pattern.segments = strings.Split(trimmed, "/")
		rules.patterns = append(rules.patterns, pattern)
	}
	return rules
}

// matches 判断相对 root 的 rel 路径（/ 分隔）是否被忽略。
// isDir 用于目录限定模式的判断。
func (rules *ignoreRules) matches(rel string, isDir bool) bool {
	if rules == nil {
		return false
	}
	rel = filepath.ToSlash(rel)
	segments := strings.Split(rel, "/")
	ignored := false
	for _, pattern := range rules.patterns {
		if pattern.dirOnly && !isDir {
			continue
		}
		if matchSegments(pattern.segments, segments, pattern.anchored) {
			if pattern.negative {
				ignored = false
			} else {
				ignored = true
			}
		}
	}
	return ignored
}

// matchSegments 执行 matchSegments 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 pattern、path、anchored 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func matchSegments(pattern, path []string, anchored bool) bool {
	return matchSegmentsFrom(pattern, 0, path, 0, anchored)
}

// matchSegmentsFrom 执行 matchSegmentsFrom 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 pattern、patternIndex、path、pathIndex、anchored 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func matchSegmentsFrom(pattern []string, patternIndex int, path []string, pathIndex int, anchored bool) bool {
	if patternIndex == len(pattern) {
		return pathIndex == len(path)
	}
	segment := pattern[patternIndex]
	switch segment {
	case "**":
		// ** 可匹配零或多段。
		for skip := pathIndex; skip <= len(path); skip++ {
			if matchSegmentsFrom(pattern, patternIndex+1, path, skip, anchored) {
				return true
			}
		}
		return false
	case "*":
		if pathIndex == len(path) {
			return false
		}
		return matchSegmentsFrom(pattern, patternIndex+1, path, pathIndex+1, anchored)
	default:
		if pathIndex == len(path) {
			return false
		}
		if !matchSegment(segment, path[pathIndex]) {
			return false
		}
		return matchSegmentsFrom(pattern, patternIndex+1, path, pathIndex+1, anchored)
	}
}

// matchSegment 匹配单个路径段：* 与 ? 不跨段。
// 参数 pattern、value 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func matchSegment(pattern, value string) bool {
	wild := strings.ReplaceAll("^"+regexp.QuoteMeta(pattern)+"$", regexp.QuoteMeta("*"), "[^/]*")
	wild = strings.ReplaceAll(wild, regexp.QuoteMeta("?"), "[^/]")
	final, err := regexp.Compile(wild)
	if err != nil {
		return false
	}
	return final.MatchString(value)
}

// matchGlob 对外暴露的 glob 匹配：pattern 不含 / 时按文件名匹配任意层级，
// 否则对相对路径整体匹配。
// 参数 pattern、rel 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func matchGlob(pattern, rel string) bool {
	if pattern == "" {
		return true
	}
	rel = filepath.ToSlash(rel)
	if !strings.Contains(pattern, "/") {
		return matchSegment(pattern, filepath.Base(rel))
	}
	return matchSegments(strings.Split(pattern, "/"), strings.Split(rel, "/"), true)
}

// walkFiles 遍历 root 下所有常规文件，应用忽略规则，按字典序输出相对路径。
// 参数 root、includeHidden 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []string、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func walkFiles(root string, includeHidden bool) ([]string, error) {
	rules := loadIgnoreRules(root)
	var files []string
	err := filepath.WalkDir(root, func(path string, entry os.DirEntry, err error) error {
		if err != nil {
			return nil // 单个条目失败不中断搜索
		}
		rel, relErr := filepath.Rel(root, path)
		if relErr != nil || rel == "." {
			return nil
		}
		name := entry.Name()
		if !includeHidden && strings.HasPrefix(name, ".") && name != "." {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if name == ".git" && entry.IsDir() {
			return filepath.SkipDir
		}
		if rules.matches(rel, entry.IsDir()) {
			if entry.IsDir() {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.IsDir() {
			files = append(files, rel)
		}
		return nil
	})
	if err != nil {
		return nil, err
	}
	return files, nil
}

type grepArguments struct {
	Pattern    string `json:"pattern"`
	Path       string `json:"path"`
	Glob       string `json:"glob"`
	IgnoreCase bool   `json:"ignoreCase"`
	Literal    bool   `json:"literal"`
	Context    int    `json:"context"`
	Limit      int    `json:"limit"`
}

// handleGrep 执行 handleGrep 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 request 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 agent.ToolResult、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func handleGrep(ctx context.Context, request agent.ToolRequest) (agent.ToolResult, error) {
	arguments, err := decodeArguments[grepArguments](request)
	if err != nil {
		return agent.ToolResult{}, err
	}
	expression := arguments.Pattern
	if arguments.Literal {
		expression = regexp.QuoteMeta(expression)
	}
	if arguments.IgnoreCase {
		expression = "(?i)" + expression
	}
	matcher, err := regexp.Compile(expression)
	if err != nil {
		return errorResult("grep: invalid pattern: %v", err)
	}
	environment := EnvironmentFrom(ctx)
	target := arguments.Path
	if target == "" {
		target = "."
	}
	root, err := resolvePath(environment, target)
	if err != nil {
		return errorResult("%s", err)
	}
	info, err := os.Stat(root)
	if err != nil {
		return errorResult("grep %s: %v", target, err)
	}
	limit := arguments.Limit
	if limit < 1 {
		limit = 100
	}
	var files []string
	if info.IsDir() {
		files, err = walkFiles(root, false)
		if err != nil {
			return errorResult("grep %s: %v", target, err)
		}
	} else {
		files = []string{filepath.Base(root)}
	}
	var output []string
	matches := 0
	for _, file := range files {
		if matches >= limit {
			output = append(output, fmt.Sprintf("[Truncated at %d matches]", limit))
			break
		}
		fullPath := root
		display := arguments.Path
		if info.IsDir() {
			fullPath = filepath.Join(root, file)
			display = filepath.Join(arguments.Path, file)
		}
		if arguments.Glob != "" && !matchGlob(arguments.Glob, file) {
			continue
		}
		lines, err := readLinesBounded(fullPath)
		if err != nil {
			continue // 二进制/不可读文件直接跳过
		}
		for index, line := range lines {
			if matches >= limit {
				break
			}
			if !matcher.MatchString(line) {
				continue
			}
			matches++
			if len(line) > maxLineLength {
				line = line[:maxLineLength] + "…"
			}
			output = append(output, fmt.Sprintf("%s:%d:%s", display, index+1, line))
			// 上下文行取匹配行前后 context 行，去重交给连续输出的阅读者。
			for offset := 1; offset <= arguments.Context; offset++ {
				before := index - offset
				after := index + offset
				if before >= 0 {
					output = append(output, fmt.Sprintf("%s-%d:%s", display, before+1, truncateRunesTo(lines[before], maxLineLength)))
				}
				if after < len(lines) {
					output = append(output, fmt.Sprintf("%s-%d:%s", display, after+1, truncateRunesTo(lines[after], maxLineLength)))
				}
			}
		}
	}
	if matches == 0 {
		return agent.ToolResult{Content: "No matches found"}, nil
	}
	text := strings.Join(output, "\n")
	if len(text) > maxOutputBytes {
		text = text[:maxOutputBytes] + "\n[truncated]"
	}
	return agent.ToolResult{Content: text}, nil
}

// readLinesBounded 读取文本文件行列表；遇到 NUL 字节按二进制放弃。
// 参数 path 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 []string、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func readLinesBounded(path string) ([]string, error) {
	file, err := os.Open(path)
	if err != nil {
		return nil, err
	}
	defer file.Close()
	var lines []string
	scanner := bufio.NewScanner(file)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	for scanner.Scan() {
		chunk := scanner.Bytes()
		if strings.ContainsRune(string(chunk), 0) {
			return nil, fmt.Errorf("binary file")
		}
		lines = append(lines, strings.TrimSuffix(string(chunk), "\r"))
	}
	if err := scanner.Err(); err != nil {
		return nil, err
	}
	if len(lines) > 200000 {
		lines = lines[:200000]
	}
	return lines, nil
}

// truncateRunesTo 执行 truncateRunesTo 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 value、limit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func truncateRunesTo(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	return value[:limit] + "…"
}

type findArguments struct {
	Pattern string `json:"pattern"`
	Path    string `json:"path"`
	Limit   int    `json:"limit"`
}

// handleFind 执行 handleFind 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 request 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 agent.ToolResult、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func handleFind(ctx context.Context, request agent.ToolRequest) (agent.ToolResult, error) {
	arguments, err := decodeArguments[findArguments](request)
	if err != nil {
		return agent.ToolResult{}, err
	}
	environment := EnvironmentFrom(ctx)
	target := arguments.Path
	if target == "" {
		target = "."
	}
	root, err := resolvePath(environment, target)
	if err != nil {
		return errorResult("%s", err)
	}
	info, err := os.Stat(root)
	if err != nil || !info.IsDir() {
		return errorResult("find %s: directory not found", target)
	}
	files, err := walkFiles(root, false)
	if err != nil {
		return errorResult("find %s: %v", target, err)
	}
	limit := arguments.Limit
	if limit < 1 {
		limit = 1000
	}
	var output []string
	for _, file := range files {
		if len(output) == limit {
			output = append(output, fmt.Sprintf("[Truncated at %d results]", limit))
			break
		}
		if matchGlob(arguments.Pattern, file) {
			output = append(output, filepath.ToSlash(file))
		}
	}
	if len(output) == 0 {
		return agent.ToolResult{Content: "No files found"}, nil
	}
	text := strings.Join(output, "\n")
	if len(text) > maxOutputBytes {
		text = text[:maxOutputBytes] + "\n[truncated]"
	}
	return agent.ToolResult{Content: text}, nil
}
