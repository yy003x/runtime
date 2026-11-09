package codingagent

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"time"

	"github.com/yy003x/sn-code/internal/application/codingagent/tools"
	"github.com/yy003x/sn-code/internal/infrastructure/codingsessions"
	"github.com/yy003x/sn-code/pkg/agent"
	"github.com/yy003x/sn-code/pkg/contract"
)

// 默认预算：一次 run 的回合数与工具调用数上限。超限终止 run 并报错，
// 防止模型在无界循环里消耗 token。
const (
	// DefaultMaxTurns 是单次 run 的最大模型回合数（回合），取自交互代理预算默认值。
	DefaultMaxTurns = 40
	// DefaultMaxToolCalls 是单次 run 的最大工具调用数（次），取自交互代理预算默认值。
	DefaultMaxToolCalls = 100
	// 压缩时保留的最近 token 预算，对齐 pi 默认值。
	DefaultKeepRecentTokens = 20000
	// 估算比例对齐 pi：~4 字符/token。
	charsPerToken = 4
)

// toolCallOutcome 区分工具结果已落盘、运行取消和已报告终态的落盘失败。
type toolCallOutcome uint8

const (
	// toolCallSucceeded 表示工具结果已落盘且运行仍可继续。
	toolCallSucceeded toolCallOutcome = iota
	// toolCallCancelled 表示工具结果已处理，但运行上下文已经取消。
	toolCallCancelled
	// toolCallFailed 表示结果落盘失败，fail 已发出 error 终态事件。
	toolCallFailed
)

// AgentSession 是一个交互会话的编排器：持有会话文件、当前 profile、
// 工具注册表与事件出口。run 由调用方在独立 goroutine 驱动；运行中的
// 输入经 steering/followUp 队列注入。所有会话文件写入都在 session.mu
// 保护下串行执行。
type AgentSession struct {
	model    Model
	registry *agent.Registry
	policy   map[string]tools.ToolPolicy
	profiles ProfileSource
	store    *codingsessions.Session
	gate     ToolGate
	emit     EventSink

	cwd        string
	agentDir   string
	maxTurns   int
	keepRecent int64

	mu            sync.Mutex
	profile       string
	steering      []string
	followUp      []string
	alwaysAllowed map[string]bool
	busy          bool
	totals        UsageTotals
}

// UsageTotals 是会话累计用量（footer 显示）。
type UsageTotals struct {
	InputTokens      int64
	OutputTokens     int64
	CacheReadTokens  int64
	CacheWriteTokens int64
}

// Deps 是构造 AgentSession 的依赖集合。
type Deps struct {
	Model    Model
	Registry *agent.Registry
	Policy   map[string]tools.ToolPolicy
	Profiles ProfileSource
	Session  *codingsessions.Session
	Gate     ToolGate
	Emit     EventSink
	CWD      string
	// AgentDir 是全局配置目录（active home 下的 sncode），用于加载全局
	// AGENTS.md 上下文与偏好。
	AgentDir string
}

// New 创建编排器。恢复会话时按 path 上的最后一次 model_change 初始化
// 当前 profile；否则使用 fallbackProfile。
// 参数 deps、fallbackProfile 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *AgentSession、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func New(deps Deps, fallbackProfile string) (*AgentSession, error) {
	if deps.Model == nil || deps.Registry == nil || deps.Profiles == nil || deps.Session == nil {
		return nil, fmt.Errorf("codingagent deps: model, registry, profiles and session are required")
	}
	if deps.Emit == nil {
		deps.Emit = func(Event) error { return nil }
	}
	if deps.Gate == nil {
		deps.Gate = DenyWritesGate{}
	}
	instance := &AgentSession{
		model:      deps.Model,
		registry:   deps.Registry,
		policy:     deps.Policy,
		profiles:   deps.Profiles,
		store:      deps.Session,
		gate:       deps.Gate,
		emit:       deps.Emit,
		cwd:        deps.CWD,
		agentDir:   deps.AgentDir,
		maxTurns:   DefaultMaxTurns,
		keepRecent: DefaultKeepRecentTokens,
	}
	path, err := deps.Session.Path("")
	if err != nil {
		return nil, err
	}
	instance.profile = codingsessions.CurrentProfile(path)
	if instance.profile == "" {
		instance.profile = fallbackProfile
	}
	if _, ok := instance.profiles.Get(instance.profile); !ok {
		return nil, fmt.Errorf("profile %q is not available", instance.profile)
	}
	return instance, nil
}

