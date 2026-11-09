package aep

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yy003x/sn-code/internal/domain/identity"
	runtimecommand "github.com/yy003x/sn-code/pkg/command"
	"github.com/yy003x/sn-code/pkg/contract"
	"github.com/yy003x/sn-code/pkg/profile"
	runtime "github.com/yy003x/sn-code/pkg/run"
	"github.com/yy003x/sn-code/pkg/session"
)

// supportedMethods 是 D7 的 MVP 方法集；事件通知方法 run.event 只由服务端
// 发出。
var supportedMethods = []string{
	"initialize", "executor.list", "executor.get", "run.start", "run.cancel",
	"run.get", "run.result", "run.events", "shutdown",
}

var errShutdownRequested = errors.New("aep shutdown requested")

// ServerConfig 组装 AEP 服务端依赖。Profiles 与 Runs 是唯一执行面：所有
// Run 生命周期都走 pkg/run durable Run，本包不建立第二套状态机。
type ServerConfig struct {
	Profiles *profile.Catalog
	Runs     *runtime.Service
	Server   ServerInfo
	// InvocationBase 是 AEP 进程的调用基准目录（等价 CLI session exec 的
	// callerCWD），用于 queued session Run 的 invocation 事实。
	InvocationBase string
	Now            func() time.Time
	// Diagnostics 接收 stderr 诊断；不得写 stdout。
	Diagnostics func(format string, args ...any)
	// ShutdownGrace 限制关停时等待非终态 Run 收口的时间；默认 15s。
	ShutdownGrace time.Duration
}

type Server struct {
	profiles       *profile.Catalog
	runs           *runtime.Service
	serverInfo     ServerInfo
	invocationBase string
	now            func() time.Time
	diag           func(format string, args ...any)
	shutdownGrace  time.Duration

	stopCh   chan struct{}
	stopOnce sync.Once

	mutex       sync.Mutex
	initialized bool
	watchers    map[string]runWatcher
	startedRuns map[string]struct{}

	writer *lineWriter
}

// NewServer 执行 NewServer 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 config 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *Server、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func NewServer(config ServerConfig) (*Server, error) {
	if config.Profiles == nil {
		return nil, fmt.Errorf("profile catalog is required")
	}
	if config.Runs == nil {
		return nil, fmt.Errorf("run service is required")
	}
	if config.Server.Name == "" || config.Server.Version == "" {
		return nil, fmt.Errorf("server name and version are required")
	}
	if config.InvocationBase == "" {
		return nil, fmt.Errorf("invocation base directory is required")
	}
	if config.Now == nil {
		config.Now = time.Now
	}
	if config.Diagnostics == nil {
		config.Diagnostics = func(string, ...any) {}
	}
	if config.ShutdownGrace <= 0 {
		config.ShutdownGrace = 15 * time.Second
	}
	return &Server{
		profiles: config.Profiles, runs: config.Runs,
		serverInfo: config.Server, invocationBase: config.InvocationBase,
		now: config.Now, diag: config.Diagnostics,
		shutdownGrace: config.ShutdownGrace,
		stopCh:        make(chan struct{}),
		watchers:      make(map[string]runWatcher),
		startedRuns:   make(map[string]struct{}),
	}, nil
}

