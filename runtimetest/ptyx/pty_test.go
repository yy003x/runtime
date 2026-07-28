//go:build darwin || linux

package ptyx

import (
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"testing"
	"time"
)

func TestStartProvidesTTYAndBidirectionalIO(t *testing.T) {
	command := exec.Command("/bin/sh", "-c", `
if [ -t 0 ] && [ -t 1 ] && [ -t 2 ]; then
  printf 'tty=yes\n'
else
  printf 'tty=no\n'
fi
IFS= read -r line
printf 'input=%s\n' "$line"
`)
	process, err := Start(command)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = process.Master.Close() })

	output := make(chan struct {
		value []byte
		err   error
	}, 1)
	go func() {
		value, readErr := process.ReadAll()
		output <- struct {
			value []byte
			err   error
		}{value: value, err: readErr}
	}()
	if _, err := process.Master.Write([]byte("hello pty\n")); err != nil {
		t.Fatal(err)
	}
	if err := process.Cmd.Wait(); err != nil {
		t.Fatalf("wait: %v", err)
	}
	result := <-output
	if result.err != nil {
		t.Fatal(result.err)
	}
	text := strings.ReplaceAll(string(result.value), "\r\n", "\n")
	if !strings.Contains(text, "tty=yes\n") || !strings.Contains(text, "input=hello pty\n") {
		t.Fatalf("output=%q", text)
	}
}

func TestSignalReachesPTYProcessGroupAndPreservesExitCode(t *testing.T) {
	ready := filepath.Join(t.TempDir(), "ready")
	command := exec.Command("/bin/sh", "-c", `
trap 'exit 130' INT
: > "$READY"
while :; do
  sleep 1
done
`)
	command.Env = append(os.Environ(), "READY="+ready)
	process, err := Start(command)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		_ = process.Signal(syscall.SIGKILL)
		_ = process.Master.Close()
	})

	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := os.Stat(ready); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("PTY child did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}
	if err := process.Signal(syscall.SIGINT); err != nil {
		t.Fatal(err)
	}
	wait := make(chan error, 1)
	go func() { wait <- process.Cmd.Wait() }()
	select {
	case err := <-wait:
		if code := ExitCode(err); code != 130 {
			t.Fatalf("exit code=%d err=%v", code, err)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("PTY child did not exit after SIGINT")
	}
}
