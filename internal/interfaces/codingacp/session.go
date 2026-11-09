package codingacp

import (
	"context"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yy003x/sn-code/internal/application/codingagent"
	"github.com/yy003x/sn-code/pkg/contract"
)

// 会话级界限：prompt 文本、流式分块、工具展示与审批等待都有上限，
// 保证 wire 帧与客户端等待始终有界。
const (
	// maxPromptTextBytes 定义当前流程使用的固定值（容量单位以声明或使用位置为准），取值来源为本实现约定。
	maxPromptTextBytes = 16 << 20
	// maxToolsPerTurn 定义当前流程使用的固定值（数量单位以名称所示对象为准），取值来源为本实现约定。
	maxToolsPerTurn = 4096
	// chunkBytes 定义当前流程使用的固定值（容量单位以声明或使用位置为准），取值来源为本实现约定。
	chunkBytes = 16 << 10
	// maxToolContentBytes 定义当前流程使用的固定值（容量单位以声明或使用位置为准），取值来源为本实现约定。
	maxToolContentBytes = 64 << 10
	// maxTitleBytes 定义当前流程使用的固定值（容量单位以声明或使用位置为准），取值来源为本实现约定。
	maxTitleBytes = 1024
	// maxDetailBytes 定义当前流程使用的固定值（容量单位以声明或使用位置为准），取值来源为本实现约定。
	maxDetailBytes = 1024
	// permissionTimeout 定义当前流程使用的固定值（时间单位以声明类型为准），取值来源为本实现约定。
	permissionTimeout = 120 * time.Second
	// permissionTextBytes 定义当前流程使用的固定值（容量单位以声明或使用位置为准），取值来源为本实现约定。
	permissionTextBytes = 16 << 10
	// terminalWriteTimeout 允许取消后的终态更新仍能写出，但不无限等待。
	terminalWriteTimeout = 10 * time.Second
)

type toolState struct {
	id, title, status string
}

// acpSession 桥接一个 AgentSession 与 ACP 客户端：把内核事件映射为
// session/update 通知，把确认门映射为 session/request_permission。
type acpSession struct {
	server  *Server
	session *codingagent.AgentSession
	gate    *acpGate
	id, cwd string

	promptWG  sync.WaitGroup
	closeOnce sync.Once

	mu        sync.Mutex
	busy      bool
	closed    bool
	runCtx    context.Context
	runCancel context.CancelFunc

	eventMu      sync.Mutex
	active       bool
	turn         uint64
	toolSequence uint64
	eventCtx     context.Context
	eventCancel  context.CancelFunc
	writeErr     error
	finishText   string
	lastFinish   contract.FinishReason
	modelFailure *codingagent.RuntimeFailure
	tools        map[string]*toolState
}

// claim 独占该会话执行一轮 prompt 或 replay；busy 或已关闭时拒绝。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (state *acpSession) claim(ctx context.Context) bool {
	state.mu.Lock()
	if state.busy || state.closed {
		state.mu.Unlock()
		return false
	}
	state.busy = true
	// Add 与 closed 检查共用同一把锁；close 开始 Wait 后不会再有新工作注册。
	state.promptWG.Add(1)
	state.runCtx, state.runCancel = context.WithCancel(ctx)
	runCtx, cancel := state.runCtx, state.runCancel
	state.mu.Unlock()
	state.eventMu.Lock()
	state.active, state.eventCtx, state.eventCancel = true, runCtx, cancel
	state.turn++
	state.writeErr = nil
	state.finishText, state.lastFinish, state.modelFailure = "", "", nil
	state.tools = map[string]*toolState{}
	state.eventMu.Unlock()
	return true
}

// runContext 执行 runContext 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 context.Context 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (state *acpSession) runContext() context.Context {
	state.mu.Lock()
	defer state.mu.Unlock()
	return state.runCtx
}

// cancelRun 由 session/cancel 与连接关闭触发：取消运行上下文即可让内核
// Run 在检查点返回。
func (state *acpSession) cancelRun() {
	state.mu.Lock()
	cancel := state.runCancel
	state.mu.Unlock()
	if cancel != nil {
		cancel()
	}
}