// Serve 在 stdin/stdout 上运行 NDJSON JSON-RPC 循环。stdin EOF、shutdown
// 方法或 ctx 取消都会触发 graceful 收口：对本连接 run.start 提交且仍非终态
// 的 Run 走既有取消路径，等待终态（受 ShutdownGrace 约束）后返回。
// Serve 一次只可调用一次。
// 参数 in、out 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) Serve(ctx context.Context, in io.Reader, out io.Writer) error {
	if in == nil || out == nil {
		return fmt.Errorf("aep transport requires stdin and stdout")
	}
	serveCtx, stopServing := context.WithCancelCause(ctx)
	defer stopServing(context.Canceled)
	server.writer = newContextLineWriter(serveCtx, out, stopServing)
	defer server.writer.stop(context.Canceled)
	if closer, ok := in.(io.Closer); ok {
		closeInput := context.AfterFunc(serveCtx, func() { _ = closer.Close() })
		defer closeInput()
	}
	// settle 与输入循环解耦：输入结束或关停触发后立即开始收口，即使响应写
	// 被不读端的满管道卡住也不阻塞取消路径；writer 在 settle 完成后才停止，
	// 半关连接（已关 stdin、仍读 stdout）在收口期间仍能收到终态事件。
	var settle sync.Once
	settleDone := make(chan struct{})
	beginSettle := func() {
		settle.Do(func() {
			go func() {
				defer close(settleDone)
				server.settleTrackedRuns(ctx)
				server.writer.stop(context.Canceled)
			}()
		})
	}
	serveErr := scanProtocolLines(serveCtx, in, func(line []byte) error {
		if err := server.handleLine(serveCtx, line); err != nil {
			return err
		}
		select {
		case <-server.stopCh:
			return errShutdownRequested
		default:
			return nil
		}
	}, func(cause error) {
		// framing/读错误意味着客户端已不可信，立即终止输出；干净 EOF 保留
		// writer 供 settle 推送终态事件。
		if !errors.Is(cause, io.EOF) {
			server.writer.stop(cause)
		}
		beginSettle()
	})
	var loopErr error
	switch {
	case serveErr == nil, errors.Is(serveErr, io.EOF):
	case errors.Is(serveErr, errShutdownRequested):
	case errors.Is(serveErr, context.Canceled):
		server.diag("aep: context canceled; shutting down")
	default:
		server.diag("aep: stdin loop stopped: %v", serveErr)
		_ = server.writer.sendError(nil, &rpcError{
			code: codeParseError, message: "Parse error: framing failure",
		})
		loopErr = serveErr
	}
	beginSettle()
	<-settleDone
	if writeErr := context.Cause(server.writer.ctx); loopErr == nil && writeErr != nil &&
		!errors.Is(writeErr, context.Canceled) && !errors.Is(writeErr, io.EOF) {
		loopErr = writeErr
	}
	return loopErr
}

// handleLine 执行 handleLine 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 line 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) handleLine(ctx context.Context, line []byte) error {
	trimmed := bytes.TrimSpace(line)
	if len(trimmed) == 0 {
		return nil
	}
	envelope, err := decodeRequestLine(trimmed)
	if err != nil {
		return server.writer.sendError(nil, &rpcError{
			code: codeParseError, message: "Parse error: " + err.Error(),
		})
	}
	if idErr := validateRequestID(envelope.ID); idErr != nil {
		return server.writer.sendError(nil, &rpcError{
			code: codeInvalidRequest, message: "Invalid Request: " + idErr.Error(),
		})
	}
	if err := validateEnvelope(envelope); err != nil {
		return server.writer.sendError(envelope.ID, &rpcError{
			code: codeInvalidRequest, message: "Invalid Request: " + err.Error(),
		})
	}
	if len(envelope.ID) == 0 {
		// MVP 未定义任何入站通知；按 JSON-RPC 2.0 语义通知不回应。
		server.diag("aep: ignored notification %q", envelope.Method)
		return nil
	}
	server.handleRequest(ctx, envelope)
	return nil
}

