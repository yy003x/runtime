package command

import (
	"bufio"
	"bytes"
	"encoding/json"
	"fmt"
	"strings"

	"github.com/yy003x/sn-code/pkg/contract"
)

type codexAdapter struct{}

func (codexAdapter) Name() string { return "codex" }

func (codexAdapter) Build(request BuildRequest) (Invocation, error) {
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
	parsed, err := codexOptions.parse(prepared.args)
	if err != nil {
		return Invocation{}, err
	}
	parsed = normalizeCodexConfigSelectors(parsed)
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
	permission, err := permissionArgv("codex", request, parsed, map[PermissionMode][]string{
		PermissionReadOnly:       {"--sandbox", "read-only"},
		PermissionWorkspaceWrite: {"--sandbox", "workspace-write"},
	}, "--sandbox", "--dangerously-bypass-approvals-and-sandbox")
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
		argv = append(argv, "-c", "model_reasoning_effort="+string(effort))
	} else if effortOption != nil {
		argv = append(argv, effortOption.tokens...)
	}
	if request.Resume != nil {
		if request.Mode != ModeInteractive {
			return Invocation{}, fmt.Errorf("resume is only supported in interactive mode")
		}
		argv = append(argv, "resume")
		if *request.Resume != "" {
			argv = append(argv, *request.Resume)
		}
	}
	if request.Mode == ModeExec {
		argv = append(argv, "exec")
		argv = append(argv, plan.exec...)
		if structuredProtocols(request.OutputProtocol) {
			argv = append(argv, "--ephemeral", "--json")
		} else {
			for _, item := range plan.stateless {
				argv = append(argv, item.tokens...)
			}
			if len(plan.output) > 1 {
				return Invocation{}, fmt.Errorf("--json is configured multiple times")
			}
			if len(plan.output) == 1 {
				argv = append(argv, plan.output[0].tokens...)
			}
		}
	}
	argv, err = appendPrompt(argv, request)
	if err != nil {
		return Invocation{}, err
	}
	return finishInvocation(request, prepared, argv)
}

