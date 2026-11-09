package codingacp

import (
	"context"
	"encoding/json"
	"io"
	"strconv"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yy003x/sn-code/internal/application/codingagent"
	"github.com/yy003x/sn-code/internal/application/codingagent/tools"
	"github.com/yy003x/sn-code/internal/infrastructure/codingsessions"
	"github.com/yy003x/sn-code/pkg/agent"
	"github.com/yy003x/sn-code/pkg/contract"
)

func TestSessionCloseWaitsForClaimedWork(t *testing.T) {
	state := &acpSession{}
	if !state.claim(context.Background()) {
		t.Fatal("claim failed")
	}
	closed := make(chan struct{})
	go func() {
		state.close()
		close(closed)
	}()
	select {
	case <-closed:
		t.Fatal("close returned before claimed work completed")
	case <-time.After(20 * time.Millisecond):
	}
	state.promptWG.Done()
	select {
	case <-closed:
	case <-time.After(time.Second):
		t.Fatal("close did not finish after claimed work completed")
	}
	if state.claim(context.Background()) {
		t.Fatal("closed session accepted new work")
	}
}

// scriptedModel 按脚本顺序回放模型行为，每次 GenerateStream 消耗一步；
// block 步骤挂起等待取消，供 busy/cancel 测试使用。
type scriptedModel struct {
	mu    sync.Mutex
	steps []modelStep
}

type modelStep struct {
	delta  string
	result contract.ModelResult
	err    *contract.RuntimeError
	block  bool
}

func (model *scriptedModel) push(step modelStep) {
	model.mu.Lock()
	defer model.mu.Unlock()
	model.steps = append(model.steps, step)
}

func (model *scriptedModel) GenerateStream(
	ctx context.Context,
	_ contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	model.mu.Lock()
	if len(model.steps) == 0 {
		model.mu.Unlock()
		return contract.ModelResult{}, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseProvider,
			Message: "scripted model exhausted",
		}
	}
	step := model.steps[0]
	model.steps = model.steps[1:]
	model.mu.Unlock()
	if step.block {
		<-ctx.Done()
		return contract.ModelResult{}, &contract.RuntimeError{
			Code: contract.ErrorCancelled, Phase: contract.PhaseProvider,
			Message: "scripted model cancelled",
		}
	}
	if step.delta != "" && sink != nil {
		_ = sink(contract.Event{
			Type:  contract.EventContentDelta,
			Model: &contract.ModelEvent{Text: step.delta},
		})
	}
	if step.err != nil {
		return contract.ModelResult{}, step.err
	}
	return step.result, nil
}

type staticProfiles struct{ id string }

func (source staticProfiles) IDs() []string { return []string{source.id} }

func (source staticProfiles) Get(id string) (codingagent.ProfileInfo, bool) {
	if id != source.id {
		return codingagent.ProfileInfo{}, false
	}
	return codingagent.ProfileInfo{ID: id, ContextWindow: 128000}, true
}

// testFactory 用真实 AgentSession 内核组装会话，只有模型是脚本替身。
type testFactory struct {
	model    *scriptedModel
	registry *agent.Registry
	policy   map[string]tools.ToolPolicy
	store    *codingsessions.Store
}

func newTestFactory(t *testing.T) (*testFactory, *scriptedModel) {
	t.Helper()
	model := &scriptedModel{}
	registry, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name:        "write",
			Description: "test write tool",
			InputSchema: json.RawMessage(`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"]}`),
		},
		Handler: func(context.Context, agent.ToolRequest) (agent.ToolResult, error) {
			return agent.ToolResult{Content: "wrote file"}, nil
		},
		Effect: contract.EffectWriteLocal,
		Risk:   contract.RiskLow,
	})
	if err != nil {
		t.Fatalf("build registry: %v", err)
	}
	return &testFactory{
		model:    model,
		registry: registry,
		policy:   map[string]tools.ToolPolicy{"write": {Effect: contract.EffectWriteLocal, Risk: contract.RiskLow}},
		store:    codingsessions.NewStore(t.TempDir()),
	}, model
}

