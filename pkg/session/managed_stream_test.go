package session

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	runtimecommand "github.com/yy003x/sn-code/pkg/command"
	"github.com/yy003x/sn-code/pkg/contract"
)

func writeStreamScript(t *testing.T, name, body string) string {
	t.Helper()
	path := filepath.Join(t.TempDir(), name)
	if err := os.WriteFile(path, []byte(body), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}

// TestCommandSessionStreamsCodexEvents 验证 managed exec 流式路径：codex
// adapter 的原生事件经 sink 顺序推送、seq 由 journal 分配，终态提取与
// canonical 等价。
func TestCommandSessionStreamsCodexEvents(t *testing.T) {
	script := writeStreamScript(t, "codex", `#!/bin/sh
printf '%s\n' \
  '{"type":"thread.started","thread_id":"t1"}' \
  '{"type":"turn.started"}' \
  '{"type":"item.started","item":{"id":"item_1","type":"command_execution","command":"/bin/echo hi","status":"in_progress"}}' \
  '{"type":"item.completed","item":{"id":"item_1","type":"command_execution","command":"/bin/echo hi","aggregated_output":"hi\n","exit_code":0,"status":"completed"}}' \
  '{"type":"item.completed","item":{"id":"item_2","type":"agent_message","text":"done"}}' \
  '{"type":"turn.completed"}'
`)
	service := newTestService(t, &scriptedGenerator{}, map[string]runtimecommand.Profile{
		"batch": {Command: script},
	}, nil)
	var events []contract.Event
	result, runtimeErr := service.RunWithEvents(context.Background(), RunRequest{
		ProfileID: "batch", Input: "run it",
		InvocationBase: filepath.Dir(script),
	}, func(event contract.Event) error {
		events = append(events, event)
		return nil
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if result.Message == nil || result.Message.Content != "done" {
		t.Fatalf("result=%#v", result)
	}
	want := []string{
		string(contract.EventExecutorToolStarted),
		string(contract.EventExecutorToolCompleted),
		string(contract.EventExecutorMessage),
	}
	if len(events) != len(want) {
		t.Fatalf("events=%v", eventTypes(events))
	}
	for index, event := range events {
		if string(event.Type) != want[index] {
			t.Fatalf("events=%v", eventTypes(events))
		}
	}
	if events[2].Executor.Text != "done" {
		t.Fatalf("message=%#v", events[2].Executor)
	}
}

// TestCommandSessionStreamsClaudeEvents 覆盖 claude selector：token delta
// 先于完整消息，终态以唯一 result 记录为准。
func TestCommandSessionStreamsClaudeEvents(t *testing.T) {
	script := writeStreamScript(t, "claude", `#!/bin/sh
printf '%s\n' \
  '{"type":"system","subtype":"init"}' \
  '{"type":"stream_event","event":{"type":"message_start","message":{"id":"msg_1"}}}' \
  '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"he"}}}' \
  '{"type":"stream_event","event":{"type":"content_block_delta","index":0,"delta":{"type":"text_delta","text":"llo"}}}' \
  '{"type":"assistant","message":{"id":"msg_1","content":[{"type":"text","text":"hello"}]}}' \
  '{"type":"result","subtype":"success","is_error":false,"result":"hello"}'
`)
	service := newTestService(t, &scriptedGenerator{}, map[string]runtimecommand.Profile{
		"batch": {Command: script},
	}, nil)
	var events []contract.Event
	result, runtimeErr := service.RunWithEvents(context.Background(), RunRequest{
		ProfileID: "batch", Input: "run it",
		InvocationBase: filepath.Dir(script),
	}, func(event contract.Event) error {
		events = append(events, event)
		return nil
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if result.Message == nil || result.Message.Content != "hello" {
		t.Fatalf("result=%#v", result)
	}
	want := []string{
		string(contract.EventExecutorMessageDelta),
		string(contract.EventExecutorMessageDelta),
		string(contract.EventExecutorMessage),
	}
	if len(events) != len(want) {
		t.Fatalf("events=%v", eventTypes(events))
	}
	for index, event := range events {
		if string(event.Type) != want[index] {
			t.Fatalf("events=%v", eventTypes(events))
		}
	}
	if text := events[0].Executor.Text + events[1].Executor.Text; text != "hello" {
		t.Fatalf("deltas=%q", text)
	}
}

// TestCommandSessionStreamsPiEvents 覆盖 pi selector：token delta 先于
// 权威 assistant 消息，终态以唯一 agent_settled 为准。
func TestCommandSessionStreamsPiEvents(t *testing.T) {
	script := writeStreamScript(t, "pi", `#!/bin/sh
	printf '%s\n' \
	  '{"type":"session","version":3,"id":"s1","cwd":"/tmp"}' \
	  '{"type":"agent_start"}' \
	  '{"type":"turn_start"}' \
	  '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"he"}}' \
	  '{"type":"message_update","assistantMessageEvent":{"type":"text_delta","contentIndex":0,"delta":"llo"}}' \
	  '{"type":"message_end","message":{"role":"assistant","content":[{"type":"text","text":"hello"}],"stopReason":"stop"}}' \
	  '{"type":"turn_end"}' \
	  '{"type":"agent_end"}' \
	  '{"type":"agent_settled","aborted":false}'
`)
	service := newTestService(t, &scriptedGenerator{}, map[string]runtimecommand.Profile{
		"batch": {Command: script},
	}, nil)
	var events []contract.Event
	result, runtimeErr := service.RunWithEvents(context.Background(), RunRequest{
		ProfileID: "batch", Input: "run it",
		InvocationBase: filepath.Dir(script),
	}, func(event contract.Event) error {
		events = append(events, event)
		return nil
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if result.Message == nil || result.Message.Content != "hello" {
		t.Fatalf("result=%#v", result)
	}
	want := []string{
		string(contract.EventExecutorMessageDelta),
		string(contract.EventExecutorMessageDelta),
		string(contract.EventExecutorMessage),
	}
	if len(events) != len(want) {
		t.Fatalf("events=%v", eventTypes(events))
	}
	for index, event := range events {
		if string(event.Type) != want[index] {
			t.Fatalf("events=%v", eventTypes(events))
		}
	}
	if text := events[0].Executor.Text + events[1].Executor.Text; text != "hello" {
		t.Fatalf("deltas=%q", text)
	}
	if events[2].Executor.NativeType != "message_end" {
		t.Fatalf("message=%#v", events[2].Executor)
	}
}

// TestCommandSessionStreamingDecodeFailureFailsRun 验证流损坏（无终态记录）
// 时 Run 按解码失败收口，不伪造成 assistant。
func TestCommandSessionStreamingDecodeFailureFailsRun(t *testing.T) {
	script := writeStreamScript(t, "codex", `#!/bin/sh
printf '%s\n' \
  '{"type":"item.completed","item":{"type":"agent_message","text":"partial"}}'
`)
	service := newTestService(t, &scriptedGenerator{}, map[string]runtimecommand.Profile{
		"batch": {Command: script},
	}, nil)
	result, runtimeErr := service.RunWithEvents(context.Background(), RunRequest{
		ProfileID: "batch", Input: "run it",
		InvocationBase: filepath.Dir(script),
	}, nil)
	if runtimeErr == nil {
		t.Fatalf("result=%#v", result)
	}
	if !strings.Contains(runtimeErr.Message, "exactly one turn.completed") {
		t.Fatalf("runtimeErr=%#v", runtimeErr)
	}
}

func newPiStreamSession(t *testing.T) runtimecommand.StreamSession {
	t.Helper()
	adapter, err := runtimecommand.Resolve("pi")
	if err != nil {
		t.Fatal(err)
	}
	return adapter.(runtimecommand.StreamDecoder).NewStreamSession()
}

func TestStreamOversizedLineDrainsRemainingOutput(t *testing.T) {
	data := strings.Repeat("x", maxStreamLineBytes) + "\n" + strings.Repeat(" \n", maxStreamLineBytes)
	reader := strings.NewReader(data)
	capture := newBoundedCapture(1024, nil)
	stream := newPiStreamSession(t)
	consumeErr := consumeStreamedStdout(reader, capture, stream, func(contract.Event) error {
		t.Fatal("oversized stream must not emit events")
		return nil
	})
	if consumeErr == nil || !strings.Contains(consumeErr.Error(), "stream record exceeds") {
		t.Fatalf("consume error = %v", consumeErr)
	}
	if reader.Len() != 0 || capture.observation("").ObservedBytes != int64(len(data)) {
		t.Fatalf("stdout not fully drained: remaining=%d observed=%d", reader.Len(), capture.observation("").ObservedBytes)
	}
	if _, err := finishStreamedStdout(stream, consumeErr); err != consumeErr {
		t.Fatalf("finish error = %v, want %v", err, consumeErr)
	}
}

func TestStreamSinkFailureIsPreservedAndDrained(t *testing.T) {
	prefix := "{\"type\":\"message_end\",\"message\":{\"role\":\"assistant\",\"content\":\"done\"}}\n" +
		"{\"type\":\"agent_settled\",\"aborted\":false}\n"
	for _, failType := range []contract.EventType{contract.EventExecutorMessage, contract.EventExecutorNative} {
		t.Run(string(failType), func(t *testing.T) {
			data := prefix + "{\"type\":\"native_extra\"}\n" + strings.Repeat("{\"type\":\"later\"}\n", 100000)
			reader := strings.NewReader(data)
			capture := newBoundedCapture(1024, nil)
			stream := newPiStreamSession(t)
			wantErr := errors.New("event sink failed")
			failed := false
			consumeErr := consumeStreamedStdout(reader, capture, stream, func(event contract.Event) error {
				if failed {
					t.Fatal("events emitted after sink failure")
				}
				if event.Type == failType {
					failed = true
					return wantErr
				}
				return nil
			})
			if !errors.Is(consumeErr, wantErr) {
				t.Fatalf("consume error = %v, want %v", consumeErr, wantErr)
			}
			if reader.Len() != 0 || capture.observation("").ObservedBytes != int64(len(data)) {
				t.Fatalf("stdout not fully drained: remaining=%d observed=%d", reader.Len(), capture.observation("").ObservedBytes)
			}
			if _, err := finishStreamedStdout(stream, consumeErr); !errors.Is(err, wantErr) {
				t.Fatalf("finish error = %v, want %v", err, wantErr)
			}
		})
	}
}

func TestStreamDecodeFailureIsPreservedAndDrained(t *testing.T) {
	data := "not JSON\n" + strings.Repeat("{\"type\":\"later\"}\n", 100000)
	reader := strings.NewReader(data)
	capture := newBoundedCapture(1024, nil)
	stream := newPiStreamSession(t)
	consumeErr := consumeStreamedStdout(reader, capture, stream, nil)
	if consumeErr == nil || !strings.Contains(consumeErr.Error(), "decode Pi JSONL") {
		t.Fatalf("consume error = %v", consumeErr)
	}
	if reader.Len() != 0 || capture.observation("").ObservedBytes != int64(len(data)) {
		t.Fatal("stdout not fully drained after decode failure")
	}
	if _, err := finishStreamedStdout(stream, consumeErr); err != consumeErr {
		t.Fatalf("finish error = %v, want %v", err, consumeErr)
	}
}

func eventTypes(events []contract.Event) []string {
	kinds := make([]string, 0, len(events))
	for _, event := range events {
		kinds = append(kinds, string(event.Type))
	}
	return kinds
}
