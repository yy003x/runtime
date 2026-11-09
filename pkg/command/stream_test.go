package command

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/sn-code/pkg/contract"
)

// streamSamplesDir 指向 P0 录制样本（capability-matrix 的回放夹具）。
const streamSamplesDir = "../../internal/testkit/commandgolden/testdata/streams"

func loadStreamSample(t *testing.T, name string) []byte {
	t.Helper()
	data, err := os.ReadFile(filepath.Join(streamSamplesDir, name))
	if err != nil {
		t.Fatalf("read stream sample %s: %v", name, err)
	}
	return data
}

// decodeStreamSample 逐行回放样本并收集归一化事件；journal 序列由调用方
// 侧分配，这里按顺序补上以通过 contract Event.Validate。
func decodeStreamSample(
	t *testing.T,
	session StreamSession,
	data []byte,
) []contract.Event {
	t.Helper()
	var events []contract.Event
	for _, line := range bytes.Split(data, []byte("\n")) {
		record := bytes.TrimSpace(line)
		if len(record) == 0 {
			continue
		}
		if err := session.DecodeLine(record, func(event contract.Event) error {
			event.Sequence = uint64(len(events) + 1)
			if err := event.Validate(); err != nil {
				t.Fatalf("event %s invalid: %v", event.Type, err)
			}
			events = append(events, event)
			return nil
		}); err != nil {
			t.Fatalf("DecodeLine %q: %v", record, err)
		}
	}
	return events
}

func eventKinds(events []contract.Event) []string {
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, string(event.Type))
	}
	return kinds
}

func TestCodexStreamSessionReplayOK(t *testing.T) {
	session := codexAdapter{}.NewStreamSession()
	events := decodeStreamSample(t, session, loadStreamSample(t, "codex-ok.jsonl"))
	if got := eventKinds(events); len(got) != 1 || got[0] != string(contract.EventExecutorMessage) {
		t.Fatalf("kinds=%v", got)
	}
	if events[0].Executor.Text != "ok" || events[0].Executor.ID != "item_0" {
		t.Fatalf("message=%#v", events[0].Executor)
	}
	output, err := session.Finish()
	if err != nil || output.Assistant != "ok" {
		t.Fatalf("finish=%#v err=%v", output, err)
	}
}

func TestCodexStreamSessionReplayTool(t *testing.T) {
	session := codexAdapter{}.NewStreamSession()
	events := decodeStreamSample(t, session, loadStreamSample(t, "codex-tool.jsonl"))
	want := []string{
		string(contract.EventExecutorMessage),
		string(contract.EventExecutorToolStarted),
		string(contract.EventExecutorToolCompleted),
		string(contract.EventExecutorMessage),
	}
	if got := eventKinds(events); !equalStrings(got, want) {
		t.Fatalf("kinds=%v", got)
	}
	toolStart := events[1].Executor
	if toolStart.ID != "item_1" || toolStart.Name != "command_execution" ||
		len(toolStart.Data) == 0 || !strings.Contains(string(toolStart.Data), "aep-spike-tool-ok") {
		t.Fatalf("toolStart=%#v", toolStart)
	}
	toolDone := events[2].Executor
	if toolDone.ID != "item_1" || toolDone.IsError ||
		!strings.Contains(toolDone.Text, "aep-spike-tool-ok") {
		t.Fatalf("toolDone=%#v", toolDone)
	}
	output, err := session.Finish()
	if err != nil || !strings.Contains(output.Assistant, "aep-spike-tool-ok") {
		t.Fatalf("finish=%#v err=%v", output, err)
	}
}

func TestCodexStreamSessionReplayCancelHasNoTerminal(t *testing.T) {
	session := codexAdapter{}.NewStreamSession()
	events := decodeStreamSample(t, session, loadStreamSample(t, "codex-cancel.jsonl"))
	if len(events) != 0 {
		t.Fatalf("events=%v", eventKinds(events))
	}
	if _, err := session.Finish(); err == nil ||
		!strings.Contains(err.Error(), "exactly one turn.completed") {
		t.Fatalf("finish err=%v", err)
	}
}

