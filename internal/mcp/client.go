package mcp

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os/exec"
	"strconv"
	"strings"
	"sync"
	"time"
)

const ProtocolVersion = "2025-11-25"

type Config struct {
	Name    string
	Command string
	Args    []string
	Dir     string
	Env     []string
	Timeout time.Duration
}

type Tool struct {
	Name        string         `json:"name"`
	Title       string         `json:"title,omitempty"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"inputSchema"`
}

type CallResult struct {
	Content           []map[string]any `json:"content"`
	StructuredContent map[string]any   `json:"structuredContent,omitempty"`
	IsError           bool             `json:"isError,omitempty"`
}

type Client struct {
	config  Config
	command *exec.Cmd
	stdin   io.WriteCloser
	cancel  context.CancelFunc
	done    chan error

	writeMu sync.Mutex
	mu      sync.Mutex
	nextID  int64
	pending map[string]chan rpcResponse
	closed  bool
	readErr error
}

type rpcMessage struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id,omitempty"`
	Method  string          `json:"method,omitempty"`
	Params  json.RawMessage `json:"params,omitempty"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcResponse struct {
	result json.RawMessage
	err    error
}

type rpcError struct {
	Code    int             `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (e *rpcError) Error() string {
	if len(e.Data) == 0 {
		return fmt.Sprintf("MCP JSON-RPC error %d: %s", e.Code, e.Message)
	}
	return fmt.Sprintf("MCP JSON-RPC error %d: %s (%s)", e.Code, e.Message, strings.TrimSpace(string(e.Data)))
}

func Start(ctx context.Context, config Config) (*Client, error) {
	if strings.TrimSpace(config.Command) == "" {
		return nil, fmt.Errorf("MCP server %s: command is required", config.Name)
	}
	if config.Timeout <= 0 {
		config.Timeout = 30 * time.Second
	}
	processContext, cancel := context.WithCancel(context.Background())
	command := exec.CommandContext(processContext, config.Command, config.Args...)
	command.Dir = config.Dir
	command.Env = config.Env
	stdin, err := command.StdinPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("MCP server %s stdin: %w", config.Name, err)
	}
	stdout, err := command.StdoutPipe()
	if err != nil {
		cancel()
		return nil, fmt.Errorf("MCP server %s stdout: %w", config.Name, err)
	}
	if err := command.Start(); err != nil {
		cancel()
		return nil, fmt.Errorf("start MCP server %s: %w", config.Name, err)
	}
	client := &Client{
		config: config, command: command, stdin: stdin, cancel: cancel,
		done: make(chan error, 1), pending: make(map[string]chan rpcResponse),
	}
	go func() { client.done <- command.Wait() }()
	go client.readLoop(stdout)
	go func() {
		select {
		case <-ctx.Done():
			client.Close()
		case <-client.done:
		}
	}()
	initialize := map[string]any{
		"protocolVersion": ProtocolVersion,
		"capabilities":    map[string]any{},
		"clientInfo":      map[string]any{"name": "sn-runtime", "version": "1"},
	}
	var initialized struct {
		ProtocolVersion string `json:"protocolVersion"`
	}
	if err := client.call(ctx, "initialize", initialize, &initialized); err != nil {
		client.Close()
		return nil, fmt.Errorf("initialize MCP server %s: %w", config.Name, err)
	}
	if strings.TrimSpace(initialized.ProtocolVersion) == "" {
		client.Close()
		return nil, fmt.Errorf("initialize MCP server %s: missing protocolVersion", config.Name)
	}
	if err := client.notify("notifications/initialized", map[string]any{}); err != nil {
		client.Close()
		return nil, fmt.Errorf("notify MCP server %s initialized: %w", config.Name, err)
	}
	return client, nil
}

func (c *Client) ListTools(ctx context.Context) ([]Tool, error) {
	var tools []Tool
	cursor := ""
	seenCursors := make(map[string]struct{})
	for pageIndex := 0; pageIndex < 100; pageIndex++ {
		params := map[string]any{}
		if cursor != "" {
			params["cursor"] = cursor
		}
		var page struct {
			Tools      []Tool `json:"tools"`
			NextCursor string `json:"nextCursor"`
		}
		if err := c.call(ctx, "tools/list", params, &page); err != nil {
			return nil, fmt.Errorf("MCP server %s tools/list: %w", c.config.Name, err)
		}
		tools = append(tools, page.Tools...)
		if len(tools) > 4096 {
			return nil, fmt.Errorf("MCP server %s returned too many tools", c.config.Name)
		}
		if page.NextCursor == "" {
			return tools, nil
		}
		if _, exists := seenCursors[page.NextCursor]; exists {
			return nil, fmt.Errorf("MCP server %s repeated tools/list cursor", c.config.Name)
		}
		seenCursors[page.NextCursor] = struct{}{}
		cursor = page.NextCursor
	}
	return nil, fmt.Errorf("MCP server %s tools/list exceeded 100 pages", c.config.Name)
}