func (factory *testFactory) build(
	store *codingsessions.Session,
	gate codingagent.ToolGate,
	emit codingagent.EventSink,
	cwd string,
) (*codingagent.AgentSession, error) {
	return codingagent.New(codingagent.Deps{
		Model:    factory.model,
		Registry: factory.registry,
		Policy:   factory.policy,
		Profiles: staticProfiles{id: "primary"},
		Session:  store,
		Gate:     gate,
		Emit:     emit,
		CWD:      cwd,
	}, "primary")
}

func (factory *testFactory) NewSession(
	cwd string,
	gate codingagent.ToolGate,
	emit codingagent.EventSink,
) (*codingagent.AgentSession, error) {
	store, err := factory.store.Create(time.Now(), cwd, "")
	if err != nil {
		return nil, err
	}
	return factory.build(store, gate, emit, cwd)
}

func (factory *testFactory) LoadSession(
	cwd, sessionID string,
	gate codingagent.ToolGate,
	emit codingagent.EventSink,
) (*codingagent.AgentSession, error) {
	summaries, err := factory.store.List(cwd)
	if err != nil {
		return nil, err
	}
	for _, summary := range summaries {
		if summary.ID != sessionID {
			continue
		}
		store, err := factory.store.OpenStoreFile(summary.Path)
		if err != nil {
			return nil, err
		}
		return factory.build(store, gate, emit, cwd)
	}
	return nil, errSessionNotFound
}

var errSessionNotFound = &RequestError{
	Code: codeInvalidParams, Message: "session not found",
}

// acpClient 分拣服务器输出：响应、update 通知与 permission 请求各归其道。
type acpClient struct {
	t         *testing.T
	input     io.Writer
	lines     chan string
	responses chan map[string]json.RawMessage
	updates   chan map[string]json.RawMessage
	permits   chan map[string]json.RawMessage
	// stash 缓存乱序到达的响应（job 并发完成时顺序不定）；await 只在
	// 测试主 goroutine 调用，无需加锁。
	stash map[string]map[string]json.RawMessage
}

func newACPClient(t *testing.T, factory SessionFactory) *acpClient {
	t.Helper()
	reader, writer := io.Pipe()
	lines := make(chan string, 512)
	server := NewServer(&chanWriter{lines: lines}, Options{Version: "test", Factory: factory})
	done := make(chan error, 1)
	go func() { done <- server.Serve(context.Background(), reader) }()
	t.Cleanup(func() {
		_ = writer.Close()
		select {
		case <-done:
		case <-time.After(5 * time.Second):
			t.Fatalf("ACP server did not stop on stdin EOF")
		}
	})
	client := &acpClient{
		t: t, input: writer, lines: lines,
		responses: make(chan map[string]json.RawMessage, 64),
		updates:   make(chan map[string]json.RawMessage, 256),
		permits:   make(chan map[string]json.RawMessage, 16),
		stash:     map[string]map[string]json.RawMessage{},
	}
	go client.pump()
	return client
}

func (client *acpClient) pump() {
	for line := range client.lines {
		decoded := map[string]json.RawMessage{}
		if err := json.Unmarshal([]byte(line), &decoded); err != nil {
			client.t.Errorf("server output is not JSON: %q", line)
			return
		}
		switch string(decoded["method"]) {
		case `"session/update"`:
			client.updates <- decoded
		case `"session/request_permission"`:
			client.permits <- decoded
		default:
			client.responses <- decoded
		}
	}
}

func (client *acpClient) request(id, method string, params any) map[string]json.RawMessage {
	client.t.Helper()
	client.send(id, method, params)
	return client.await(id)
}

// send 只写请求行，不等待响应；供后台 goroutine 使用（testing 禁止在
// 非 testing goroutine 中 FailNow）。
func (client *acpClient) send(id, method string, params any) {
	if err := writeJSONLine(client.input, map[string]any{
		"jsonrpc": "2.0", "id": id, "method": method, "params": params,
	}); err != nil {
		client.t.Errorf("write %s: %v", method, err)
	}
}

