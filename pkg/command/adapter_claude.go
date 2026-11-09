package command

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yy003x/sn-code/pkg/contract"
)

type claudeAdapter struct{}

func (claudeAdapter) Name() string { return "claude" }

func (claudeAdapter) Build(request BuildRequest) (Invocation, error) {
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
	parsed, err := claudeOptions.parse(prepared.args)
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
	permission, err := permissionArgv("claude", request, parsed, map[PermissionMode][]string{
		PermissionReadOnly:       {"--permission-mode", "plan"},
		PermissionWorkspaceWrite: {"--permission-mode", "acceptEdits"},
	}, "--permission-mode", "--dangerously-skip-permissions")
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
	if effort != "" {
		argv = append(argv, "--effort", string(effort))
	} else if effortOption != nil {
		argv = append(argv, effortOption.tokens...)
	}
	if request.Resume != nil {
		if request.Mode != ModeInteractive {
			return Invocation{}, fmt.Errorf("resume is only supported in interactive mode")
		}
		argv = append(argv, "--resume")
		if *request.Resume != "" {
			argv = append(argv, *request.Resume)
		}
	}
	if request.Mode == ModeExec {
		argv = append(argv, plan.exec...)
		switch request.OutputProtocol {
		case OutputCanonical:
			argv = append(
				argv,
				"--no-session-persistence",
				"--output-format", "json",
			)
		case OutputStreaming:
			// stream-json 需要 --verbose 才被 CLI 接受（P0 实测）；selector
			// 内部注入，不放宽用户配置 --verbose 的拒绝规则。
			argv = append(
				argv,
				"--no-session-persistence",
				"--output-format", "stream-json",
				"--verbose",
				"--include-partial-messages",
			)
		default:
			for _, item := range plan.stateless {
				argv = append(argv, item.tokens...)
			}
			if len(plan.output) > 1 {
				return Invocation{}, fmt.Errorf("--output-format is configured multiple times")
			}
			if len(plan.output) == 1 {
				argv = append(argv, plan.output[0].tokens...)
			}
			for _, item := range plan.input {
				argv = append(argv, item.tokens...)
			}
		}
		argv = append(argv, "-p")
	}
	argv, err = appendPrompt(argv, request)
	if err != nil {
		return Invocation{}, err
	}
	return finishInvocation(request, prepared, argv)
}

func (claudeAdapter) Decode(stdout []byte) (CanonicalOutput, error) {
	if len(stdout) > maxCanonicalRecordBytes {
		return CanonicalOutput{}, fmt.Errorf("Claude canonical result exceeds 1048576 bytes")
	}
	var result struct {
		Type    string  `json:"type"`
		Subtype string  `json:"subtype"`
		IsError *bool   `json:"is_error"`
		Result  *string `json:"result"`
	}
	if err := decodeSingleJSON(stdout, &result); err != nil {
		return CanonicalOutput{}, err
	}
	if result.Type != "result" || result.Subtype != "success" ||
		result.IsError == nil || *result.IsError ||
		result.Result == nil {
		return CanonicalOutput{}, fmt.Errorf("Claude canonical output is not a successful result")
	}
	if len(*result.Result) > maxCanonicalRecordBytes {
		return CanonicalOutput{}, fmt.Errorf("Claude assistant result exceeds 1048576 bytes")
	}
	return CanonicalOutput{Assistant: *result.Result}, nil
}

// NewStreamSession 执行 NewStreamSession 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 StreamSession 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (claudeAdapter) NewStreamSession() StreamSession {
	return &claudeStreamSession{}
}

// claudeStreamSession 增量解码 `-p --output-format stream-json` NDJSON。终态
// 校验与 canonical Decode 同一纪律：唯一成功 result 记录；token 级 text
// delta 投递 executor.message.delta，thinking/SSE plumbing 不投递。
type claudeStreamSession struct {
	messageID    string
	resultCount  int
	resultOK     bool
	assistant    string
	assistantSet bool
	streamErr    error
}