func TestClaudeStreamSessionReplayOK(t *testing.T) {
	session := claudeAdapter{}.NewStreamSession()
	events := decodeStreamSample(t, session, loadStreamSample(t, "claude-ok.jsonl"))
	kinds := eventKinds(events)
	messages := countKind(events, contract.EventExecutorMessage)
	deltas := countKind(events, contract.EventExecutorMessageDelta)
	if messages != 1 || deltas < 1 {
		t.Fatalf("kinds=%v", kinds)
	}
	// token delta 先于完整消息（stream 顺序）。
	if kinds[len(kinds)-1] != string(contract.EventExecutorMessage) {
		t.Fatalf("kinds=%v", kinds)
	}
	for _, event := range events {
		if event.Type == contract.EventExecutorMessageDelta && event.Executor.Text != "ok" {
			t.Fatalf("delta text=%q", event.Executor.Text)
		}
	}
	last := events[len(events)-1].Executor
	if last.Text != "ok" {
		t.Fatalf("message=%#v", last)
	}
	output, err := session.Finish()
	if err != nil || output.Assistant != "ok" {
		t.Fatalf("finish=%#v err=%v", output, err)
	}
}

func TestClaudeStreamSessionReplayTool(t *testing.T) {
	session := claudeAdapter{}.NewStreamSession()
	events := decodeStreamSample(t, session, loadStreamSample(t, "claude-tool.jsonl"))
	if countKind(events, contract.EventExecutorToolStarted) < 1 ||
		countKind(events, contract.EventExecutorToolCompleted) < 1 ||
		countKind(events, contract.EventExecutorMessage) < 1 {
		t.Fatalf("kinds=%v", eventKinds(events))
	}
	var started []contract.ExecutorEvent
	for _, event := range events {
		if event.Type == contract.EventExecutorToolStarted {
			started = append(started, *event.Executor)
		}
	}
	if started[0].Name != "ToolSearch" || !strings.HasPrefix(started[0].ID, "call_") {
		t.Fatalf("toolStart=%#v", started[0])
	}
	output, err := session.Finish()
	if err != nil || !strings.Contains(output.Assistant, "aep-spike-tool-ok") {
		t.Fatalf("finish=%#v err=%v", output, err)
	}
}

func TestClaudeStreamSessionReplayCancelHasNoTerminal(t *testing.T) {
	session := claudeAdapter{}.NewStreamSession()
	events := decodeStreamSample(t, session, loadStreamSample(t, "claude-cancel.jsonl"))
	// 样本在 thinking 阶段被 SIGTERM：只有 thinking delta（不投递）、无
	// text delta、无终态 result 记录。
	if len(events) != 0 {
		t.Fatalf("kinds=%v", eventKinds(events))
	}
	if _, err := session.Finish(); err == nil ||
		!strings.Contains(err.Error(), "exactly one result record") {
		t.Fatalf("finish err=%v", err)
	}
}

func TestStreamSessionRejectsMalformedLine(t *testing.T) {
	for _, session := range []StreamSession{
		codexAdapter{}.NewStreamSession(),
		claudeAdapter{}.NewStreamSession(),
		piAdapter{}.NewStreamSession(),
	} {
		if err := session.DecodeLine([]byte("{not json"), nil); err == nil {
			t.Fatalf("%T accepted malformed line", session)
		}
		if _, err := session.Finish(); err == nil {
			t.Fatalf("%T finish accepted poisoned stream", session)
		}
	}
}