func (client *acpClient) await(id string) map[string]json.RawMessage {
	client.t.Helper()
	if cached, ok := client.stash[id]; ok {
		delete(client.stash, id)
		return cached
	}
	deadline := time.After(5 * time.Second)
	for {
		select {
		case response := <-client.responses:
			var gotID string
			_ = json.Unmarshal(response["id"], &gotID)
			if gotID == id {
				return response
			}
			client.stash[gotID] = response
		case <-deadline:
			client.t.Fatalf("timed out waiting for response %q", id)
		}
	}
}

func (client *acpClient) notify(method string, params any) {
	client.t.Helper()
	if err := writeJSONLine(client.input, map[string]any{
		"jsonrpc": "2.0", "method": method, "params": params,
	}); err != nil {
		client.t.Fatalf("write %s: %v", method, err)
	}
}

// nextUpdate 取下一条 session/update 通知并解码 update 载荷。
func (client *acpClient) nextUpdate() map[string]any {
	client.t.Helper()
	select {
	case notification := <-client.updates:
		var params struct {
			Update map[string]any `json:"update"`
		}
		if err := json.Unmarshal(notification["params"], &params); err != nil {
			client.t.Fatalf("update params invalid: %v", err)
		}
		return params.Update
	case <-time.After(5 * time.Second):
		client.t.Fatalf("timed out waiting for session/update")
		return nil
	}
}

// drainUpdates 读取全部剩余 update（本轮更新都先于 prompt 响应）。
func (client *acpClient) drainUpdates() []map[string]any {
	client.t.Helper()
	var values []map[string]any
	for {
		select {
		case <-client.updates:
			continue
		default:
			return values
		}
	}
}

// answerPermission 应答一条待处理的权限请求。
func (client *acpClient) answerPermission(optionId string) {
	client.t.Helper()
	select {
	case request := <-client.permits:
		var id string
		if err := json.Unmarshal(request["id"], &id); err != nil {
			client.t.Fatalf("permission request id invalid: %v", err)
		}
		if err := writeJSONLine(client.input, map[string]any{
			"jsonrpc": "2.0", "id": id,
			"result": map[string]any{
				"outcome": map[string]any{"kind": "selected", "optionId": optionId},
			},
		}); err != nil {
			client.t.Fatalf("write permission answer: %v", err)
		}
	case <-time.After(5 * time.Second):
		client.t.Fatalf("timed out waiting for session/request_permission")
	}
}

// initialize 完成握手并断言一期能力声明。
func (client *acpClient) initialize() {
	client.t.Helper()
	response := client.request("init-1", "initialize", map[string]any{
		"protocolVersion":    1,
		"clientCapabilities": map[string]any{},
	})
	if string(response["error"]) != "" {
		client.t.Fatalf("initialize failed: %v", string(response["error"]))
	}
	var result struct {
		ProtocolVersion int `json:"protocolVersion"`
		AgentInfo       struct {
			Name string `json:"name"`
		} `json:"agentInfo"`
		AgentCapabilities struct {
			LoadSession bool `json:"loadSession"`
		} `json:"agentCapabilities"`
		AuthMethods []any `json:"authMethods"`
	}
	if err := json.Unmarshal(response["result"], &result); err != nil {
		client.t.Fatalf("initialize result invalid: %v", err)
	}
	if result.ProtocolVersion != 1 || result.AgentInfo.Name != "sn-code" {
		client.t.Fatalf("initialize result = %+v", result)
	}
	if !result.AgentCapabilities.LoadSession || len(result.AuthMethods) != 0 {
		client.t.Fatalf("capabilities not declared honestly: %+v", result)
	}
}

func (client *acpClient) newSession(cwd string) string {
	client.t.Helper()
	response := client.request("new-1", "session/new", map[string]any{"cwd": cwd})
	if string(response["error"]) != "" {
		client.t.Fatalf("session/new failed: %v", string(response["error"]))
	}
	var result struct {
		SessionId string `json:"sessionId"`
	}
	if err := json.Unmarshal(response["result"], &result); err != nil || result.SessionId == "" {
		client.t.Fatalf("session/new result invalid: %v %q", err, string(response["result"]))
	}
	return result.SessionId
}