// SessionID 返回会话文件 ID。
func (session *AgentSession) SessionID() string {
	return session.store.Header.ID
}

// Name 返回会话显示名（最近一次 session_info 的 name）；未命名返回空串。
func (session *AgentSession) Name() string {
	path, err := session.store.Path("")
	if err != nil {
		return ""
	}
	name := ""
	for _, entry := range path {
		if entry.SessionInfo != nil && entry.SessionInfo.Name != "" {
			name = entry.SessionInfo.Name
		}
	}
	return name
}

// SessionFilePath 返回会话文件路径（/export、调试使用）。
func (session *AgentSession) SessionFilePath() string {
	return session.store.FilePath()
}

// Profile 返回当前 profile ID。
func (session *AgentSession) Profile() string {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.profile
}

// SetProfile 切换 profile 并写入 model_change；运行中切换只影响下一次
// 模型调用。
// 参数 id 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) SetProfile(id string) error {
	if _, ok := session.profiles.Get(id); !ok {
		return fmt.Errorf("profile %q is not available", id)
	}
	session.mu.Lock()
	previous := session.profile
	session.profile = id
	session.mu.Unlock()
	if previous == id {
		return nil
	}
	session.mu.Lock()
	_, err := session.store.AppendModelChange(id)
	session.mu.Unlock()
	return err
}

// Busy 报告是否有 run 正在进行。
func (session *AgentSession) Busy() bool {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.busy
}

// Steer 把运行中的输入排队为 steering：当前回合结束后、下一次模型调用前
// 注入（对齐 pi 的 steer 语义）。
// 参数 text 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (session *AgentSession) Steer(text string) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.steering = append(session.steering, text)
}

// FollowUp 把输入排队为 follow-up：仅当模型即将停止时作为新输入注入。
// 参数 text 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (session *AgentSession) FollowUp(text string) {
	session.mu.Lock()
	defer session.mu.Unlock()
	session.followUp = append(session.followUp, text)
}

// Totals 返回会话累计用量。
func (session *AgentSession) Totals() UsageTotals {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.totals
}

// Run 执行一次以 prompt 开始的 agent run：模型→工具→模型循环，直到模型
// 不再请求工具且 followUp 队列为空。调用方负责在独立 goroutine 中执行。
func (session *AgentSession) Run(ctx context.Context, prompt string) {
	session.mu.Lock()
	if session.busy {
		session.mu.Unlock()
		session.Steer(prompt)
		_ = session.emit(Event{Type: EventNotice, Text: "a run is already active; input queued as steering"})
		return
	}
	session.busy = true
	session.mu.Unlock()
	defer func() {
		session.mu.Lock()
		session.busy = false
		session.mu.Unlock()
	}()
	if _, err := session.appendMessage(contract.Message{Role: contract.RoleUser, Content: prompt}); err != nil {
		session.fail(err, "session write failed")
		return
	}
	_ = session.emit(Event{Type: EventRunStarted})
	session.runLoop(ctx)
}

// runLoop 执行 runLoop 对应的包内处理，并遵循调用方提供的输入契约。
func (session *AgentSession) runLoop(ctx context.Context) {
	for {
		terminal := session.runUntilStop(ctx)
		if terminal {
			return
		}
		next, found := session.takeFollowUp()
		if !found {
			_ = session.emit(Event{Type: EventRunFinished, Text: "completed"})
			return
		}
		if _, err := session.appendMessage(contract.Message{Role: contract.RoleUser, Content: next}); err != nil {
			session.fail(err, "session write failed")
			return
		}
	}
}

