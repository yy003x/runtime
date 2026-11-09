//go:build darwin || linux

package codingtui

import (
	"context"

	"github.com/yy003x/sn-code/internal/application/codingagent"
)

// gateRequest 是确认门请求在 UI 与 run 协程间的握手：UI 回答后经
// response 返回决定（缓冲 1，UI 单次应答不阻塞）。
type gateRequest struct {
	call     codingagent.ToolCallInfoForGate
	response chan codingagent.GateDecision
}

// appGate 把 AgentSession 的确认门桥接到 TUI：Confirm 阻塞 run 协程直到
// 用户应答或上下文取消（取消视为拒绝）。
type appGate struct {
	requests chan<- gateRequest
}

// Confirm 执行 Confirm 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 call 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 codingagent.GateDecision 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (gate appGate) Confirm(ctx context.Context, call codingagent.ToolCallInfoForGate) codingagent.GateDecision {
	request := gateRequest{call: call, response: make(chan codingagent.GateDecision, 1)}
	select {
	case gate.requests <- request:
	case <-ctx.Done():
		return codingagent.GateDeny
	}
	select {
	case decision := <-request.response:
		return decision
	case <-ctx.Done():
		return codingagent.GateDeny
	}
}