func stopReason(response map[string]json.RawMessage) string {
	var result struct {
		StopReason string `json:"stopReason"`
	}
	_ = json.Unmarshal(response["result"], &result)
	return result.StopReason
}

func errorCode(response map[string]json.RawMessage) int64 {
	var value struct {
		Code int64 `json:"code"`
	}
	_ = json.Unmarshal(response["error"], &value)
	return value.Code
}

func errorReason(response map[string]json.RawMessage) string {
	var value struct {
		Data struct {
			Reason string `json:"reason"`
		} `json:"data"`
	}
	_ = json.Unmarshal(response["error"], &value)
	return value.Data.Reason
}

func textPrompt(text string) []map[string]any {
	return []map[string]any{{"type": "text", "text": text}}
}

func TestSessionMethodsRequireInitialize(t *testing.T) {
	factory, _ := newTestFactory(t)
	client := newACPClient(t, factory)
	response := client.request("p-1", "session/prompt", map[string]any{
		"sessionId": "any", "prompt": textPrompt("hi"),
	})
	if errorCode(response) != codeInvalidRequest {
		t.Fatalf("code = %d, want invalid request", errorCode(response))
	}
}

func TestInitializeTwiceRejected(t *testing.T) {
	factory, _ := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	response := client.request("init-2", "initialize", map[string]any{"protocolVersion": 1})
	if errorCode(response) != codeInvalidRequest {
		t.Fatalf("code = %d, want invalid request", errorCode(response))
	}
}

func TestSessionNewValidatesCwdAndUnsupportedInputs(t *testing.T) {
	cwd := t.TempDir()
	cases := []struct {
		name   string
		params map[string]any
	}{
		{"relative cwd", map[string]any{"cwd": "relative/path"}},
		{"missing cwd", map[string]any{}},
		{"nonexistent cwd", map[string]any{"cwd": "/definitely/not/a/directory"}},
		{"mcp servers", map[string]any{"cwd": cwd, "mcpServers": []any{map[string]any{}}}},
		{"additional directories", map[string]any{"cwd": cwd, "additionalDirectories": []any{cwd}}},
	}
	for _, testCase := range cases {
		factory, _ := newTestFactory(t)
		client := newACPClient(t, factory)
		client.initialize()
		response := client.request("new-1", "session/new", testCase.params)
		if errorCode(response) != codeInvalidParams {
			t.Fatalf("%s: code = %d (%s), want invalid params",
				testCase.name, errorCode(response), errorReason(response))
		}
	}
}

func TestPromptUnknownSessionRejected(t *testing.T) {
	factory, _ := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	response := client.request("p-1", "session/prompt", map[string]any{
		"sessionId": "missing", "prompt": textPrompt("hi"),
	})
	if errorCode(response) != codeInvalidParams {
		t.Fatalf("code = %d, want invalid params", errorCode(response))
	}
}

func TestPromptRejectsNonTextBlocks(t *testing.T) {
	factory, _ := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	sessionID := client.newSession(t.TempDir())
	response := client.request("p-1", "session/prompt", map[string]any{
		"sessionId": sessionID,
		"prompt":    []any{map[string]any{"type": "image", "image": map[string]any{"data": "eg", "mimeType": "image/png"}}},
	})
	if errorCode(response) != codeInvalidParams {
		t.Fatalf("code = %d, want invalid params", errorCode(response))
	}
}

