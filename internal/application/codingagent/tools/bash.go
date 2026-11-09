package tools

import (
	"bytes"
	"context"
	"fmt"
	"os"
	"os/exec"
	"strings"
	"sync"
	"syscall"
	"time"

	"github.com/yy003x/sn-code/pkg/agent"
)

type bashArguments struct {
	Command string  `json:"command"`
	Timeout float64 `json:"timeout"`
}

// handleBash 以登录用户的 shell 执行命令字符串。对齐 pi 语义：
// 输出取尾部 2000 行/50KB，全量输出写入临时文件并在尾部注明路径；
// 非零退出码标记 isError；超时或取消时向整个进程组 SIGTERM→SIGKILL。
// 参数 request 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 agent.ToolResult、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func handleBash(ctx context.Context, request agent.ToolRequest) (agent.ToolResult, error) {
	arguments, err := decodeArguments[bashArguments](request)
	if err != nil {
		return agent.ToolResult{}, err
	}
	if strings.TrimSpace(arguments.Command) == "" {
		return errorResult("bash: command is required")
	}
	environment := EnvironmentFrom(ctx)
	cwd := environment.CWD
	if cwd == "" {
		if wd, wdErr := os.Getwd(); wdErr == nil {
			cwd = wd
		}
	}
	if info, statErr := os.Stat(cwd); statErr != nil || !info.IsDir() {
		return errorResult("bash: working directory %s is not available", cwd)
	}
	shell := os.Getenv("SHELL")
	if shell == "" {
		shell = "/bin/bash"
	}
	command := exec.Command(shell, "-c", arguments.Command)
	command.Dir = cwd
	command.Env = append(
		os.Environ(),
		"SN_CODE_SESSION_ID="+environment.SessionID,
		"SN_CODE_PROFILE="+environment.Profile,
		"SN_CODE_MODEL="+environment.Model,
	)
	// 独立进程组，超时/取消可以整组终止（shell 的子进程不会逃逸）。
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	output := newStreamingCommandOutput()
	command.Stdout = output
	command.Stderr = output
	if err := command.Start(); err != nil {
		_, temporaryPath := output.finish()
		_ = os.Remove(temporaryPath)
		return errorResult("bash: %v", err)
	}

	processDone := make(chan error, 1)
	go func() { processDone <- command.Wait() }()
	var timeoutTimer <-chan time.Time
	if arguments.Timeout > 0 {
		timer := time.NewTimer(time.Duration(arguments.Timeout * float64(time.Second)))
		defer timer.Stop()
		timeoutTimer = timer.C
	}
	timedOut := false
	var waitErr error
	select {
	case waitErr = <-processDone:
	case <-timeoutTimer:
		timedOut = true
		terminateProcessGroup(command.Process.Pid)
		<-processDone
	case <-ctx.Done():
		terminateProcessGroup(command.Process.Pid)
		<-processDone
		_, temporaryPath := output.finish()
		_ = os.Remove(temporaryPath)
		return agent.ToolResult{
			Content: "Command was interrupted",
			IsError: true,
		}, nil
	}

	text, temporaryPath := output.finish()
	notes := make([]string, 0, 2)
	if temporaryPath != "" {
		notes = append(notes, fmt.Sprintf("[Full output: %s]", temporaryPath))
	}
	if timedOut {
		return agent.ToolResult{
			Content: strings.TrimSpace(text + "\nCommand timed out after " + fmt.Sprintf("%gs", arguments.Timeout)),
			IsError: true,
		}, nil
	}
	exitCode := 0
	if waitErr != nil {
		if exitError, ok := waitErr.(*exec.ExitError); ok {
			exitCode = exitError.ExitCode()
		} else {
			return errorResult("bash: %v", waitErr)
		}
	}
	body := text
	for _, note := range notes {
		body += "\n" + note
	}
	if exitCode != 0 {
		body += fmt.Sprintf("\nCommand exited with code %d", exitCode)
		return agent.ToolResult{Content: body, IsError: true}, nil
	}
	return agent.ToolResult{Content: body}, nil
}

