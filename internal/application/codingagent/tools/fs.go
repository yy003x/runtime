package tools

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yy003x/sn-code/pkg/agent"
)

// 输出上限对齐 pi：行数 2000、字节 50KB、单行 500 字符。
const (
	// maxOutputLines 定义当前流程使用的固定值（数量单位以名称所示对象为准），取值来源为本实现约定。
	maxOutputLines = 2000
	// maxOutputBytes 定义当前流程使用的固定值（容量单位以声明或使用位置为准），取值来源为本实现约定。
	maxOutputBytes = 50 * 1024
	// maxLineLength 定义当前流程使用的固定值（数量单位以名称所示对象为准），取值来源为本实现约定。
	maxLineLength = 500
)

// resolvePath 执行 resolvePath 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 environment、path 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func resolvePath(environment Environment, path string) (string, error) {
	if path == "" {
		return "", errors.New("path is required")
	}
	if filepath.IsAbs(path) {
		return filepath.Clean(path), nil
	}
	base := environment.CWD
	if base == "" {
		wd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		base = wd
	}
	return filepath.Join(base, path), nil
}

// decodeArguments 执行 decodeArguments 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 request 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 T、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func decodeArguments[T any](request agent.ToolRequest) (T, error) {
	var arguments T
	if err := json.Unmarshal(request.Arguments, &arguments); err != nil {
		return arguments, fmt.Errorf("decode %s arguments: %w", request.Name, err)
	}
	return arguments, nil
}

// errorResult 执行 errorResult 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 format、values 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 agent.ToolResult、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func errorResult(format string, values ...any) (agent.ToolResult, error) {
	return agent.ToolResult{Content: fmt.Sprintf(format, values...), IsError: true}, nil
}

type readArguments struct {
	Path   string `json:"path"`
	Offset int    `json:"offset"`
	Limit  int    `json:"limit"`
}

// handleRead 执行 handleRead 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 request 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 agent.ToolResult、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func handleRead(ctx context.Context, request agent.ToolRequest) (agent.ToolResult, error) {
	arguments, err := decodeArguments[readArguments](request)
	if err != nil {
		return agent.ToolResult{}, err
	}
	path, err := resolvePath(EnvironmentFrom(ctx), arguments.Path)
	if err != nil {
		return errorResult("%s", err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return errorResult("read %s: %v", arguments.Path, err)
	}
	if info.IsDir() {
		return errorResult("read %s: is a directory", arguments.Path)
	}
	if isImageFile(path) {
		// 图片进入上下文需要 vision content parts，v1 以提示文本代替。
		return agent.ToolResult{
			Content: fmt.Sprintf("%s is an image; image input is not supported yet", arguments.Path),
		}, nil
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return errorResult("read %s: %v", arguments.Path, err)
	}
	content := strings.TrimPrefix(string(raw), "\uFEFF")
	lines := strings.Split(content, "\n")
	// 末尾换行产生的空行不计为文件行。
	if len(lines) > 0 && lines[len(lines)-1] == "" {
		lines = lines[:len(lines)-1]
	}
	total := len(lines)
	offset := arguments.Offset
	if offset < 1 {
		offset = 1
	}
	if offset > total {
		return errorResult("read %s: offset %d is beyond the end of the file (%d lines)", arguments.Path, offset, total)
	}
	limit := arguments.Limit
	if limit < 1 || limit > maxOutputLines {
		limit = maxOutputLines
	}
	selected := lines[offset-1:]
	if len(selected) > limit {
		selected = selected[:limit]
	}
	var builder strings.Builder
	for index, line := range selected {
		if index > 0 {
			builder.WriteByte('\n')
		}
		if len(line) > maxLineLength {
			builder.WriteString(line[:maxLineLength])
			builder.WriteString("…")
		} else {
			builder.WriteString(line)
		}
	}
	text := builder.String()
	shown := len(selected)
	note := ""
	if shown == limit && offset+shown-1 < total {
		note = fmt.Sprintf("\n\n[Showing lines %d-%d of %d. Use offset=%d to continue.]", offset, offset+shown-1, total, offset+shown)
	}
	if len(text) > maxOutputBytes {
		text = text[:maxOutputBytes]
		note = fmt.Sprintf("\n\n[Output truncated at %d bytes. Use offset/limit to read in chunks.]", maxOutputBytes)
	}
	return agent.ToolResult{Content: text + note}, nil
}