func TestPromptStreamsTextAndEndsTurn(t *testing.T) {
	factory, model := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	sessionID := client.newSession(t.TempDir())
	model.push(modelStep{
		delta:  "hello world",
		result: contract.ModelResult{Message: contract.Message{Role: contract.RoleAssistant, Content: "hello world"}, FinishReason: contract.FinishStop},
	})
	response := client.request("p-1", "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": textPrompt("say hi"),
	})
	if string(response["error"]) != "" {
		t.Fatalf("prompt failed: %v", string(response["error"]))
	}
	update := client.nextUpdate()
	if update["sessionUpdate"] != "agent_message_chunk" {
		t.Fatalf("first update = %v", update)
	}
	if update["content"].(map[string]any)["text"] != "hello world" {
		t.Fatalf("chunk content = %v", update["content"])
	}
	if reason := stopReason(response); reason != stopEndTurn {
		t.Fatalf("stopReason = %q, want end_turn", reason)
	}
	if extra := client.drainUpdates(); len(extra) != 0 {
		t.Fatalf("unexpected extra updates: %v", extra)
	}
}

func TestPromptSplitsLargeChunks(t *testing.T) {
	factory, model := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	sessionID := client.newSession(t.TempDir())
	long := strings.Repeat("ab", chunkBytes/2+128)
	model.push(modelStep{
		delta:  long,
		result: contract.ModelResult{Message: contract.Message{Role: contract.RoleAssistant, Content: long}, FinishReason: contract.FinishStop},
	})
	response := client.request("p-1", "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": textPrompt("go long"),
	})
	if string(response["error"]) != "" {
		t.Fatalf("prompt failed: %v", string(response["error"]))
	}
	first := client.nextUpdate()
	second := client.nextUpdate()
	for _, update := range []map[string]any{first, second} {
		if update["sessionUpdate"] != "agent_message_chunk" {
			t.Fatalf("update = %v", update)
		}
	}
	firstText := first["content"].(map[string]any)["text"].(string)
	secondText := second["content"].(map[string]any)["text"].(string)
	if len(firstText) != chunkBytes || len(firstText+secondText) != len(long) {
		t.Fatalf("chunk split lengths = %d + %d, total %d", len(firstText), len(secondText), len(long))
	}
}

func TestPromptReportsMaxTokens(t *testing.T) {
	factory, model := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	sessionID := client.newSession(t.TempDir())
	model.push(modelStep{
		delta:  "cut short",
		result: contract.ModelResult{Message: contract.Message{Role: contract.RoleAssistant, Content: "cut short"}, FinishReason: contract.FinishLength},
	})
	response := client.request("p-1", "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": textPrompt("hi"),
	})
	if reason := stopReason(response); reason != stopMaxTokens {
		t.Fatalf("stopReason = %q, want max_tokens", reason)
	}
}

func TestPromptMapsModelFailureToRPCError(t *testing.T) {
	factory, model := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	sessionID := client.newSession(t.TempDir())
	model.push(modelStep{err: &contract.RuntimeError{
		Code: contract.ErrorAuthenticationFailed, Phase: contract.PhaseProvider,
		Message: "provider rejected the request",
	}})
	response := client.request("p-1", "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": textPrompt("hi"),
	})
	if errorCode(response) != codeInternalError {
		t.Fatalf("code = %d, want internal error", errorCode(response))
	}
	var value struct {
		Data struct {
			Code   string `json:"code"`
			Detail string `json:"detail"`
		} `json:"data"`
	}
	_ = json.Unmarshal(response["error"], &value)
	if value.Data.Code != "model_request_failed" || value.Data.Detail != "provider rejected the request" {
		t.Fatalf("error data = %+v", value.Data)
	}
}

func TestPromptBusyThenCancelEndsCancelled(t *testing.T) {
	factory, model := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	sessionID := client.newSession(t.TempDir())
	model.push(modelStep{block: true})

	// 第一个 prompt 占据会话并阻塞在模型上；随后第二个 prompt 必须收到
	// busy 错误，cancel 后第一个以 cancelled 收尾。
	client.send("p-1", "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": textPrompt("first"),
	})
	time.Sleep(100 * time.Millisecond)
	busy := client.request("p-2", "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": textPrompt("second"),
	})
	if errorCode(busy) != codeInvalidParams || !strings.Contains(errorReason(busy), "busy") {
		t.Fatalf("busy error = %d %q", errorCode(busy), errorReason(busy))
	}
	client.notify("session/cancel", map[string]any{"sessionId": sessionID})
	response := client.await("p-1")
	if reason := stopReason(response); reason != stopCancelled {
		t.Fatalf("stopReason = %q, want cancelled", reason)
	}
}

