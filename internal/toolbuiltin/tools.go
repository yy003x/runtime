package toolbuiltin

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"unicode/utf8"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
)

const (
	maxToolOutputBytes               = 1 << 20
	maxReadOnlyToolErrorBytes        = 4 << 10
	ExecutionImplementation          = "runtime.toolbuiltin"
	ExecutionImplementationVersion   = 4
	toolExecutionConfigSchemaVersion = 2
)

var (
	errPathRequired      = errors.New("path is required")
	errOutsideWorkspace  = errors.New("path is outside configured workspace roots")
	errSymlinkNotAllowed = errors.New("path contains symlink component")
	errPathNotDirectory  = errors.New("path is not a directory")
)

type Options struct {
	Names []string
	Roots []string
	CWD   string
}

// Bundle 保存选中的内置工具及其冻结后的非 secret 执行配置，composition root
// 用它把内置工具和 manifest 工具组合为同一个 Agent registry。
type Bundle struct {
	Tools         []agent.RegisteredTool
	Configuration json.RawMessage
}

func Build(options Options) (*agent.Registry, error) {
	bundle, err := BuildBundle(options)
	if err != nil {
		return nil, err
	}
	return agent.NewRegistryWithToolExecution(agent.ToolExecutionIdentity{
		Implementation:        ExecutionImplementation,
		ImplementationVersion: ExecutionImplementationVersion,
		Configuration:         bundle.Configuration,
	}, bundle.Tools...)
}

// BuildBundle 准备选中的内置定义和 handler，但不创建独立 registry；返回的配置
// 不包含环境变量 secret。
func BuildBundle(options Options) (Bundle, error) {
	if options.CWD == "" {
		current, err := os.Getwd()
		if err != nil {
			return Bundle{}, err
		}
		options.CWD = current
	}
	if len(options.Roots) == 0 {
		options.Roots = []string{options.CWD}
	}
	resolver, err := newResolver(options.Roots, options.CWD)
	if err != nil {
		return Bundle{}, err
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
	}
	values := make([]agent.RegisteredTool, 0, len(options.Names))
	for _, name := range options.Names {
		value, exists := available[name]
		if !exists {
			return Bundle{}, fmt.Errorf("unknown built-in tool %q", name)
		}
		values = append(values, value)
	}
	configuration, err := json.Marshal(toolExecutionConfiguration{
		SchemaVersion:  toolExecutionConfigSchemaVersion,
		WorkspaceRoots: snapshotWorkspaceRoots(resolver.roots),
		CWD:            resolver.cwd,
	})
	if err != nil {
		return Bundle{}, err
	}
	return Bundle{Tools: values, Configuration: configuration}, nil
}

// IsBuiltin 判断名称是否属于 Runtime 固定内置工具集；manifest 工具名由 Tool
// Catalog 独立解析。
func IsBuiltin(name string) bool {
	switch name {
	case "read_file", "list_directory", "write_file":
		return true
	default:
		return false
	}
}

type resolver struct {
	roots     []workspaceRoot
	cwd       string
	testHooks *resolverTestHooks
}

type workspaceRoot struct {
	lexical   string
	canonical string
	device    uint64
	inode     uint64
}

type toolExecutionConfiguration struct {
	SchemaVersion  int                          `json:"schema_version"`
	WorkspaceRoots []toolExecutionWorkspaceRoot `json:"workspace_roots"`
	CWD            string                       `json:"cwd"`
}

type toolExecutionWorkspaceRoot struct {
	Lexical   string `json:"lexical"`
	Canonical string `json:"canonical"`
	Device    uint64 `json:"device"`
	Inode     uint64 `json:"inode"`
}