// isImageFile 执行 isImageFile 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 path 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func isImageFile(path string) bool {
	switch strings.ToLower(filepath.Ext(path)) {
	case ".jpg", ".jpeg", ".png", ".gif", ".webp", ".bmp":
		return true
	}
	return false
}

type writeArguments struct {
	Path    string `json:"path"`
	Content string `json:"content"`
}

// handleWrite 执行 handleWrite 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 request 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 agent.ToolResult、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func handleWrite(ctx context.Context, request agent.ToolRequest) (agent.ToolResult, error) {
	arguments, err := decodeArguments[writeArguments](request)
	if err != nil {
		return agent.ToolResult{}, err
	}
	path, err := resolvePath(EnvironmentFrom(ctx), arguments.Path)
	if err != nil {
		return errorResult("%s", err)
	}
	if err := os.MkdirAll(filepath.Dir(path), 0o755); err != nil {
		return errorResult("write %s: %v", arguments.Path, err)
	}
	mode := os.FileMode(0o644)
	if info, statErr := os.Stat(path); statErr == nil {
		mode = info.Mode().Perm()
	} else if !os.IsNotExist(statErr) {
		return errorResult("write %s: %v", arguments.Path, statErr)
	}
	// 原子写：临时文件 + rename，避免半写状态。
	temporary, err := os.CreateTemp(filepath.Dir(path), ".sn-code-write-*")
	if err != nil {
		return errorResult("write %s: %v", arguments.Path, err)
	}
	defer os.Remove(temporary.Name())
	if err := temporary.Chmod(mode); err != nil {
		temporary.Close()
		return errorResult("write %s: %v", arguments.Path, err)
	}
	if _, err := temporary.WriteString(arguments.Content); err != nil {
		temporary.Close()
		return errorResult("write %s: %v", arguments.Path, err)
	}
	if err := temporary.Close(); err != nil {
		return errorResult("write %s: %v", arguments.Path, err)
	}
	if err := os.Rename(temporary.Name(), path); err != nil {
		return errorResult("write %s: %v", arguments.Path, err)
	}
	return agent.ToolResult{Content: fmt.Sprintf("Successfully wrote to %s", arguments.Path)}, nil
}

type editPair struct {
	OldText string `json:"oldText"`
	NewText string `json:"newText"`
}

type editArguments struct {
	Path  string     `json:"path"`
	Edits []editPair `json:"edits"`
}