func TestSessionLimitPerConnection(t *testing.T) {
	factory, _ := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	for index := range maxSessionsPerConnection {
		if err := writeJSONLine(client.input, map[string]any{
			"jsonrpc": "2.0", "id": "n-" + strconv.Itoa(index), "method": "session/new",
			"params": map[string]any{"cwd": t.TempDir()},
		}); err != nil {
			t.Fatalf("write session/new: %v", err)
		}
	}
	// 排空前 32 个成功响应后，第 33 个必须被上限拒绝。
	for index := range maxSessionsPerConnection {
		_ = client.await("n-" + strconv.Itoa(index))
	}
	if err := writeJSONLine(client.input, map[string]any{
		"jsonrpc": "2.0", "id": "n-over", "method": "session/new",
		"params": map[string]any{"cwd": t.TempDir()},
	}); err != nil {
		t.Fatalf("write overflow session/new: %v", err)
	}
	response := client.await("n-over")
	if errorCode(response) != codeInvalidParams {
		t.Fatalf("code = %d, want invalid params", errorCode(response))
	}
}

func TestSessionLoadReplaysHistoryAndContinues(t *testing.T) {
	factory, model := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	cwd := t.TempDir()
	sessionID := client.newSession(cwd)
	model.push(modelStep{
		delta:  "first answer",
		result: contract.ModelResult{Message: contract.Message{Role: contract.RoleAssistant, Content: "first answer"}, FinishReason: contract.FinishStop},
	})
	if response := client.request("p-1", "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": textPrompt("first question"),
	}); stopReason(response) != stopEndTurn {
		t.Fatalf("first prompt stopReason = %q", stopReason(response))
	}
	client.drainUpdates()
	model.push(modelStep{
		delta:  "second answer",
		result: contract.ModelResult{Message: contract.Message{Role: contract.RoleAssistant, Content: "second answer"}, FinishReason: contract.FinishStop},
	})
	// load 同一会话：先回放历史，再允许续跑。响应晚于全部回放更新到达，
	// 所以响应到手后直接排空 update 通道即可。
	loadResponse := client.request("load-1", "session/load", map[string]any{
		"sessionId": sessionID, "cwd": cwd,
	})
	if string(loadResponse["error"]) != "" {
		t.Fatalf("session/load failed: %v", string(loadResponse["error"]))
	}
	var replayed []string
	for {
		select {
		case notification := <-client.updates:
			var params struct {
				Update struct {
					SessionUpdate string `json:"sessionUpdate"`
					Content       struct {
						Text string `json:"text"`
					} `json:"content"`
				} `json:"update"`
			}
			if err := json.Unmarshal(notification["params"], &params); err != nil {
				t.Fatalf("replay update invalid: %v", err)
			}
			replayed = append(replayed, params.Update.SessionUpdate+":"+params.Update.Content.Text)
		default:
			goto drained
		}
	}
drained:
	want := []string{"user_message_chunk:first question", "agent_message_chunk:first answer"}
	if len(replayed) != len(want) {
		t.Fatalf("replayed = %v, want %v", replayed, want)
	}
	for index := range want {
		if replayed[index] != want[index] {
			t.Fatalf("replay[%d] = %q, want %q", index, replayed[index], want[index])
		}
	}
	if response := client.request("p-2", "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": textPrompt("second question"),
	}); stopReason(response) != stopEndTurn {
		t.Fatalf("second prompt stopReason = %q", stopReason(response))
	}
}

func TestSessionLoadUnknownIDRejected(t *testing.T) {
	factory, _ := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	response := client.request("load-1", "session/load", map[string]any{
		"sessionId": "missing", "cwd": t.TempDir(),
	})
	// 测试工厂把未知会话表达为 invalid params 协议错误并原样透传。
	if errorCode(response) != codeInvalidParams {
		t.Fatalf("code = %d, want invalid params", errorCode(response))
	}
}