func (codexAdapter) Decode(stdout []byte) (CanonicalOutput, error) {
	scanner := bufio.NewScanner(bytes.NewReader(stdout))
	scanner.Buffer(make([]byte, 64<<10), maxCanonicalRecordBytes+1)
	terminalCount := 0
	terminalSeen := false
	assistant := ""
	assistantSeen := false
	for scanner.Scan() {
		line := scanner.Bytes()
		if len(bytes.TrimSpace(line)) == 0 {
			continue
		}
		var event struct {
			Type    string          `json:"type"`
			Message json.RawMessage `json:"message"`
			Item    struct {
				Type string `json:"type"`
				Text string `json:"text"`
			} `json:"item"`
		}
		if err := json.Unmarshal(line, &event); err != nil {
			return CanonicalOutput{}, fmt.Errorf("decode Codex JSONL: %w", err)
		}
		switch event.Type {
		case "error", "turn.failed":
			return CanonicalOutput{}, fmt.Errorf("Codex canonical output reported %s", event.Type)
		case "turn.completed":
			terminalCount++
			terminalSeen = true
		case "item.completed":
			if terminalSeen {
				return CanonicalOutput{}, fmt.Errorf("Codex item completed after terminal")
			}
			if event.Item.Type == "agent_message" {
				if len(event.Item.Text) > maxCanonicalRecordBytes {
					return CanonicalOutput{}, fmt.Errorf("Codex assistant result exceeds 1048576 bytes")
				}
				assistant = event.Item.Text
				assistantSeen = true
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return CanonicalOutput{}, fmt.Errorf("decode Codex JSONL: record exceeds 1048576 bytes: %w", err)
	}
	if terminalCount != 1 {
		return CanonicalOutput{}, fmt.Errorf(
			"Codex canonical output requires exactly one turn.completed, got %d",
			terminalCount,
		)
	}
	if !assistantSeen {
		return CanonicalOutput{}, fmt.Errorf("Codex canonical output has no assistant result")
	}
	return CanonicalOutput{Assistant: assistant}, nil
}

// NewStreamSession 执行 NewStreamSession 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 StreamSession 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (codexAdapter) NewStreamSession() StreamSession {
	return &codexStreamSession{}
}

// codexStreamSession 增量解码 `exec --ephemeral --json` NDJSON。终态校验与
// canonical Decode 同一纪律：唯一 turn.completed、terminal 后无 item、
// 存在 agent_message assistant。
type codexStreamSession struct {
	terminalCount int
	terminalSeen  bool
	assistant     string
	assistantSeen bool
	streamErr     error
}

// DecodeLine 执行 DecodeLine 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 line、emit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *codexStreamSession) DecodeLine(
	line []byte,
	emit func(contract.Event) error,
) error {
	if session.streamErr != nil {
		return session.streamErr
	}
	var record struct {
		Type string          `json:"type"`
		Item json.RawMessage `json:"item"`
	}
	if err := json.Unmarshal(line, &record); err != nil {
		session.streamErr = fmt.Errorf("decode Codex JSONL: %w", err)
		return session.streamErr
	}
	var item struct {
		ID               string `json:"id"`
		Type             string `json:"type"`
		Text             string `json:"text"`
		Command          string `json:"command"`
		AggregatedOutput string `json:"aggregated_output"`
		ExitCode         *int   `json:"exit_code"`
		Status           string `json:"status"`
	}
	if len(record.Item) > 0 {
		if err := json.Unmarshal(record.Item, &item); err != nil {
			session.streamErr = fmt.Errorf("decode Codex item: %w", err)
			return session.streamErr
		}
	}
	switch record.Type {
	case "error", "turn.failed":
		session.streamErr = fmt.Errorf(
			"Codex streaming output reported %s", record.Type,
		)
		return session.streamErr
	case "turn.completed":
		session.terminalCount++
		session.terminalSeen = true
		return nil
	case "turn.started", "thread.started":
		// lifecycle 已由 Runtime run.* 事件投影，原生 turn/thread 边界不重复投递。
		return nil
	case "item.started":
		if item.Type != "command_execution" {
			return nil
		}
		return emitEvent(newExecutorEvent(
			contract.EventExecutorToolStarted, "item.started", item.ID,
			"command_execution", "", false,
			map[string]any{"command": item.Command},
		), emit)
	case "item.completed":
		if session.terminalSeen {
			session.streamErr = fmt.Errorf("Codex item completed after terminal")
			return session.streamErr
		}
		switch item.Type {
		case "agent_message":
			if len(item.Text) > maxCanonicalRecordBytes {
				session.streamErr = fmt.Errorf(
					"Codex assistant result exceeds 1048576 bytes",
				)
				return session.streamErr
			}
			session.assistant = item.Text
			session.assistantSeen = true
			if item.Text == "" {
				return nil
			}
			return emitEvent(newExecutorEvent(
				contract.EventExecutorMessage, "item.completed", item.ID,
				"", item.Text, false, nil,
			), emit)
		case "command_execution":
			failed := item.ExitCode != nil && *item.ExitCode != 0
			return emitEvent(newExecutorEvent(
				contract.EventExecutorToolCompleted, "item.completed", item.ID,
				"command_execution", item.AggregatedOutput, failed,
				map[string]any{
					"command": item.Command, "exit_code": item.ExitCode,
					"status": item.Status,
				},
			), emit)
		default:
			return emitEvent(newExecutorEvent(
				contract.EventExecutorNative, "item.completed", item.ID,
				"", "", false, json.RawMessage(record.Item),
			), emit)
		}
	default:
		return emitEvent(newExecutorEvent(
			contract.EventExecutorNative, record.Type, "", "", "", false,
			json.RawMessage(line),
		), emit)
	}
}

