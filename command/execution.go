package command

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"runtime"
	"strings"
)

type ExecutionRequest struct {
	Args           []string
	Prompt         string
	CWD            string
	Stdin          io.Reader
	Stdout         io.Writer
	Stderr         io.Writer
	TerminalDriver string
}

type ExecutionResult struct {
	State          string `json:"state"`
	ExitCode       int    `json:"exit_code,omitempty"`
	Stdout         string `json:"stdout,omitempty"`
	Stderr         string `json:"stderr,omitempty"`
	LaunchHandle   string `json:"launch_handle,omitempty"`
	CaptureQuality string `json:"capture_quality"`
}

type Runner struct {
	environ func() []string
	run     func(context.Context, string, ...string) *exec.Cmd
}

func NewRunner() *Runner {
	return &Runner{
		environ: os.Environ,
		run:     exec.CommandContext,
	}
}

func (runner *Runner) Execute(
	ctx context.Context,
	profile Profile,
	request ExecutionRequest,
) (ExecutionResult, error) {
	if runner == nil {
		runner = NewRunner()
	}
	args, stdin, err := preparePrompt(profile, request)
	if err != nil {
		return ExecutionResult{}, err
	}
	resolved, err := prepareInvocation(profile, args, runner.environ())
	if err != nil {
		return ExecutionResult{}, err
	}
	switch profile.Transport {
	case TransportTTY:
		return runner.executeProcess(ctx, resolved, request, stdin)
	case TransportTmux:
		return runner.launchTmux(
			ctx, resolved, request, profile.PromptDelivery == PromptPaste,
		)
	case TransportTerminal:
		return runner.launchTerminal(
			ctx, resolved, request, profile.PromptDelivery == PromptPaste,
		)
	default:
		return ExecutionResult{}, fmt.Errorf("unsupported transport %q", profile.Transport)
	}
}

func preparePrompt(
	profile Profile,
	request ExecutionRequest,
) ([]string, io.Reader, error) {
	args := append([]string(nil), request.Args...)
	stdin := request.Stdin
	switch profile.PromptDelivery {
	case PromptArgv:
		if request.Prompt != "" {
			args = append(args, request.Prompt)
		}
	case PromptStdin:
		if request.Prompt != "" {
			stdin = strings.NewReader(request.Prompt)
		}
	case PromptPaste:
		if profile.Transport == TransportTTY {
			return nil, nil, fmt.Errorf("paste delivery requires a detached carrier")
		}
	case PromptManual:
		if request.Prompt != "" {
			return nil, nil, fmt.Errorf("manual prompt delivery does not accept automatic input")
		}
	default:
		return nil, nil, fmt.Errorf("unsupported prompt delivery %q", profile.PromptDelivery)
	}
	return args, stdin, nil
}

func (runner *Runner) executeProcess(
	ctx context.Context,
	resolved Invocation,
	request ExecutionRequest,
	stdin io.Reader,
) (ExecutionResult, error) {
	command := runner.run(ctx, resolved.Path, resolved.Argv[1:]...)
	command.Env = resolved.Environment
	command.Dir = request.CWD
	command.Stdin = stdin
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	if request.Stdout != nil {
		command.Stdout = io.MultiWriter(request.Stdout, &stdout)
	}
	if request.Stderr != nil {
		command.Stderr = io.MultiWriter(request.Stderr, &stderr)
	}
	err := command.Run()
	exitCode := 0
	if err != nil {
		var exitError *exec.ExitError
		if !errors.As(err, &exitError) {
			return ExecutionResult{}, err
		}
		exitCode = exitError.ExitCode()
	}
	return ExecutionResult{
		State: "completed", ExitCode: exitCode,
		Stdout: stdout.String(), Stderr: stderr.String(),
		CaptureQuality: "parsed",
	}, nil
}

func (runner *Runner) launchTmux(
	ctx context.Context,
	resolved Invocation,
	request ExecutionRequest,
	paste bool,
) (ExecutionResult, error) {
	arguments := []string{"new-session", "-d", "-P", "-F", "#{session_id}:#{window_id}.#{pane_id}"}
	if request.CWD != "" {
		arguments = append(arguments, "-c", request.CWD)
	}
	arguments = append(arguments, "--", "/usr/bin/env", "-i")
	arguments = append(arguments, resolved.Environment...)
	arguments = append(arguments, resolved.Path)
	arguments = append(arguments, resolved.Argv[1:]...)
	command := runner.run(ctx, "tmux", arguments...)
	output, err := command.CombinedOutput()
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("launch tmux: %w: %s", err, strings.TrimSpace(string(output)))
	}
	handle := strings.TrimSpace(string(output))
	if handle == "" {
		return ExecutionResult{}, fmt.Errorf("launch tmux returned an empty handle")
	}
	if paste && request.Prompt != "" {
		if err := runner.tmuxPaste(ctx, handle, request.Prompt); err != nil {
			return ExecutionResult{}, err
		}
	}
	return ExecutionResult{
		State: "submitted", LaunchHandle: handle,
		CaptureQuality: "transcript_only",
	}, nil
}