// runUntilStop 执行内层循环（模型→工具→…），直到模型不再请求工具、取消、
// 或出错。返回 true 表示终态事件已发出（aborted/error），外层无需再发。
func (session *AgentSession) runUntilStop(ctx context.Context) bool {
	toolCallsUsed := 0
	for round := 0; round < session.maxTurns; round++ {
		if ctx.Err() != nil {
			_ = session.emit(Event{Type: EventRunFinished, Text: "aborted"})
			return true
		}
		if !session.drainSteering() {
			_ = session.emit(Event{Type: EventRunFinished, Text: "aborted"})
			return true
		}
		if err := session.maybeAutoCompact(ctx); err != nil {
			session.fail(err, "auto compaction failed")
			return true
		}
		request, estimate, err := session.buildRequest()
		if err != nil {
			session.fail(err, "context build failed")
			return true
		}
		_ = session.emit(Event{
			Type: EventTurnStarted,
			Text: fmt.Sprintf("round %d, ~%d tokens context", round+1, estimate),
		})

		result, runtimeErr := session.generate(ctx, request)
		if result.Message.Content != "" || len(result.Message.ToolCalls) > 0 {
			if _, err := session.appendMessage(result.Message); err != nil {
				session.fail(err, "session write failed")
				return true
			}
		}
		if result.Usage.InputTokens != nil || result.Usage.OutputTokens != nil {
			session.recordUsage(result.Usage)
			usage := result.Usage
			_ = session.emit(Event{
				Type: EventAssistantDone, Message: &result.Message,
				Usage: &usage, Finish: result.FinishReason,
			})
		} else {
			_ = session.emit(Event{
				Type: EventAssistantDone, Message: &result.Message, Finish: result.FinishReason,
			})
		}
		if runtimeErr != nil {
			if ctx.Err() != nil {
				_ = session.emit(Event{Type: EventRunFinished, Text: "aborted"})
				return true
			}
			_ = session.emit(Event{Type: EventModelError, Err: &RuntimeFailure{
				Code: runtimeErr.Code, Phase: runtimeErr.Phase, Message: runtimeErr.Message,
			}})
			_ = session.emit(Event{Type: EventRunFinished, Text: "error"})
			return true
		}
		if result.FinishReason != contract.FinishToolCall || len(result.Message.ToolCalls) == 0 {
			return false
		}
		for _, call := range result.Message.ToolCalls {
			toolCallsUsed++
			if toolCallsUsed > DefaultMaxToolCalls {
				session.fail(fmt.Errorf("tool call budget of %d exceeded", DefaultMaxToolCalls), "budget exceeded")
				return true
			}
			switch session.executeToolCall(ctx, call) {
			case toolCallCancelled:
				_ = session.emit(Event{Type: EventRunFinished, Text: "aborted"})
				return true
			case toolCallFailed:
				return true
			}
		}
	}
	session.fail(fmt.Errorf("round budget of %d exceeded", session.maxTurns), "budget exceeded")
	return true
}

// drainSteering 注入排队的 steering 输入；返回 false 表示写入失败。
func (session *AgentSession) drainSteering() bool {
	for {
		session.mu.Lock()
		if len(session.steering) == 0 {
			session.mu.Unlock()
			return true
		}
		next := session.steering[0]
		session.steering = session.steering[1:]
		_, err := session.store.AppendMessage(contract.Message{Role: contract.RoleUser, Content: next})
		session.mu.Unlock()
		if err != nil {
			session.fail(err, "session write failed")
			return false
		}
	}
}

// takeFollowUp 执行 takeFollowUp 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 string、bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) takeFollowUp() (string, bool) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if len(session.followUp) == 0 {
		return "", false
	}
	next := session.followUp[0]
	session.followUp = session.followUp[1:]
	return next, true
}

// generate 执行一次流式模型调用，把 content delta 转发为应用事件。
// 参数 request 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 contract.ModelResult、*contract.RuntimeError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	accumulated := ""
	sink := func(event contract.Event) error {
		if ctx.Err() != nil {
			return ctx.Err()
		}
		if event.Type == contract.EventContentDelta && event.Model != nil && event.Model.Text != "" {
			accumulated += event.Model.Text
			return session.emit(Event{Type: EventTextDelta, Text: event.Model.Text})
		}
		return nil
	}
	result, runtimeErr := session.model.GenerateStream(ctx, request, sink)
	if runtimeErr != nil && accumulated != "" && result.Message.Content == "" {
		// 失败/取消但已流出文本时保留部分内容，便于检视与续跑。
		result.Message = contract.Message{Role: contract.RoleAssistant, Content: accumulated}
	}
	return result, runtimeErr
}

