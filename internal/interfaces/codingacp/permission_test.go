package codingacp

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yy003x/sn-code/pkg/contract"
)

// toolTurn 压入一轮“模型请求写文件”的脚本步：先工具调用，再收尾文本。
func toolTurn(model *scriptedModel, callID, finalText string) {
	model.push(modelStep{
		result: contract.ModelResult{
			Message: contract.Message{
				Role: contract.RoleAssistant,
				ToolCalls: []contract.ToolCall{{
					ID:        callID,
					Name:      "write",
					Arguments: json.RawMessage(`{"path":"a.txt","content":"hi"}`),
				}},
			},
			FinishReason: contract.FinishToolCall,
		},
	})
	model.push(modelStep{
		delta: finalText,
		result: contract.ModelResult{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: finalText},
			FinishReason: contract.FinishStop,
		},
	})
}

// expectToolLifecycle 断言一次工具调用的 update 状态机：pending → in_progress。
func expectToolLifecycle(t *testing.T, client *acpClient) {
	t.Helper()
	declared := client.nextUpdate()
	if declared["sessionUpdate"] != "tool_call" || declared["status"] != "pending" {
		t.Fatalf("tool declaration = %v", declared)
	}
	if declared["kind"] != "edit" || declared["title"] != "write" {
		t.Fatalf("tool declaration metadata = %v", declared)
	}
	if _, ok := declared["toolCallId"].(string); !ok {
		t.Fatalf("tool declaration missing toolCallId: %v", declared)
	}
	started := client.nextUpdate()
	if started["sessionUpdate"] != "tool_call_update" || started["status"] != "in_progress" {
		t.Fatalf("tool start = %v", started)
	}
}

func TestPermissionAllowOnceExecutesTool(t *testing.T) {
	factory, model := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	sessionID := client.newSession(t.TempDir())
	toolTurn(model, "call-1", "file written")

	client.send("p-1", "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": textPrompt("write a.txt"),
	})
	// 权限请求在 tool_call 声明之后到达；确认 option-0（allow once）。
	expectToolLifecycle(t, client)
	client.answerPermission("option-0")
	completed := client.nextUpdate()
	if completed["sessionUpdate"] != "tool_call_update" || completed["status"] != "completed" {
		t.Fatalf("tool completion = %v", completed)
	}
	response := client.await("p-1")
	if reason := stopReason(response); reason != stopEndTurn {
		t.Fatalf("stopReason = %q, want end_turn", reason)
	}
}

func TestPermissionAllowAlwaysSkipsSecondRequest(t *testing.T) {
	factory, model := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	sessionID := client.newSession(t.TempDir())
	toolTurn(model, "call-1", "first done")
	toolTurn(model, "call-2", "second done")

	// 第一轮选 allow always（会话级记忆），prompt 在后台跑，主协程应答权限。
	client.send("p-1", "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": textPrompt("write a.txt"),
	})
	expectToolLifecycle(t, client)
	client.answerPermission("option-1")
	if response := client.await("p-1"); stopReason(response) != stopEndTurn {
		t.Fatalf("first stopReason = %q", stopReason(response))
	}
	client.drainUpdates()

	// 第二轮同工具不再触发权限请求，直接执行到终态。
	client.send("p-2", "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": textPrompt("write again"),
	})
	expectToolLifecycle(t, client)
	select {
	case request := <-client.permits:
		t.Fatalf("unexpected permission request after allow_always: %v", string(request["id"]))
	case <-time.After(300 * time.Millisecond):
		// 无权限请求即通过；同轮工具已直接执行。
	}
	if response := client.await("p-2"); stopReason(response) != stopEndTurn {
		t.Fatalf("second stopReason = %q", stopReason(response))
	}
}

func TestPermissionRejectDeniesTool(t *testing.T) {
	factory, model := newTestFactory(t)
	client := newACPClient(t, factory)
	client.initialize()
	sessionID := client.newSession(t.TempDir())
	toolTurn(model, "call-1", "denied, moving on")

	client.send("p-1", "session/prompt", map[string]any{
		"sessionId": sessionID, "prompt": textPrompt("write a.txt"),
	})
	expectToolLifecycle(t, client)
	client.answerPermission("option-2")
	failed := client.nextUpdate()
	if failed["sessionUpdate"] != "tool_call_update" || failed["status"] != "failed" {
		t.Fatalf("denied tool status = %v", failed)
	}
	response := client.await("p-1")
	if reason := stopReason(response); reason != stopEndTurn {
		t.Fatalf("stopReason = %q, want end_turn", reason)
	}
}