func snapshotWorkspaceRoots(
	roots []workspaceRoot,
) []toolExecutionWorkspaceRoot {
	values := make([]toolExecutionWorkspaceRoot, len(roots))
	for index, root := range roots {
		values[index] = toolExecutionWorkspaceRoot{
			Lexical: root.lexical, Canonical: root.canonical,
			Device: root.device, Inode: root.inode,
		}
	}
	return values
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
		device, inode, err := workspaceRootIdentity(canonical)
		if err != nil {
			return nil, err
		}
		values = append(values, workspaceRoot{
			lexical: filepath.Clean(absolute), canonical: filepath.Clean(canonical),
			device: device, inode: inode,
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
	path, err := resolver.resolveWorkspacePath(input.Path)
	if err != nil {
		return readOnlyFilesystemFailure(err)
	}
	file, _, err := resolver.openReadFile(path)
	if err != nil {
		return readOnlyFilesystemFailure(err)
	}
	data, err := boundedRead(file)
	if err != nil {
		_ = file.Close()
		return readOnlyFilesystemFailure(err)
	}
	if err := file.Close(); err != nil {
		return readOnlyFilesystemFailure(err)
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return readOnlyToolFailure(
			"invalid_utf8",
			"file is not valid UTF-8 text",
		)
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
	path, err := resolver.resolveWorkspacePath(input.Path)
	if err != nil {
		return readOnlyFilesystemFailure(err)
	}
	directory, err := resolver.openListDirectory(path)
	if err != nil {
		return readOnlyFilesystemFailure(err)
	}
	entries, err := boundedDirectoryEntries(directory)
	if err != nil {
		_ = directory.Close()
		return readOnlyFilesystemFailure(err)
	}
	if err := directory.Close(); err != nil {
		return readOnlyFilesystemFailure(err)
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
	data, err := json.Marshal(values)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf(
			"encode directory listing: %w",
			err,
		)
	}
	if len(data) > maxToolOutputBytes {
		return readOnlyToolFailure(
			"directory_too_large",
			"directory exceeds the listing limit",
		)
	}
	return agent.ToolResult{Content: string(data)}, nil
}

type readOnlyToolErrorEnvelope struct {
	Error readOnlyToolError `json:"error"`
}

type readOnlyToolError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func readOnlyFilesystemFailure(err error) (agent.ToolResult, error) {
	switch {
	case errors.Is(err, errPathRequired):
		return readOnlyToolFailure("invalid_path", "path is required")
	case errors.Is(err, errOutsideWorkspace):
		return readOnlyToolFailure(
			"outside_workspace",
			"path is outside configured workspace roots",
		)
	case errors.Is(err, errSymlinkNotAllowed):
		return readOnlyToolFailure(
			"symlink_not_allowed",
			"symlink paths are not allowed",
		)
	case errors.Is(err, errPathNotDirectory):
		return readOnlyToolFailure(
			"not_directory",
			"path is not a directory",
		)
	case errors.Is(err, errPathNotRegular):
		return readOnlyToolFailure(
			"not_regular_file",
			"path is not a regular file",
		)
	case errors.Is(err, errReadHardlink):
		return readOnlyToolFailure(
			"hardlink_not_allowed",
			"files with multiple hard links are not allowed",
		)
	case errors.Is(err, errFileTooLarge):
		return readOnlyToolFailure(
			"file_too_large",
			"file exceeds the read limit",
		)
	case errors.Is(err, errDirectoryTooLarge):
		return readOnlyToolFailure(
			"directory_too_large",
			"directory exceeds the listing limit",
		)
	case errors.Is(err, errWorkspaceRootChanged):
		return readOnlyToolFailure(
			"workspace_changed",
			"workspace root identity changed",
		)
	case errors.Is(err, os.ErrNotExist):
		return readOnlyToolFailure("not_found", "path does not exist")
	case errors.Is(err, os.ErrPermission):
		return readOnlyToolFailure(
			"permission_denied",
			"filesystem access was denied",
		)
	default:
		return readOnlyToolFailure(
			"io_error",
			"filesystem operation failed",
		)
	}
}

func readOnlyToolFailure(
	code string,
	message string,
) (agent.ToolResult, error) {
	content, err := json.Marshal(readOnlyToolErrorEnvelope{
		Error: readOnlyToolError{Code: code, Message: message},
	})
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf(
			"encode read-only tool error: %w",
			err,
		)
	}
	if len(content) == 0 || len(content) > maxReadOnlyToolErrorBytes {
		return agent.ToolResult{}, fmt.Errorf(
			"read-only tool error exceeds %d bytes",
			maxReadOnlyToolErrorBytes,
		)
	}
	return agent.ToolResult{Content: string(content), IsError: true}, nil
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
	path, err := resolver.resolveWorkspacePath(input.Path)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if err := resolver.writeFileAt(path, input.Content); err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: `{"written":true}`}, nil
}

func (resolver *resolver) absoluteWithinRoot(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", errPathRequired
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
	return "", errOutsideWorkspace
}

func (resolver *resolver) rootFor(path string) (string, error) {
	for _, root := range resolver.roots {
		relative, err := filepath.Rel(root.canonical, path)
		if err == nil && relative != ".." &&
			!strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
			return root.canonical, nil
		}
	}
	return "", errOutsideWorkspace
}

func rejectSymlinkComponents(root, path string) error {
	relative, err := filepath.Rel(root, path)
	if err != nil || relative == ".." ||
		strings.HasPrefix(relative, ".."+string(os.PathSeparator)) {
		return errOutsideWorkspace
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
			return fmt.Errorf("%w %s", errSymlinkNotAllowed, current)
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