// handleEdit 执行 handleEdit 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 request 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 agent.ToolResult、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func handleEdit(ctx context.Context, request agent.ToolRequest) (agent.ToolResult, error) {
	arguments, err := decodeArguments[editArguments](request)
	if err != nil {
		return agent.ToolResult{}, err
	}
	path, err := resolvePath(EnvironmentFrom(ctx), arguments.Path)
	if err != nil {
		return errorResult("%s", err)
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return errorResult("edit %s: %v", arguments.Path, err)
	}
	// 对齐 pi：剥 BOM、统一 LF 匹配；若原文以 CRLF 为主，写回时统一 CRLF。
	bom := ""
	content := string(raw)
	if strings.HasPrefix(content, "\uFEFF") {
		bom = "\uFEFF"
		content = content[3:]
	}
	newline := "\n"
	if strings.Count(content, "\r\n")*2 > strings.Count(content, "\n") {
		newline = "\r\n"
	}
	normalized := strings.ReplaceAll(content, "\r\n", "\n")

	// 每个 oldText 必须在原始内容中恰好出现一次，且替换区间互不重叠。
	type replacement struct {
		start int
		end   int
		text  string
	}
	replacements := make([]replacement, 0, len(arguments.Edits))
	occupied := make([][2]int, 0, len(arguments.Edits))
	for _, pair := range arguments.Edits {
		if pair.OldText == "" {
			return errorResult("edit %s: oldText must not be empty", arguments.Path)
		}
		first := strings.Index(normalized, pair.OldText)
		if first < 0 {
			return errorResult("edit %s: oldText not found:\n%s", arguments.Path, truncateForError(pair.OldText))
		}
		if strings.Index(normalized[first+1:], pair.OldText) >= 0 {
			return errorResult("edit %s: oldText matches more than once:\n%s", arguments.Path, truncateForError(pair.OldText))
		}
		end := first + len(pair.OldText)
		for _, span := range occupied {
			if first < span[1] && span[0] < end {
				return errorResult("edit %s: edits overlap", arguments.Path)
			}
		}
		occupied = append(occupied, [2]int{first, end})
		replacements = append(replacements, replacement{start: first, end: end, text: pair.NewText})
	}
	sort.Slice(replacements, func(left, right int) bool {
		return replacements[left].start < replacements[right].start
	})
	var builder strings.Builder
	cursor := 0
	for _, item := range replacements {
		builder.WriteString(normalized[cursor:item.start])
		builder.WriteString(item.text)
		cursor = item.end
	}
	builder.WriteString(normalized[cursor:])
	updated := builder.String()
	if newline == "\r\n" {
		updated = strings.ReplaceAll(updated, "\n", "\r\n")
	}
	if err := os.WriteFile(path, []byte(bom+updated), 0o644); err != nil {
		return errorResult("edit %s: %v", arguments.Path, err)
	}
	return agent.ToolResult{
		Content: fmt.Sprintf("Successfully replaced %d block(s) in %s", len(replacements), arguments.Path),
	}, nil
}

// truncateForError 执行 truncateForError 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 text 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func truncateForError(text string) string {
	runes := []rune(text)
	if len(runes) > 200 {
		return string(runes[:200]) + "…"
	}
	return text
}

type lsArguments struct {
	Path  string `json:"path"`
	Limit int    `json:"limit"`
}

// handleLs 执行 handleLs 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 request 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 agent.ToolResult、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func handleLs(ctx context.Context, request agent.ToolRequest) (agent.ToolResult, error) {
	arguments, err := decodeArguments[lsArguments](request)
	if err != nil {
		return agent.ToolResult{}, err
	}
	target := arguments.Path
	if target == "" {
		target = "."
	}
	path, err := resolvePath(EnvironmentFrom(ctx), target)
	if err != nil {
		return errorResult("%s", err)
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return errorResult("ls %s: %v", target, err)
	}
	sort.Slice(entries, func(left, right int) bool {
		return strings.ToLower(entries[left].Name()) < strings.ToLower(entries[right].Name())
	})
	limit := arguments.Limit
	if limit < 1 || limit > maxOutputLines {
		limit = 500
	}
	lines := make([]string, 0, len(entries))
	for _, entry := range entries {
		if len(lines) == limit {
			lines = append(lines, fmt.Sprintf("[Truncated at %d entries]", limit))
			break
		}
		if entry.IsDir() {
			lines = append(lines, entry.Name()+"/")
		} else {
			lines = append(lines, entry.Name())
		}
	}
	if len(lines) == 0 {
		return agent.ToolResult{Content: "(empty directory)"}, nil
	}
	output := strings.Join(lines, "\n")
	if len(output) > maxOutputBytes {
		output = output[:maxOutputBytes] + "\n[truncated]"
	}
	return agent.ToolResult{Content: output}, nil
}
