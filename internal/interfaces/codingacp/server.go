package codingacp

import (
	"context"
	"encoding/json"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"time"

	"github.com/yy003x/sn-code/internal/application/codingagent"
)

// maxSessionsPerConnection 限制单条 ACP 连接的并发会话数。
const maxSessionsPerConnection = 32

// SessionFactory 按 cwd 建立会话：新会话落盘到该 cwd 的会话目录；恢复
// 会话按完整会话 ID 在该 cwd 下精确匹配。由 composition root 适配注入。
type SessionFactory interface {
	// NewSession 执行 NewSession 对应的包内处理，并遵循调用方提供的输入契约。
	// 参数 cwd、gate、emit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
	// 返回 *codingagent.AgentSession、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	NewSession(cwd string, gate codingagent.ToolGate, emit codingagent.EventSink) (*codingagent.AgentSession, error)
	// LoadSession 执行 LoadSession 对应的包内处理，并遵循调用方提供的输入契约。
	// 参数 cwd、sessionID、gate、emit 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
	// 返回 *codingagent.AgentSession、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
	LoadSession(cwd, sessionID string, gate codingagent.ToolGate, emit codingagent.EventSink) (*codingagent.AgentSession, error)
}

// Options 隔离传输与模型/工作区初始化：版本串与会话工厂。
type Options struct {
	Version string
	Factory SessionFactory
}

// Server 拥有单条本地 ACP 连接的全部独立会话。
type Server struct {
	mu          sync.Mutex
	options     Options
	transport   *transport
	initialized bool
	closed      bool
	creating    int
	sessions    map[string]*acpSession
	closeOnce   sync.Once
}

// NewServer 执行 NewServer 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 output、options 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *Server 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func NewServer(output io.Writer, options Options) *Server {
	server := &Server{options: options, sessions: map[string]*acpSession{}}
	server.transport = newTransport(output, server.admit)
	return server
}

// Serve 执行 Serve 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 input 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) Serve(ctx context.Context, input io.Reader) error {
	defer server.Close()
	return server.transport.Serve(ctx, input)
}

// Close 取消本连接的全部运行并等待收尾；不再使用的会话文件随之关闭。
func (server *Server) Close() {
	server.closeOnce.Do(func() {
		server.transport.Close()
		server.mu.Lock()
		server.closed = true
		states := make([]*acpSession, 0, len(server.sessions))
		for _, state := range server.sessions {
			states = append(states, state)
		}
		server.sessions = map[string]*acpSession{}
		server.mu.Unlock()
		var closing sync.WaitGroup
		for _, state := range states {
			closing.Add(1)
			go func(state *acpSession) { defer closing.Done(); state.close() }(state)
		}
		closing.Wait()
	})
}

// findSession 执行 findSession 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 id 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *acpSession、*RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) findSession(id string) (*acpSession, *RequestError) {
	server.mu.Lock()
	defer server.mu.Unlock()
	state := server.sessions[id]
	if id == "" || state == nil {
		return nil, invalid("unknown sessionId for this connection")
	}
	return state, nil
}