// handleRequest 执行 handleRequest 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 envelope 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (server *Server) handleRequest(ctx context.Context, envelope rpcRequestEnvelope) {
	if !server.isInitialized() && envelope.Method != "initialize" {
		_ = server.writer.sendError(envelope.ID, notInitializedError())
		return
	}
	var result any
	var rpcErr *rpcError
	switch envelope.Method {
	case "initialize":
		result, rpcErr = server.handleInitialize(envelope.Params)
	case "executor.list":
		result = executorListResult{Executors: executorList(server.profiles, server.invocationBase)}
	case "executor.get":
		result, rpcErr = server.handleExecutorGet(envelope.Params)
	case "run.start":
		result, rpcErr = server.handleRunStart(ctx, envelope.Params)
	case "run.cancel":
		result, rpcErr = server.handleRunCancel(ctx, envelope.Params)
	case "run.get":
		result, rpcErr = server.handleRunGet(ctx, envelope.Params)
	case "run.result":
		result, rpcErr = server.handleRunResult(ctx, envelope.Params)
	case "run.events":
		result, rpcErr = server.handleRunEvents(ctx, envelope.Params)
	case "shutdown":
		result, rpcErr = server.handleShutdown(envelope.Params)
	default:
		rpcErr = methodNotFoundError(envelope.Method)
	}
	if rpcErr != nil {
		_ = server.writer.sendError(envelope.ID, rpcErr)
		return
	}
	if err := server.writer.sendResult(envelope.ID, result); err != nil {
		server.diag("aep: write response: %v", err)
	}
}

// handleInitialize 执行 handleInitialize 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 params 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 any、*rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) handleInitialize(params json.RawMessage) (any, *rpcError) {
	var value initializeParams
	if len(bytes.TrimSpace(params)) > 0 {
		if err := decodeParams(params, &value); err != nil {
			return nil, invalidParamsError("invalid params: %v", err)
		}
	}
	server.mutex.Lock()
	already := server.initialized
	server.initialized = true
	server.mutex.Unlock()
	if already {
		return nil, &rpcError{
			code:    codeInvalidRequest,
			message: "initialize may only be called once per connection",
		}
	}
	if value.ClientInfo != nil {
		server.diag(
			"aep: client %s %s", value.ClientInfo.Name, value.ClientInfo.Version,
		)
	}
	return InitializeResult{
		Protocol: ProtocolName, Version: ProtocolVersion,
		Server: server.serverInfo, Methods: supportedMethods,
	}, nil
}

// handleExecutorGet 执行 handleExecutorGet 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 params 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 any、*rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) handleExecutorGet(params json.RawMessage) (any, *rpcError) {
	var value executorGetParams
	if err := decodeParams(params, &value); err != nil {
		return nil, invalidParamsError("invalid params: %v", err)
	}
	definition, ok := executorDefinitionByID(value.Executor)
	if !ok {
		return nil, notFoundError("executor %s not found", value.Executor)
	}
	return executorDescriptor(definition, server.profiles, server.invocationBase), nil
}

