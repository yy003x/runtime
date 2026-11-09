package codingagent

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"

	"github.com/yy003x/sn-code/internal/infrastructure/codingsessions"
	"github.com/yy003x/sn-code/pkg/contract"
)

// 压缩参数对齐 pi 默认值：触发预留 16384 token，保留最近
// DefaultKeepRecentTokens（见 session.go）。
const (
	// reserveTokens 定义当前流程使用的固定值（token 数量），取值来源为本实现约定。
	reserveTokens = 16384
	// 工具结果序列化进摘要请求时的单条上限，避免摘要请求大于被摘要的上下文。
	toolResultSummaryMaxChars = 2000
)

// summarizationSystemPrompt 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
const summarizationSystemPrompt = `You are a context summarization assistant. Your task is to read a conversation between a user and an AI assistant, then produce a structured summary following the exact format specified.

Do NOT continue the conversation. Do NOT respond to any questions in the conversation. ONLY output the structured summary.`

// summarizationPrompt 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
const summarizationPrompt = `The messages above are a conversation to summarize. Create a structured context checkpoint summary that another LLM will use to continue the work.

Use this EXACT format:

## Goal
[What is the user trying to accomplish? Can be multiple items if the session covers different tasks.]

## Constraints & Preferences
- [Any constraints, preferences, or requirements mentioned by user]
- [Or "(none)" if none were mentioned]

## Progress
### Done
- [x] [Completed tasks/changes]

### In Progress
- [ ] [Current work]

### Blocked
- [Issues preventing progress, if any]

## Key Decisions
- **[Decision]**: [Brief rationale]

## Next Steps
1. [Ordered list of what should happen next]

## Critical Context
- [Any data, examples, or references needed to continue]
- [Or "(none)" if not applicable]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// updateSummarizationPrompt 定义当前流程使用的固定值（无量纲），取值来源为本实现约定。
const updateSummarizationPrompt = `The messages above are NEW conversation messages to incorporate into the existing summary provided in <previous-summary> tags.

Update the existing structured summary with new information. RULES:
- PRESERVE all existing information from the previous summary
- ADD new progress, decisions, and context from the new messages
- UPDATE the Progress section: move items from "In Progress" to "Done" when completed
- UPDATE "Next Steps" based on what was accomplished
- PRESERVE exact file paths, function names, and error messages
- If something is no longer relevant, you may remove it

Use this EXACT format:

## Goal
[Preserve existing goals, add new ones if the task expanded]

## Constraints & Preferences
- [Preserve existing, add new ones discovered]

## Progress
### Done
- [x] [Include previously done items AND newly completed items]

### In Progress
- [ ] [Current work - update based on progress]

### Blocked
- [Current blockers - remove if resolved]

## Key Decisions
- **[Decision]**: [Brief rationale] (preserve all previous, add new)

## Next Steps
1. [Update based on current state]

## Critical Context
- [Preserve important context, add new if needed]

