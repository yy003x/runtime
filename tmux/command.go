package tmux

import (
	"bytes"
	"context"
	"errors"
	"fmt"
	"os/exec"
)

type osCommandRunner struct{}

func (osCommandRunner) Run(
	ctx context.Context,
	spec CommandSpec,
) (CommandResult, error) {
	command := exec.CommandContext(ctx, spec.Path, spec.Args...)
	command.Env = spec.Env
	command.Dir = spec.Dir
	command.Stdin = spec.Stdin
	if spec.Stdout != nil || spec.Stderr != nil {
		command.Stdout = spec.Stdout
		command.Stderr = spec.Stderr
		err := command.Run()
		exitCode := commandExitCode(err)
		if err != nil && exitCode < 0 {
			return CommandResult{}, err
		}
		return CommandResult{ExitCode: exitCode}, nil
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	command.Stdout = &stdout
	command.Stderr = &stderr
	err := command.Run()
	exitCode := commandExitCode(err)
	if err != nil && exitCode < 0 {
		return CommandResult{}, err
	}
	return CommandResult{
		Stdout: stdout.Bytes(), Stderr: stderr.Bytes(), ExitCode: exitCode,
	}, nil
}

func commandExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}

func commandFailure(operation string, result CommandResult) error {
	message := string(bytes.TrimSpace(result.Stderr))
	if message == "" {
		message = string(bytes.TrimSpace(result.Stdout))
	}
	if message == "" {
		message = fmt.Sprintf("exit status %d", result.ExitCode)
	}
	return fmt.Errorf("%s: %s", operation, message)
}
