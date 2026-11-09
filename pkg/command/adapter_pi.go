package command

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yy003x/sn-code/pkg/contract"
)

// piAdapter 登记 basename `pi`（@earendil-works/pi-coding-agent）。headless
// 入口按 P0-3 实测选定 `-p --mode json --no-session`：canonical 与 streaming
// 共用同一 selector，one-shot JSONL 事件流与 managed exec「每 Run 一个进程」
// 模型吻合。
type piAdapter struct{}

// Name 执行 Name 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (piAdapter) Name() string { return "pi" }

// Build 执行 Build 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 request 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 Invocation、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (piAdapter) Build(request BuildRequest) (Invocation, error) {
	if request.Mode != ModeInteractive && request.Mode != ModeExec {
		return Invocation{}, fmt.Errorf("unsupported command mode %q", request.Mode)
	}
	if request.OutputProtocol != OutputNative &&
		!structuredProtocols(request.OutputProtocol) {
		return Invocation{}, fmt.Errorf("unsupported output protocol %q", request.OutputProtocol)
	}
	if request.Mode == ModeInteractive &&
		structuredProtocols(request.OutputProtocol) {
		return Invocation{}, fmt.Errorf("interactive mode does not support canonical output")
	}
	prepared, err := prepareEffectiveConfig(request)
	if err != nil {
		return Invocation{}, err
	}
	parsed, err := piOptions.parse(prepared.args)
	if err != nil {
		return Invocation{}, err
	}
	plan, err := classifyOptions(parsed, request.Mode, request.OutputProtocol)
	if err != nil {
		return Invocation{}, err
	}
	model, effort, _, err := effectiveTyped(request)
	if err != nil {
		return Invocation{}, err
	}
	modelOption, err := oneSelector("model", plan.model, model != "")
	if err != nil {
		return Invocation{}, err
	}
	effortOption, err := oneSelector("effort", plan.effort, effort != "")
	if err != nil {
		return Invocation{}, err
	}
	// pi 无 sandbox 机制；read_only 映射到只读工具集（P0 实测内置清单），
	// workspace_write 无等价档位，显式拒绝。
	permission, err := permissionArgv("pi", request, parsed, map[PermissionMode][]string{
		PermissionReadOnly: {"--tools", "read,grep,find,ls"},
	}, "--tools", "--no-tools", "--no-builtin-tools", "--exclude-tools")
	if err != nil {
		return Invocation{}, err
	}
	argv := []string{request.Profile.Command}
	argv = append(argv, plan.common...)
	argv = append(argv, permission...)
	if model != "" {
		argv = append(argv, "--model", model)
	} else if modelOption != nil {
		argv = append(argv, modelOption.tokens...)
	}
	// Runtime Effort 枚举（low..max）与 pi thinking level 词汇直接重合。
	if effort != "" {
		argv = append(argv, "--thinking", string(effort))
	} else if effortOption != nil {
		argv = append(argv, effortOption.tokens...)
	}
	if request.Resume != nil {
		if request.Mode != ModeInteractive {
			return Invocation{}, fmt.Errorf("resume is only supported in interactive mode")
		}
		if *request.Resume == "" {
			argv = append(argv, "--continue")
		} else {
			argv = append(argv, "--session", *request.Resume)
		}
	}
	if request.Mode == ModeExec {
		argv = append(argv, plan.exec...)
		if structuredProtocols(request.OutputProtocol) {
			argv = append(argv, "-p", "--mode", "json", "--no-session")
		} else {
			for _, item := range plan.stateless {
				argv = append(argv, item.tokens...)
			}
			if len(plan.output) > 1 {
				return Invocation{}, fmt.Errorf("--mode is configured multiple times")
			}
			if len(plan.output) == 1 {
				argv = append(argv, plan.output[0].tokens...)
			}
			for _, item := range plan.input {
				argv = append(argv, item.tokens...)
			}
		}
	}
	argv, err = appendPrompt(argv, request)
	if err != nil {
		return Invocation{}, err
	}
	return finishInvocation(request, prepared, argv)
}

// Decode 逐行回放 stdout 到 piStreamSession，终态校验与 streaming 共用同一
// 纪律：恰好一个 agent_settled、terminal 后无消息/工具记录、存在 assistant。
// 参数 stdout 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 CanonicalOutput、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (piAdapter) Decode(stdout []byte) (CanonicalOutput, error) {
	session := &piStreamSession{}
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 64<<10), maxCanonicalRecordBytes+1)
	for scanner.Scan() {
		line := bytes.TrimSpace(scanner.Bytes())
		if len(line) == 0 {
			continue
		}
		if err := session.DecodeLine(line, nil); err != nil {
			return CanonicalOutput{}, err
		}
	}
	if err := scanner.Err(); err != nil {
		return CanonicalOutput{}, fmt.Errorf(
			"decode Pi JSONL: record exceeds 1048576 bytes: %w", err,
		)
	}
	return session.Finish()
}

