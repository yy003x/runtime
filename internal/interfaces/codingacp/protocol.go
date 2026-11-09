// Package codingacp 把 sn-code 的 AgentSession 内核适配到 Agent Client
// Protocol v1（agentclientprotocol.com）的 stdio JSON-RPC 接入：编辑器等
// ACP 客户端通过本包驱动代理，不启动第二个 sn-code 进程。协议 DTO 在本包
// 手写投影，不引入外部 SDK；会话、模型与工具全部复用 codingagent 内核。
//
// 注意区分：仓内 executor=acp 的 ACP client adapter（外部 agent 作为 CLI
// Profile）与 AEP（durable Run 的 HTTP 入站协议）都是独立协议，与这里的
// ACP server 模式无关。
package codingacp

import (
	"encoding/json"
	"fmt"
)

// JSON-RPC 2.0 错误码；requestCancelled 沿用 LSP 的扩展惯例码。
const (
	// codeParseError 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeParseError = -32700
	// codeInvalidRequest 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeInvalidRequest = -32600
	// codeMethodNotFound 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeMethodNotFound = -32601
	// codeInvalidParams 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeInvalidParams = -32602
	// codeInternalError 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeInternalError = -32603
	// codeRequestCancelled 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	codeRequestCancelled = -32800
)

// RequestError 是 JSON-RPC 响应错误。Data 只携带原因标识与摘要，
// 不回显请求输入正文。
type RequestError struct {
	Code    int64  `json:"code"`
	Message string `json:"message"`
	Data    any    `json:"data,omitempty"`
}

// Error 执行 Error 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (err *RequestError) Error() string {
	if err == nil {
		return "<nil>"
	}
	return fmt.Sprintf("acp request error %d: %s", err.Code, err.Message)
}

// NewParseError 执行 NewParseError 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 data 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func NewParseError(data any) *RequestError {
	return &RequestError{Code: codeParseError, Message: "Parse error", Data: data}
}

// NewInvalidRequest 执行 NewInvalidRequest 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 data 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func NewInvalidRequest(data any) *RequestError {
	return &RequestError{Code: codeInvalidRequest, Message: "Invalid request", Data: data}
}

// NewMethodNotFound 执行 NewMethodNotFound 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 method 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func NewMethodNotFound(method string) *RequestError {
	return &RequestError{Code: codeMethodNotFound, Message: "Method not found", Data: map[string]any{"method": method}}
}

// NewInvalidParams 执行 NewInvalidParams 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 data 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func NewInvalidParams(data any) *RequestError {
	return &RequestError{Code: codeInvalidParams, Message: "Invalid params", Data: data}
}

// NewInternalError 执行 NewInternalError 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 data 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func NewInternalError(data any) *RequestError {
	return &RequestError{Code: codeInternalError, Message: "Internal error", Data: data}
}

// NewRequestCancelled 执行 NewRequestCancelled 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 data 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func NewRequestCancelled(data any) *RequestError {
	return &RequestError{Code: codeRequestCancelled, Message: "Request cancelled", Data: data}
}

// invalid 执行 invalid 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 reason 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func invalid(reason string) *RequestError {
	return NewInvalidParams(map[string]any{"reason": reason})
}

// internalFailure 执行 internalFailure 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 code 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func internalFailure(code string) *RequestError {
	return NewInternalError(map[string]any{"code": code})
}

// decode 把 params 解码为方法 DTO；宽松忽略未知字段，结构不符才拒绝。
// 参数 raw 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 T、*RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func decode[T any](raw json.RawMessage) (T, *RequestError) {
	var value T
	if len(raw) == 0 || string(raw) == "null" || json.Unmarshal(raw, &value) != nil {
		return value, invalid("params must match the method's object schema")
	}
	return value, nil
}

// ---- 入站请求 DTO ----

type initializeRequest struct {
	ProtocolVersion int `json:"protocolVersion"`
}

type newSessionRequest struct {
	Cwd                   string            `json:"cwd"`
	McpServers            []json.RawMessage `json:"mcpServers"`
	AdditionalDirectories []string          `json:"additionalDirectories"`
}

type loadSessionRequest struct {
	SessionId             string            `json:"sessionId"`
	Cwd                   string            `json:"cwd"`
	McpServers            []json.RawMessage `json:"mcpServers"`
	AdditionalDirectories []string          `json:"additionalDirectories"`
}

type promptRequest struct {
	SessionId string         `json:"sessionId"`
	Prompt    []contentBlock `json:"prompt"`
}

type cancelRequest struct {
	SessionId string `json:"sessionId"`
}