// executeToolCall 校验→确认门→执行→落盘一条工具调用；返回值区分可继续、
// 已取消与已发出 error 终态的存储失败，避免调用方覆盖终态。
// 参数 call 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 toolCallOutcome 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) executeToolCall(ctx context.Context, call contract.ToolCall) toolCallOutcome {
	started := time.Now()
	_ = session.emit(Event{Type: EventToolStarted, Call: &ToolCallInfo{
		ID: call.ID, Name: call.Name, Arguments: cloneArguments(call.Arguments),
	}})
	result := session.runTool(ctx, call)
	duration := time.Since(started).Milliseconds()
	message := contract.Message{
		Role:       contract.RoleTool,
		ToolCallID: call.ID,
		Content:    result.Content,
		IsError:    result.IsError,
	}
	if _, err := session.appendMessage(message); err != nil {
		session.fail(err, "session write failed")
		return toolCallFailed
	}
	_ = session.emit(Event{Type: EventToolFinished, Call: &ToolCallInfo{
		ID: call.ID, Name: call.Name, Arguments: cloneArguments(call.Arguments),
		DurationMS: duration, Content: result.Content, IsError: result.IsError,
	}})
	if ctx.Err() != nil {
		return toolCallCancelled
	}
	return toolCallSucceeded
}

// runTool 执行确认门与注册表调用，返回工具结果。
// 参数 call 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 agent.ToolResult 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) runTool(ctx context.Context, call contract.ToolCall) agent.ToolResult {
	current := session.Profile()
	environment := tools.Environment{
		CWD:       session.cwd,
		SessionID: session.store.Header.ID,
		Profile:   current,
	}
	if info, ok := session.profiles.Get(current); ok {
		environment.Model = info.Model
	}
	if policy, known := session.policy[call.Name]; known && policy.Effect != contract.EffectReadOnly {
		session.mu.Lock()
		allowed := session.alwaysAllowed[call.Name]
		session.mu.Unlock()
		if !allowed {
			switch session.gate.Confirm(ctx, ToolCallInfoForGate{
				CallID: call.ID, Name: call.Name, Effect: policy.Effect, Risk: policy.Risk,
				Arguments: cloneArguments(call.Arguments),
			}) {
			case GateDeny:
				return agent.ToolResult{
					Content: fmt.Sprintf("User denied execution of tool %s", call.Name),
					IsError: true,
				}
			case GateAlways:
				session.mu.Lock()
				if session.alwaysAllowed == nil {
					session.alwaysAllowed = map[string]bool{}
				}
				session.alwaysAllowed[call.Name] = true
				session.mu.Unlock()
			}
		}
	}
	result, err := session.registry.Execute(tools.WithEnvironment(ctx, environment), agent.ToolRequest{
		RunID:     session.store.Header.ID,
		CallID:    call.ID,
		Name:      call.Name,
		Arguments: cloneArguments(call.Arguments),
	})
	if err != nil {
		// 校验失败或未知工具按 pi 语义转为 isError 结果，循环继续。
		return agent.ToolResult{Content: err.Error(), IsError: true}
	}
	return result
}

// cloneArguments 执行 cloneArguments 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 arguments 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 json.RawMessage 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func cloneArguments(arguments json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), arguments...)
}

// appendMessage 执行 appendMessage 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 message 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 codingsessions.Entry、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) appendMessage(message contract.Message) (codingsessions.Entry, error) {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.store.AppendMessage(message)
}

// recordUsage 执行 recordUsage 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 usage 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (session *AgentSession) recordUsage(usage contract.Usage) {
	session.mu.Lock()
	defer session.mu.Unlock()
	if usage.InputTokens != nil {
		session.totals.InputTokens += *usage.InputTokens
	}
	if usage.OutputTokens != nil {
		session.totals.OutputTokens += *usage.OutputTokens
	}
	if usage.CacheReadTokens != nil {
		session.totals.CacheReadTokens += *usage.CacheReadTokens
	}
	if usage.CacheWriteTokens != nil {
		session.totals.CacheWriteTokens += *usage.CacheWriteTokens
	}
}