// Finish 执行 Finish 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 CanonicalOutput、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *codexStreamSession) Finish() (CanonicalOutput, error) {
	if session.streamErr != nil {
		return CanonicalOutput{}, session.streamErr
	}
	if session.terminalCount != 1 {
		return CanonicalOutput{}, fmt.Errorf(
			"Codex streaming output requires exactly one turn.completed, got %d",
			session.terminalCount,
		)
	}
	if !session.assistantSeen {
		return CanonicalOutput{}, fmt.Errorf("Codex streaming output has no assistant result")
	}
	return CanonicalOutput{Assistant: session.assistant}, nil
}

var codexOptions = newOptionParser([]optionSpec{
	{name: "--config", aliases: []string{"-c", "--config"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--enable", aliases: []string{"--enable"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--disable", aliases: []string{"--disable"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--strict-config", aliases: []string{"--strict-config"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--image", aliases: []string{"-i", "--image"}, arity: arityVariadic, minValues: 1, scope: scopeCommon, kind: kindGeneric},
	{name: "--model", aliases: []string{"-m", "--model"}, arity: arityValue, scope: scopeCommon, kind: kindModel},
	{name: "--oss", aliases: []string{"--oss"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--local-provider", aliases: []string{"--local-provider"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--profile", aliases: []string{"-p", "--profile"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--sandbox", aliases: []string{"-s", "--sandbox"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--dangerously-bypass-approvals-and-sandbox", aliases: []string{"--dangerously-bypass-approvals-and-sandbox"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--dangerously-bypass-hook-trust", aliases: []string{"--dangerously-bypass-hook-trust"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--cd", aliases: []string{"-C", "--cd"}, arity: arityValue, scope: scopeCommon, kind: kindCWD},
	{name: "--add-dir", aliases: []string{"--add-dir"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--ask-for-approval", aliases: []string{"-a", "--ask-for-approval"}, arity: arityValue, scope: scopeCommon, kind: kindGeneric},
	{name: "--search", aliases: []string{"--search"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "--no-daemon", aliases: []string{"--no-daemon"}, arity: arityFlag, scope: scopeCommon, kind: kindGeneric},
	{name: "exec", aliases: []string{"exec", "e"}, arity: arityFlag, scope: scopeExec, kind: kindMode},
	{name: "--skip-git-repo-check", aliases: []string{"--skip-git-repo-check"}, arity: arityFlag, scope: scopeExec, kind: kindGeneric},
	{name: "--ephemeral", aliases: []string{"--ephemeral"}, arity: arityFlag, scope: scopeExec, kind: kindStateless},
	{name: "--ignore-user-config", aliases: []string{"--ignore-user-config"}, arity: arityFlag, scope: scopeExec, kind: kindGeneric},
	{name: "--ignore-rules", aliases: []string{"--ignore-rules"}, arity: arityFlag, scope: scopeExec, kind: kindGeneric},
	{name: "--color", aliases: []string{"--color"}, arity: arityValue, scope: scopeExec, kind: kindGeneric},
	{name: "--json", aliases: []string{"--json"}, arity: arityFlag, scope: scopeExec, kind: kindOutput},
	{name: "--output-last-message", aliases: []string{"-o", "--output-last-message"}, arity: arityValue, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "--output-schema", aliases: []string{"--output-schema"}, arity: arityValue, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "resume", aliases: []string{"resume"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
	{name: "fork", aliases: []string{"fork"}, arity: arityFlag, scope: scopeExec, kind: kindCanonicalForbidden, forbidInCanon: true},
})

func normalizeCodexConfigSelectors(values []parsedOption) []parsedOption {
	result := make([]parsedOption, len(values))
	copy(result, values)
	for index := range result {
		if result[index].spec.name != "--config" ||
			len(result[index].values) != 1 {
			continue
		}
		key, _, exists := strings.Cut(result[index].values[0], "=")
		if !exists {
			continue
		}
		spec := *result[index].spec
		switch key {
		case "model":
			spec.kind = kindModel
		case "model_reasoning_effort":
			spec.kind = kindEffort
		default:
			continue
		}
		result[index].spec = &spec
	}
	return result
}