// DecodeLine 执行 DecodeLine 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 line、emit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *claudeStreamSession) DecodeLine(
	line []byte,
	emit func(contract.Event) error,
) error {
	if session.streamErr != nil {
		return session.streamErr
	}
	var record struct {
		Type    string          `json:"type"`
		Event   json.RawMessage `json:"event"`
		Message json.RawMessage `json:"message"`
	}
	if err := json.Unmarshal(line, &record); err != nil {
		session.streamErr = fmt.Errorf("decode Claude stream JSONL: %w", err)
		return session.streamErr
	}
	switch record.Type {
	case "system":
		return nil
	case "stream_event":
		return session.decodeStreamEvent(record.Event, emit)
	case "assistant":
		return session.decodeAssistant(record.Message, emit)
	case "user":
		return session.decodeUser(record.Message, emit)
	case "result":
		session.resultCount++
		var result struct {
			Subtype string  `json:"subtype"`
			IsError *bool   `json:"is_error"`
			Result  *string `json:"result"`
		}
		if err := json.Unmarshal(line, &result); err != nil {
			session.streamErr = fmt.Errorf("decode Claude result record: %w", err)
			return session.streamErr
		}
		session.resultOK = result.Subtype == "success" &&
			result.IsError != nil && !*result.IsError && result.Result != nil
		if session.resultOK {
			session.assistant = *result.Result
			session.assistantSet = true
		}
		return nil
	default:
		return emitEvent(newExecutorEvent(
			contract.EventExecutorNative, record.Type, "", "", "", false,
			json.RawMessage(line),
		), emit)
	}
}

// decodeStreamEvent 执行 decodeStreamEvent 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 rawEvent、emit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *claudeStreamSession) decodeStreamEvent(
	rawEvent json.RawMessage,
	emit func(contract.Event) error,
) error {
	var event struct {
		Type    string `json:"type"`
		Message struct {
			ID string `json:"id"`
		} `json:"message"`
		Delta struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"delta"`
	}
	if err := json.Unmarshal(rawEvent, &event); err != nil {
		session.streamErr = fmt.Errorf("decode Claude stream event: %w", err)
		return session.streamErr
	}
	switch event.Type {
	case "message_start":
		session.messageID = event.Message.ID
		return nil
	case "content_block_delta":
		if event.Delta.Type != "text_delta" || event.Delta.Text == "" {
			return nil
		}
		return emitEvent(newExecutorEvent(
			contract.EventExecutorMessageDelta, "content_block_delta",
			session.messageID, "", event.Delta.Text, false, nil,
		), emit)
	default:
		return nil
	}
}

// decodeAssistant 执行 decodeAssistant 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 rawMessage、emit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *claudeStreamSession) decodeAssistant(
	rawMessage json.RawMessage,
	emit func(contract.Event) error,
) error {
	var message struct {
		ID      string `json:"id"`
		Content []struct {
			Type  string          `json:"type"`
			Text  string          `json:"text"`
			ID    string          `json:"id"`
			Name  string          `json:"name"`
			Input json.RawMessage `json:"input"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rawMessage, &message); err != nil {
		session.streamErr = fmt.Errorf("decode Claude assistant message: %w", err)
		return session.streamErr
	}
	var textParts []string
	for _, block := range message.Content {
		switch block.Type {
		case "text":
			textParts = append(textParts, block.Text)
		case "tool_use":
			if err := emitEvent(newExecutorEvent(
				contract.EventExecutorToolStarted, "tool_use", block.ID,
				block.Name, "", false, block.Input,
			), emit); err != nil {
				return err
			}
		}
	}
	if len(textParts) == 0 {
		return nil
	}
	return emitEvent(newExecutorEvent(
		contract.EventExecutorMessage, "assistant", message.ID, "",
		strings.Join(textParts, "\n"), false, nil,
	), emit)
}

// decodeUser 执行 decodeUser 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 rawMessage、emit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *claudeStreamSession) decodeUser(
	rawMessage json.RawMessage,
	emit func(contract.Event) error,
) error {
	var message struct {
		Content []struct {
			Type      string          `json:"type"`
			ToolUseID string          `json:"tool_use_id"`
			Content   json.RawMessage `json:"content"`
			IsError   bool            `json:"is_error"`
		} `json:"content"`
	}
	if err := json.Unmarshal(rawMessage, &message); err != nil {
		session.streamErr = fmt.Errorf("decode Claude tool result: %w", err)
		return session.streamErr
	}
	for _, block := range message.Content {
		if block.Type != "tool_result" {
			continue
		}
		text := ""
		var data any
		if len(block.Content) > 0 && block.Content[0] == '"' {
			_ = json.Unmarshal(block.Content, &text)
		} else {
			data = block.Content
		}
		if err := emitEvent(newExecutorEvent(
			contract.EventExecutorToolCompleted, "tool_result",
			block.ToolUseID, "", text, block.IsError, data,
		), emit); err != nil {
			return err
		}
	}
	return nil
}