// NewStreamSession 执行 NewStreamSession 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 StreamSession 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (piAdapter) NewStreamSession() StreamSession {
	return &piStreamSession{}
}

// piStreamSession 增量解码 `--mode json` JSONL。token 级 text_delta 投递
// executor.message.delta；assistant message_end 为权威消息；tool_execution_
// start/end 投递工具事件；thinking/toolcall 增量与原生管线记录不投递。
type piStreamSession struct {
	settledCount  int
	terminalSeen  bool
	assistant     string
	assistantSeen bool
	streamErr     error
}

// DecodeLine 执行 DecodeLine 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 line、emit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *piStreamSession) DecodeLine(
	line []byte,
	emit func(contract.Event) error,
) error {
	if session.streamErr != nil {
		return session.streamErr
	}
	var record struct {
		Type    string `json:"type"`
		Aborted bool   `json:"aborted"`
		Event   struct {
			Type  string `json:"type"`
			Delta string `json:"delta"`
		} `json:"assistantMessageEvent"`
		Message struct {
			Role    string          `json:"role"`
			Content json.RawMessage `json:"content"`
		} `json:"message"`
		ToolCallID string          `json:"toolCallId"`
		ToolName   string          `json:"toolName"`
		Args       json.RawMessage `json:"args"`
		Result     json.RawMessage `json:"result"`
		IsError    bool            `json:"isError"`
	}
	if err := json.Unmarshal(line, &record); err != nil {
		session.streamErr = fmt.Errorf("decode Pi JSONL: %w", err)
		return session.streamErr
	}
	switch record.Type {
	case "session", "entry_appended", "agent_start", "agent_end",
		"turn_start", "turn_end", "message_start", "tool_execution_update":
		// 原生生命周期/管线与部分结果不投递：lifecycle 由 Runtime run.* 投影，
		// 终态以 assistant message_end 与 agent_settled 为权威。
		return nil
	case "message_update":
		if session.terminalSeen {
			session.streamErr = fmt.Errorf("Pi message updated after terminal")
			return session.streamErr
		}
		if record.Event.Type != "text_delta" || record.Event.Delta == "" {
			return nil
		}
		return emitEvent(newExecutorEvent(
			contract.EventExecutorMessageDelta, "text_delta", "", "",
			record.Event.Delta, false, nil,
		), emit)
	case "message_end":
		if session.terminalSeen {
			session.streamErr = fmt.Errorf("Pi message completed after terminal")
			return session.streamErr
		}
		if record.Message.Role != "assistant" {
			return nil
		}
		text := piMessageText(record.Message.Content)
		if len(text) > maxCanonicalRecordBytes {
			session.streamErr = fmt.Errorf(
				"Pi assistant result exceeds 1048576 bytes",
			)
			return session.streamErr
		}
		session.assistant = text
		session.assistantSeen = true
		if text == "" {
			return nil
		}
		return emitEvent(newExecutorEvent(
			contract.EventExecutorMessage, "message_end", "", "",
			text, false, nil,
		), emit)
	case "tool_execution_start":
		if session.terminalSeen {
			session.streamErr = fmt.Errorf("Pi tool execution started after terminal")
			return session.streamErr
		}
		return emitEvent(newExecutorEvent(
			contract.EventExecutorToolStarted, "tool_execution_start",
			record.ToolCallID, record.ToolName, "", false, record.Args,
		), emit)
	case "tool_execution_end":
		if session.terminalSeen {
			session.streamErr = fmt.Errorf("Pi tool execution completed after terminal")
			return session.streamErr
		}
		text, data := piToolResult(record.Result)
		return emitEvent(newExecutorEvent(
			contract.EventExecutorToolCompleted, "tool_execution_end",
			record.ToolCallID, record.ToolName, text, record.IsError, data,
		), emit)
	case "agent_settled":
		session.settledCount++
		session.terminalSeen = true
		if record.Aborted {
			session.streamErr = fmt.Errorf("Pi streaming output reported aborted")
			return session.streamErr
		}
		return nil
	default:
		return emitEvent(newExecutorEvent(
			contract.EventExecutorNative, record.Type, "", "", "", false,
			json.RawMessage(line),
		), emit)
	}
}

