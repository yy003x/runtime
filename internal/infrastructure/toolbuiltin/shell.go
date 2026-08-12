package toolbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/pkg/agent"
)

const (
	// shellMaxWallTime 是单次 shell tool 的硬性上限。run budget 的 ctx deadline
	// 可能更短；实际生效的是两者中更短者。模型无法把 timeout 拉到该上限之外。
	shellMaxWallTime = 2 * time.Minute
	shellTermGrace   = 2 * time.Second
	shellMaxStdout   = 1 << 20
	shellMaxStderr   = 64 << 10
)

// shellApprovalSchema 是 user_confirmation pause 暴露给调用方的 resume 输入
// 契约：只接受一个布尔 approved 字段。
var shellApprovalSchema = json.RawMessage(
	`{"type":"object","properties":{"approved":{"type":"boolean"}},"required":["approved"],"additionalProperties":false}`,
)

type shellInput struct {
	Command string   `json:"command"`
	Args    []string `json:"args"`
}

type shellApproval struct {
	Approved bool `json:"approved"`
}

type shellResult struct {
	Stdout   string `json:"stdout"`
	Stderr   string `json:"stderr"`
	ExitCode *int   `json:"exit_code,omitempty"`
	Signal   string `json:"signal,omitempty"`
	TimedOut bool   `json:"timed_out,omitempty"`
}

// shell 是 high-risk write_external tool。首次执行不产生副作用，先返回
// user_confirmation pause；Resume 携带 approval 重跑本 handler 时才真正启动
// 子进程。它复用 managed_exec 的进程组 + 有界管道 + terminateAndWait 模式，
// 但剥离 canonical 解码与 helper 重启，是一个纯 child-process runner。
func (resolver *resolver) shell(
	ctx context.Context,
	request agent.ToolRequest,
) (agent.ToolResult, error) {
	var input shellInput
	if err := decodeArguments(request.Arguments, &input); err != nil {
		return agent.ToolResult{}, err
	}
	if strings.TrimSpace(input.Command) == "" {
		return agent.ToolResult{}, fmt.Errorf("command is required")
	}
	// 无 approval：副作用未发生，返回确认门禁。effect 由 kernel 保持 started。
	// Pause.ID 每次生成都唯一（而非基于 callID 的确定值），避免多阶段确认时
	// 同一 tool call 再次 pause 产生 ID 与内容完全相同的 agent.paused 事件，
	// 触发 session 层的 pause 唯一性校验。
	if len(request.Approval) == 0 {
		pauseID, err := identity.New("pause")
		if err != nil {
			return agent.ToolResult{}, fmt.Errorf("generate confirmation id: %w", err)
		}
		return agent.ToolResult{
			Pause: &agent.Pause{
				ID:          pauseID,
				Kind:        agent.PauseKindUserConfirmation,
				Prompt:      shellConfirmationPrompt(input),
				InputSchema: append(json.RawMessage(nil), shellApprovalSchema...),
			},
		}, nil
	}
	var approval shellApproval
	if err := decodeArguments(request.Approval, &approval); err != nil {
		return agent.ToolResult{}, err
	}
	if !approval.Approved {
		return shellRejected(), nil
	}
	stdout, stderr, exitCode, signal, timedOut, err := runChildProcess(
		ctx, input.Command, input.Args, resolver.cwd, resolver.shellTimeout,
	)
	if err != nil {
		return shellFailure("launch_failed", err.Error()), nil
	}
	payload, err := json.Marshal(shellResult{
		Stdout:   truncateForJSON(stdout, shellMaxStdout),
		Stderr:   truncateForJSON(stderr, shellMaxStderr),
		ExitCode: exitCode, Signal: signal, TimedOut: timedOut,
	})
	if err != nil {
		return agent.ToolResult{}, fmt.Errorf("encode shell result: %w", err)
	}
	return agent.ToolResult{Content: string(payload)}, nil
}

func shellConfirmationPrompt(input shellInput) string {
	argv := append([]string{input.Command}, input.Args...)
	// argv token 逐个展示，避免拼成单行后被误当成可复制粘贴的 shell 字符串。
	return "Approve shell tool execution (write_external, high risk): " +
		strings.Join(mapQuoted(argv), " ")
}

func mapQuoted(values []string) []string {
	quoted := make([]string, len(values))
	for index, value := range values {
		quoted[index] = fmt.Sprintf("%q", value)
	}
	return quoted
}

func shellRejected() agent.ToolResult {
	data, _ := json.Marshal(readOnlyToolErrorEnvelope{
		Error: readOnlyToolError{
			Code: "not_approved", Message: "shell execution was not approved",
		},
	})
	return agent.ToolResult{Content: string(data), IsError: true}
}

func shellFailure(code, message string) agent.ToolResult {
	data, _ := json.Marshal(readOnlyToolErrorEnvelope{
		Error: readOnlyToolError{Code: code, Message: message},
	})
	return agent.ToolResult{Content: string(data), IsError: true}
}

func truncateForJSON(value []byte, limit int) string {
	if len(value) > limit {
		return string(value[:limit]) + "...[truncated]"
	}
	return string(value)
}

