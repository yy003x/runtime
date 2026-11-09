// Package codingagent 实现 sn-code 的交互式编码代理编排：pi 语义的
// agent 回合循环、steering/followUp 双队列、系统提示词构建、上下文压缩，
// 以及写副作用工具的确认门。模型调用复用 pkg/model，工具校验与执行复用
// pkg/agent.Registry；本包不感知 TUI。
package codingagent

import (
	"context"
	"encoding/json"

	"github.com/yy003x/sn-code/pkg/contract"
)

// Event 是发给交互层的应用事件。单个 run 内有序；不同 run 之间由
// RunStarted/RunFinished 分界。
type Event struct {
	Type EventType

	Text    string                // delta / 提示文本
	Message *contract.Message     // 完整的 assistant/user 消息
	Call    *ToolCallInfo         // 工具调用信息
	Usage   *contract.Usage       // 本轮模型用量
	Finish  contract.FinishReason // assistant_done 的模型停止原因
	Err     *RuntimeFailure       // 错误信息
}

type EventType string

const (
	// EventRunStarted 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EventRunStarted EventType = "run_started"
	// EventTurnStarted 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EventTurnStarted EventType = "turn_started"
	// EventTextDelta 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EventTextDelta EventType = "text_delta"
	// EventAssistantDone 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EventAssistantDone EventType = "assistant_done"
	// EventUserMessage 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EventUserMessage EventType = "user_message"
	// EventToolStarted 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EventToolStarted EventType = "tool_started"
	// EventToolFinished 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EventToolFinished EventType = "tool_finished"
	// EventRunFinished 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EventRunFinished EventType = "run_finished"
	// EventModelError 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EventModelError EventType = "model_error"
	// EventNotice 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	EventNotice EventType = "notice"
)

// ToolCallInfo 是工具调用在事件流中的展示信息。
type ToolCallInfo struct {
	ID         string
	Name       string
	Arguments  json.RawMessage
	DurationMS int64
	Content    string
	IsError    bool
}

// RuntimeFailure 携带 RuntimeError 的可展示摘要。
type RuntimeFailure struct {
	Code    contract.ErrorCode
	Phase   contract.ErrorPhase
	Message string
}

// EventSink 接收应用事件；返回错误会中止当前 run。
type EventSink func(Event) error

// Model 是编排层依赖的最小模型接口；*model.Service 天然满足。
type Model interface {
	// GenerateStream 执行 GenerateStream 对应的包内处理，并遵循调用方提供的输入契约。
	// 参数 request、sink 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
	// 返回 contract.ModelResult、*contract.RuntimeError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	GenerateStream(
		ctx context.Context,
		request contract.GenerateRequest,
		sink contract.EventSink,
	) (contract.ModelResult, *contract.RuntimeError)
}

// ProfileInfo 是 /model 选择与上下文预算需要的 profile 摘要。
type ProfileInfo struct {
	ID            string
	Driver        string
	Model         string
	ContextWindow int64
	ReservedOut   int64
}

// ProfileSource 提供 API Profile 的只读视图，由 composition root 适配
// pkg/profile catalog 注入。
type ProfileSource interface {
	// IDs 执行 IDs 对应的包内处理，并遵循调用方提供的输入契约。
	// 返回 []string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	IDs() []string
	// Get 执行 Get 对应的包内处理，并遵循调用方提供的输入契约。
	// 参数 id 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
	// 返回 ProfileInfo、bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	Get(id string) (ProfileInfo, bool)
}

// GateDecision 是确认门的三种结果。
type GateDecision int

const (
	// GateOnce 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	GateOnce GateDecision = iota
	// GateAlways 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	GateAlways
	// GateDeny 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
	GateDeny
)

// ToolCallInfoForGate 是确认门展示所需的调用摘要。
type ToolCallInfoForGate struct {
	CallID    string
	Name      string
	Effect    contract.Effect
	Risk      contract.Risk
	Arguments json.RawMessage
}

// ToolGate 在写副作用工具执行前请求人工确认；实现方可以阻塞等待用户输入。
type ToolGate interface {
	// Confirm 执行 Confirm 对应的包内处理，并遵循调用方提供的输入契约。
	// 参数 call 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
	// 返回 GateDecision 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	Confirm(ctx context.Context, call ToolCallInfoForGate) GateDecision
}

// AllowAllGate 是无人值守场景的默认门：全部放行（打印模式）。
type AllowAllGate struct{}

// Confirm 执行 Confirm 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 GateDecision 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (AllowAllGate) Confirm(context.Context, ToolCallInfoForGate) GateDecision {
	return GateOnce
}

// DenyWritesGate 拒绝一切写副作用（安全默认的测试门）。
type DenyWritesGate struct{}

// Confirm 执行 Confirm 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 GateDecision 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (DenyWritesGate) Confirm(context.Context, ToolCallInfoForGate) GateDecision {
	return GateDeny
}