// admit 在传输接收顺序中串行执行：prompt 的归属与取消在 worker 调度
// 可能重排消息之前，就在这里确立。
// 参数 method、raw 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 admittedCall、*RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) admit(ctx context.Context, method string, raw json.RawMessage) (admittedCall, *RequestError) {
	if method == "initialize" {
		return server.admitInitialize(raw)
	}
	server.mu.Lock()
	initialized, closed := server.initialized, server.closed
	server.mu.Unlock()
	if !initialized || closed {
		return nil, NewInvalidRequest(map[string]any{
			"reason": "initialize the connection before calling session methods",
		})
	}
	switch method {
	case "session/new":
		request, err := decode[newSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		if len(request.AdditionalDirectories) != 0 {
			return nil, invalid("additionalDirectories is not supported")
		}
		if len(request.McpServers) != 0 {
			return nil, invalid("client MCP servers are not supported")
		}
		return server.admitSession(ctx, request.Cwd, "", false)
	case "session/load":
		request, err := decode[loadSessionRequest](raw)
		if err != nil {
			return nil, err
		}
		if request.SessionId == "" {
			return nil, invalid("sessionId is required")
		}
		if len(request.AdditionalDirectories) != 0 {
			return nil, invalid("additionalDirectories is not supported")
		}
		if len(request.McpServers) != 0 {
			return nil, invalid("client MCP servers are not supported")
		}
		return server.admitSession(ctx, request.Cwd, request.SessionId, true)
	case "session/prompt":
		request, err := decode[promptRequest](raw)
		if err != nil {
			return nil, err
		}
		state, err := server.findSession(request.SessionId)
		if err != nil {
			return nil, err
		}
		text, err := promptText(request.Prompt)
		if err != nil {
			return nil, err
		}
		if !state.claim(ctx) {
			return nil, invalid("session is busy; wait for its prompt response or send session/cancel")
		}
		return func(context.Context) (any, *RequestError) {
			defer state.promptWG.Done()
			return state.prompt(text)
		}, nil
	case "session/cancel":
		request, err := decode[cancelRequest](raw)
		if err != nil {
			return nil, err
		}
		if state, _ := server.findSession(request.SessionId); state != nil {
			state.cancelRun()
		}
		return nil, nil
	default:
		return nil, NewMethodNotFound(method)
	}
}

// admitInitialize 执行 admitInitialize 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 raw 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 admittedCall、*RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) admitInitialize(raw json.RawMessage) (admittedCall, *RequestError) {
	request, err := decode[initializeRequest](raw)
	if err != nil {
		return nil, err
	}
	if request.ProtocolVersion == 0 {
		return nil, invalid("protocolVersion is required")
	}
	server.mu.Lock()
	if server.initialized || server.closed {
		server.mu.Unlock()
		return nil, NewInvalidRequest(map[string]any{
			"reason": "connection already initialized or closed",
		})
	}
	server.initialized = true
	version := server.options.Version
	server.mu.Unlock()
	return func(context.Context) (any, *RequestError) {
		// 认证即本机 active home 下的 API Profile 配置，无需独立认证方法；
		// 图像与嵌入上下文按一期实际能力如实声明为不支持。
		return initializeResult{
			ProtocolVersion: 1,
			AgentInfo: agentInfo{
				Name: "sn-code", Title: "SN Code", Version: version,
			},
			AgentCapabilities: agentCapabilities{
				LoadSession: true,
				PromptCapabilities: promptCapabilities{
					Image: false, EmbeddedContext: false,
				},
			},
			AuthMethods: []authMethod{},
		}, nil
	}, nil
}

// normalizeCwd 要求绝对、可访问且真实存在的目录，并解析符号链接。
// 参数 cwd 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string、*RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func normalizeCwd(cwd string) (string, *RequestError) {
	if !filepath.IsAbs(cwd) || strings.ContainsRune(cwd, 0) {
		return "", invalid("cwd must be an absolute directory path")
	}
	resolved, err := filepath.EvalSymlinks(cwd)
	if err != nil {
		return "", invalid("cwd must be an existing accessible directory")
	}
	info, err := os.Stat(resolved)
	if err != nil || !info.IsDir() {
		return "", invalid("cwd must be an existing directory")
	}
	return filepath.Clean(resolved), nil
}