// runChildProcess 启动一个独立进程组的子进程，有界捕获 stdout/stderr，并在
// ctx 取消、deadline 超限或输出超限时向整个进程组发送 SIGTERM（宽限后
// SIGKILL）。command 以 argv 形式传递，不经 shell 插值。
//
// 已知边界：进程级、无容器隔离。子进程及其同组后代被 SIGKILL(-pgid) 收口；
// 但若后代通过显式 setpgid 脱离本进程组、且继承 stdout/stderr 写端，io.Copy
// 将因写端不关闭而永不 EOF，copyGroup.Wait 阻塞，进而拖住整个调用。这是该
// 边界接受的固有风险（与 session/managed_exec 同形），靠 UserConfirmation
// 在执行前展示完整 argv 由人确认兜底。
func runChildProcess(
	parent context.Context,
	command string,
	args []string,
	cwd string,
	maxWallTime time.Duration,
) (stdout, stderr []byte, exitCode *int, signal string, timedOut bool, err error) {
	deadlineCtx, cancel := context.WithTimeout(parent, maxWallTime)
	defer cancel()
	cmd := exec.Command(command, args...)
	cmd.Dir = cwd
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		return nil, nil, nil, "", false, fmt.Errorf("open null stdin: %w", err)
	}
	defer stdin.Close()
	cmd.Stdin = stdin
	stdoutPipe, err := cmd.StdoutPipe()
	if err != nil {
		return nil, nil, nil, "", false, fmt.Errorf("open stdout pipe: %w", err)
	}
	stderrPipe, err := cmd.StderrPipe()
	if err != nil {
		return nil, nil, nil, "", false, fmt.Errorf("open stderr pipe: %w", err)
	}
	if err := cmd.Start(); err != nil {
		return nil, nil, nil, "", false, fmt.Errorf("start command: %w", err)
	}
	pid := cmd.Process.Pid
	limitSignal := make(chan struct{}, 1)
	stdoutCapture := newShellCapture(shellMaxStdout, limitSignal)
	stderrCapture := newShellCapture(shellMaxStderr, limitSignal)
	var copyGroup sync.WaitGroup
	copyGroup.Add(2)
	go func() {
		defer copyGroup.Done()
		_, _ = io.Copy(stdoutCapture, stdoutPipe)
	}()
	go func() {
		defer copyGroup.Done()
		_, _ = io.Copy(stderrCapture, stderrPipe)
	}()
	waited := make(chan error, 1)
	go func() {
		// 先排空管道再 Wait：os/exec 在 Wait 时关闭读端，提前 Wait 会截断输出。
		copyGroup.Wait()
		waited <- cmd.Wait()
	}()
	select {
	case <-waited:
	case <-deadlineCtx.Done():
		// deadlineCtx 同时承载 parent cancel 与 per-command 上限；只有真正的
		// deadline 超限记为 timed_out，parent 取消由上层 cancellation 语义表达。
		if errors.Is(deadlineCtx.Err(), context.DeadlineExceeded) {
			timedOut = true
		}
		terminateProcessGroup(pid, waited)
	case <-limitSignal:
		terminateProcessGroup(pid, waited)
	}
	exitCode = shellExitCode(cmd.ProcessState)
	signal = shellSignal(cmd.ProcessState)
	return stdoutCapture.bytes(), stderrCapture.bytes(), exitCode, signal, timedOut, nil
}

func terminateProcessGroup(pid int, waited <-chan error) error {
	_ = syscall.Kill(-pid, syscall.SIGTERM)
	timer := time.NewTimer(shellTermGrace)
	defer timer.Stop()
	select {
	case err := <-waited:
		return err
	case <-timer.C:
	}
	_ = syscall.Kill(-pid, syscall.SIGKILL)
	return <-waited
}

func shellExitCode(state *os.ProcessState) *int {
	if state == nil {
		return nil
	}
	value := state.ExitCode()
	if value < 0 {
		return nil
	}
	return &value
}

func shellSignal(state *os.ProcessState) string {
	if state == nil {
		return ""
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	return status.Signal().String()
}

// shellCapture 是一个有界、并发安全的字节缓冲：超过 limit 后继续计数但不增长
// 缓冲，并通过 notify 通知调用方触发进程组终止。
type shellCapture struct {
	mu       sync.Mutex
	limit    int
	observed int64
	prefix   []byte
	exceeded bool
	notify   chan<- struct{}
}

func newShellCapture(limit int, notify chan<- struct{}) *shellCapture {
	return &shellCapture{limit: limit, notify: notify}
}

func (capture *shellCapture) Write(value []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.observed += int64(len(value))
	remaining := capture.limit - len(capture.prefix)
	if remaining > 0 {
		if remaining > len(value) {
			remaining = len(value)
		}
		capture.prefix = append(capture.prefix, value[:remaining]...)
	}
	if capture.observed > int64(capture.limit) && !capture.exceeded {
		capture.exceeded = true
		select {
		case capture.notify <- struct{}{}:
		default:
		}
	}
	return len(value), nil
}

func (capture *shellCapture) bytes() []byte {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return append([]byte(nil), capture.prefix...)
}

func (capture *shellCapture) exceededLimit() bool {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.exceeded
}
