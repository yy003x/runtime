package tmux

import (
	"context"
	"fmt"
	"os"
	"strings"
	"time"
)

type TaskRequest struct {
	RunID      string
	CWD        string
	Command    string
	Prompt     string
	ResultFile string
	DoneFile   string
	Bracketed  bool
}

type TaskResult struct {
	Session  string
	Stdout   string
	ExitCode int
	Done     bool
	Result   bool
}

type TaskObserver func(phase string, values map[string]any) error

func (b *Backend) ExecuteTask(ctx context.Context, request TaskRequest, observer TaskObserver) (TaskResult, error) {
	session, err := b.StartShell(ctx, request.RunID, request.CWD, request.Command)
	if err != nil {
		return TaskResult{ExitCode: 1}, err
	}
	defer func() { _ = b.Kill(context.Background(), session) }()
	if observer != nil {
		if err := observer("ready", map[string]any{"tmux_session": session, "alive": true}); err != nil {
			return TaskResult{Session: session, ExitCode: 1}, err
		}
	}
	prompt := completionPrompt(request.Prompt, request.ResultFile, request.DoneFile)
	if err := b.Send(ctx, session, prompt, SendOptions{Submit: true, Bracketed: request.Bracketed, Stabilize: true}); err != nil {
		return TaskResult{Session: session, ExitCode: 1}, err
	}
	if observer != nil {
		if err := observer("processing", map[string]any{"tmux_session": session, "prompt_submitted": true}); err != nil {
			return TaskResult{Session: session, ExitCode: 1}, err
		}
	}
	lastOutput := ""
	for {
		if ctx.Err() != nil {
			return TaskResult{Session: session, Stdout: lastOutput, ExitCode: 1}, ctx.Err()
		}
		if _, err := os.Stat(request.DoneFile); err == nil {
			_, resultErr := os.Stat(request.ResultFile)
			output, _ := b.Capture(ctx, session, 200)
			return TaskResult{Session: session, Stdout: output, Done: true, Result: resultErr == nil}, nil
		}
		if output, captureErr := b.Capture(ctx, session, 200); captureErr == nil {
			lastOutput = output
		}
		alive, hasErr := b.HasSession(ctx, session)
		if hasErr != nil || !alive {
			return TaskResult{Session: session, Stdout: lastOutput, ExitCode: 1}, nil
		}
		timer := time.NewTimer(b.config.PollInterval)
		select {
		case <-ctx.Done():
			timer.Stop()
			return TaskResult{Session: session, Stdout: lastOutput, ExitCode: 1}, ctx.Err()
		case <-timer.C:
		}
	}
}

func completionPrompt(prompt, resultFile, doneFile string) string {
	return fmt.Sprintf(`%s

## Tmux task completion contract

完成 result.json 写入并重新读取校验后，最后一步创建空完成标记：
- result_file: %s
- done_file: %s

必须先原子写入并校验 result_file，再执行 touch "$AGENTRUN_DONE_FILE"。终端输出不能替代 result_file；未创建 done_file 不视为完成。
`, strings.TrimRight(prompt, "\n"), resultFile, doneFile)
}