func (runner *Runner) tmuxPaste(ctx context.Context, handle, prompt string) error {
	bufferName := fmt.Sprintf("sn-runtime-%d", os.Getpid())
	for _, invocation := range [][]string{
		{"set-buffer", "-b", bufferName, "--", prompt},
		{"paste-buffer", "-d", "-b", bufferName, "-t", handle},
		{"send-keys", "-t", handle, "Enter"},
	} {
		command := runner.run(ctx, "tmux", invocation...)
		if output, err := command.CombinedOutput(); err != nil {
			return fmt.Errorf("tmux %s: %w: %s", invocation[0], err, strings.TrimSpace(string(output)))
		}
	}
	return nil
}

func (runner *Runner) launchTerminal(
	ctx context.Context,
	resolved Invocation,
	request ExecutionRequest,
	paste bool,
) (ExecutionResult, error) {
	if runtime.GOOS != "darwin" {
		return ExecutionResult{}, fmt.Errorf("terminal transport is only supported on macOS")
	}
	driver := strings.TrimSpace(request.TerminalDriver)
	if driver == "" {
		return ExecutionResult{}, fmt.Errorf("terminal_driver is required")
	}
	commandText := shellCommand(resolved)
	var script string
	var arguments []string
	switch driver {
	case "ghostty":
		script = ghosttyScript
		initial := ""
		if paste && request.Prompt != "" {
			initial = request.Prompt + "\n"
		}
		arguments = []string{request.CWD, commandText, initial}
		arguments = append(arguments, resolved.Environment...)
	case "iterm2":
		script = iterm2Script
		commandText = shellCommandAt(resolved, request.CWD)
		initial := ""
		if paste && request.Prompt != "" {
			initial = request.Prompt
		}
		arguments = []string{commandText, initial}
	default:
		return ExecutionResult{}, fmt.Errorf("unsupported terminal driver %q", driver)
	}
	command := runner.run(ctx, "osascript", append([]string{"-e", script, "--"}, arguments...)...)
	output, err := command.CombinedOutput()
	if err != nil {
		return ExecutionResult{}, fmt.Errorf("launch %s: %w: %s", driver, err, strings.TrimSpace(string(output)))
	}
	handle := strings.TrimSpace(string(output))
	if handle == "" {
		return ExecutionResult{}, fmt.Errorf("launch %s returned an empty handle", driver)
	}
	return ExecutionResult{
		State: "submitted", LaunchHandle: handle,
		CaptureQuality: "transcript_only",
	}, nil
}

func shellCommand(resolved Invocation) string {
	values := []string{"/usr/bin/env", "-i"}
	values = append(values, resolved.Environment...)
	values = append(values, resolved.Path)
	values = append(values, resolved.Argv[1:]...)
	quoted := make([]string, 0, len(values))
	for _, value := range values {
		quoted = append(quoted, shellQuote(value))
	}
	return strings.Join(quoted, " ")
}

func shellCommandAt(resolved Invocation, cwd string) string {
	command := shellCommand(resolved)
	if cwd == "" {
		return command
	}
	return "cd -- " + shellQuote(cwd) + " && exec " + command
}

func shellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

const ghosttyScript = `
on run argv
  set cwd to item 1 of argv
  set commandText to item 2 of argv
  set initialText to item 3 of argv
  set environmentValues to {}
  if (count of argv) > 3 then set environmentValues to items 4 thru -1 of argv
  tell application "Ghostty"
    activate
    set cfg to new surface configuration
    if cwd is not "" then set initial working directory of cfg to cwd
    set command of cfg to commandText
    set environment variables of cfg to environmentValues
    set wait after command of cfg to true
    if initialText is not "" then set initial input of cfg to initialText
    set win to new window with configuration cfg
    return id of win
  end tell
end run`

const iterm2Script = `
on run argv
  set commandText to item 1 of argv
  set initialText to item 2 of argv
  tell application "iTerm2"
    activate
    set win to (create window with default profile command commandText)
    if initialText is not "" then
      tell current session of win to write text initialText
    end if
    return id of win
  end tell
end run`