// release 结束一轮执行：未到终态的工具补发 failed，再复位占用。
func (state *acpSession) release() {
	state.eventMu.Lock()
	for _, tool := range state.tools {
		if tool.status != "completed" && tool.status != "failed" {
			state.updateToolLocked(tool, "failed", "Tool did not complete before the turn ended.")
		}
	}
	state.active = false
	state.eventMu.Unlock()
	state.mu.Lock()
	if state.runCancel != nil {
		state.runCancel()
	}
	state.busy, state.runCancel = false, nil
	state.mu.Unlock()
}

// close 关闭会话：取消运行、等待收尾（有界），再关闭会话文件。
func (state *acpSession) close() {
	state.closeOnce.Do(func() {
		state.mu.Lock()
		state.closed = true
		state.mu.Unlock()
		state.cancelRun()
		done := make(chan struct{})
		go func() {
			state.promptWG.Wait()
			close(done)
		}()
		select {
		case <-done:
		case <-time.After(closeTimeout):
			// 顽固的 run 由进程退出兜底；会话文件是 append-only，
			// 极端情况下未刷写的尾部条目丢失可接受。
		}
		if state.session != nil {
			_ = state.session.Close()
		}
	})
}

// boundedText 按 UTF-8 边界截断并标注，保证展示文本有界。
// 参数 value、limit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func boundedText(value string, limit int) string {
	if len(value) <= limit {
		return value
	}
	end := limit
	for end > 0 && !utf8.RuneStart(value[end]) {
		end--
	}
	return value[:end] + "\n[truncated]"
}

// sendLocked 发送 session/update 通知。取消路径仍允许终态更新先于 prompt
// 响应写出，因此使用不受取消影响的派生上下文加超时。
// 参数 update 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (state *acpSession) sendLocked(update any) error {
	if state.writeErr != nil {
		return state.writeErr
	}
	if state.eventCtx == nil {
		return context.Canceled
	}
	ctx, cancel := context.WithTimeout(context.WithoutCancel(state.eventCtx), terminalWriteTimeout)
	defer cancel()
	err := state.server.transport.Notify(ctx, "session/update", sessionUpdateParams{
		SessionId: state.id, Update: update,
	})
	if err != nil {
		state.writeErr = err
		if state.eventCancel != nil {
			state.eventCancel()
		}
	}
	return err
}

// chunkLocked 发送流式文本块；每帧不超过 chunkBytes，拼接文本不变。
// 参数 kind、text 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (state *acpSession) chunkLocked(kind, text string) {
	text = strings.ToValidUTF8(text, "�")
	if text == "" {
		return
	}
	for len(text) > 0 {
		end := min(len(text), chunkBytes)
		for end < len(text) && end > 0 && !utf8.RuneStart(text[end]) {
			end--
		}
		if state.sendLocked(messageChunkUpdate{
			SessionUpdate: kind,
			Content:       textBlock{Type: "text", Text: text[:end]},
		}) != nil {
			return
		}
		text = text[end:]
	}
}

// toolKind 把 sn-code 工具名映射为 ACP 展示类别；未知工具归为 other。
// 参数 name 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func toolKind(name string) string {
	switch name {
	case "read":
		return "read"
	case "grep", "find", "ls":
		return "search"
	case "write", "edit":
		return "edit"
	case "bash":
		return "execute"
	default:
		return "other"
	}
}

// ensureToolLocked 为原始工具调用 ID 建立 wire 工具身份（t{turn}-{seq}）；
// 同一原始 ID 复用首个未终结的 wire 状态。
// 参数 rawID、title 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *toolState、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (state *acpSession) ensureToolLocked(rawID, title string) (*toolState, error) {
	if !state.active {
		return nil, context.Canceled
	}
	if tool := state.tools[rawID]; tool != nil && tool.status != "completed" && tool.status != "failed" {
		return tool, nil
	}
	if len(state.tools) >= maxToolsPerTurn {
		return nil, errToolLimit
	}
	state.toolSequence++
	tool := &toolState{
		id:     "t" + strconv.FormatUint(state.turn, 10) + "-" + strconv.FormatUint(state.toolSequence, 10),
		title:  boundedText(title, maxTitleBytes),
		status: "pending",
	}
	state.tools[rawID] = tool
	err := state.sendLocked(toolCallUpdate{
		SessionUpdate: "tool_call",
		ToolCallId:    tool.id,
		Title:         tool.title,
		Kind:          toolKind(title),
		Status:        "pending",
	})
	return tool, err
}

