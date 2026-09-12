package toolbuiltin

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io/fs"
	"path/filepath"
	"regexp"
	"strings"
	"unicode/utf8"

	"github.com/yy003x/runtime/pkg/agent"
)

const (
	maxGrepMatches = 200
	maxGrepFiles   = 256
)

func (resolver *resolver) grep(
	_ context.Context,
	request agent.ToolRequest,
) (agent.ToolResult, error) {
	var input struct {
		Pattern string `json:"pattern"`
		Path    string `json:"path"`
		Glob    string `json:"glob"`
		Regex   bool   `json:"regex"`
	}
	if err := decodeArguments(request.Arguments, &input); err != nil {
		return agent.ToolResult{}, err
	}
	if strings.TrimSpace(input.Pattern) == "" {
		return agent.ToolResult{}, fmt.Errorf("pattern is required")
	}
	searchPath := input.Path
	if strings.TrimSpace(searchPath) == "" {
		searchPath = "."
	}
	start, err := resolver.resolveWorkspacePath(searchPath)
	if err != nil {
		return readOnlyFilesystemFailure(err)
	}
	root := filepath.Join(append([]string{start.root.canonical}, start.components...)...)
	var matcher func(string) bool
	if input.Regex {
		compiled, compileErr := regexp.Compile(input.Pattern)
		if compileErr != nil {
			return readOnlyToolFailure("invalid_regex", compileErr.Error())
		}
		matcher = compiled.MatchString
	} else {
		pattern := input.Pattern
		matcher = func(line string) bool {
			return strings.Contains(line, pattern)
		}
	}
	type match struct {
		Path    string `json:"path"`
		Line    int    `json:"line"`
		Preview string `json:"preview"`
	}
	matches := make([]match, 0)
	files := 0
	truncated := false
	walkErr := filepath.WalkDir(root, func(path string, entry fs.DirEntry, walk error) error {
		if walk != nil {
			return walk
		}
		if entry.IsDir() {
			name := entry.Name()
			if name == ".git" || name == "node_modules" {
				return filepath.SkipDir
			}
			return nil
		}
		if !entry.Type().IsRegular() {
			return nil
		}
		if input.Glob != "" {
			ok, globErr := filepath.Match(input.Glob, entry.Name())
			if globErr != nil {
				return globErr
			}
			if !ok {
				return nil
			}
		}
		if files >= maxGrepFiles || truncated {
			truncated = true
			return fs.SkipAll
		}
		resolved, resolveErr := resolver.resolveWorkspacePath(path)
		if resolveErr != nil {
			return nil
		}
		content, readErr := resolver.readGrepFile(resolved)
		if readErr != nil {
			return nil
		}
		files++
		relative, relErr := filepath.Rel(resolver.cwd, path)
		if relErr != nil {
			relative = path
		}
		for index, line := range strings.Split(content, "\n") {
			if !matcher(line) {
				continue
			}
			preview := line
			if len(preview) > 240 {
				preview = preview[:240]
			}
			matches = append(matches, match{
				Path:    filepath.ToSlash(relative),
				Line:    index + 1,
				Preview: preview,
			})
			if len(matches) >= maxGrepMatches {
				truncated = true
				return fs.SkipAll
			}
		}
		return nil
	})
	if walkErr != nil {
		return readOnlyFilesystemFailure(walkErr)
	}
	payload, err := json.Marshal(struct {
		Matches   []match `json:"matches"`
		Truncated bool    `json:"truncated"`
	}{Matches: matches, Truncated: truncated})
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("encode grep matches: %w", err)
	}
	if len(payload) > maxToolOutputBytes {
		return readOnlyToolFailure("output_too_large", "grep output exceeds 1MiB")
	}
	return agent.ToolResult{Content: string(payload)}, nil
}

func (resolver *resolver) readGrepFile(path resolvedWorkspacePath) (string, error) {
	file, _, err := resolver.openReadFile(path)
	if err != nil {
		return "", err
	}
	data, err := boundedRead(file)
	closeErr := file.Close()
	if err != nil {
		return "", err
	}
	if closeErr != nil {
		return "", closeErr
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return "", fmt.Errorf("file is not valid UTF-8 text")
	}
	return string(data), nil
}