func TestBoundedExecutorDataTruncatesOversizeExcerpt(t *testing.T) {
	value := map[string]any{"text": strings.Repeat("x", contract.MaxExecutorEventDataBytes)}
	encoded, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	bounded := boundedExecutorData(encoded)
	if len(bounded) > contract.MaxExecutorEventDataBytes {
		t.Fatalf("bounded=%d", len(bounded))
	}
	if !json.Valid(bounded) {
		t.Fatalf("bounded=%s", bounded)
	}
	var decoded map[string]any
	if err := json.Unmarshal(bounded, &decoded); err != nil {
		t.Fatal(err)
	}
	if text, _ := decoded["text"].(string); len(text) > 300 {
		t.Fatalf("text len=%d", len(text))
	}
}

func countKind(
	events []contract.Event,
	kind contract.EventType,
) int {
	count := 0
	for _, event := range events {
		if event.Type == kind {
			count++
		}
	}
	return count
}

func equalStrings(left, right []string) bool {
	if len(left) != len(right) {
		return false
	}
	for index := range left {
		if left[index] != right[index] {
			return false
		}
	}
	return true
}

func TestBuildCodexStreamingMatchesCanonicalSelector(t *testing.T) {
	root := t.TempDir()
	commandPath := writeCommandFixture(t, root, "codex")
	prompt := "reply ok"
	buildFor := func(protocol OutputProtocol) []string {
		invocation, err := Build(BuildRequest{
			Mode:           ModeExec,
			OutputProtocol: protocol,
			Profile:        Profile{Command: commandPath},
			ArgvPrompt:     &prompt,
			InvocationBase: root,
		})
		if err != nil {
			t.Fatal(err)
		}
		return invocation.Argv
	}
	canonical := buildFor(OutputCanonical)
	streaming := buildFor(OutputStreaming)
	if !equalStrings(canonical, streaming) {
		t.Fatalf("canonical=%q streaming=%q", canonical, streaming)
	}
	if !equalStrings(streaming[len(streaming)-4:],
		[]string{"--ephemeral", "--json", "--", prompt}) {
		t.Fatalf("streaming tail=%q", streaming)
	}
}