// handleRunStart 执行 handleRunStart 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 params 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 any、*rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) handleRunStart(
	ctx context.Context,
	params json.RawMessage,
) (any, *rpcError) {
	var value RunStartParams
	if err := decodeParams(params, &value); err != nil {
		return nil, invalidParamsError("invalid params: %v", err)
	}
	if value.Executor == "acp" {
		return nil, invalidParamsError(
			"executor %q is rejected in the AEP MVP", value.Executor,
		)
	}
	definition, ok := executorDefinitionByID(value.Executor)
	if !ok {
		return nil, invalidParamsError("unknown executor %q", value.Executor)
	}
	if !definition.registered {
		return nil, executorUnavailableError(value.Executor, "EXECUTOR_NOT_REGISTERED")
	}
	if value.Profile == "" {
		return nil, invalidParamsError("profile is required")
	}
	entry, exists := server.profiles.Resolve(value.Profile)
	if !exists || entry.Kind != profile.KindCommand || entry.Command == nil {
		return nil, invalidParamsError(
			"profile %q is not a CLI Profile", value.Profile,
		)
	}
	derived, ok := executorForCommand(entry.Command.Command)
	if !ok || derived != value.Executor {
		return nil, invalidParamsError(
			"profile %q does not resolve to executor %q", value.Profile, value.Executor,
		)
	}
	if strings.TrimSpace(value.Input.Prompt) == "" {
		return nil, invalidParamsError("prompt must not be empty")
	}
	if !filepath.IsAbs(value.Input.CWD) ||
		filepath.Clean(value.Input.CWD) != value.Input.CWD {
		return nil, invalidParamsError(
			"cwd must be an absolute normalized path",
		)
	}
	// cwd 是调用方输入：不存在或不是目录属于 invalid params，不能混入
	// executor 探测错误（EXECUTOR_NOT_INSTALLED）。
	if info, err := os.Stat(value.Input.CWD); err != nil || !info.IsDir() {
		return nil, invalidParamsError(
			"cwd %q must be an existing directory", value.Input.CWD,
		)
	}
	// probe 复用调用方 cwd：相对路径命令的可执行文件可能就位于该目录下；
	// 存在性已由上方检查保证，此处失败即为 executor 不可解析。
	command := *entry.Command
	command.CWD = value.Input.CWD
	if err := probeCommand(command, server.invocationBase); err != nil {
		return nil, executorUnavailableError(value.Executor, "EXECUTOR_NOT_INSTALLED")
	}
	sessionID := value.Input.SessionID
	if sessionID == "" {
		generated, err := session.NewID()
		if err != nil {
			return nil, internalError(err)
		}
		sessionID = generated
	} else if err := identity.Validate(sessionID, "session"); err != nil {
		return nil, invalidParamsError("sessionId: %v", err)
	}
	if value.Input.ResumeFromRunID != "" {
		if err := identity.Validate(value.Input.ResumeFromRunID, "run"); err != nil {
			return nil, invalidParamsError("resumeFromRunId: %v", err)
		}
		source, err := server.runs.Get(ctx, value.Input.ResumeFromRunID)
		switch {
		case errors.Is(err, runtime.ErrNotFound):
			return nil, invalidParamsError(
				"resumeFromRunId %q not found", value.Input.ResumeFromRunID,
			)
		case err != nil:
			return nil, internalError(err)
		}
		// AEP resume 只是同一 managed Session 的新 Turn/Run；不推断 Provider
		// 原生 resume identity（Runtime Session ID 与 Provider session 无关）。
		if source.Request.Kind != runtime.KindSession ||
			source.Request.SessionID == "" {
			return nil, invalidParamsError(
				"resumeFromRunId %q is not a session Run", value.Input.ResumeFromRunID,
			)
		}
		if value.Input.SessionID != "" &&
			value.Input.SessionID != source.Request.SessionID {
			return nil, invalidParamsError(
				"sessionId conflicts with resumeFromRunId session %s",
				source.Request.SessionID,
			)
		}
		sessionID = source.Request.SessionID
	}
	request := runtime.Request{
		Kind: runtime.KindSession, ProfileID: value.Profile,
		Input: value.Input.Prompt, SessionID: sessionID,
		SessionRetention: string(session.RetentionStandard),
		CWD:              value.Input.CWD,
		InvocationBase:   server.invocationBase,
	}
	if value.Input.Options != nil {
		request.Model = value.Input.Options.Model
		request.Effort = value.Input.Options.ReasoningEffort
		if value.Input.Options.TimeoutMS < 0 {
			return nil, invalidParamsError("timeoutMs must not be negative")
		}
		request.TimeoutMS = value.Input.Options.TimeoutMS
		if permissions := value.Input.Options.Permissions; permissions != nil {
			mode, err := runtimecommand.ParsePermissionMode(permissions.Mode)
			if err != nil {
				return nil, invalidParamsError("options.permissions: %v", err)
			}
			request.Permissions = string(mode)
		}
	}
	record, runtimeErr := server.runs.Submit(ctx, request)
	if runtimeErr != nil {
		return nil, errorFromRuntime(runtimeErr)
	}
	server.trackRun(record.ID, value.Executor)
	return runStartResult{
		RunID: record.ID, Status: projectRunStatus(record), SessionID: sessionID,
	}, nil
}

