// Package toolmcp adapts configured read-only MCP tools to the Agent tool
// registry. Each execution creates one bounded Streamable HTTP MCP session and
// never retries or follows redirects.
package toolmcp

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"mime"
	"net/http"
	"os"
	"strings"
	"unicode/utf8"

	"github.com/yy003x/runtime/internal/infrastructure/envref"
	"github.com/yy003x/runtime/internal/infrastructure/toolconfig"
	"github.com/yy003x/runtime/pkg/agent"
)

const (
	ExecutionImplementation        = "runtime.toolmcp"
	ExecutionImplementationVersion = 2

	// defaultRequestedProtocolVersion 是客户端在 initialize 中声明的首选协议
	// 版本；服务端可在 allowed 集合内协商回更早版本。协议版本现在完全可配置，
	// 对齐 2026-07-28 Streamable HTTP spec（client/server 可协商支持集合内的
	// 任意版本，不再硬绑单一 negotiated 常量）。
	defaultRequestedProtocolVersion = "2025-06-18"
	maxToolResultBytes              = 1 << 20
	maxFailureBytes                 = 4 << 10
)

// defaultAllowedProtocolVersions 是允许服务端协商回来的版本白名单。服务端
// 返回的 protocolVersion 必须落在该集合内，否则按不支持收口。
var defaultAllowedProtocolVersions = []string{
	"2025-06-18", "2024-11-05",
}

type Options struct {
	LookupEnv                func(string) (string, bool)
	HTTPClient               *http.Client
	RequestedProtocolVersion string
	AllowedProtocolVersions  []string
}

type Bundle struct {
	Tools         []agent.RegisteredTool
	Configuration json.RawMessage
}

func Build(
	manifests []toolconfig.Manifest,
	options Options,
) (Bundle, error) {
	configuration, err := toolconfig.CanonicalJSON(manifests)
	if err != nil {
		return Bundle{}, err
	}
	lookup := options.LookupEnv
	if lookup == nil {
		lookup = os.LookupEnv
	}
	client := singleAttemptClient(options.HTTPClient)
	requested, allowed, err := resolveProtocolOptions(
		options.RequestedProtocolVersion, options.AllowedProtocolVersions,
	)
	if err != nil {
		return Bundle{}, err
	}
	registered := make([]agent.RegisteredTool, 0, len(manifests))
	seen := make(map[string]struct{}, len(manifests))
	for _, source := range manifests {
		manifest := source.Clone()
		if _, exists := seen[manifest.Name]; exists {
			return Bundle{}, fmt.Errorf("duplicate tool %q", manifest.Name)
		}
		seen[manifest.Name] = struct{}{}
		handler := &handler{
			manifest: manifest, lookupEnv: lookup, client: client,
			requestedProtocolVersion: requested,
			allowedProtocolVersions:  allowed,
		}
		registered = append(registered, agent.RegisteredTool{
			Definition: manifest.Definition(), Handler: handler.execute,
		})
	}
	return Bundle{Tools: registered, Configuration: configuration}, nil
}

func resolveProtocolOptions(
	requested string,
	allowed []string,
) (string, []string, error) {
	if strings.TrimSpace(requested) == "" {
		requested = defaultRequestedProtocolVersion
	}
	if len(allowed) == 0 {
		allowed = append([]string(nil), defaultAllowedProtocolVersions...)
	}
	seen := make(map[string]struct{}, len(allowed))
	for _, version := range allowed {
		if strings.TrimSpace(version) == "" {
			return "", nil, fmt.Errorf(
				"allowed protocol versions must not contain empty strings",
			)
		}
		if _, exists := seen[version]; exists {
			return "", nil, fmt.Errorf(
				"allowed protocol versions contain duplicate %q", version,
			)
		}
		seen[version] = struct{}{}
	}
	if _, accepts := seen[requested]; !accepts {
		return "", nil, fmt.Errorf(
			"requested protocol version %q must be in the allowed set",
			requested,
		)
	}
	return requested, allowed, nil
}

type handler struct {
	manifest                 toolconfig.Manifest
	lookupEnv                func(string) (string, bool)
	client                   *http.Client
	requestedProtocolVersion string
	allowedProtocolVersions  []string
}

