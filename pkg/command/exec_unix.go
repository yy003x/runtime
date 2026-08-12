//go:build darwin || linux

package command

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

type StdinMode string

const (
	StdinInherit StdinMode = "inherit"
	StdinTTY     StdinMode = "tty"
	StdinNull    StdinMode = "null"
)

// ReplaceProcess replaces the Runtime process with an already-built target
// invocation. Profile, Session, and Tmux choose stdin ownership explicitly at
// their ingress.
func ReplaceProcess(resolved Invocation, stdinMode StdinMode) error {
	if len(resolved.Argv) == 0 {
		return fmt.Errorf("invocation argv is empty")
	}
	if stdinMode == StdinInherit ||
		stdinMode == StdinTTY && isTerminalFD(int(os.Stdin.Fd())) {
		return execInvocation(resolved)
	}
	var (
		file *os.File
		err  error
	)
	switch stdinMode {
	case StdinTTY:
		file, err = os.OpenFile("/dev/tty", os.O_RDWR, 0)
	case StdinNull:
		file, err = os.Open("/dev/null")
	default:
		return fmt.Errorf("unsupported stdin mode %q", stdinMode)
	}
	if err != nil {
		if stdinMode == StdinTTY {
			return fmt.Errorf("open controlling TTY: %w", err)
		}
		return fmt.Errorf("open /dev/null: %w", err)
	}
	defer file.Close()
	original, err := unix.Dup(0)
	if err != nil {
		return fmt.Errorf("duplicate stdin: %w", err)
	}
	defer unix.Close(original) //nolint:errcheck
	if err := unix.Dup2(int(file.Fd()), 0); err != nil {
		return fmt.Errorf("replace stdin: %w", err)
	}
	if err := execInvocation(resolved); err != nil {
		_ = unix.Dup2(original, 0)
		return err
	}
	return nil
}

func execInvocation(resolved Invocation) error {
	originalCWD, err := os.Open(".")
	if err != nil {
		return fmt.Errorf("open current working directory: %w", err)
	}
	defer originalCWD.Close()
	if resolved.CWD != "" {
		if err := os.Chdir(resolved.CWD); err != nil {
			return fmt.Errorf("enter cwd %q: %w", resolved.CWD, err)
		}
	}
	if err := syscall.Exec(
		resolved.Path, resolved.Argv, resolved.Environment,
	); err != nil {
		if restoreErr := unix.Fchdir(int(originalCWD.Fd())); restoreErr != nil {
			return fmt.Errorf(
				"exec target: %w; restore cwd: %v", err, restoreErr,
			)
		}
		return err
	}
	return nil
}