// handleRunCancel 执行 handleRunCancel 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 params 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 any、*rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) handleRunCancel(
	ctx context.Context,
	params json.RawMessage,
) (any, *rpcError) {
	runID, rpcErr := server.decodeRunID(params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if rpcErr := server.requireSessionRun(ctx, runID); rpcErr != nil {
		return nil, rpcErr
	}
	value, err := server.runs.Cancel(ctx, runID)
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		return nil, notFoundError("run %s not found", runID)
	case errors.Is(err, runtime.ErrConflict):
		return nil, conflictError("%s", err)
	case err != nil:
		return nil, internalError(err)
	}
	return runCancelResult{RunID: value.ID, Status: projectRunStatus(value)}, nil
}

// handleRunGet 执行 handleRunGet 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 params 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 any、*rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) handleRunGet(
	ctx context.Context,
	params json.RawMessage,
) (any, *rpcError) {
	runID, rpcErr := server.decodeRunID(params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	record, rpcErr := server.sessionRun(ctx, runID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	return RunView{
		RunID: record.ID, Executor: server.executorForRun(record),
		Status:    projectRunStatus(record),
		CreatedAt: formatTimestamp(record.CreatedAt),
		SessionID: record.Request.SessionID, Error: projectRunError(record),
	}, nil
}

// runResultAvailable 判定 Run 是否可返回 result：终态，或
// needs_reconciliation——后者按 D4 投影为 failed + reconciliation_required，
// run.result 与 run.get 保持同一投影，避免客户端拿到 failed 却永远取不到
// result。
// 参数 state 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func runResultAvailable(state runtime.State) bool {
	return state.Terminal() || state == runtime.StateNeedsReconciliation
}

// handleRunResult 执行 handleRunResult 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 params 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 any、*rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) handleRunResult(
	ctx context.Context,
	params json.RawMessage,
) (any, *rpcError) {
	runID, rpcErr := server.decodeRunID(params)
	if rpcErr != nil {
		return nil, rpcErr
	}
	record, rpcErr := server.sessionRun(ctx, runID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	if !runResultAvailable(record.State) {
		return nil, conflictError(
			"run %s is not terminal (status %s)", runID, projectRunStatus(record),
		)
	}
	shape := parseSessionResult(record)
	view := RunResultView{
		RunID: record.ID, Status: projectRunStatus(record),
		Artifacts: []ArtifactRef{}, ExitCode: shape.ExitCode,
		FinishedAt:   formatTimestamp(record.UpdatedAt),
		Completeness: "complete", Error: projectRunError(record),
	}
	if shape.Message != nil {
		view.Summary = shape.Message.Content
	}
	return view, nil
}

// handleRunEvents 执行 handleRunEvents 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 params 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 any、*rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) handleRunEvents(
	ctx context.Context,
	params json.RawMessage,
) (any, *rpcError) {
	var value runEventsParams
	if err := decodeParams(params, &value); err != nil {
		return nil, invalidParamsError("invalid params: %v", err)
	}
	if value.RunID == "" {
		return nil, invalidParamsError("runId is required")
	}
	limit := value.Limit
	if limit == 0 {
		limit = 100
	}
	if limit < 1 || limit > runtime.MaxListLimit {
		return nil, invalidParamsError(
			"limit must be between 1 and %d", runtime.MaxListLimit,
		)
	}
	record, rpcErr := server.sessionRun(ctx, value.RunID)
	if rpcErr != nil {
		return nil, rpcErr
	}
	executor := server.executorForRun(record)
	events, err := server.runs.Events(ctx, value.RunID, value.AfterSeq, limit)
	if err != nil {
		if errors.Is(err, runtime.ErrNotFound) {
			return nil, notFoundError("run %s not found", value.RunID)
		}
		return nil, internalError(err)
	}
	projected := make([]EventEnvelope, 0, len(events))
	for _, event := range events {
		envelope, ok := projectEvent(event, value.RunID, executor, server.now)
		if ok {
			projected = append(projected, envelope)
		}
	}
	return runEventsResult{Events: projected}, nil
}