// contentBlock 是 ACP v1 内容块的接收投影。一期只消费文本块；text 字段
// 保留原始 JSON，以兼容客户端实现中的平铺与 SDK 嵌套两种 wire 形态。
type contentBlock struct {
	Type  string          `json:"type,omitempty"`
	Text  json.RawMessage `json:"text,omitempty"`
	Image *struct {
		Data     string `json:"data"`
		MimeType string `json:"mimeType"`
	} `json:"image,omitempty"`
}

// textBlockText 解出文本块内容：平铺 {"type":"text","text":"..."} 与 SDK
// 嵌套 {"type":"text","text":{"type":"text","text":"..."}} 都接受。
// 参数 raw 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string、bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func textBlockText(raw json.RawMessage) (string, bool) {
	if len(raw) == 0 {
		return "", false
	}
	var flat string
	if err := json.Unmarshal(raw, &flat); err == nil {
		return flat, true
	}
	var nested struct {
		Text string `json:"text"`
	}
	if err := json.Unmarshal(raw, &nested); err == nil {
		return nested.Text, true
	}
	return "", false
}

// ---- 出站结果 DTO ----

type initializeResult struct {
	ProtocolVersion   int               `json:"protocolVersion"`
	AgentInfo         agentInfo         `json:"agentInfo"`
	AgentCapabilities agentCapabilities `json:"agentCapabilities"`
	AuthMethods       []authMethod      `json:"authMethods"`
}

type agentInfo struct {
	Name    string `json:"name"`
	Title   string `json:"title"`
	Version string `json:"version"`
}

type agentCapabilities struct {
	LoadSession        bool               `json:"loadSession"`
	PromptCapabilities promptCapabilities `json:"promptCapabilities"`
}

// promptCapabilities 按一期实际支持如实声明：不接收图像与嵌入上下文。
type promptCapabilities struct {
	Image           bool `json:"image"`
	EmbeddedContext bool `json:"embeddedContext"`
}

type authMethod struct {
	Id          string `json:"id"`
	Name        string `json:"name"`
	Description string `json:"description,omitempty"`
}

type newSessionResult struct {
	SessionId string `json:"sessionId"`
}

// loadSessionResult 是空对象；session/load 的会话身份由 sessionId 参数表达。
type loadSessionResult struct{}

const (
	// stopEndTurn 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	stopEndTurn = "end_turn"
	// stopCancelled 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	stopCancelled = "cancelled"
	// stopMaxTokens 定义当前流程使用的固定值（token 数量），取值来源为本实现约定。
	stopMaxTokens = "max_tokens"
)

type promptResult struct {
	StopReason string `json:"stopReason"`
}

// ---- session/update 通知 DTO ----

type sessionUpdateParams struct {
	SessionId string `json:"sessionId"`
	Update    any    `json:"update"`
}

// messageChunkUpdate 承载 agent_message_chunk / user_message_chunk。
type messageChunkUpdate struct {
	SessionUpdate string    `json:"sessionUpdate"`
	Content       textBlock `json:"content"`
}

type textBlock struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

// toolCallUpdate 首次声明一个工具调用。
type toolCallUpdate struct {
	SessionUpdate string `json:"sessionUpdate"`
	ToolCallId    string `json:"toolCallId"`
	Title         string `json:"title"`
	Kind          string `json:"kind"`
	Status        string `json:"status"`
}

// toolCallStatusUpdate 推进工具调用状态；终态更新可携带结果内容。
type toolCallStatusUpdate struct {
	SessionUpdate string            `json:"sessionUpdate"`
	ToolCallId    string            `json:"toolCallId"`
	Status        string            `json:"status"`
	Content       []toolCallContent `json:"content,omitempty"`
}

// toolCallContent 是工具结果内容的包装层（type 固定 "content"）。
type toolCallContent struct {
	Type    string    `json:"type"`
	Content textBlock `json:"content"`
}

// ---- session/request_permission（server→client 请求）DTO ----

type requestPermissionParams struct {
	SessionId string             `json:"sessionId"`
	ToolCall  permissionToolCall `json:"toolCall"`
	Options   []permissionOption `json:"options"`
}

type permissionToolCall struct {
	ToolCallId string            `json:"toolCallId"`
	Title      string            `json:"title"`
	Status     string            `json:"status"`
	Content    []toolCallContent `json:"content,omitempty"`
}

type permissionOption struct {
	OptionId string `json:"optionId"`
	Name     string `json:"name"`
	Kind     string `json:"kind"`
}

type requestPermissionResult struct {
	Outcome permissionOutcome `json:"outcome"`
}

type permissionOutcome struct {
	Kind     string `json:"kind"`
	OptionId string `json:"optionId,omitempty"`
}
