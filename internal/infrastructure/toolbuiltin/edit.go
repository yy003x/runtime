package toolbuiltin

import (
	"bytes"
	"context"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/yy003x/runtime/pkg/agent"
)

func (resolver *resolver) edit(
	_ context.Context,
	request agent.ToolRequest,
) (agent.ToolResult, error) {
	var input struct {
		Path      string `json:"path"`
		OldString string `json:"old_string"`
		NewString string `json:"new_string"`
	}
	if err := decodeArguments(request.Arguments, &input); err != nil {
		return agent.ToolResult{}, err
	}
	if input.OldString == "" {
		return agent.ToolResult{}, fmt.Errorf("old_string is required")
	}
	if input.OldString == input.NewString {
		return agent.ToolResult{}, fmt.Errorf("old_string and new_string must differ")
	}
	path, err := resolver.resolveWorkspacePath(input.Path)
	if err != nil {
		return agent.ToolResult{}, err
	}
	file, _, err := resolver.openReadFile(path)
	if err != nil {
		return agent.ToolResult{}, err
	}
	data, err := boundedRead(file)
	closeErr := file.Close()
	if err != nil {
		return agent.ToolResult{}, err
	}
	if closeErr != nil {
		return agent.ToolResult{}, closeErr
	}
	if bytes.IndexByte(data, 0) >= 0 || !utf8.Valid(data) {
		return agent.ToolResult{}, fmt.Errorf("file is not valid UTF-8 text")
	}
	content := string(data)
	count := strings.Count(content, input.OldString)
	if count == 0 {
		return agent.ToolResult{}, fmt.Errorf("old_string was not found")
	}
	if count > 1 {
		return agent.ToolResult{}, fmt.Errorf("old_string matched %d times; it must match exactly once", count)
	}
	updated := strings.Replace(content, input.OldString, input.NewString, 1)
	if len(updated) > maxToolOutputBytes {
		return agent.ToolResult{}, fmt.Errorf("content exceeds %d bytes", maxToolOutputBytes)
	}
	if err := resolver.writeFileAt(path, updated); err != nil {
		return agent.ToolResult{}, err
	}
	return agent.ToolResult{Content: `{"edited":true}`}, nil
}
