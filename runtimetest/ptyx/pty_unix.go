//go:build darwin || linux

// Package ptyx provides the small PTY surface used by SN Runtime golden
// tests. It intentionally lives under runtimetest so production binaries do
// not depend on the PTY implementation.
package ptyx

import (
	"errors"
	"io"
	"os"
	"os/exec"
	"syscall"

	"github.com/creack/pty"
)

// Process is a child process attached to a pseudo-terminal.
type Process struct {
	Cmd    *exec.Cmd
	Master *os.File
}

// Start launches cmd in a new session with one PTY connected to stdin, stdout,
// and stderr.
func Start(cmd *exec.Cmd) (*Process, error) {
	master, err := pty.StartWithSize(cmd, &pty.Winsize{Rows: 40, Cols: 120})
	if err != nil {
		return nil, err
	}
	return &Process{Cmd: cmd, Master: master}, nil
}

// Signal sends a signal to the whole foreground process group created for the
// PTY command.
func (p *Process) Signal(signal syscall.Signal) error {
	if p == nil || p.Cmd == nil || p.Cmd.Process == nil {
		return os.ErrInvalid
	}
	pgid, err := syscall.Getpgid(p.Cmd.Process.Pid)
	if err != nil {
		return err
	}
	return syscall.Kill(-pgid, signal)
}

// ReadAll drains the PTY. Linux reports EIO after the slave side closes; that
// condition is the PTY equivalent of EOF and is normalized here.
func (p *Process) ReadAll() ([]byte, error) {
	if p == nil || p.Master == nil {
		return nil, os.ErrInvalid
	}
	value, err := io.ReadAll(p.Master)
	if errors.Is(err, syscall.EIO) {
		err = nil
	}
	return value, err
}

// ExitCode returns the portable process exit status represented by Wait.
func ExitCode(err error) int {
	if err == nil {
		return 0
	}
	var exitError *exec.ExitError
	if errors.As(err, &exitError) {
		return exitError.ExitCode()
	}
	return -1
}