var errToolLimit = &RequestError{
	Code: codeInternalError, Message: "turn tool update limit reached",
}

// updateToolLocked 推进工具状态；终态更新可携带结果文本（有界）。
// 参数 tool、status、text 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (state *acpSession) updateToolLocked(tool *toolState, status, text string) {
	if tool.status == "completed" || tool.status == "failed" {
		return
	}
	if status != "" {
		tool.status = status
	}
	update := toolCallStatusUpdate{
		SessionUpdate: "tool_call_update",
		ToolCallId:    tool.id,
		Status:        tool.status,
	}
	if text != "" {
		update.Content = []toolCallContent{{
			Type:    "content",
			Content: textBlock{Type: "text", Text: boundedText(text, maxToolContentBytes)},
		}}
	}
	_ = state.sendLocked(update)
}

// handleEvent 是 AgentSession 的事件出口：把内核事件映射为 session/update。
// sn 内核忽略 sink 错误，输出失败经 writeErr 取消运行上下文传播。
// 参数 event 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (state *acpSession) handleEvent(event codingagent.Event) error {
	state.eventMu.Lock()
	defer state.eventMu.Unlock()
	if !state.active {
		return nil
	}
	cancelled := state.eventCtx.Err() != nil
	switch event.Type {
	case codingagent.EventTextDelta:
		if cancelled {
			return nil
		}
		state.chunkLocked("agent_message_chunk", event.Text)
	case codingagent.EventAssistantDone:
		// 文本已按 delta 流出，这里只跟踪停止原因（max_tokens 判定用）。
		state.lastFinish = event.Finish
	case codingagent.EventToolStarted:
		if cancelled || event.Call == nil {
			return nil
		}
		if tool, err := state.ensureToolLocked(event.Call.ID, event.Call.Name); err == nil {
			state.updateToolLocked(tool, "in_progress", "")
		}
	case codingagent.EventToolFinished:
		if event.Call == nil {
			return nil
		}
		tool, _ := state.ensureToolLocked(event.Call.ID, event.Call.Name)
		if tool != nil {
			status := "completed"
			if event.Call.IsError || cancelled {
				status = "failed"
			}
			state.updateToolLocked(tool, status, event.Call.Content)
		}
	case codingagent.EventModelError:
		if event.Err != nil {
			state.modelFailure = event.Err
		}
	case codingagent.EventRunFinished:
		state.finishText = event.Text
	}
	return nil
}

// prompt 同步执行一轮 AgentSession.Run 并按终止形态返回 stopReason；
// 模型失败以 JSON-RPC 错误返回，不伪装为 end_turn。
// 参数 text 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 any、*RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (state *acpSession) prompt(text string) (any, *RequestError) {
	defer state.release()
	ctx := state.runContext()
	if ctx.Err() != nil {
		return promptResult{StopReason: stopCancelled}, nil
	}
	state.session.Run(ctx, text)
	state.eventMu.Lock()
	writeErr := state.writeErr
	finish, failure, lastFinish := state.finishText, state.modelFailure, state.lastFinish
	state.eventMu.Unlock()
	if writeErr != nil {
		return nil, internalFailure("session_update_failed")
	}
	if ctx.Err() != nil {
		return promptResult{StopReason: stopCancelled}, nil
	}
	switch {
	case finish == "error":
		data := map[string]any{"code": "model_request_failed"}
		if failure != nil {
			data["detail"] = boundedText(failure.Message, maxDetailBytes)
		}
		return nil, &RequestError{
			Code: codeInternalError, Message: "model request failed", Data: data,
		}
	case finish == "aborted":
		return promptResult{StopReason: stopCancelled}, nil
	case lastFinish == contract.FinishLength:
		return promptResult{StopReason: stopMaxTokens}, nil
	}
	return promptResult{StopReason: stopEndTurn}, nil
}