func (c *Client) CallTool(ctx context.Context, name string, arguments map[string]any) (CallResult, error) {
	var result CallResult
	err := c.call(ctx, "tools/call", map[string]any{"name": name, "arguments": arguments}, &result)
	if err != nil {
		return CallResult{}, fmt.Errorf("MCP server %s tools/call %s: %w", c.config.Name, name, err)
	}
	return result, nil
}

func (c *Client) Close() {
	c.mu.Lock()
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = make(map[string]chan rpcResponse)
	c.mu.Unlock()
	_ = c.stdin.Close()
	c.cancel()
	for _, channel := range pending {
		select {
		case channel <- rpcResponse{err: fmt.Errorf("MCP server %s closed", c.config.Name)}:
		default:
		}
	}
}

func (c *Client) call(ctx context.Context, method string, params any, output any) error {
	c.mu.Lock()
	if c.closed {
		err := c.readErr
		if err == nil {
			err = fmt.Errorf("MCP server %s is closed", c.config.Name)
		}
		c.mu.Unlock()
		return err
	}
	c.nextID++
	id := c.nextID
	key := strconv.FormatInt(id, 10)
	channel := make(chan rpcResponse, 1)
	c.pending[key] = channel
	c.mu.Unlock()
	defer func() {
		c.mu.Lock()
		delete(c.pending, key)
		c.mu.Unlock()
	}()
	if err := c.write(map[string]any{"jsonrpc": "2.0", "id": id, "method": method, "params": params}); err != nil {
		return err
	}
	timeout := time.NewTimer(c.config.Timeout)
	defer timeout.Stop()
	select {
	case response := <-channel:
		if response.err != nil {
			return response.err
		}
		if output == nil || len(response.result) == 0 {
			return nil
		}
		if err := json.Unmarshal(response.result, output); err != nil {
			return fmt.Errorf("decode MCP server %s response: %w", c.config.Name, err)
		}
		return nil
	case <-ctx.Done():
		return ctx.Err()
	case <-timeout.C:
		return fmt.Errorf("MCP server %s request %s timed out after %s", c.config.Name, method, c.config.Timeout)
	}
}

func (c *Client) notify(method string, params any) error {
	return c.write(map[string]any{"jsonrpc": "2.0", "method": method, "params": params})
}

func (c *Client) write(value any) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	c.writeMu.Lock()
	defer c.writeMu.Unlock()
	if _, err := c.stdin.Write(append(data, '\n')); err != nil {
		return fmt.Errorf("write MCP server %s: %w", c.config.Name, err)
	}
	return nil
}

func (c *Client) readLoop(reader io.Reader) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 16*1024*1024)
	for scanner.Scan() {
		line := scanner.Bytes()
		var message rpcMessage
		if err := json.Unmarshal(line, &message); err != nil {
			c.fail(fmt.Errorf("decode MCP server %s stdout: %w", c.config.Name, err))
			return
		}
		if len(message.ID) == 0 {
			continue
		}
		key := normalizeID(message.ID)
		if message.Method != "" {
			_ = c.write(map[string]any{
				"jsonrpc": "2.0", "id": json.RawMessage(message.ID),
				"error": map[string]any{"code": -32601, "message": "client method not supported"},
			})
			continue
		}
		c.mu.Lock()
		channel := c.pending[key]
		c.mu.Unlock()
		if channel == nil {
			continue
		}
		response := rpcResponse{result: message.Result}
		if message.Error != nil {
			response.err = message.Error
		}
		select {
		case channel <- response:
		default:
		}
	}
	if err := scanner.Err(); err != nil {
		c.fail(fmt.Errorf("read MCP server %s stdout: %w", c.config.Name, err))
		return
	}
	c.fail(fmt.Errorf("MCP server %s stdout closed", c.config.Name))
}

func (c *Client) fail(err error) {
	c.mu.Lock()
	if c.readErr == nil {
		c.readErr = err
	}
	if c.closed {
		c.mu.Unlock()
		return
	}
	c.closed = true
	pending := c.pending
	c.pending = make(map[string]chan rpcResponse)
	c.mu.Unlock()
	c.cancel()
	for _, channel := range pending {
		select {
		case channel <- rpcResponse{err: err}:
		default:
		}
	}
}

func normalizeID(raw json.RawMessage) string {
	var number json.Number
	if err := json.Unmarshal(raw, &number); err == nil {
		return number.String()
	}
	var text string
	if err := json.Unmarshal(raw, &text); err == nil {
		return text
	}
	return strings.TrimSpace(string(raw))
}
