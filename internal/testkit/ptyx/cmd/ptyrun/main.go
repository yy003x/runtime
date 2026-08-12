//go:build darwin || linux

// ptyrun executes one command with a controlling pseudo-terminal and mirrors
// its combined output. Release checks use it to exercise interactive CLI
// entrypoints without depending on platform-specific script(1) syntax.
package main

import (
	"fmt"
	"os"
	"os/exec"

	"github.com/yy003x/runtime/internal/testkit/ptyx"
)

type readResult struct {
	value []byte
	err   error
}

func main() {
	if len(os.Args) < 2 {
		fmt.Fprintln(os.Stderr, "usage: ptyrun <command> [args...]")
		os.Exit(2)
	}
	command := exec.Command(os.Args[1], os.Args[2:]...)
	command.Env = os.Environ()
	process, err := ptyx.Start(command)
	if err != nil {
		fmt.Fprintf(os.Stderr, "start command under PTY: %v\n", err)
		os.Exit(1)
	}
	defer process.Master.Close()

	read := make(chan readResult, 1)
	go func() {
		value, readErr := process.ReadAll()
		read <- readResult{value: value, err: readErr}
	}()
	waitErr := process.Cmd.Wait()
	result := <-read
	if len(result.value) > 0 {
		_, _ = os.Stdout.Write(result.value)
	}
	if result.err != nil {
		fmt.Fprintf(os.Stderr, "read PTY output: %v\n", result.err)
		os.Exit(1)
	}
	if waitErr != nil {
		code := ptyx.ExitCode(waitErr)
		if code < 0 {
			fmt.Fprintf(os.Stderr, "wait for command: %v\n", waitErr)
			os.Exit(1)
		}
		os.Exit(code)
	}
}