// admitSession 执行 admitSession 对应的包内处理，并遵循调用方提供的输入契约。
// 参数 cwd、sessionID、load 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 admittedCall、*RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (server *Server) admitSession(ctx context.Context, cwd, sessionID string, load bool) (admittedCall, *RequestError) {
	normalized, err := normalizeCwd(cwd)
	if err != nil {
		return nil, err
	}
	server.mu.Lock()
	if load {
		if existing := server.sessions[sessionID]; existing != nil {
			server.mu.Unlock()
			if existing.cwd != normalized {
				return nil, invalid("an active session cannot change cwd; reconnect to load it in another directory")
			}
			if !existing.claim(ctx) {
				return nil, invalid("session is busy")
			}
			return func(context.Context) (any, *RequestError) {
				defer existing.promptWG.Done()
				defer existing.release()
				if err := existing.replay(); err != nil {
					return nil, internalFailure("session_replay_failed")
				}
				return loadSessionResult{}, nil
			}, nil
		}
	}
	if server.options.Factory == nil || len(server.sessions)+server.creating >= maxSessionsPerConnection {
		server.mu.Unlock()
		return nil, invalid("session factory unavailable or connection session limit reached")
	}
	server.creating++
	server.mu.Unlock()
	return func(ctx context.Context) (any, *RequestError) {
		defer func() {
			server.mu.Lock()
			server.creating--
			server.mu.Unlock()
		}()
		if ctx.Err() != nil {
			return nil, NewRequestCancelled(nil)
		}
		// gate 与 emit 先绑定到会话状态骨架，再建立 AgentSession，使
		// permission 与事件流从一开始就有关联载体。
		state := &acpSession{server: server, cwd: normalized}
		state.gate = &acpGate{state: state}
		var agent *codingagent.AgentSession
		var factoryErr error
		if load {
			agent, factoryErr = server.options.Factory.LoadSession(normalized, sessionID, state.gate, state.handleEvent)
		} else {
			agent, factoryErr = server.options.Factory.NewSession(normalized, state.gate, state.handleEvent)
		}
		if factoryErr != nil {
			var wire *RequestError
			if errors.As(factoryErr, &wire) {
				return nil, wire
			}
			return nil, internalFailure("session_creation_failed")
		}
		if ctx.Err() != nil {
			_ = agent.Close()
			return nil, NewRequestCancelled(nil)
		}
		state.mu.Lock()
		state.session, state.id = agent, agent.SessionID()
		state.mu.Unlock()
		if load && state.id != sessionID {
			_ = agent.Close()
			return nil, internalFailure("session_identity_mismatch")
		}
		server.mu.Lock()
		rejected := server.closed || server.sessions[state.id] != nil
		if !rejected {
			server.sessions[state.id] = state
		}
		server.mu.Unlock()
		if rejected {
			_ = agent.Close()
			return nil, invalid("session is already active or connection is closed")
		}
		if load {
			if !state.claim(ctx) {
				return nil, invalid("session is busy")
			}
			defer state.promptWG.Done()
			defer state.release()
			if err := state.replay(); err != nil {
				return nil, internalFailure("session_replay_failed")
			}
			return loadSessionResult{}, nil
		}
		return newSessionResult{SessionId: state.id}, nil
	}, nil
}

// requestFailure 保留已是协议错误的失败，其余归并为内部错误。
// 参数 err、code 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 *RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func requestFailure(err error, code string) *RequestError {
	var wire *RequestError
	if errors.As(err, &wire) {
		return wire
	}
	return internalFailure(code)
}

// promptText 把内容块收敛为单个文本提示；一期只接受文本块。
// 参数 blocks 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string、*RequestError 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func promptText(blocks []contentBlock) (string, *RequestError) {
	if len(blocks) == 0 || len(blocks) > 256 {
		return "", invalid("prompt must contain between 1 and 256 content blocks")
	}
	var parts []string
	total := 0
	for _, block := range blocks {
		value, ok := textBlockText(block.Text)
		switch {
		case ok:
			total += len(value)
			if total > maxPromptTextBytes {
				return "", invalid("prompt text exceeds 16 MiB")
			}
			parts = append(parts, value)
		case block.Image != nil || block.Type == "image":
			return "", invalid("image content blocks are not supported")
		default:
			return "", invalid("only text content blocks are supported")
		}
	}
	joined := strings.Join(parts, "\n\n")
	if strings.TrimSpace(joined) == "" {
		return "", invalid("prompt must contain non-empty text")
	}
	return joined, nil
}

// closeTimeout 限制单会话收尾等待；顽固 run 由进程退出兜底。
const closeTimeout = 10 * time.Second