// streamingCommandOutput 在命令运行期间把完整输出顺序写入临时文件，
// 内存中只保留有界尾部，避免持续输出在进程退出前耗尽内存。
type streamingCommandOutput struct {
	mu            sync.Mutex
	tail          bytes.Buffer
	temporary     *os.File
	temporaryPath string
	writeFailed   bool
}

// newStreamingCommandOutput 创建流式输出收集器；临时文件创建失败时仍保留
// 有界尾部，finish 会报告没有可引用的完整输出文件。
// 返回 *streamingCommandOutput 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func newStreamingCommandOutput() *streamingCommandOutput {
	temporary, err := os.CreateTemp("", "sn-code-bash-*")
	output := &streamingCommandOutput{}
	if err == nil {
		output.temporary = temporary
		output.temporaryPath = temporary.Name()
	}
	return output
}

// Write 接收 stdout/stderr 的字节块，返回输入长度以满足 io.Writer；
// 临时文件错误只影响全量输出留存，不改变命令本身的执行结果。
// 参数 chunk 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 int、error 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func (output *streamingCommandOutput) Write(chunk []byte) (int, error) {
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.temporary != nil && !output.writeFailed {
		if _, err := output.temporary.Write(chunk); err != nil {
			output.writeFailed = true
		}
	}
	_, _ = output.tail.Write(chunk)
	if overflow := output.tail.Len() - maxOutputBytes; overflow > 0 {
		output.tail.Next(overflow)
	}
	return len(chunk), nil
}

// finish 关闭临时文件并返回有界尾部；只有输出确实超限且全量写入成功时，
// 才保留并返回临时文件路径，否则清理临时文件。
func (output *streamingCommandOutput) finish() (string, string) {
	output.mu.Lock()
	defer output.mu.Unlock()
	if output.temporary == nil {
		return tailTruncate(output.tail.String(), maxOutputLines, maxOutputBytes), ""
	}
	info, statErr := output.temporary.Stat()
	if err := output.temporary.Close(); err != nil {
		output.writeFailed = true
	}
	oversized := !output.writeFailed && statErr == nil &&
		(info.Size() > int64(maxOutputBytes) || strings.Count(output.tail.String(), "\n") >= maxOutputLines)
	if !oversized {
		_ = os.Remove(output.temporaryPath)
		return tailTruncate(output.tail.String(), maxOutputLines, maxOutputBytes), ""
	}
	return tailTruncate(output.tail.String(), maxOutputLines, maxOutputBytes), output.temporaryPath
}

// tailTruncate 保留输出的尾部：超过行数或字节上限时从头裁剪。
// 参数 text、maxLines、maxBytes 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
// 返回 string 表示处理结果；error 非空时报告失败原因，其余结果按零值或约定处理。
func tailTruncate(text string, maxLines, maxBytes int) string {
	lines := strings.Split(text, "\n")
	if len(lines) <= maxLines && len(text) <= maxBytes {
		return text
	}
	if len(lines) > maxLines {
		lines = lines[len(lines)-maxLines:]
	}
	text = strings.Join(lines, "\n")
	if len(text) > maxBytes {
		text = text[len(text)-maxBytes:]
	}
	return text
}

// terminateProcessGroup 先 SIGTERM 后 SIGKILL 整个进程组。
// 参数 pid 分别提供该操作所需的输入；调用方负责满足所属类型的约束。
func terminateProcessGroup(pid int) {
	processGroup := -pid
	_ = syscall.Kill(processGroup, syscall.SIGTERM)
	deadline := time.Now().Add(2 * time.Second)
	for time.Now().Before(deadline) {
		if syscall.Kill(processGroup, 0) != nil {
			return
		}
		time.Sleep(20 * time.Millisecond)
	}
	_ = syscall.Kill(processGroup, syscall.SIGKILL)
}
