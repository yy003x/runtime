package toolbuiltin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
)

const maxToolOutputBytes = 1 << 20

type Options struct {
	Names []string
	Roots []string
	CWD   string
}

func Build(options Options) (*agent.Registry, error) {
	if options.CWD == "" {
		current, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		options.CWD = current
	}
	if len(options.Roots) == 0 {
		options.Roots = []string{options.CWD}
	}
	resolver, err := newResolver(options.Roots, options.CWD)
	if err != nil {
		return nil, err
	}
	available := map[string]agent.RegisteredTool{
		"read_file": {
			Definition: contract.ToolSpec{
				Name:        "read_file",
				Description: "Read a UTF-8 text file within configured workspace roots.",
				InputSchema: json.RawMessage(
					`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`,
				),
			},
			Handler: resolver.readFile,
		},
		"list_directory": {
			Definition: contract.ToolSpec{
				Name:        "list_directory",
				Description: "List one directory within configured workspace roots.",
				InputSchema: json.RawMessage(
					`{"type":"object","properties":{"path":{"type":"string"}},"required":["path"],"additionalProperties":false}`,
				),
			},
			Handler: resolver.listDirectory,
		},
		"write_file": {
			Definition: contract.ToolSpec{
				Name:        "write_file",
				Description: "Atomically write a UTF-8 file within configured workspace roots.",
				InputSchema: json.RawMessage(
					`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`,
				),
			},
			Handler: resolver.writeFile,
		},
		"exec_command": {
			Definition: contract.ToolSpec{
				Name:        "exec_command",
				Description: "Execute an argv array without a shell within configured workspace roots.",
				InputSchema: json.RawMessage(
					`{"type":"object","properties":{"argv":{"type":"array","items":{"type":"string"},"minItems":1},"cwd":{"type":"string"},"timeout_seconds":{"type":"integer","minimum":1,"maximum":300}},"required":["argv"],"additionalProperties":false}`,
				),
			},
			Handler: resolver.execCommand,
		},
	}
	values := make([]agent.RegisteredTool, 0, len(options.Names))
	for _, name := range options.Names {
		value, exists := available[name]
		if !exists {
			return nil, fmt.Errorf("unknown built-in tool %q", name)
		}
		values = append(values, value)
	}
	return agent.NewRegistry(values...)
}

type resolver struct {
	roots []workspaceRoot
	cwd   string
}

type workspaceRoot struct {
	lexical   string
	canonical string
}

func newResolver(roots []string, cwd string) (*resolver, error) {
	values := make([]workspaceRoot, 0, len(roots))
	for _, root := range roots {
		absolute, err := filepath.Abs(root)
		if err != nil {
			return nil, err
		}
		canonical, err := filepath.EvalSymlinks(absolute)
		if err != nil {
			return nil, err
		}
		info, err := os.Stat(canonical)
		if err != nil {
			return nil, err
		}
		if !info.IsDir() {
			return nil, fmt.Errorf("workspace root must be a directory")
		}
		values = append(values, workspaceRoot{
			lexical: filepath.Clean(absolute), canonical: filepath.Clean(canonical),
		})
	}
	absoluteCWD, err := filepath.Abs(cwd)
	if err != nil {
		return nil, err
	}
	canonicalCWD, err := filepath.EvalSymlinks(absoluteCWD)
	if err != nil {
		return nil, err
	}
	return &resolver{
		roots: values, cwd: filepath.Clean(canonicalCWD),
	}, nil
}