// Finish 执行 Finish 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 CanonicalOutput、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *piStreamSession) Finish() (CanonicalOutput, error) {
	if session.streamErr != nil {
		return CanonicalOutput{}, session.streamErr
	}
	if session.settledCount != 1 {
		return CanonicalOutput{}, fmt.Errorf(
			"Pi streaming output requires exactly one agent_settled, got %d",
			session.settledCount,
		)
	}
	if !session.assistantSeen {
		return CanonicalOutput{}, fmt.Errorf("Pi streaming output has no assistant result")
	}
	return CanonicalOutput{Assistant: session.assistant}, nil
}

// piMessageText 提取 message.content 文本：content 为 string 时原样返回，
// 为 block 数组时拼接全部 text block（thinking/toolCall block 不计入）。
func piMessageText(content json.RawMessage) string {
	if len(content) == 0 {
		return ""
	}
	if content[0] == '"' {
		var text string
		if err := json.Unmarshal(content, &text); err != nil {
			return ""
		}
		return text
	}
	var blocks []struct {
		Type string `json:"type"`
		Text string `json:"text"`
	}
	if err := json.Unmarshal(content, &blocks); err != nil {
		return ""
	}
	var parts []string
	for _, block := range blocks {
		if block.Type == "text" {
			parts = append(parts, block.Text)
		}
	}
	return strings.Join(parts, "\n")
}

// piToolResult 拆分 tool_execution_end 的 result：content 文本聚合为 Text，
// structuredContent 保留为 Data 摘录。
// 参数 result 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string、json.RawMessage 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func piToolResult(result json.RawMessage) (string, json.RawMessage) {
	if len(result) == 0 {
		return "", nil
	}
	var parsed struct {
		Content           json.RawMessage `json:"content"`
		StructuredContent json.RawMessage `json:"structuredContent"`
	}
	if err := json.Unmarshal(result, &parsed); err != nil {
		return "", boundedExecutorData(result)
	}
	if len(parsed.StructuredContent) > 0 {
		return piMessageText(parsed.Content), parsed.StructuredContent
	}
	return piMessageText(parsed.Content), nil
}

var piOptions = newOptionParser([]optionSpec{
	{name: "--provider", aliases: []string{"--provider"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--model", aliases: []string{"--model"}, arity: arityValue, scope: scopeCommon, kind: kindModel},
	{name: "--system-prompt", aliases: []string{"--system-prompt"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--append-system-prompt", aliases: []string{"--append-system-prompt"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--thinking", aliases: []string{"--thinking"}, arity: arityValue, scope: scopeCommon, kind: kindEffort},
	{name: "--models", aliases: []string{"--models"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--name", aliases: []string{"-n", "--name"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-tools", aliases: []string{"-nt", "--no-tools"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-builtin-tools", aliases: []string{"-nbt", "--no-builtin-tools"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--tools", aliases: []string{"-t", "--tools"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--exclude-tools", aliases: []string{"-xt", "--exclude-tools"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--extension", aliases: []string{"-e", "--extension"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-extensions", aliases: []string{"-ne", "--no-extensions"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-mcp", aliases: []string{"--no-mcp"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--skill", aliases: []string{"--skill"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-skills", aliases: []string{"-ns", "--no-skills"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--prompt-template", aliases: []string{"--prompt-template"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-prompt-templates", aliases: []string{"-np", "--no-prompt-templates"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--theme", aliases: []string{"--theme"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-themes", aliases: []string{"--no-themes"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--use-theme", aliases: []string{"--use-theme"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-context-files", aliases: []string{"-nc", "--no-context-files"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--tui-mode", aliases: []string{"--tui-mode"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--approve", aliases: []string{"-a", "--approve"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-approve", aliases: []string{"-na", "--no-approve"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--offline", aliases: []string{"--offline"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	// 会话状态与一次性工具改变进程行为，structured 下拒绝（selector 拥有
	// --mode/--no-session；Runtime 不从 Session ID 推断原生 resume identity）。
	{name: "--continue", aliases: []string{"-c", "--continue"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--resume", aliases: []string{"-r", "--resume"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--session", aliases: []string{"--session"}, arity: arityValue, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--session-id", aliases: []string{"--session-id"}, arity: arityValue, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--session-dir", aliases: []string{"--session-dir"}, arity: arityValue, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--fork", aliases: []string{"--fork"}, arity: arityValue, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--export", aliases: []string{"--export"}, arity: arityValue, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--list-models", aliases: []string{"--list-models"}, arity: arityOptional, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--verbose", aliases: []string{"--verbose"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--print", aliases: []string{"-p", "--print"}, arity: arityFlag, scope: scopeExec, kind: kindMode},
	{name: "--mode", aliases: []string{"--mode"}, arity: arityValue, scope: scopeExec, kind: kindOutput},
	{name: "--no-session", aliases: []string{"--no-session"}, arity: arityFlag, scope: scopeExec, kind: kindStateless},
})