func (handler *handler) execute(
	ctx context.Context,
	request agent.ToolRequest,
) (agent.ToolResult, error) {
	headers, secrets, err := resolveHeaders(
		handler.manifest.Executor.Headers, handler.lookupEnv,
	)
	if err != nil {
		return failure("configuration_error", err.Error(), nil), nil
	}
	ctx, cancel := context.WithTimeout(
		ctx, handler.manifest.Executor.Duration(),
	)
	defer cancel()
	client := sessionClient{
		endpoint: handler.manifest.Executor.Endpoint,
		headers:  headers, secrets: secrets, client: handler.client,
		maxResponseBytes:         handler.manifest.Executor.MaxResponseBytes,
		requestedProtocolVersion: handler.requestedProtocolVersion,
		allowedProtocolVersions:  handler.allowedProtocolVersions,
	}
	result, err := client.call(
		ctx, handler.manifest.Executor.RemoteTool, request.Arguments,
	)
	if err != nil {
		return failure(
			classifyFailure(err), redact(err.Error(), secrets), secrets,
		), nil
	}
	result.Content = redact(result.Content, secrets)
	if len(result.Content) > maxToolResultBytes {
		return failure(
			"response_too_large", "MCP tool result exceeds the output limit",
			secrets,
		), nil
	}
	return result, nil
}

func singleAttemptClient(source *http.Client) *http.Client {
	if source == nil {
		source = http.DefaultClient
	}
	clone := *source
	clone.Timeout = 0
	clone.CheckRedirect = func(
		_ *http.Request,
		_ []*http.Request,
	) error {
		return http.ErrUseLastResponse
	}
	return &clone
}

func resolveHeaders(
	configured map[string]string,
	lookup func(string) (string, bool),
) (http.Header, []string, error) {
	headers := make(http.Header, len(configured))
	var secrets []string
	for name, reference := range configured {
		resolved, err := envref.Expand(reference, func(name string) (string, bool) {
			value, exists := lookup(name)
			if !exists || value == "" {
				return "", false
			}
			secrets = append(secrets, value)
			return value, true
		})
		if err != nil {
			return nil, nil, fmt.Errorf("resolve header %q: %w", name, err)
		}
		if resolved == "" || strings.ContainsAny(resolved, "\r\n\x00") {
			return nil, nil, fmt.Errorf("resolved header %q is invalid", name)
		}
		secrets = append(secrets, resolved)
		headers.Set(name, resolved)
	}
	return headers, uniqueNonEmpty(secrets), nil
}

type sessionClient struct {
	endpoint                 string
	headers                  http.Header
	secrets                  []string
	client                   *http.Client
	maxResponseBytes         int64
	sessionID                string
	protocolVersion          string
	requestedProtocolVersion string
	allowedProtocolVersions  []string
}

func (client *sessionClient) call(
	ctx context.Context,
	remoteTool string,
	arguments json.RawMessage,
) (agent.ToolResult, error) {
	initialize, err := client.exchange(ctx, rpcRequest{
		JSONRPC: "2.0", ID: 1, Method: "initialize",
		Params: initializeParams{
			ProtocolVersion: client.requestedProtocolVersion,
			Capabilities:    struct{}{},
			ClientInfo: clientInfo{
				Name: "sn-runtime", Version: "1",
			},
		},
	}, 1, true)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("initialize MCP session: %w", err)
	}
	var initialized initializeResult
	if err := decodeResult(initialize.Result, &initialized); err != nil {
		return agent.ToolResult{}, fmt.Errorf("decode initialize result: %w", err)
	}
	if !protocolAllowed(initialized.ProtocolVersion, client.allowedProtocolVersions) {
		return agent.ToolResult{}, fmt.Errorf(
			"server negotiated unsupported protocol version %q",
			initialized.ProtocolVersion,
		)
	}
	client.protocolVersion = initialized.ProtocolVersion
	if _, err := client.exchange(ctx, rpcRequest{
		JSONRPC: "2.0", Method: "notifications/initialized",
	}, 0, false); err != nil {
		return agent.ToolResult{}, fmt.Errorf("send initialized notification: %w", err)
	}
	response, err := client.exchange(ctx, rpcRequest{
		JSONRPC: "2.0", ID: 2, Method: "tools/call",
		Params: callParams{Name: remoteTool, Arguments: arguments},
	}, 2, true)
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("call MCP tool: %w", err)
	}
	return decodeCallResult(response.Result)
}

