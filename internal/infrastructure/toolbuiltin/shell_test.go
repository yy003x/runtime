package toolbuiltin

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/pkg/agent"
)

func newShellRegistry(t *testing.T) *agent.Registry {
	t.Helper()
	root := t.TempDir()
	registry, err := Build(Options{
		Names: []string{"shell"}, Roots: []string{root}, CWD: root,
	})
	if err != nil {
		t.Fatalf("build shell registry: %v", err)
	}
	return registry
}

func TestShellToolTerminatesOnTimeout(t *testing.T) {
	root := t.TempDir()
	registry, err := Build(Options{
		Names: []string{"shell"}, Roots: []string{root}, CWD: root,
		MaxShellWallTime: 300 * time.Millisecond,
	})
	if err != nil {
		t.Fatalf("build shell registry: %v", err)
	}
	start := time.Now()
	result, err := registry.Execute(context.Background(), agent.ToolRequest{
		Name:      "shell",
		Arguments: json.RawMessage(`{"command":"sleep","args":["5"]}`),
		Approval:  json.RawMessage(`{"approved":true}`),
	})
	elapsed := time.Since(start)
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	var payload shellResult
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode shell result: %v (content=%s)", err, result.Content)
	}
	if !payload.TimedOut {
		t.Fatalf("expected timed_out after deadline, got %#v", payload)
	}
	// sleep 5 在 300ms deadline 后被进程组 SIGTERM 立即终止（2s grace 的 SIGKILL
	// 兜底这里用不到）。2s 上限排除"命令正常跑完 5s"的退化。
	if elapsed > 2*time.Second {
		t.Fatalf(
			"deadline termination not effective: elapsed=%v payload=%#v",
			elapsed, payload,
		)
	}
}

func TestShellToolRequiresUserConfirmationBeforeSideEffect(t *testing.T) {
	registry := newShellRegistry(t)
	result, err := registry.Execute(context.Background(), agent.ToolRequest{
		Name:      "shell",
		Arguments: json.RawMessage(`{"command":"echo","args":["hi"]}`),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Pause == nil ||
		result.Pause.Kind != agent.PauseKindUserConfirmation ||
		result.Content != "" || result.IsError {
		t.Fatalf("expected user_confirmation pause, got %#v", result)
	}
	if !strings.Contains(result.Pause.Prompt, `"echo"`) ||
		!strings.Contains(result.Pause.Prompt, `"hi"`) {
		t.Fatalf("pause prompt=%q", result.Pause.Prompt)
	}
	var approval struct {
		Properties map[string]any `json:"properties"`
	}
	if err := json.Unmarshal(result.Pause.InputSchema, &approval); err != nil {
		t.Fatalf("decode approval schema: %v", err)
	}
	if _, ok := approval.Properties["approved"]; !ok {
		t.Fatalf("approval schema=%s", result.Pause.InputSchema)
	}
}

func TestShellToolExecutesAfterApproval(t *testing.T) {
	registry := newShellRegistry(t)
	result, err := registry.Execute(context.Background(), agent.ToolRequest{
		Name:      "shell",
		Arguments: json.RawMessage(`{"command":"echo","args":["sn-runtime-shell"]}`),
		Approval:  json.RawMessage(`{"approved":true}`),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Pause != nil || result.IsError {
		t.Fatalf("expected real result, got %#v", result)
	}
	var payload shellResult
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode shell result: %v (content=%s)", err, result.Content)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 0 {
		t.Fatalf("exit code=%v", payload.ExitCode)
	}
	if !strings.Contains(payload.Stdout, "sn-runtime-shell") {
		t.Fatalf("stdout=%q", payload.Stdout)
	}
}

func TestShellToolRejectsWhenNotApproved(t *testing.T) {
	registry := newShellRegistry(t)
	result, err := registry.Execute(context.Background(), agent.ToolRequest{
		Name:      "shell",
		Arguments: json.RawMessage(`{"command":"echo","args":["x"]}`),
		Approval:  json.RawMessage(`{"approved":false}`),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Pause != nil || !result.IsError ||
		!strings.Contains(result.Content, "not_approved") {
		t.Fatalf("expected rejected result, got %#v", result)
	}
}

func TestShellToolReportsNonZeroExitWithoutIsError(t *testing.T) {
	registry := newShellRegistry(t)
	// false 以状态 1 退出；shell tool 仍以正常 result（非 IsError）回传退出码，
	// 让模型据 exit_code 自行判断，而不是把命令失败当作 tool 执行失败。
	result, err := registry.Execute(context.Background(), agent.ToolRequest{
		Name:      "shell",
		Arguments: json.RawMessage(`{"command":"false"}`),
		Approval:  json.RawMessage(`{"approved":true}`),
	})
	if err != nil {
		t.Fatalf("execute: %v", err)
	}
	if result.Pause != nil || result.IsError {
		t.Fatalf("expected normal result with exit code, got %#v", result)
	}
	var payload shellResult
	if err := json.Unmarshal([]byte(result.Content), &payload); err != nil {
		t.Fatalf("decode shell result: %v", err)
	}
	if payload.ExitCode == nil || *payload.ExitCode != 1 {
		t.Fatalf("exit code=%v content=%s", payload.ExitCode, result.Content)
	}
}