// handleShutdown 执行 handleShutdown 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 params 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 any、*rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) handleShutdown(params json.RawMessage) (any, *rpcError) {
	var value struct{}
	if err := decodeParams(params, &value); err != nil {
		return nil, invalidParamsError("invalid params: %v", err)
	}
	server.stopOnce.Do(func() { close(server.stopCh) })
	return value, nil
}

// decodeRunID 执行 decodeRunID 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 params 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string、*rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) decodeRunID(params json.RawMessage) (string, *rpcError) {
	var value runIDParams
	if err := decodeParams(params, &value); err != nil {
		return "", invalidParamsError("invalid params: %v", err)
	}
	if value.RunID == "" {
		return "", invalidParamsError("runId is required")
	}
	// 语法非法的 runId 在 decode 层拒绝为 invalid params；Store 的 ID 校验
	// 错误否则会经 sessionRun 被误映射成 internal error。
	if err := identity.Validate(value.RunID, "run"); err != nil {
		return "", invalidParamsError("runId: %v", err)
	}
	return value.RunID, nil
}

// sessionRun 执行 sessionRun 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 runID 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 runtime.Record、*rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) sessionRun(
	ctx context.Context,
	runID string,
) (runtime.Record, *rpcError) {
	record, err := server.runs.Get(ctx, runID)
	switch {
	case errors.Is(err, runtime.ErrNotFound):
		return runtime.Record{}, notFoundError("run %s not found", runID)
	case err != nil:
		return runtime.Record{}, internalError(err)
	case record.Request.Kind != runtime.KindSession:
		return runtime.Record{}, conflictError(
			"AEP manages kind=session Runs only (run %s is %s)",
			runID, record.Request.Kind,
		)
	}
	return record, nil
}

// requireSessionRun 执行 requireSessionRun 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 runID 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *rpcError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) requireSessionRun(
	ctx context.Context,
	runID string,
) *rpcError {
	_, rpcErr := server.sessionRun(ctx, runID)
	return rpcErr
}

// executorForRun 从 Run 的 Profile command basename 推导 executor；Profile
// 已删除或不再是 CLI Profile 时返回空串，由 DTO 如实省略。
// 参数 record 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) executorForRun(record runtime.Record) string {
	entry, exists := server.profiles.Resolve(record.Request.ProfileID)
	if !exists || entry.Kind != profile.KindCommand || entry.Command == nil {
		return ""
	}
	executor, ok := executorForCommand(entry.Command.Command)
	if !ok {
		return ""
	}
	return executor
}

// isInitialized 执行 isInitialized 对应的包内处理，并遵循调用方提供的输入契约。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) isInitialized() bool {
	server.mutex.Lock()
	defer server.mutex.Unlock()
	return server.initialized
}

// trackRun 执行 trackRun 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 runID、executor 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (server *Server) trackRun(runID, executor string) {
	server.mutex.Lock()
	server.startedRuns[runID] = struct{}{}
	server.mutex.Unlock()
	server.spawnWatcher(runID, executor)
}

// spawnWatcher 订阅 durable journal 事件并按 seq 顺序推送 run.event 通知；
// Watch 在 terminal + settled 读到后返回。
// runWatcher 记录一个 Run 的事件 watcher；done 在 Watch 返回（终态事件已
// 投递或失败）后关闭，供关停路径 drain。
type runWatcher struct {
	cancel context.CancelFunc
	done   chan struct{}
}

// watcherDrainGrace 是 Run settle 后等待 watcher 读完 journal 并投递终态
// 事件的窗口；超时才强制取消，避免 DB state 与 journal 轮询的时序差丢事件。
const watcherDrainGrace = 2 * time.Second