type rpcRequest struct {
	JSONRPC string `json:"jsonrpc"`
	ID      int    `json:"id,omitempty"`
	Method  string `json:"method"`
	Params  any    `json:"params,omitempty"`
}

type initializeParams struct {
	ProtocolVersion string     `json:"protocolVersion"`
	Capabilities    struct{}   `json:"capabilities"`
	ClientInfo      clientInfo `json:"clientInfo"`
}

type clientInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type callParams struct {
	Name      string          `json:"name"`
	Arguments json.RawMessage `json:"arguments"`
}

type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  json.RawMessage `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int64           `json:"code"`
	Message string          `json:"message"`
	Data    json.RawMessage `json:"data,omitempty"`
}

func (client *sessionClient) exchange(
	ctx context.Context,
	payload rpcRequest,
	expectedID int,
	requireResponse bool,
) (rpcResponse, error) {
	body, err := json.Marshal(payload)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("encode JSON-RPC request: %w", err)
	}
	request, err := http.NewRequestWithContext(
		ctx, http.MethodPost, client.endpoint, bytes.NewReader(body),
	)
	if err != nil {
		return rpcResponse{}, fmt.Errorf("build HTTP request: %w", err)
	}
	for name, values := range client.headers {
		request.Header[name] = append([]string(nil), values...)
	}
	request.Header.Set("Accept", "application/json, text/event-stream")
	request.Header.Set("Content-Type", "application/json")
	if client.protocolVersion != "" {
		request.Header.Set("MCP-Protocol-Version", client.protocolVersion)
	}
	if client.sessionID != "" {
		request.Header.Set("Mcp-Session-Id", client.sessionID)
	}
	response, err := client.client.Do(request)
	if err != nil {
		if errors.Is(err, context.Canceled) || errors.Is(err, context.DeadlineExceeded) {
			return rpcResponse{}, errMCPTimeout
		}
		return rpcResponse{}, errMCPTransport
	}
	defer response.Body.Close()
	if err := client.acceptSessionID(response.Header.Get("Mcp-Session-Id")); err != nil {
		return rpcResponse{}, err
	}
	data, err := readBounded(response.Body, client.maxResponseBytes)
	if err != nil {
		return rpcResponse{}, err
	}
	if err := response.Body.Close(); err != nil {
		return rpcResponse{}, fmt.Errorf("close MCP response: %w", err)
	}
	if response.StatusCode < http.StatusOK || response.StatusCode >= http.StatusMultipleChoices {
		return rpcResponse{}, fmt.Errorf(
			"%w: HTTP status %d", errMCPHTTPStatus, response.StatusCode,
		)
	}
	if len(bytes.TrimSpace(data)) == 0 {
		if requireResponse {
			return rpcResponse{}, fmt.Errorf("MCP endpoint returned an empty response")
		}
		return rpcResponse{}, nil
	}
	responses, err := decodeHTTPResponse(response.Header.Get("Content-Type"), data)
	if err != nil {
		return rpcResponse{}, err
	}
	if !requireResponse {
		return rpcResponse{}, nil
	}
	for _, value := range responses {
		if idMatches(value.ID, expectedID) {
			if value.JSONRPC != "2.0" {
				return rpcResponse{}, fmt.Errorf("invalid JSON-RPC version")
			}
			if value.Error != nil {
				return rpcResponse{}, &mcpRPCError{
					code:    value.Error.Code,
					message: value.Error.Message,
				}
			}
			if len(value.Result) == 0 || bytes.Equal(value.Result, []byte("null")) {
				return rpcResponse{}, fmt.Errorf("JSON-RPC response has no result")
			}
			return value, nil
		}
	}
	return rpcResponse{}, fmt.Errorf(
		"MCP response has no JSON-RPC result for id %d", expectedID,
	)
}

func (client *sessionClient) acceptSessionID(value string) error {
	if value == "" {
		return nil
	}
	if len(value) > 1024 || !utf8.ValidString(value) {
		return fmt.Errorf("MCP endpoint returned an invalid session ID")
	}
	for index := 0; index < len(value); index++ {
		if value[index] < 0x21 || value[index] > 0x7e {
			return fmt.Errorf("MCP endpoint returned an invalid session ID")
		}
	}
	if client.sessionID != "" && client.sessionID != value {
		return fmt.Errorf("MCP endpoint changed the session ID")
	}
	client.sessionID = value
	return nil
}

func readBounded(reader io.Reader, maximum int64) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(reader, maximum+1))
	if err != nil {
		return nil, fmt.Errorf("read MCP response: %w", err)
	}
	if int64(len(data)) > maximum {
		return nil, fmt.Errorf("%w: %d bytes", errMCPTooLarge, maximum)
	}
	return data, nil
}

func decodeHTTPResponse(contentType string, data []byte) ([]rpcResponse, error) {
	mediaType, _, err := mime.ParseMediaType(contentType)
	if err != nil {
		return nil, fmt.Errorf("MCP response has invalid Content-Type")
	}
	var documents [][]byte
	switch strings.ToLower(mediaType) {
	case "application/json":
		documents = [][]byte{data}
	case "text/event-stream":
		documents, err = decodeSSE(data)
		if err != nil {
			return nil, err
		}
	default:
		return nil, fmt.Errorf("unsupported MCP Content-Type %q", mediaType)
	}
	responses := make([]rpcResponse, 0, len(documents))
	for _, document := range documents {
		var probe map[string]json.RawMessage
		if err := decodeStrictObject(document, &probe); err != nil {
			return nil, fmt.Errorf("decode JSON-RPC response: %w", err)
		}
		if _, hasID := probe["id"]; !hasID {
			continue
		}
		var response rpcResponse
		if err := decodeStrictObject(document, &response); err != nil {
			return nil, fmt.Errorf("decode JSON-RPC response: %w", err)
		}
		responses = append(responses, response)
	}
	return responses, nil
}

func decodeSSE(data []byte) ([][]byte, error) {
	data = bytes.ReplaceAll(data, []byte("\r\n"), []byte("\n"))
	data = bytes.ReplaceAll(data, []byte("\r"), []byte("\n"))
	blocks := bytes.Split(data, []byte("\n\n"))
	var documents [][]byte
	for _, block := range blocks {
		var fields [][]byte
		for _, line := range bytes.Split(block, []byte("\n")) {
			if len(line) == 0 || line[0] == ':' {
				continue
			}
			if bytes.HasPrefix(line, []byte("data:")) {
				value := line[len("data:"):]
				if len(value) > 0 && value[0] == ' ' {
					value = value[1:]
				}
				fields = append(fields, value)
			}
		}
		if len(fields) > 0 {
			documents = append(documents, bytes.Join(fields, []byte("\n")))
		}
	}
	if len(documents) == 0 {
		return nil, fmt.Errorf("MCP event stream contains no data events")
	}
	return documents, nil
}

func decodeStrictObject(data []byte, target any) error {
	// Imported indirectly through a small local wrapper to make every MCP
	// document reject duplicate names, unknown fields, trailing data and null.
	return decodeObject(data, target)
}

type initializeResult struct {
	ProtocolVersion string          `json:"protocolVersion"`
	Capabilities    json.RawMessage `json:"capabilities"`
	ServerInfo      json.RawMessage `json:"serverInfo"`
	Instructions    string          `json:"instructions,omitempty"`
}

type callResult struct {
	Content           []json.RawMessage `json:"content"`
	StructuredContent json.RawMessage   `json:"structuredContent,omitempty"`
	IsError           bool              `json:"isError,omitempty"`
	Meta              json.RawMessage   `json:"_meta,omitempty"`
}

func decodeResult(data json.RawMessage, target any) error {
	return decodeObject(data, target)
}

func decodeCallResult(data json.RawMessage) (agent.ToolResult, error) {
	var value callResult
	if err := decodeResult(data, &value); err != nil {
		return agent.ToolResult{}, fmt.Errorf("decode tool result: %w", err)
	}
	if len(value.Content) == 0 && len(value.StructuredContent) == 0 {
		return agent.ToolResult{}, fmt.Errorf("MCP tool result has no content")
	}
	var textBlocks []string
	allText := len(value.Content) > 0
	for _, raw := range value.Content {
		var block struct {
			Type string `json:"type"`
			Text string `json:"text,omitempty"`
		}
		if err := decodeObject(raw, &block); err != nil || block.Type != "text" {
			allText = false
			break
		}
		textBlocks = append(textBlocks, block.Text)
	}
	var content string
	if allText {
		content = strings.Join(textBlocks, "\n")
	} else if len(value.StructuredContent) > 0 {
		canonical, err := canonicalJSON(value.StructuredContent)
		if err != nil {
			return agent.ToolResult{}, fmt.Errorf("decode structured tool content: %w", err)
		}
		content = string(canonical)
	} else {
		canonical, err := canonicalJSON(value.Content)
		if err != nil {
			return agent.ToolResult{}, fmt.Errorf("decode MCP tool content: %w", err)
		}
		content = string(canonical)
	}
	return agent.ToolResult{Content: content, IsError: value.IsError}, nil
}

func idMatches(raw json.RawMessage, expected int) bool {
	var number int
	return json.Unmarshal(raw, &number) == nil && number == expected
}

// MCP failure sentinels decouple classifyFailure from error message wording.
// exchange returns these directly or wraps them with fmt.Errorf("...: %w"), so
// errors.Is reaches them through the method wrapping applied in call.
var (
	errMCPTimeout    = errors.New("MCP request cancelled or timed out")
	errMCPTransport  = errors.New("MCP transport failed")
	errMCPHTTPStatus = errors.New("MCP endpoint returned an unsuccessful HTTP status")
	errMCPTooLarge   = errors.New("MCP response exceeds the byte limit")
)

// mcpRPCError carries a structured JSON-RPC error code so classification no
// longer depends on the "JSON-RPC error" message substring. Its Error text
// keeps the previous format so redacted failure messages stay stable.
type mcpRPCError struct {
	code    int64
	message string
}

func (e *mcpRPCError) Error() string {
	return fmt.Sprintf("JSON-RPC error %d: %s", e.code, e.message)
}

func classifyFailure(err error) string {
	var remote *mcpRPCError
	switch {
	case errors.Is(err, errMCPTimeout):
		return "timeout"
	case errors.Is(err, errMCPTooLarge):
		return "response_too_large"
	case errors.Is(err, errMCPHTTPStatus):
		return "http_error"
	case errors.As(err, &remote):
		return "remote_error"
	case errors.Is(err, errMCPTransport):
		return "transport_error"
	default:
		return "protocol_error"
	}
}

type failureEnvelope struct {
	Error failureDetail `json:"error"`
}

type failureDetail struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func failure(code, message string, secrets []string) agent.ToolResult {
	message = redact(message, secrets)
	if len(message) > maxFailureBytes/2 {
		message = message[:maxFailureBytes/2] + "..."
	}
	data, err := json.Marshal(failureEnvelope{
		Error: failureDetail{Code: code, Message: message},
	})
	if err != nil || len(data) > maxFailureBytes {
		data = []byte(`{"error":{"code":"tool_error","message":"MCP tool failed"}}`)
	}
	return agent.ToolResult{Content: string(data), IsError: true}
}

func redact(value string, secrets []string) string {
	for _, secret := range secrets {
		if secret != "" {
			value = strings.ReplaceAll(value, secret, "[REDACTED]")
		}
	}
	return value
}

func protocolAllowed(version string, allowed []string) bool {
	for _, candidate := range allowed {
		if candidate == version {
			return true
		}
	}
	return false
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]struct{}, len(values))
	result := make([]string, 0, len(values))
	for _, value := range values {
		if value == "" {
			continue
		}
		if _, exists := seen[value]; exists {
			continue
		}
		seen[value] = struct{}{}
		result = append(result, value)
	}
	return result
}

func canonicalJSON(value any) ([]byte, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return nil, err
	}
	var decoded any
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	if err := decoder.Decode(&decoded); err != nil {
		return nil, err
	}
	return json.Marshal(decoded)
}
