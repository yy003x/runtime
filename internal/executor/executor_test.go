package executor

import (
	"context"
	"errors"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"sync"
	"sync/atomic"
	"syscall"
	"testing"
	"time"
)

func TestRunStreamsAndCapturesOutput(t *testing.T) {
	var stdout strings.Builder
	var stderr strings.Builder
	var outputLock sync.Mutex
	var first atomic.Int32
	result, err := Run(context.Background(), Options{
		Argv: []string{"sh", "-c", "printf 'out'; printf 'err' >&2"},
		Env:  os.Environ(),
		Observer: Observer{
			Stdout: func(value []byte) {
				outputLock.Lock()
				defer outputLock.Unlock()
				stdout.Write(value)
			},
			Stderr: func(value []byte) {
				outputLock.Lock()
				defer outputLock.Unlock()
				stderr.Write(value)
			},
			FirstOutput: func() { first.Add(1) },
		},
	})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.ExitCode != 0 || result.Stdout != "out" || result.Stderr != "err" {
		t.Fatalf("result=%#v", result)
	}
	if stdout.String() != "out" || stderr.String() != "err" || first.Load() != 1 {
		t.Fatalf("stdout=%q stderr=%q first=%d", stdout.String(), stderr.String(), first.Load())
	}
}

func TestRunResolvesBinaryFromProvidedPath(t *testing.T) {
	root := t.TempDir()
	tool := filepath.Join(root, "runtime-test-tool")
	if err := os.WriteFile(tool, []byte("#!/bin/sh\nprintf 'path-ok'\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	result, err := Run(context.Background(), Options{
		Argv: []string{"runtime-test-tool"},
		Env:  []string{"PATH=" + root},
	})
	if err != nil || result.Stdout != "path-ok" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestResolveEnvShebangWithDYLDInterpose(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("DYLD shebang workaround is macOS specific")
	}
	root := t.TempDir()
	script := filepath.Join(root, "tool")
	if err := os.WriteFile(script, []byte("#!/usr/bin/env -S sh -e\nprintf ok\n"), 0o755); err != nil {
		t.Fatal(err)
	}
	env := []string{"PATH=/bin:/usr/bin", "DYLD_INSERT_LIBRARIES=/tmp/runtime-test.dylib"}
	interpreter, args := resolveEnvShebang(script, env)
	if filepath.Base(interpreter) != "sh" {
		t.Fatalf("interpreter=%q", interpreter)
	}
	if len(args) != 2 || args[0] != "-e" || args[1] != script {
		t.Fatalf("args=%q", args)
	}
}

func TestRunCancellationKillsProcessGroup(t *testing.T) {
	ctx, cancel := context.WithTimeout(context.Background(), 150*time.Millisecond)
	defer cancel()
	var info ProcessInfo
	started := time.Now()
	result, err := Run(ctx, Options{
		Argv:        []string{"sh", "-c", "trap '' INT TERM; sleep 30 & wait"},
		Env:         os.Environ(),
		GracePeriod: 100 * time.Millisecond,
		Observer:    Observer{Started: func(value ProcessInfo) { info = value }},
	})
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if time.Since(started) > 2*time.Second {
		t.Fatalf("cancellation took too long: %s", time.Since(started))
	}
	if info.PID <= 0 || info.PGID != info.PID {
		t.Fatalf("process info=%#v", info)
	}
	deadline := time.Now().Add(2 * time.Second)
	for {
		err := syscall.Kill(-info.PGID, 0)
		if errors.Is(err, syscall.ESRCH) {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("process group %d still exists: %v", info.PGID, err)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