// Finish 执行 Finish 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 CanonicalOutput、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *claudeStreamSession) Finish() (CanonicalOutput, error) {
	if session.streamErr != nil {
		return CanonicalOutput{}, session.streamErr
	}
	if session.resultCount != 1 {
		return CanonicalOutput{}, fmt.Errorf(
			"Claude streaming output requires exactly one result record, got %d",
			session.resultCount,
		)
	}
	if !session.resultOK || !session.assistantSet {
		return CanonicalOutput{}, fmt.Errorf(
			"Claude streaming output is not a successful result",
		)
	}
	if len(session.assistant) > maxCanonicalRecordBytes {
		return CanonicalOutput{}, fmt.Errorf("Claude assistant result exceeds 1048576 bytes")
	}
	return CanonicalOutput{Assistant: session.assistant}, nil
}

var claudeOptions = newOptionParser([]optionSpec{
	{name: "--add-dir", aliases: []string{"--add-dir"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--agent", aliases: []string{"--agent"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--agents", aliases: []string{"--agents"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--allow-dangerously-skip-permissions", aliases: []string{"--allow-dangerously-skip-permissions"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--allowed-tools", aliases: []string{"--allowedTools", "--allowed-tools"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--append-system-prompt", aliases: []string{"--append-system-prompt"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--ax-screen-reader", aliases: []string{"--ax-screen-reader"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--bare", aliases: []string{"--bare"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--betas", aliases: []string{"--betas"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--brief", aliases: []string{"--brief"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--chrome", aliases: []string{"--chrome"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--dangerously-skip-permissions", aliases: []string{"--dangerously-skip-permissions"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--disable-slash-commands", aliases: []string{"--disable-slash-commands"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--disallowed-tools", aliases: []string{"--disallowedTools", "--disallowed-tools"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--effort", aliases: []string{"--effort"}, arity: arityValue, scope: scopeCommon, kind: kindEffort},
	{name: "--exclude-dynamic-system-prompt-sections", aliases: []string{"--exclude-dynamic-system-prompt-sections"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--file", aliases: []string{"--file"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--mcp-config", aliases: []string{"--mcp-config"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--model", aliases: []string{"--model"}, arity: arityValue, scope: scopeCommon, kind: kindModel},
	{name: "--name", aliases: []string{"-n", "--name"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-chrome", aliases: []string{"--no-chrome"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--permission-mode", aliases: []string{"--permission-mode"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--plugin-dir", aliases: []string{"--plugin-dir"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--plugin-url", aliases: []string{"--plugin-url"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--safe-mode", aliases: []string{"--safe-mode"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--setting-sources", aliases: []string{"--setting-sources"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--settings", aliases: []string{"--settings"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--strict-mcp-config", aliases: []string{"--strict-mcp-config"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--system-prompt", aliases: []string{"--system-prompt"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--tools", aliases: []string{"--tools"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--verbose", aliases: []string{"--verbose"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--print", aliases: []string{"-p", "--print"}, arity: arityFlag, scope: scopeExec, kind: kindMode},
	{name: "--fallback-model", aliases: []string{"--fallback-model"}, arity: arityValue, scope: scopeExec, kind: kindGeneric},
	{name: "--max-budget-usd", aliases: []string{"--max-budget-usd"}, arity: arityValue, scope: scopeExec, kind: kindGeneric},
	{name: "--no-session-persistence", aliases: []string{"--no-session-persistence"}, arity: arityFlag, scope: scopeExec, kind: kindStateless},
	{name: "--output-format", aliases: []string{"--output-format"}, arity: arityValue, scope: scopeExec, kind: kindOutput},
	{name: "--input-format", aliases: []string{"--input-format"}, arity: arityValue, scope: scopeExec, kind: kindInput, forbidInCanon: true},
	{name: "--continue", aliases: []string{"-c", "--continue"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--resume", aliases: []string{"-r", "--resume"}, arity: arityOptional, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--session-id", aliases: []string{"--session-id"}, arity: arityValue, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--fork-session", aliases: []string{"--fork-session"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--background", aliases: []string{"--bg", "--background"}, arity: arityFlag, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--worktree", aliases: []string{"-w", "--worktree"}, arity: arityOptional, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--tmux", aliases: []string{"--tmux"}, arity: arityOptional, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--debug-file", aliases: []string{"--debug-file"}, arity: arityValue, scope: scopeCommon, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--json-schema", aliases: []string{"--json-schema"}, arity: arityValue, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--forward-subagent-text", aliases: []string{"--forward-subagent-text"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--include-hook-events", aliases: []string{"--include-hook-events"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--include-partial-messages", aliases: []string{"--include-partial-messages"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--replay-user-messages", aliases: []string{"--replay-user-messages"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--prompt-suggestions", aliases: []string{"--prompt-suggestions"}, arity: arityOptional, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
})