// fail 执行 fail 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 err、notice 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (session *AgentSession) fail(err error, notice string) {
	_ = session.emit(Event{Type: EventModelError, Err: &RuntimeFailure{Message: notice + ": " + err.Error()}})
	_ = session.emit(Event{Type: EventRunFinished, Text: "error"})
}

// systemPromptText 组装当前系统提示词；读取只依赖不可变字段。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) systemPromptText() string {
	return BuildSystemPrompt(session.cwd, tools.PromptSnippets(), LoadContextFiles(session.cwd, session.agentDir))
}

// buildRequest 从会话路径重建上下文并构造模型请求。
// 返回 contract.GenerateRequest、int64、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) buildRequest() (contract.GenerateRequest, int64, error) {
	path, err := session.store.Path("")
	if err != nil {
		return contract.GenerateRequest{}, 0, err
	}
	contextEntries := codingsessions.ContextEntries(path)
	messages := codingsessions.ContextMessages(contextEntries)
	systemPrompt := session.systemPromptText()
	request := contract.GenerateRequest{
		ModelProfile: session.Profile(),
		Input: contract.ModelRequest{
			System:   systemPrompt,
			Messages: messages,
			Tools:    session.registry.Definitions(),
		},
	}
	return request, EstimateTokens(systemPrompt, messages), nil
}

// EstimateTokens 按 ~4 字符/token 估算上下文规模（压缩与 footer 共用）。
// 参数 systemPrompt、messages 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 int64 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func EstimateTokens(systemPrompt string, messages []contract.Message) int64 {
	total := int64(len(systemPrompt))
	for _, message := range messages {
		total += int64(len(message.Content))
		for _, call := range message.ToolCalls {
			total += int64(len(call.Arguments))
		}
	}
	return total / charsPerToken
}

// ContextEstimate 返回当前上下文的 token 估算与窗口大小；持锁读取，
// 可在 run 进行中从 UI 线程安全调用。
func (session *AgentSession) ContextEstimate() (estimate int64, window int64) {
	session.mu.Lock()
	profile := session.profile
	_, estimate, err := session.contextEstimateLocked()
	session.mu.Unlock()
	if err != nil {
		return 0, 0
	}
	info, ok := session.profiles.Get(profile)
	if !ok || info.ContextWindow <= 0 {
		window = 128000
	} else {
		window = info.ContextWindow
	}
	return estimate, window
}

// RunUserBash 执行用户直发的 shell 命令（!command）。record 为 true 时把
// 命令与输出以 user 消息写入会话上下文，对齐 pi 的 bashExecution 语义。
// 参数 command、record 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string、bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) RunUserBash(ctx context.Context, command string, record bool) (string, bool) {
	current := session.Profile()
	environment := tools.Environment{
		CWD:       session.cwd,
		SessionID: session.store.Header.ID,
		Profile:   current,
	}
	if info, ok := session.profiles.Get(current); ok {
		environment.Model = info.Model
	}
	arguments, _ := json.Marshal(map[string]string{"command": command})
	result, err := session.registry.Execute(tools.WithEnvironment(ctx, environment), agent.ToolRequest{
		RunID:     session.store.Header.ID,
		CallID:    "user_bash",
		Name:      "bash",
		Arguments: arguments,
	})
	if err != nil {
		return err.Error(), true
	}
	if record {
		content := fmt.Sprintf("I ran this command:\n$ %s\n%s", command, result.Content)
		if _, err := session.appendMessage(contract.Message{
			Role: contract.RoleUser, Content: content,
		}); err != nil {
			return result.Content, result.IsError
		}
	}
	return result.Content, result.IsError
}

// NameSession 写入会话显示名。
// 参数 name 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) NameSession(name string) error {
	session.mu.Lock()
	defer session.mu.Unlock()
	_, err := session.store.AppendSessionInfo(name)
	return err
}

// Close 关闭底层会话文件。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) Close() error {
	session.mu.Lock()
	defer session.mu.Unlock()
	return session.store.Close()
}

// Store 暴露底层会话文件句柄（只读使用）。
// 返回 *codingsessions.Session 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (session *AgentSession) Store() *codingsessions.Session {
	return session.store
}