// spawnWatcher 执行 spawnWatcher 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 runID、executor 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func (server *Server) spawnWatcher(runID, executor string) {
	// watcher 绑定 Serve 生命周期：Serve 返回前统一 drain 或取消。
	watchCtx, cancel := context.WithCancel(context.Background())
	watcher := runWatcher{cancel: cancel, done: make(chan struct{})}
	server.mutex.Lock()
	server.watchers[runID] = watcher
	server.mutex.Unlock()
	go func() {
		defer func() {
			close(watcher.done)
			cancel()
			server.mutex.Lock()
			delete(server.watchers, runID)
			server.mutex.Unlock()
		}()
		_, err := server.runs.Watch(watchCtx, runID, 0, func(event contract.Event) error {
			envelope, ok := projectEvent(event, runID, executor, server.now)
			if !ok {
				return nil
			}
			return server.writer.sendNotification("run.event", envelope)
		})
		if err != nil && watchCtx.Err() == nil &&
			!errors.Is(err, context.Canceled) {
			server.diag("aep: watch run %s stopped: %v", runID, err)
		}
	}()
}

// settleTrackedRuns 在关停时对本连接提交且仍非终态的 Run 走既有取消路径，
// 并在 ShutdownGrace 内等待终态；随后统一取消 watcher。
func (server *Server) settleTrackedRuns(ctx context.Context) {
	server.mutex.Lock()
	runIDs := make([]string, 0, len(server.startedRuns))
	for runID := range server.startedRuns {
		runIDs = append(runIDs, runID)
	}
	watchers := make([]runWatcher, 0, len(server.watchers))
	for _, watcher := range server.watchers {
		watchers = append(watchers, watcher)
	}
	server.mutex.Unlock()
	stopCtx, cancel := context.WithTimeout(
		context.WithoutCancel(ctx), server.shutdownGrace,
	)
	defer cancel()
	for _, runID := range runIDs {
		record, err := server.runs.Get(stopCtx, runID)
		if err != nil {
			server.diag("aep: shutdown inspect run %s: %v", runID, err)
			continue
		}
		if record.State.Terminal() {
			continue
		}
		if _, err := server.runs.Cancel(stopCtx, runID); err != nil {
			server.diag("aep: shutdown cancel run %s: %v", runID, err)
		}
	}
	deadline := time.Now().Add(server.shutdownGrace)
waitSettled:
	for time.Now().Before(deadline) {
		if server.trackedRunsSettled(stopCtx, runIDs) {
			break
		}
		timer := time.NewTimer(100 * time.Millisecond)
		select {
		case <-stopCtx.Done():
			// 宽限用尽即停止等待并取消 watcher；继续轮询只会对过期 ctx
			// 反复查询且拉长实际关停时间。
			timer.Stop()
			break waitSettled
		case <-timer.C:
		}
	}
	// Run 已 settle 后 watcher 只差读 journal（100ms 轮询）：先 drain 让终态
	// 事件完整投递给半关连接，仅 drain 超时才强制取消。
	drainDeadline := time.Now().Add(watcherDrainGrace)
	for _, watcher := range watchers {
		select {
		case <-watcher.done:
		case <-time.After(time.Until(drainDeadline)):
			watcher.cancel()
		}
	}
}

// trackedRunsSettled 执行 trackedRunsSettled 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 runIDs 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 bool 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) trackedRunsSettled(
	ctx context.Context,
	runIDs []string,
) bool {
	for _, runID := range runIDs {
		record, err := server.runs.Get(ctx, runID)
		if err != nil || !record.State.Terminal() {
			return false
		}
	}
	return true
}

// validateRequestID 单独校验 id 形状：存在时必须是字符串或数字，非法 id 的
// 错误响应按 JSON-RPC 规范回 null id。
// 参数 id 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func validateRequestID(id json.RawMessage) error {
	if len(id) == 0 {
		return nil
	}
	var value any
	decoder := json.NewDecoder(bytes.NewReader(id))
	decoder.UseNumber()
	if err := decoder.Decode(&value); err != nil {
		return fmt.Errorf("id must be a JSON string or number")
	}
	switch value.(type) {
	case string, json.Number:
		return nil
	default:
		return fmt.Errorf("id must be a JSON string or number")
	}
}