Keep each section concise. Preserve exact file paths, function names, and error messages.`

// ErrNothingToCompact 表示当前会话没有可压缩的历史。
var ErrNothingToCompact = errors.New("nothing to compact")

// maybeAutoCompact 在上下文估算超过「窗口 − 预留」时自动压缩。预留取
// 16384 与 profile 声明的保留输出 token 的较大者。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) maybeAutoCompact(ctx context.Context) error {
	estimate, window := session.ContextEstimate()
	if window <= 0 {
		return nil
	}
	reserve := int64(reserveTokens)
	if info, ok := session.profiles.Get(session.Profile()); ok && info.ReservedOut > reserve {
		reserve = info.ReservedOut
	}
	if estimate <= window-reserve {
		return nil
	}
	compacted, err := session.compact(ctx, "")
	if err != nil || !compacted {
		return err
	}
	_ = session.emit(Event{Type: EventNotice, Text: fmt.Sprintf(
		"context compacted (~%d tokens before)", estimate)})
	return nil
}

// Compact 手动压缩（/compact）。instructions 非空时作为摘要的额外焦点。
// 参数 instructions 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) Compact(ctx context.Context, instructions string) error {
	compacted, err := session.compact(ctx, instructions)
	if err != nil {
		return err
	}
	if !compacted {
		return ErrNothingToCompact
	}
	return nil
}

// compact 执行一次压缩：选定切点 → 序列化被摘要区间 → 单次摘要调用 →
// 追加 compaction 条目。返回是否发生了压缩。
// 参数 instructions 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 bool、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) compact(ctx context.Context, instructions string) (bool, error) {
	// 第一步在锁内完成快照：切点、被摘要文本、firstKeptEntryID。
	session.mu.Lock()
	path, err := session.store.Path("")
	if err != nil {
		session.mu.Unlock()
		return false, err
	}
	if len(path) == 0 || path[len(path)-1].Type == codingsessions.EntryTypeCompaction {
		// 最新条目已是压缩：没有新的可压缩历史。
		session.mu.Unlock()
		return false, nil
	}
	boundaryStart, previousSummary := compactionBoundary(path)
	cutIndex := findCutPoint(path, boundaryStart, session.keepRecent)
	if cutIndex <= boundaryStart {
		session.mu.Unlock()
		return false, nil
	}
	firstKeptID := path[cutIndex].ID
	serialized := serializeForSummary(path[boundaryStart:cutIndex])
	if strings.TrimSpace(serialized) == "" {
		session.mu.Unlock()
		return false, nil
	}
	_, estimate, err := session.contextEstimateLocked()
	session.mu.Unlock()
	if err != nil {
		return false, err
	}

	// 第二步摘要调用不持锁：期间新追加的条目落在切点之后，天然属于保留区。
	summary, usage, err := session.summarize(ctx, serialized, previousSummary, instructions)
	if err != nil {
		return false, err
	}
	session.recordUsage(usage)

	session.mu.Lock()
	_, appendErr := session.store.AppendCompaction(summary, firstKeptID, estimate)
	session.mu.Unlock()
	if appendErr != nil {
		return false, appendErr
	}
	return true, nil
}

// compactionBoundary 返回上一次压缩的摘要边界：被摘要区间的起点是上次
// 压缩 FirstKeptEntryID 对应的条目（其之前的原始条目已被摘要覆盖）。
func compactionBoundary(path []codingsessions.Entry) (int, string) {
	for index := len(path) - 1; index >= 0; index-- {
		entry := path[index]
		if entry.Type != codingsessions.EntryTypeCompaction || entry.Compaction == nil {
			continue
		}
		for kept := index; kept >= 0; kept-- {
			if path[kept].ID == entry.Compaction.FirstKeptEntryID {
				return kept, entry.Compaction.Summary
			}
		}
		return index + 1, entry.Compaction.Summary
	}
	return 0, ""
}

// findCutPoint 从最新条目向前累计估算 token，超过 keepRecentTokens 后在
// 不早于该位置的合法切点切开。合法切点是 user/assistant 消息条目——
// 永不切在 tool 结果上（结果必须跟着它的调用）。
func findCutPoint(path []codingsessions.Entry, startIndex int, keepRecent int64) int {
	cutPoints := make([]int, 0, len(path))
	for index := startIndex; index < len(path); index++ {
		if isCutPointEntry(path[index]) {
			cutPoints = append(cutPoints, index)
		}
	}
	if len(cutPoints) == 0 {
		return startIndex
	}
	cutIndex := cutPoints[0]
	var accumulated int64
	for index := len(path) - 1; index >= startIndex; index-- {
		tokens := estimateEntryTokens(path[index])
		if tokens == 0 {
			continue
		}
		accumulated += tokens
		if accumulated >= keepRecent {
			// 取不早于当前位置的最近合法切点。
			for _, candidate := range cutPoints {
				if candidate >= index {
					cutIndex = candidate
					break
				}
			}
			if cutIndex < index {
				cutIndex = cutPoints[len(cutPoints)-1]
			}
			break
		}
	}
	return cutIndex
}

// isCutPointEntry 执行 isCutPointEntry 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 entry 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func isCutPointEntry(entry codingsessions.Entry) bool {
	if entry.Type != codingsessions.EntryTypeMessage || entry.Message == nil {
		return false
	}
	return entry.Message.Role == contract.RoleUser || entry.Message.Role == contract.RoleAssistant
}

// estimateEntryTokens 按 chars/4 估算一个条目占用的上下文 token。
// 参数 entry 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 int64 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func estimateEntryTokens(entry codingsessions.Entry) int64 {
	if entry.Message == nil {
		return 0
	}
	chars := int64(len(entry.Message.Content))
	for _, call := range entry.Message.ToolCalls {
		chars += int64(len(call.Name)) + int64(len(call.Arguments))
	}
	return chars / charsPerToken
}

// serializeForSummary 把被摘要的消息序列化为带角色标注的文本，工具结果
// 截断到上限，避免模型把摘要输入当作要继续的对话。
// 参数 entries 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func serializeForSummary(entries []codingsessions.Entry) string {
	parts := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Message == nil {
			continue
		}
		switch entry.Message.Role {
		case contract.RoleUser:
			if entry.Message.Content != "" {
				parts = append(parts, "[User]: "+entry.Message.Content)
			}
		case contract.RoleAssistant:
			if entry.Message.Content != "" {
				parts = append(parts, "[Assistant]: "+entry.Message.Content)
			}
			if len(entry.Message.ToolCalls) > 0 {
				calls := make([]string, 0, len(entry.Message.ToolCalls))
				for _, call := range entry.Message.ToolCalls {
					calls = append(calls, formatToolCall(call))
				}
				parts = append(parts, "[Assistant tool calls]: "+strings.Join(calls, "; "))
			}
		case contract.RoleTool:
			content := entry.Message.Content
			if len(content) > toolResultSummaryMaxChars {
				content = content[:toolResultSummaryMaxChars] +
					fmt.Sprintf("\n\n[... %d more characters truncated]", len(content)-toolResultSummaryMaxChars)
			}
			if content != "" {
				parts = append(parts, "[Tool result]: "+content)
			}
		}
	}
	return strings.Join(parts, "\n\n")
}

// formatToolCall 执行 formatToolCall 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 call 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func formatToolCall(call contract.ToolCall) string {
	var arguments map[string]any
	if err := json.Unmarshal(call.Arguments, &arguments); err != nil {
		return call.Name + "(?)"
	}
	pairs := make([]string, 0, len(arguments))
	for key, value := range arguments {
		encoded, err := json.Marshal(value)
		if err != nil {
			continue
		}
		pairs = append(pairs, fmt.Sprintf("%s=%s", key, encoded))
	}
	return call.Name + "(" + strings.Join(pairs, ", ") + ")"
}

// summarize 发起一次摘要请求；有先前摘要时使用 UPDATE 变体。
// 参数 serialized、previousSummary、instructions 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string、contract.Usage、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) summarize(
	ctx context.Context,
	serialized, previousSummary, instructions string,
) (string, contract.Usage, error) {
	var builder strings.Builder
	builder.WriteString("<conversation>\n")
	builder.WriteString(serialized)
	builder.WriteString("\n</conversation>\n\n")
	basePrompt := summarizationPrompt
	if previousSummary != "" {
		basePrompt = updateSummarizationPrompt
		builder.WriteString("<previous-summary>\n")
		builder.WriteString(previousSummary)
		builder.WriteString("\n</previous-summary>\n\n")
	}
	builder.WriteString(basePrompt)
	if instructions != "" {
		builder.WriteString("\n\nAdditional focus: ")
		builder.WriteString(instructions)
	}
	result, runtimeErr := session.model.GenerateStream(ctx, contract.GenerateRequest{
		ModelProfile: session.Profile(),
		Input: contract.ModelRequest{
			System:   summarizationSystemPrompt,
			Messages: []contract.Message{{Role: contract.RoleUser, Content: builder.String()}},
		},
	}, func(contract.Event) error { return nil })
	if runtimeErr != nil {
		return "", contract.Usage{}, fmt.Errorf("summarization failed: %s", runtimeErr.Message)
	}
	if len(result.Message.ToolCalls) > 0 {
		return "", result.Usage, errors.New("summarization attempted to call a tool")
	}
	if result.FinishReason == contract.FinishLength {
		return "", result.Usage, errors.New("summarization hit the token cap and the summary is incomplete")
	}
	if strings.TrimSpace(result.Message.Content) == "" {
		return "", result.Usage, errors.New("summarization returned an empty summary")
	}
	return result.Message.Content, result.Usage, nil
}

// contextEstimateLocked 是压缩快照阶段的持锁估算；调用方必须已持有
// session.mu。
// 返回 contract.GenerateRequest、int64、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) contextEstimateLocked() (contract.GenerateRequest, int64, error) {
	path, err := session.store.Path("")
	if err != nil {
		return contract.GenerateRequest{}, 0, err
	}
	messages := codingsessions.ContextMessages(codingsessions.ContextEntries(path))
	request := contract.GenerateRequest{
		ModelProfile: session.profile,
		Input: contract.ModelRequest{
			System:   session.systemPromptText(),
			Messages: messages,
			Tools:    session.registry.Definitions(),
		},
	}
	return request, EstimateTokens(request.Input.System, messages), nil
}
