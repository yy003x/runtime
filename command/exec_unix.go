//go:build darwin || linux

package command

import (
	"fmt"
	"os"
	"syscall"

	"golang.org/x/sys/unix"
)

func replaceProcess(resolved Invocation) error {
	return syscall.Exec(resolved.Path, resolved.Argv, resolved.Environment)
}

func replaceProcessWithInput(resolved Invocation, input string) error {
	file, err := os.CreateTemp("", ".sn-runtime-prompt-*")
	if err != nil {
		return fmt.Errorf("create prompt input: %w", err)
	}
	path := file.Name()
	_ = os.Remove(path)
	defer file.Close()
	if _, err := file.WriteString(input); err != nil {
		return fmt.Errorf("write prompt input: %w", err)
	}
	if _, err := file.Seek(0, 0); err != nil {
		return fmt.Errorf("rewind prompt input: %w", err)
	}
	original, err := unix.Dup(0)
	if err != nil {
		return fmt.Errorf("duplicate stdin: %w", err)
	}
	defer unix.Close(original) //nolint:errcheck
	if err := unix.Dup2(int(file.Fd()), 0); err != nil {
		return fmt.Errorf("replace stdin: %w", err)
	}
	if err := replaceProcess(resolved); err != nil {
		_ = unix.Dup2(original, 0)
		return err
	}
	return nil
}