func (resolver *resolver) readFile(
	_ context.Context,
	request agent.ToolRequest,
) (agent.ToolResult, error) {
	var input struct {
		Path string `json:"path"`
	}
	if err := decodeArguments(request.Arguments, &input); err != nil {
		return agent.ToolResult{}, err
	}
	path, err := resolver.resolveExisting(input.Path, false)
	if err != nil {
		return agent.ToolResult{}, err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if !info.Mode().IsRegular() || info.Size() > maxToolOutputBytes {
		return agent.ToolResult{}, fmt.Errorf("file must be regular and no larger than %d bytes", maxToolOutputBytes)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return agent.ToolResult{}, fmt.Errorf("file is not valid UTF-8 text")
	}
	return agent.ToolResult{Content: string(data)}, nil
}

func (resolver *resolver) listDirectory(
	_ context.Context,
	request agent.ToolRequest,
) (agent.ToolResult, error) {
	var input struct {
		Path string `json:"path"`
	}
	if err := decodeArguments(request.Arguments, &input); err != nil {
		return agent.ToolResult{}, err
	}
	path, err := resolver.resolveExisting(input.Path, true)
	if err != nil {
		return agent.ToolResult{}, err
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return agent.ToolResult{}, err
	}
	type item struct {
		Name string `json:"name"`
		Type string `json:"type"`
	}
	values := make([]item, 0, len(entries))
	for _, entry := range entries {
		kind := "file"
		if entry.IsDir() {
			kind = "directory"
		} else if entry.Type()&os.ModeSymlink != 0 {
			kind = "symlink"
		}
		values = append(values, item{Name: entry.Name(), Type: kind})
	}
	sort.Slice(values, func(left, right int) bool {
		return values[left].Name < values[right].Name
	})
	data, _ := json.Marshal(values)
	return agent.ToolResult{Content: string(data)}, nil
}

func (resolver *resolver) writeFile(
	_ context.Context,
	request agent.ToolRequest,
) (agent.ToolResult, error) {
	var input struct {
		Path    string `json:"path"`
		Content string `json:"content"`
	}
	if err := decodeArguments(request.Arguments, &input); err != nil {
		return agent.ToolResult{}, err
	}
	if len(input.Content) > maxToolOutputBytes {
		return agent.ToolResult{}, fmt.Errorf("content exceeds %d bytes", maxToolOutputBytes)
	}
	path, err := resolver.resolveWritable(input.Path)
	if err != nil {
		return agent.ToolResult{}, err
	}
	parent := filepath.Dir(path)
	temp, err := os.CreateTemp(parent, ".runtime-tool-*.tmp")
	if err != nil {
		return agent.ToolResult{}, err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return agent.ToolResult{}, err
	}
	if _, err := io.WriteString(temp, input.Content); err != nil {
		temp.Close()
		return agent.ToolResult{}, err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return agent.ToolResult{}, err
	}
	if err := temp.Close(); err != nil {
		return agent.ToolResult{}, err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: `{"written":true}`}, nil
}

func (resolver *resolver) execCommand(
	ctx context.Context,
	request agent.ToolRequest,
) (agent.ToolResult, error) {
	var input struct {
		Argv           []string `json:"argv"`
		CWD            string   `json:"cwd"`
		TimeoutSeconds int      `json:"timeout_seconds"`
	}
	if err := decodeArguments(request.Arguments, &input); err != nil {
		return agent.ToolResult{}, err
	}
	if len(input.Argv) == 0 || len(input.Argv) > 256 {
		return agent.ToolResult{}, fmt.Errorf("argv must contain 1 to 256 items")
	}
	for _, value := range input.Argv {
		if value == "" || strings.ContainsRune(value, 0) {
			return agent.ToolResult{}, fmt.Errorf("argv contains an invalid item")
		}
	}
	cwd := input.CWD
	if cwd == "" {
		cwd = resolver.cwd
	}
	cwd, err := resolver.resolveExisting(cwd, true)
	if err != nil {
		return agent.ToolResult{}, err
	}
	timeout := time.Duration(input.TimeoutSeconds) * time.Second
	if timeout <= 0 {
		timeout = 60 * time.Second
	}
	if timeout > 5*time.Minute {
		return agent.ToolResult{}, fmt.Errorf("timeout_seconds exceeds 300")
	}
	commandContext, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	command := exec.CommandContext(commandContext, input.Argv[0], input.Argv[1:]...)
	command.Dir = cwd
	var output limitedBuffer
	command.Stdout = &output
	command.Stderr = &output
	err = command.Run()
	result, _ := json.Marshal(map[string]any{
		"output": output.String(), "success": err == nil,
	})
	if output.overflow {
		return agent.ToolResult{}, fmt.Errorf("command output exceeds %d bytes", maxToolOutputBytes)
	}
	if err != nil {
		return agent.ToolResult{Content: string(result), IsError: true}, nil
	}
	return agent.ToolResult{Content: string(result)}, nil
}

func (resolver *resolver) resolveExisting(
	value string,
	requireDirectory bool,
) (string, error) {
	path, err := resolver.absoluteWithinRoot(value)
	if err != nil {
		return "", err
	}
	root, err := resolver.rootFor(path)
	if err != nil {
		return "", err
	}
	if err := rejectSymlinkComponents(root, path); err != nil {
		return "", err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if requireDirectory && !info.IsDir() {
		return "", fmt.Errorf("path is not a directory")
	}
	return path, nil
}

func (resolver *resolver) resolveWritable(value string) (string, error) {
	path, err := resolver.absoluteWithinRoot(value)
	if err != nil {
		return "", err
	}
	root, err := resolver.rootFor(path)
	if err != nil {
		return "", err
	}
	if err := rejectSymlinkComponents(root, filepath.Dir(path)); err != nil {
		return "", err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return "", fmt.Errorf("write target must be a regular file, not a symlink")
		}
	} else if !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	return path, nil
}

func (resolver *resolver) absoluteWithinRoot(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("path is required")
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(resolver.cwd, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	for _, root := range resolver.roots {
		for _, base := range []string{root.canonical, root.lexical} {
			relative, err := filepath.Rel(base, absolute)
			if err == nil && relative != ".." &&
				!strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
				return filepath.Join(root.canonical, relative), nil
			}
		}
	}
	return "", fmt.Errorf("path is outside configured workspace roots")
}

func (resolver *resolver) rootFor(path string) (string, error) {
	for _, root := range resolver.roots {
		relative, err := filepath.Rel(root.canonical, path)
		if err == nil && relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return root.canonical, nil
		}
	}
	return "", fmt.Errorf("path is outside configured workspace roots")
}

func rejectSymlinkComponents(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return fmt.Errorf("path is outside configured workspace roots")
	}
	current := root
	remainder := relative
	for _, part := range strings.Split(remainder, string(os.PathSeparator)) {
		if part == "" || part == "." {
			continue
		}
		current = filepath.Join(current, part)
		info, err := os.Lstat(current)
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 {
			return fmt.Errorf("path contains symlink component %s", current)
		}
	}
	return nil
}

func decodeArguments(value json.RawMessage, target any) error {
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("tool arguments contain trailing JSON")
	}
	return nil
}

type limitedBuffer struct {
	value    bytes.Buffer
	overflow bool
}

func (buffer *limitedBuffer) Write(value []byte) (int, error) {
	if buffer.value.Len()+len(value) > maxToolOutputBytes {
		remaining := maxToolOutputBytes - buffer.value.Len()
		if remaining > 0 {
			_, _ = buffer.value.Write(value[:remaining])
		}
		buffer.overflow = true
		return len(value), nil
	}
	return buffer.value.Write(value)
}

func (buffer *limitedBuffer) String() string {
	return buffer.value.String()
}
