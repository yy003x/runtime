package aep

// AEP DTO 只覆盖 MVP 方法集（D7）与 P1 支持的字段（D3/D6）；协议字段命名
// 与 docs/aep/spec.md §4 一致（camelCase）。

// ServerInfo 标识 AEP 服务端实现。
type ServerInfo struct {
	Name    string `json:"name"`
	Version string `json:"version"`
}

type clientInfo struct {
	Name    string `json:"name,omitempty"`
	Version string `json:"version,omitempty"`
}

type initializeParams struct {
	ClientInfo *clientInfo `json:"clientInfo,omitempty"`
}

// InitializeResult 返回协议协商结果：本服务端只实现 ProtocolVersion 一个
// 版本，methods 是支持的方法清单。
type InitializeResult struct {
	Protocol string     `json:"protocol"`
	Version  string     `json:"version"`
	Server   ServerInfo `json:"server"`
	Methods  []string   `json:"methods"`
}

// Capabilities 是 AEP §4.1 的能力位；必须按当前 adapter 投影如实上报，
// 不得按协议可能性宣称。
type Capabilities struct {
	Streaming        bool `json:"streaming"`
	Cancellation     bool `json:"cancellation"`
	StructuredEvents bool `json:"structuredEvents"`
	SessionResume    bool `json:"sessionResume"`
	NativeResult     bool `json:"nativeResult"`
}

// ExecutorDescriptor 描述一个执行器及其可用性。
type ExecutorDescriptor struct {
	ID           string            `json:"id"`
	Name         string            `json:"name"`
	Version      string            `json:"version,omitempty"`
	Available    bool              `json:"available"`
	Capabilities Capabilities      `json:"capabilities"`
	Metadata     map[string]string `json:"metadata,omitempty"`
}

type executorListResult struct {
	Executors []ExecutorDescriptor `json:"executors"`
}

type executorGetParams struct {
	Executor string `json:"executor"`
}

// RunStartParams 按 D3：profile 必填，executor 由 Profile command basename
// 推导并校验一致。
type RunStartParams struct {
	Executor string   `json:"executor"`
	Profile  string   `json:"profile"`
	Input    RunInput `json:"input"`
}

// RunInput 按 D6：MVP 接受 prompt/cwd/sessionId/resumeFromRunId 与
// options.model/options.reasoningEffort/options.timeoutMs/options.permissions；
// maxOutputBytes、metadata 未实现即被严格解码显式拒绝。
type RunInput struct {
	Prompt          string      `json:"prompt"`
	CWD             string      `json:"cwd"`
	SessionID       string      `json:"sessionId,omitempty"`
	ResumeFromRunID string      `json:"resumeFromRunId,omitempty"`
	Options         *RunOptions `json:"options,omitempty"`
}

type RunOptions struct {
	Model           string          `json:"model,omitempty"`
	ReasoningEffort string          `json:"reasoningEffort,omitempty"`
	TimeoutMS       int64           `json:"timeoutMs,omitempty"`
	Permissions     *RunPermissions `json:"permissions,omitempty"`
}

// RunPermissions 按 AEP §4：MVP 只接受 mode；approvalPolicy 没有安全等价
// 映射，严格解码下作为未知字段显式拒绝。
type RunPermissions struct {
	Mode string `json:"mode"`
}

type runStartResult struct {
	RunID     string `json:"runId"`
	Status    string `json:"status"`
	SessionID string `json:"sessionId"`
}

type runIDParams struct {
	RunID string `json:"runId"`
}

// RunError 是 AEP Run/RunResult 上的错误投影。
type RunError struct {
	Code    string `json:"code"`
	Phase   string `json:"phase,omitempty"`
	Message string `json:"message"`
}

// RunView 是 run.get 的 Run 投影；Runtime Record 未单独跟踪的 started/
// finished/exitCode 字段如实省略。
type RunView struct {
	RunID     string    `json:"runId"`
	Executor  string    `json:"executor"`
	Status    string    `json:"status"`
	CreatedAt string    `json:"createdAt"`
	SessionID string    `json:"sessionId,omitempty"`
	Error     *RunError `json:"error,omitempty"`
}

// UsageView 仅在执行器提供可信数据时填写（D4）；P1 的 session 终态不含
// usage，恒省略。
type UsageView struct {
	InputTokens  int64 `json:"inputTokens,omitempty"`
	OutputTokens int64 `json:"outputTokens,omitempty"`
	TotalTokens  int64 `json:"totalTokens,omitempty"`
}

// ArtifactRef 保留协议字段；MVP 恒为空数组（D4）。
type ArtifactRef struct {
	ArtifactID string `json:"artifactId"`
	Type       string `json:"type"`
	URI        string `json:"uri"`
}

// RunResultView 是 run.result 的终态投影；仅在 Run terminal 后可查询。
type RunResultView struct {
	RunID        string        `json:"runId"`
	Status       string        `json:"status"`
	Summary      string        `json:"summary,omitempty"`
	ExitCode     *int          `json:"exitCode,omitempty"`
	Usage        *UsageView    `json:"usage,omitempty"`
	Artifacts    []ArtifactRef `json:"artifacts"`
	Error        *RunError     `json:"error,omitempty"`
	FinishedAt   string        `json:"finishedAt"`
	Completeness string        `json:"completeness"`
}

type runEventsParams struct {
	RunID    string `json:"runId"`
	AfterSeq uint64 `json:"afterSeq,omitempty"`
	Limit    int    `json:"limit,omitempty"`
}

type runEventsResult struct {
	Events []EventEnvelope `json:"events"`
}

type runCancelResult struct {
	RunID  string `json:"runId"`
	Status string `json:"status"`
}

// EventSource 标识事件来源执行器与原生事件类型（兜底事件使用）。
type EventSource struct {
	Executor        string `json:"executor,omitempty"`
	NativeEventType string `json:"nativeEventType,omitempty"`
}

// EventEnvelope 是 AEP §5.1 的统一事件外层；eventId 由 run_id:seq 确定性
// 派生（D4），seq 复用 journal sequence。
type EventEnvelope struct {
	Protocol  string       `json:"protocol"`
	Version   string       `json:"version"`
	EventID   string       `json:"eventId"`
	RunID     string       `json:"runId"`
	Seq       uint64       `json:"seq"`
	Timestamp string       `json:"timestamp"`
	Type      string       `json:"type"`
	Data      any          `json:"data"`
	Source    *EventSource `json:"source,omitempty"`
}
