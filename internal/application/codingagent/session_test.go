package codingagent

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yy003x/sn-code/internal/application/codingagent/tools"
	"github.com/yy003x/sn-code/internal/infrastructure/codingsessions"
	"github.com/yy003x/sn-code/pkg/agent"
	"github.com/yy003x/sn-code/pkg/contract"
)

func TestToolResultWriteFailureEmitsOnlyErrorTerminalEvent(t *testing.T) {
	fixture := newFixture(t, 200000, []scriptedResponse{
		{result: toolCallResult(contract.ToolCall{ID: "c1", Name: "close_store", Arguments: json.RawMessage(`{}`)})},
	})
	if err := fixture.session.registry.Register(agent.RegisteredTool{
		Definition: contract.ToolSpec{Name: "close_store", InputSchema: json.RawMessage(`{"type":"object"}`)},
		Handler: func(context.Context, agent.ToolRequest) (agent.ToolResult, error) {
			if err := fixture.store.Close(); err != nil {
				t.Fatalf("close store: %v", err)
			}
			return agent.ToolResult{Content: "done"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	fixture.session.Run(context.Background(), "run tool")
	events := fixture.recorder.all()
	finished := make([]Event, 0, 1)
	for _, event := range events {
		if event.Type == EventRunFinished {
			finished = append(finished, event)
		}
	}
	if len(finished) != 1 || finished[0].Text != "error" {
		t.Fatalf("terminal events = %+v", finished)
	}
	if fixture.recorder.count(EventModelError) != 1 {
		t.Fatalf("model error events = %+v", events)
	}
}

// scriptedModel 按脚本顺序回放模型响应，记录收到的请求。
type scriptedModel struct {
	mu        sync.Mutex
	requests  []contract.GenerateRequest
	script    []scriptedResponse
	callIndex int
}

type scriptedResponse struct {
	deltas []string
	result contract.ModelResult
	err    *contract.RuntimeError
}

func (model *scriptedModel) GenerateStream(
	ctx context.Context,
	request contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	model.mu.Lock()
	index := model.callIndex
	model.callIndex++
	model.requests = append(model.requests, request)
	model.mu.Unlock()
	if index >= len(model.script) {
		return contract.ModelResult{}, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseRequest,
			Message: "script exhausted",
		}
	}
	response := model.script[index]
	for _, delta := range response.deltas {
		if err := sink(contract.Event{
			Type:  contract.EventContentDelta,
			Model: &contract.ModelEvent{Text: delta},
		}); err != nil {
			return contract.ModelResult{}, &contract.RuntimeError{
				Code: contract.ErrorCancelled, Phase: contract.PhaseConsumer, Message: err.Error(),
			}
		}
	}
	if response.err != nil {
		return contract.ModelResult{Message: response.result.Message}, response.err
	}
	return response.result, nil
}

func (model *scriptedModel) requestCount() int {
	model.mu.Lock()
	defer model.mu.Unlock()
	return len(model.requests)
}

func (model *scriptedModel) request(index int) contract.GenerateRequest {
	model.mu.Lock()
	defer model.mu.Unlock()
	return model.requests[index]
}

// fakeProfiles 提供 ProfileSource 的固定视图。
type fakeProfiles struct {
	entries map[string]ProfileInfo
}

func (profiles fakeProfiles) IDs() []string {
	ids := make([]string, 0, len(profiles.entries))
	for id := range profiles.entries {
		ids = append(ids, id)
	}
	return ids
}

func (profiles fakeProfiles) Get(id string) (ProfileInfo, bool) {
	info, ok := profiles.entries[id]
	return info, ok
}

// recordingGate 按队列回放确认决定并记录被确认的调用。
type recordingGate struct {
	mu         sync.Mutex
	decisions  []GateDecision
	confirmed  []string
	alwaysSeen map[string]bool
}

func (gate *recordingGate) Confirm(ctx context.Context, call ToolCallInfoForGate) GateDecision {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	if gate.alwaysSeen == nil {
		gate.alwaysSeen = map[string]bool{}
	}
	decision := GateDeny
	if len(gate.decisions) > 0 {
		decision = gate.decisions[0]
		gate.decisions = gate.decisions[1:]
	}
	gate.confirmed = append(gate.confirmed, call.Name)
	if decision == GateAlways {
		gate.alwaysSeen[call.Name] = true
	}
	return decision
}

func (gate *recordingGate) confirmedNames() []string {
	gate.mu.Lock()
	defer gate.mu.Unlock()
	return append([]string(nil), gate.confirmed...)
}

// eventRecorder 收集应用事件。
type eventRecorder struct {
	mu     sync.Mutex
	events []Event
}

func (recorder *eventRecorder) emit(event Event) error {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	recorder.events = append(recorder.events, event)
	return nil
}

func (recorder *eventRecorder) all() []Event {
	recorder.mu.Lock()
	defer recorder.mu.Unlock()
	return append([]Event(nil), recorder.events...)
}

func (recorder *eventRecorder) count(eventType EventType) int {
	total := 0
	for _, event := range recorder.all() {
		if event.Type == eventType {
			total++
		}
	}
	return total
}

type fixture struct {
	session  *AgentSession
	store    *codingsessions.Session
	model    *scriptedModel
	gate     *recordingGate
	recorder *eventRecorder
	cwd      string
}

func newFixture(t *testing.T, window int64, script []scriptedResponse) *fixture {
	t.Helper()
	cwd := t.TempDir()
	store, err := codingsessions.NewStore(t.TempDir()).Create(time.Now(), cwd, "")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	model := &scriptedModel{script: script}
	gate := &recordingGate{}
	recorder := &eventRecorder{}
	registry, err := tools.Registry()
	if err != nil {
		t.Fatalf("tool registry: %v", err)
	}
	session, err := New(Deps{
		Model:    model,
		Registry: registry,
		Policy:   tools.Policy(),
		Profiles: fakeProfiles{entries: map[string]ProfileInfo{
			"p1": {ID: "p1", Driver: "anthropic-messages", Model: "m1", ContextWindow: window, ReservedOut: 8192},
			"p2": {ID: "p2", Driver: "openai-chat-completions", Model: "m2", ContextWindow: window, ReservedOut: 4096},
		}},
		Session:  store,
		Gate:     gate,
		Emit:     recorder.emit,
		CWD:      cwd,
		AgentDir: t.TempDir(),
	}, "p1")
	if err != nil {
		t.Fatalf("new agent session: %v", err)
	}
	return &fixture{
		session:  session,
		store:    store,
		model:    model,
		gate:     gate,
		recorder: recorder,
		cwd:      cwd,
	}
}

func stopResult(text string) contract.ModelResult {
	return contract.ModelResult{
		Message:      contract.Message{Role: contract.RoleAssistant, Content: text},
		FinishReason: contract.FinishStop,
	}
}

func toolCallResult(calls ...contract.ToolCall) contract.ModelResult {
	return contract.ModelResult{
		Message:      contract.Message{Role: contract.RoleAssistant, ToolCalls: calls},
		FinishReason: contract.FinishToolCall,
	}
}

func arguments(t *testing.T, value map[string]any) json.RawMessage {
	t.Helper()
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	return encoded
}

func TestRunTextOnlyRound(t *testing.T) {
	usage := int64(120)
	fixture := newFixture(t, 200000, []scriptedResponse{
		{deltas: []string{"hel", "lo"}, result: contract.ModelResult{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "hello"},
			FinishReason: contract.FinishStop,
			Usage:        contract.Usage{InputTokens: &usage},
		}},
	})
	fixture.session.Run(context.Background(), "hi there")

	if fixture.model.requestCount() != 1 {
		t.Fatalf("model calls = %d", fixture.model.requestCount())
	}
	request := fixture.model.request(0)
	if request.ModelProfile != "p1" {
		t.Fatalf("model profile = %q", request.ModelProfile)
	}
	if len(request.Input.Messages) != 1 || request.Input.Messages[0].Content != "hi there" {
		t.Fatalf("request messages = %+v", request.Input.Messages)
	}
	if !strings.Contains(request.Input.System, "<tools>") || !strings.Contains(request.Input.System, "<cwd>") {
		t.Fatalf("system prompt missing sections:\n%s", request.Input.System)
	}
	if len(request.Input.Tools) != 7 {
		t.Fatalf("tool definitions = %d", len(request.Input.Tools))
	}
	if got := fixture.recorder.count(EventTextDelta); got != 2 {
		t.Fatalf("text deltas = %d", got)
	}
	if got := fixture.recorder.count(EventRunFinished); got != 1 {
		t.Fatalf("run finished = %d", got)
	}
	if fixture.session.Totals().InputTokens != 120 {
		t.Fatalf("usage totals = %+v", fixture.session.Totals())
	}
	path, err := fixture.store.Path("")
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 2 || path[1].Message.Content != "hello" {
		t.Fatalf("session path = %+v", path)
	}
}

func TestRunToolCallRoundsAndAlwaysAllow(t *testing.T) {
	fixture := newFixture(t, 200000, []scriptedResponse{
		{result: toolCallResult(contract.ToolCall{
			ID: "c1", Name: "read", Arguments: arguments(t, map[string]any{"path": "notes.txt"}),
		})},
		{result: toolCallResult(contract.ToolCall{
			ID: "c2", Name: "bash", Arguments: arguments(t, map[string]any{"command": "echo gate"}),
		})},
		{result: toolCallResult(contract.ToolCall{
			ID: "c3", Name: "bash", Arguments: arguments(t, map[string]any{"command": "echo again"}),
		})},
		{result: stopResult("done")},
	})
	if err := os.WriteFile(filepath.Join(fixture.cwd, "notes.txt"), []byte("alpha beta"), 0o644); err != nil {
		t.Fatal(err)
	}
	fixture.gate.decisions = []GateDecision{GateAlways}
	fixture.session.Run(context.Background(), "inspect notes")

	if fixture.model.requestCount() != 4 {
		t.Fatalf("model calls = %d", fixture.model.requestCount())
	}
	// read 只读不过门；bash 第一次 Always 后被记住，不再确认。
	if names := fixture.gate.confirmedNames(); len(names) != 1 || names[0] != "bash" {
		t.Fatalf("gate confirmed = %v", names)
	}
	path, err := fixture.store.Path("")
	if err != nil {
		t.Fatal(err)
	}
	var toolResults []contract.Message
	for _, entry := range path {
		if entry.Message != nil && entry.Message.Role == contract.RoleTool {
			toolResults = append(toolResults, *entry.Message)
		}
	}
	if len(toolResults) != 3 {
		t.Fatalf("tool results = %d", len(toolResults))
	}
	if toolResults[0].Content != "alpha beta" || toolResults[0].ToolCallID != "c1" {
		t.Fatalf("read result = %+v", toolResults[0])
	}
	if !strings.Contains(toolResults[1].Content, "gate") || toolResults[1].IsError {
		t.Fatalf("bash result = %+v", toolResults[1])
	}
	if !strings.Contains(toolResults[2].Content, "again") {
		t.Fatalf("second bash result = %+v", toolResults[2])
	}
	// 第二次模型请求的上下文包含第一条工具结果。
	second := fixture.model.request(1)
	if len(second.Input.Messages) != 3 {
		t.Fatalf("second request messages = %+v", second.Input.Messages)
	}
	if second.Input.Messages[2].Role != contract.RoleTool ||
		second.Input.Messages[2].ToolCallID != "c1" {
		t.Fatalf("second request missing tool result: %+v", second.Input.Messages)
	}
	if got := fixture.recorder.count(EventToolFinished); got != 3 {
		t.Fatalf("tool finished events = %d", got)
	}
}

func TestGateDenyBlocksExecution(t *testing.T) {
	fixture := newFixture(t, 200000, []scriptedResponse{
		{result: toolCallResult(contract.ToolCall{
			ID: "c1", Name: "bash",
			Arguments: arguments(t, map[string]any{"command": "touch denied-marker.txt"}),
		})},
		{result: stopResult("ok")},
	})
	fixture.gate.decisions = []GateDecision{GateDeny}
	fixture.session.Run(context.Background(), "run command")

	if _, err := os.Stat(filepath.Join(fixture.cwd, "denied-marker.txt")); err == nil {
		t.Fatal("denied bash command must not execute")
	}
	path, _ := fixture.store.Path("")
	// path: user → assistant(tool_calls) → tool(拒绝结果) → assistant("ok")
	denied := path[2].Message
	if denied.Role != contract.RoleTool || !denied.IsError ||
		!strings.Contains(denied.Content, "User denied execution of tool bash") {
		t.Fatalf("denied result = %+v", denied)
	}
}

func TestSteeringInjectedBeforeModelCall(t *testing.T) {
	fixture := newFixture(t, 200000, []scriptedResponse{
		{result: stopResult("done")},
	})
	fixture.session.Steer("extra note")
	fixture.session.Run(context.Background(), "main task")

	// 排队的 steering 在第一轮模型调用前注入。
	first := fixture.model.request(0)
	if len(first.Input.Messages) != 2 ||
		first.Input.Messages[0].Content != "main task" ||
		first.Input.Messages[1].Content != "extra note" {
		t.Fatalf("steering not injected: %+v", first.Input.Messages)
	}
}

func TestFollowUpContinuesRun(t *testing.T) {
	fixture := newFixture(t, 200000, []scriptedResponse{
		{result: stopResult("first")},
		{result: stopResult("second")},
	})
	fixture.session.FollowUp("and then?")
	fixture.session.Run(context.Background(), "begin")

	if fixture.model.requestCount() != 2 {
		t.Fatalf("model calls = %d", fixture.model.requestCount())
	}
	second := fixture.model.request(1)
	roles := make([]string, 0, len(second.Input.Messages))
	for _, message := range second.Input.Messages {
		roles = append(roles, string(message.Role))
	}
	if strings.Join(roles, ",") != "user,assistant,user" {
		t.Fatalf("second request roles = %v", roles)
	}
	if second.Input.Messages[2].Content != "and then?" {
		t.Fatalf("followUp message = %+v", second.Input.Messages[2])
	}
	if got := fixture.recorder.count(EventRunFinished); got != 1 {
		t.Fatalf("run finished = %d", got)
	}
}

func TestSetProfilePersistsModelChange(t *testing.T) {
	fixture := newFixture(t, 200000, []scriptedResponse{
		{result: stopResult("a")},
		{result: stopResult("b")},
	})
	fixture.session.Run(context.Background(), "first")
	if err := fixture.session.SetProfile("p2"); err != nil {
		t.Fatalf("set profile: %v", err)
	}
	fixture.session.Run(context.Background(), "second")

	if fixture.model.request(1).ModelProfile != "p2" {
		t.Fatalf("second request profile = %q", fixture.model.request(1).ModelProfile)
	}
	path, _ := fixture.store.Path("")
	found := false
	for _, entry := range path {
		if entry.Type == codingsessions.EntryTypeModelChange && entry.ModelChange.Profile == "p2" {
			found = true
		}
	}
	if !found {
		t.Fatal("model_change entry missing")
	}
}

func TestRunUserBashRecordsContext(t *testing.T) {
	fixture := newFixture(t, 200000, nil)
	output, isErr := fixture.session.RunUserBash(context.Background(), "echo sn-code", true)
	if isErr || !strings.Contains(output, "sn-code") {
		t.Fatalf("user bash output = %q isErr=%v", output, isErr)
	}
	path, _ := fixture.store.Path("")
	if len(path) != 1 {
		t.Fatalf("path entries = %d", len(path))
	}
	message := path[0].Message
	if message.Role != contract.RoleUser || !strings.Contains(message.Content, "I ran this command:") ||
		!strings.Contains(message.Content, "echo sn-code") {
		t.Fatalf("recorded bash message = %+v", message)
	}
}

func TestRunUserBashFailureKeepsValidContext(t *testing.T) {
	fixture := newFixture(t, 200000, []scriptedResponse{{result: stopResult("done")}})
	_, isErr := fixture.session.RunUserBash(context.Background(), "exit 3", true)
	if !isErr {
		t.Fatal("failed shell command reported success")
	}
	path, err := fixture.store.Path("")
	if err != nil {
		t.Fatal(err)
	}
	if len(path) != 1 || path[0].Message == nil {
		t.Fatalf("recorded entries = %+v", path)
	}
	if err := path[0].Message.Validate(); err != nil {
		t.Fatalf("invalid recorded shell message: %v", err)
	}
	fixture.session.Run(context.Background(), "continue")
	if fixture.model.requestCount() != 1 {
		t.Fatalf("model calls = %d", fixture.model.requestCount())
	}
	request := fixture.model.request(0)
	if err := request.Validate(); err != nil {
		t.Fatalf("invalid request after shell failure: %v", err)
	}
	if !strings.Contains(request.Input.Messages[0].Content, "exit 3") {
		t.Fatalf("shell command missing from context: %+v", request.Input.Messages)
	}
}

func TestAutoCompactionTriggersAndRebuildsContext(t *testing.T) {
	fixture := newFixture(t, 400, []scriptedResponse{
		{result: stopResult(strings.Repeat("assistant-b ", 40))},
		{result: stopResult("SUMMARY-TEXT")},
		{result: stopResult("final answer")},
	})
	// 缩小保留预算，使第二轮 user 消息后的累计超过阈值。
	fixture.session.keepRecent = 10
	userFirst := strings.Repeat("user-a ", 60)
	fixture.session.Run(context.Background(), userFirst)
	fixture.session.Run(context.Background(), "user-c")

	if fixture.model.requestCount() != 3 {
		t.Fatalf("model calls = %d", fixture.model.requestCount())
	}
	// 第二次调用是摘要请求：专用系统提示 + <conversation> 包裹的旧历史。
	summaryRequest := fixture.model.request(1)
	if summaryRequest.Input.System != summarizationSystemPrompt {
		t.Fatalf("summary system prompt = %q", summaryRequest.Input.System)
	}
	if !strings.Contains(summaryRequest.Input.Messages[0].Content, "<conversation>") ||
		!strings.Contains(summaryRequest.Input.Messages[0].Content, "user-a") {
		t.Fatalf("summary prompt = %q", summaryRequest.Input.Messages[0].Content)
	}
	if strings.Contains(summaryRequest.Input.Messages[0].Content, "user-c") {
		t.Fatalf("summary must not include kept region: %q", summaryRequest.Input.Messages[0].Content)
	}
	// 压缩后的生成请求：摘要包裹消息 + 保留的 assistant/user。
	final := fixture.model.request(2)
	if len(final.Input.Messages) != 3 {
		t.Fatalf("post-compaction messages = %+v", final.Input.Messages)
	}
	wrapped := fmt.Sprintf(codingsessions.CompactionSummaryWrap, "SUMMARY-TEXT")
	if final.Input.Messages[0].Content != wrapped {
		t.Fatalf("compaction summary message = %q, want %q", final.Input.Messages[0].Content, wrapped)
	}
	if final.Input.Messages[1].Role != contract.RoleAssistant ||
		final.Input.Messages[2].Content != "user-c" {
		t.Fatalf("kept region = %+v", final.Input.Messages[1:])
	}
	// 会话文件中记录了 compaction 条目，firstKept 指向 assistant-b。
	path, _ := fixture.store.Path("")
	found := false
	for _, entry := range path {
		if entry.Type == codingsessions.EntryTypeCompaction {
			found = true
			if entry.Compaction.FirstKeptEntryID != path[1].ID {
				t.Fatalf("firstKept = %q want %q", entry.Compaction.FirstKeptEntryID, path[1].ID)
			}
		}
	}
	if !found {
		t.Fatal("compaction entry missing")
	}
}

func TestFindCutPointNeverCutsToolResult(t *testing.T) {
	userEntry := codingsessions.Entry{Type: codingsessions.EntryTypeMessage,
		Message: &contract.Message{Role: contract.RoleUser, Content: "u"}}
	assistantEntry := codingsessions.Entry{Type: codingsessions.EntryTypeMessage,
		Message: &contract.Message{Role: contract.RoleAssistant, Content: "a"}}
	hugeToolResult := codingsessions.Entry{Type: codingsessions.EntryTypeMessage,
		Message: &contract.Message{Role: contract.RoleTool, Content: strings.Repeat("x", 4000)}}
	path := []codingsessions.Entry{userEntry, assistantEntry, hugeToolResult, assistantEntry, userEntry}

	// 从尾部累计在 tool 结果条目上超阈值：切点必须推进到其后的 assistant。
	if got := findCutPoint(path, 0, 50); got != 3 {
		t.Fatalf("cut point = %d, want 3", got)
	}
	// 阈值只能在整个区间内凑齐时，退回第一个合法切点。
	if got := findCutPoint(path, 0, 100000); got != 0 {
		t.Fatalf("fallback cut point = %d, want 0", got)
	}
}

func TestCompactionBoundary(t *testing.T) {
	userA := codingsessions.Entry{ID: "a", Type: codingsessions.EntryTypeMessage,
		Message: &contract.Message{Role: contract.RoleUser, Content: "a"}}
	userB := codingsessions.Entry{ID: "b", Type: codingsessions.EntryTypeMessage,
		Message: &contract.Message{Role: contract.RoleUser, Content: "b"}}
	userC := codingsessions.Entry{ID: "c", Type: codingsessions.EntryTypeMessage,
		Message: &contract.Message{Role: contract.RoleUser, Content: "c"}}
	compaction := codingsessions.Entry{ID: "k", Type: codingsessions.EntryTypeCompaction,
		Compaction: &codingsessions.Compaction{Summary: "S", FirstKeptEntryID: "b"}}

	start, previous := compactionBoundary([]codingsessions.Entry{userA, userB, compaction, userC})
	if start != 1 || previous != "S" {
		t.Fatalf("boundary = %d %q", start, previous)
	}
	// FirstKeptEntryID 缺失时退化为压缩条目之后。
	missing := codingsessions.Entry{ID: "k2", Type: codingsessions.EntryTypeCompaction,
		Compaction: &codingsessions.Compaction{Summary: "S2", FirstKeptEntryID: "gone"}}
	start, previous = compactionBoundary([]codingsessions.Entry{userA, missing, userC})
	if start != 2 || previous != "S2" {
		t.Fatalf("fallback boundary = %d %q", start, previous)
	}
}

func TestManualCompactWithNothingToCompact(t *testing.T) {
	fixture := newFixture(t, 200000, nil)
	if err := fixture.session.Compact(context.Background(), ""); err != ErrNothingToCompact {
		t.Fatalf("compact error = %v", err)
	}
}

func TestBuildSystemPromptSections(t *testing.T) {
	prompt := BuildSystemPrompt("/w/d", tools.PromptSnippets(), []ContextFile{
		{Path: "/w/d/AGENTS.md", Content: "use go"},
	})
	if !strings.HasPrefix(prompt, "You are a coding assistant") {
		t.Fatalf("preamble missing: %q", prompt[:60])
	}
	order := []string{"<tools>", "<rules>", "<project_context>", "<cwd>"}
	position := 0
	for _, marker := range order {
		index := strings.Index(prompt, marker)
		if index < position {
			t.Fatalf("section %s out of order in:\n%s", marker, prompt)
		}
		position = index
	}
	if !strings.Contains(prompt, `<project_instructions path="/w/d/AGENTS.md">`) ||
		!strings.Contains(prompt, "use go") {
		t.Fatalf("project context missing: %s", prompt)
	}
	if !strings.HasSuffix(strings.TrimSpace(prompt), "/w/d\n</cwd>") {
		t.Fatalf("cwd section malformed: %q", prompt[len(prompt)-40:])
	}
}

func TestLoadContextFilesOrderAndPrecedence(t *testing.T) {
	root := t.TempDir()
	nested := filepath.Join(root, "nested")
	deep := filepath.Join(nested, "deep")
	if err := os.MkdirAll(deep, 0o755); err != nil {
		t.Fatal(err)
	}
	files := map[string]string{
		filepath.Join(root, "AGENTS.md"):          "root agents",
		filepath.Join(nested, "CLAUDE.md"):        "nested claude",
		filepath.Join(deep, "AGENTS.override.md"): "deep override",
		filepath.Join(deep, "AGENTS.md"):          "deep agents",
	}
	for path, body := range files {
		if err := os.WriteFile(path, []byte(body), 0o644); err != nil {
			t.Fatal(err)
		}
	}
	agentDir := t.TempDir()
	if err := os.WriteFile(filepath.Join(agentDir, "AGENTS.md"), []byte("global"), 0o644); err != nil {
		t.Fatal(err)
	}

	loaded := LoadContextFiles(deep, agentDir)
	var bodies []string
	for _, file := range loaded {
		bodies = append(bodies, file.Content)
	}
	// 全局最先，然后自外向内；同目录 override 优先于 AGENTS.md。
	want := []string{"global", "root agents", "nested claude", "deep override"}
	if strings.Join(bodies, "|") != strings.Join(want, "|") {
		t.Fatalf("context files = %v", bodies)
	}
}

func TestValidationErrorBecomesToolErrorResult(t *testing.T) {
	fixture := newFixture(t, 200000, []scriptedResponse{
		{result: toolCallResult(contract.ToolCall{
			ID: "c1", Name: "read", Arguments: json.RawMessage(`{"path": 42}`),
		})},
		{result: stopResult("handled")},
	})
	fixture.session.Run(context.Background(), "bad args")

	path, _ := fixture.store.Path("")
	result := path[2].Message
	if result.Role != contract.RoleTool || !result.IsError {
		t.Fatalf("validation failure should be an error tool result: %+v", result)
	}
	if got := fixture.recorder.count(EventRunFinished); got != 1 {
		t.Fatalf("run finished = %d", got)
	}
}

// 编译期断言：脚本模型满足编排层的最小模型接口，测试门实现 ToolGate。
var (
	_ Model         = (*scriptedModel)(nil)
	_ ProfileSource = fakeProfiles{}
	_ ToolGate      = (*recordingGate)(nil)
)