func TestBuildClaudeStreamingSelector(t *testing.T) {
	root := t.TempDir()
	commandPath := writeCommandFixture(t, root, "claude")
	prompt := "reply ok"
	invocation, err := Build(BuildRequest{
		Mode:           ModeExec,
		OutputProtocol: OutputStreaming,
		Profile:        Profile{Command: commandPath},
		ArgvPrompt:     &prompt,
		InvocationBase: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		commandPath,
		"--no-session-persistence",
		"--output-format", "stream-json",
		"--verbose",
		"--include-partial-messages",
		"-p",
		"--", prompt,
	}
	if !equalStrings(invocation.Argv, want) {
		t.Fatalf("argv=%q want=%q", invocation.Argv, want)
	}
}

func TestPiStreamSessionReplayOK(t *testing.T) {
	session := piAdapter{}.NewStreamSession()
	events := decodeStreamSample(t, session, loadStreamSample(t, "pi-ok.jsonl"))
	kinds := eventKinds(events)
	if countKind(events, contract.EventExecutorMessage) != 1 ||
		countKind(events, contract.EventExecutorMessageDelta) < 1 {
		t.Fatalf("kinds=%v", kinds)
	}
	// token delta 先于完整消息，最终消息为权威文本。
	if kinds[len(kinds)-1] != string(contract.EventExecutorMessage) {
		t.Fatalf("kinds=%v", kinds)
	}
	var deltaText strings.Builder
	for _, event := range events {
		if event.Type == contract.EventExecutorMessageDelta {
			deltaText.WriteString(event.Executor.Text)
		}
	}
	last := events[len(events)-1].Executor
	if deltaText.String() != "ok" || last.Text != "ok" {
		t.Fatalf("deltas=%q message=%#v", deltaText.String(), last)
	}
	output, err := session.Finish()
	if err != nil || output.Assistant != "ok" {
		t.Fatalf("finish=%#v err=%v", output, err)
	}
}

func TestPiStreamSessionReplayTool(t *testing.T) {
	session := piAdapter{}.NewStreamSession()
	events := decodeStreamSample(t, session, loadStreamSample(t, "pi-tool.jsonl"))
	if countKind(events, contract.EventExecutorToolStarted) != 1 ||
		countKind(events, contract.EventExecutorToolCompleted) != 1 ||
		countKind(events, contract.EventExecutorMessage) != 1 {
		t.Fatalf("kinds=%v", eventKinds(events))
	}
	toolStart := events[0]
	if toolStart.Type != contract.EventExecutorToolStarted ||
		toolStart.Executor.Name != "bash" ||
		!strings.HasPrefix(toolStart.Executor.ID, "call_") ||
		!strings.Contains(string(toolStart.Executor.Data), "aep-spike-tool-ok") {
		t.Fatalf("toolStart=%#v", toolStart)
	}
	toolDone := events[1]
	if toolDone.Type != contract.EventExecutorToolCompleted || toolDone.Executor.IsError ||
		!strings.Contains(toolDone.Executor.Text, "aep-spike-tool-ok") ||
		!strings.Contains(string(toolDone.Executor.Data), "exit_code") {
		t.Fatalf("toolDone=%#v", toolDone)
	}
	output, err := session.Finish()
	if err != nil || !strings.Contains(output.Assistant, "aep-spike-tool-ok") {
		t.Fatalf("finish=%#v err=%v", output, err)
	}
}

func TestPiStreamSessionReplayCancelHasNoTerminal(t *testing.T) {
	session := piAdapter{}.NewStreamSession()
	events := decodeStreamSample(t, session, loadStreamSample(t, "pi-cancel.jsonl"))
	// 样本在 thinking 阶段被 SIGTERM：无 text delta、无 assistant message_end、
	// 无 agent_settled 终态记录。
	if len(events) != 0 {
		t.Fatalf("kinds=%v", eventKinds(events))
	}
	if _, err := session.Finish(); err == nil ||
		!strings.Contains(err.Error(), "exactly one agent_settled") {
		t.Fatalf("finish err=%v", err)
	}
}

func TestBuildPiStreamingMatchesCanonicalSelector(t *testing.T) {
	root := t.TempDir()
	commandPath := writeCommandFixture(t, root, "pi")
	prompt := "reply ok"
	buildFor := func(protocol OutputProtocol) []string {
		invocation, err := Build(BuildRequest{
			Mode:           ModeExec,
			OutputProtocol: protocol,
			Profile:        Profile{Command: commandPath},
			ArgvPrompt:     &prompt,
			InvocationBase: root,
		})
		if err != nil {
			t.Fatal(err)
		}
		return invocation.Argv
	}
	canonical := buildFor(OutputCanonical)
	streaming := buildFor(OutputStreaming)
	if !equalStrings(canonical, streaming) {
		t.Fatalf("canonical=%q streaming=%q", canonical, streaming)
	}
	want := []string{
		commandPath, "-p", "--mode", "json", "--no-session", "--", prompt,
	}
	if !equalStrings(streaming, want) {
		t.Fatalf("streaming argv=%q want=%q", streaming, want)
	}
}

func TestBuildPiTypedOverridesAndResume(t *testing.T) {
	root := t.TempDir()
	commandPath := writeCommandFixture(t, root, "pi")
	effort := EffortHigh
	invocation, err := Build(BuildRequest{
		Mode:           ModeExec,
		OutputProtocol: OutputStreaming,
		Profile:        Profile{Command: commandPath},
		Overrides:      Overrides{Model: strPtr("glm-5.3"), Effort: &effort},
		ArgvPrompt:     strPtr("reply ok"),
		InvocationBase: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	joined := strings.Join(invocation.Argv, " ")
	if !strings.Contains(joined, "--model glm-5.3") ||
		!strings.Contains(joined, "--thinking high") {
		t.Fatalf("argv=%q", invocation.Argv)
	}
	resumeID := "01a1818d"
	resumed, err := Build(BuildRequest{
		Mode:           ModeInteractive,
		OutputProtocol: OutputNative,
		Profile:        Profile{Command: commandPath},
		Resume:         &resumeID,
		InvocationBase: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !equalStrings(resumed.Argv[len(resumed.Argv)-2:],
		[]string{"--session", resumeID}) {
		t.Fatalf("resume argv=%q", resumed.Argv)
	}
	latest, err := Build(BuildRequest{
		Mode:           ModeInteractive,
		OutputProtocol: OutputNative,
		Profile:        Profile{Command: commandPath},
		Resume:         strPtr(""),
		InvocationBase: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if latest.Argv[len(latest.Argv)-1] != "--continue" {
		t.Fatalf("resume argv=%q", latest.Argv)
	}
}

func TestBuildPiRejectsForbiddenAndInvalidCombos(t *testing.T) {
	for _, test := range []struct {
		name    string
		args    []string
		mode    Mode
		wantErr string
	}{
		{
			name: "pi_user_continue",
			args: []string{"--continue"},
			mode: ModeExec, wantErr: "--continue is incompatible with structured output",
		},
		{
			name: "pi_user_session",
			args: []string{"--session", "abc"},
			mode: ModeExec, wantErr: "--session is incompatible with structured output",
		},
		{
			name:    "pi_interactive",
			mode:    ModeInteractive,
			wantErr: "interactive mode does not support canonical output",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Build(BuildRequest{
				Mode:           test.mode,
				OutputProtocol: OutputStreaming,
				Profile:        Profile{Command: "pi", Args: test.args},
				Symbolic:       true,
				InvocationBase: "/__sn_cli_symbolic_cwd__",
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err=%v want %q", err, test.wantErr)
			}
		})
	}
	if err := CheckProfile(Profile{Command: "pi"}); err != nil {
		t.Fatalf("check profile: %v", err)
	}
}

func strPtr(value string) *string { return &value }

func TestBuildStreamingRejectsForbiddenAndInvalidCombos(t *testing.T) {
	for _, test := range []struct {
		name    string
		command string
		args    []string
		mode    Mode
		wantErr string
	}{
		{
			name:    "claude_user_verbose",
			command: "claude", args: []string{"--verbose"},
			mode: ModeExec, wantErr: "--verbose is incompatible with structured output",
		},
		{
			name:    "claude_user_partial_messages",
			command: "claude", args: []string{"--include-partial-messages"},
			mode: ModeExec, wantErr: "--include-partial-messages is incompatible with structured output",
		},
		{
			name:    "codex_output_last_message",
			command: "codex", args: []string{"--output-last-message", "/tmp/out"},
			mode: ModeExec, wantErr: "--output-last-message is incompatible with structured output",
		},
		{
			name:    "codex_interactive",
			command: "codex",
			mode:    ModeInteractive,
			wantErr: "interactive mode does not support canonical output",
		},
		{
			name:    "grok_unsupported",
			command: "grok",
			mode:    ModeExec,
			wantErr: "unsupported output protocol",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			_, err := Build(BuildRequest{
				Mode:           test.mode,
				OutputProtocol: OutputStreaming,
				Profile:        Profile{Command: test.command, Args: test.args},
				Symbolic:       true,
				InvocationBase: "/__sn_cli_symbolic_cwd__",
			})
			if err == nil || !strings.Contains(err.Error(), test.wantErr) {
				t.Fatalf("err=%v want %q", err, test.wantErr)
			}
		})
	}
}

func TestCheckProfileValidatesStreamingPlan(t *testing.T) {
	for _, command := range []string{"codex", "claude", "grok"} {
		profile := Profile{Command: command}
		if err := CheckProfile(profile); err != nil {
			t.Fatalf("%s: %v", command, err)
		}
	}
}