// replay 把既有会话分支回放为 update 流：用户与助手消息按块回放，工具
// 调用按 pending→终态重建。压缩摘要不回放，条目按文件原始顺序迭代。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (state *acpSession) replay() error {
	state.eventMu.Lock()
	defer state.eventMu.Unlock()
	entries, err := state.session.Store().Path("")
	if err != nil {
		return err
	}
	// 工具结果消息只携带 call ID，名称从助手消息的工具调用预先收集。
	names := map[string]string{}
	for _, entry := range entries {
		if entry.Message == nil || entry.Message.Role != contract.RoleAssistant {
			continue
		}
		for _, call := range entry.Message.ToolCalls {
			names[call.ID] = call.Name
		}
	}
	for _, entry := range entries {
		if state.eventCtx.Err() != nil {
			return state.eventCtx.Err()
		}
		message := entry.Message
		if message == nil {
			continue
		}
		switch message.Role {
		case contract.RoleUser:
			state.chunkLocked("user_message_chunk", message.Content)
		case contract.RoleAssistant:
			if message.Content != "" {
				state.chunkLocked("agent_message_chunk", message.Content)
			}
			for _, call := range message.ToolCalls {
				_, _ = state.ensureToolLocked(call.ID, call.Name)
			}
		case contract.RoleTool:
			tool := state.tools[message.ToolCallID]
			if tool == nil {
				tool, _ = state.ensureToolLocked(message.ToolCallID, names[message.ToolCallID])
			}
			if tool != nil {
				status := "completed"
				if message.IsError {
					status = "failed"
				}
				state.updateToolLocked(tool, status, message.Content)
			}
		}
	}
	return state.writeErr
}

// acpGate 把内核确认门映射为 session/request_permission：allow_once/
// allow_always/reject_once 三选项；超时、断连、取消或未识别的响应一律
// 不视为批准。
type acpGate struct {
	state *acpSession
}

// Confirm 执行 Confirm 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 call 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 codingagent.GateDecision 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (gate *acpGate) Confirm(ctx context.Context, call codingagent.ToolCallInfoForGate) codingagent.GateDecision {
	state := gate.state
	// EventToolStarted 先于确认门发出，wire 工具已建立；此处只取其身份。
	state.eventMu.Lock()
	tool := state.tools[call.CallID]
	active := state.active
	state.eventMu.Unlock()
	if tool == nil || !active {
		return codingagent.GateDeny
	}
	arguments := ""
	if len(call.Arguments) != 0 {
		arguments = string(call.Arguments)
	}
	pending := permissionToolCall{
		ToolCallId: tool.id,
		Title:      boundedText(call.Name, maxTitleBytes),
		Status:     "pending",
	}
	if arguments != "" {
		pending.Content = []toolCallContent{{
			Type:    "content",
			Content: textBlock{Type: "text", Text: boundedText(arguments, permissionTextBytes)},
		}}
	}
	params := requestPermissionParams{
		SessionId: state.id,
		ToolCall:  pending,
		Options: []permissionOption{
			{OptionId: "option-0", Name: "Allow once", Kind: "allow_once"},
			{OptionId: "option-1", Name: "Allow for session", Kind: "allow_always"},
			{OptionId: "option-2", Name: "Deny", Kind: "reject_once"},
		},
	}
	callCtx, cancel := context.WithTimeout(ctx, permissionTimeout)
	defer cancel()
	var answer requestPermissionResult
	if err := state.server.transport.Call(callCtx, "session/request_permission", params, &answer); err != nil {
		return codingagent.GateDeny
	}
	if callCtx.Err() != nil {
		return codingagent.GateDeny
	}
	if answer.Outcome.Kind != "selected" {
		return codingagent.GateDeny
	}
	switch answer.Outcome.OptionId {
	case "option-0":
		return codingagent.GateOnce
	case "option-1":
		return codingagent.GateAlways
	}
	return codingagent.GateDeny
}
