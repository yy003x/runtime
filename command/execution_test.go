package command

import (
	"context"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestRunnerTTYArgvAndEnvironment(t *testing.T) {
	script := writeExecutable(t, `#!/bin/sh
printf '%s|%s' "$1" "$RUNTIME_EXECUTION_TEST"
`)
	runner := NewRunner()
	result, err := runner.Execute(context.Background(), Profile{
		Binary: script, Transport: TransportTTY, PromptDelivery: PromptArgv,
		Env: map[string]*string{"RUNTIME_EXECUTION_TEST": stringPointer("configured")},
	}, ExecutionRequest{Args: []string{"native"}, Prompt: "prompt"})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "completed" || result.ExitCode != 0 ||
		result.Stdout != "native|configured" {
		t.Fatalf("result=%#v", result)
	}
}

func TestRunnerTTYStdinDelivery(t *testing.T) {
	script := writeExecutable(t, `#!/bin/sh
IFS= read -r value
printf 'input:%s' "$value"
`)
	result, err := NewRunner().Execute(context.Background(), Profile{
		Binary: script, Transport: TransportTTY, PromptDelivery: PromptStdin,
	}, ExecutionRequest{Prompt: "hello"})
	if err != nil {
		t.Fatal(err)
	}
	if result.Stdout != "input:hello" || result.CaptureQuality != "parsed" {
		t.Fatalf("result=%#v", result)
	}
}

func TestRunnerManualRejectsAutomaticInput(t *testing.T) {
	_, err := NewRunner().Execute(context.Background(), Profile{
		Binary: "/bin/true", Transport: TransportTTY, PromptDelivery: PromptManual,
	}, ExecutionRequest{Prompt: "must not be delivered"})
	if err == nil || !strings.Contains(err.Error(), "manual prompt delivery") {
		t.Fatalf("err=%v", err)
	}
}

func TestRunnerTmuxPastesOnlyForPasteDelivery(t *testing.T) {
	temp := t.TempDir()
	logFile := filepath.Join(temp, "tmux.log")
	writeNamedExecutable(t, temp, "tmux", `#!/bin/sh
printf '%s\n' "$*" >> "$RUNTIME_TMUX_LOG"
if [ "$1" = "new-session" ]; then printf '%s\n' '$1:@1.%1'; fi
`)
	t.Setenv("PATH", temp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUNTIME_TMUX_LOG", logFile)
	runner := NewRunner()
	for _, fixture := range []struct {
		name      string
		delivery  PromptDelivery
		wantPaste bool
	}{
		{name: "argv", delivery: PromptArgv},
		{name: "paste", delivery: PromptPaste, wantPaste: true},
	} {
		t.Run(fixture.name, func(t *testing.T) {
			if err := os.WriteFile(logFile, nil, 0o600); err != nil {
				t.Fatal(err)
			}
			result, err := runner.Execute(context.Background(), Profile{
				Binary: "/bin/echo", Transport: TransportTmux,
				PromptDelivery: fixture.delivery,
			}, ExecutionRequest{Prompt: "hello"})
			if err != nil {
				t.Fatal(err)
			}
			if result.State != "submitted" || result.LaunchHandle != "$1:@1.%1" ||
				result.CaptureQuality != "transcript_only" {
				t.Fatalf("result=%#v", result)
			}
			content, err := os.ReadFile(logFile)
			if err != nil {
				t.Fatal(err)
			}
			hasPaste := strings.Contains(string(content), "set-buffer")
			if hasPaste != fixture.wantPaste {
				t.Fatalf("log=%q wantPaste=%v", content, fixture.wantPaste)
			}
		})
	}
}

func TestRunnerTerminalBuildsDriverRequest(t *testing.T) {
	if runtime.GOOS != "darwin" {
		t.Skip("terminal transport is macOS-only")
	}
	temp := t.TempDir()
	logFile := filepath.Join(temp, "osascript.log")
	writeNamedExecutable(t, temp, "osascript", `#!/bin/sh
printf '%s\n' "$*" > "$RUNTIME_OSASCRIPT_LOG"
printf 'window-fixture\n'
`)
	t.Setenv("PATH", temp+string(os.PathListSeparator)+os.Getenv("PATH"))
	t.Setenv("RUNTIME_OSASCRIPT_LOG", logFile)
	result, err := NewRunner().Execute(context.Background(), Profile{
		Binary: "/bin/echo", Transport: TransportTerminal, PromptDelivery: PromptPaste,
	}, ExecutionRequest{Prompt: "hello", TerminalDriver: "ghostty"})
	if err != nil {
		t.Fatal(err)
	}
	if result.State != "submitted" || result.LaunchHandle != "window-fixture" {
		t.Fatalf("result=%#v", result)
	}
	content, err := os.ReadFile(logFile)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(content), "hello") ||
		!strings.Contains(string(content), "/bin/echo") {
		t.Fatalf("osascript args=%q", content)
	}
}

func TestShellQuote(t *testing.T) {
	if got := shellQuote("a'b c"); got != `'a'"'"'b c'` {
		t.Fatalf("shellQuote=%q", got)
	}
}

func TestShellCommandAtPreservesWorkingDirectory(t *testing.T) {
	command := shellCommandAt(Invocation{
		Path: "/bin/echo", Argv: []string{"echo", "hello"},
	}, "/tmp/space dir")
	if command != "cd -- '/tmp/space dir' && exec '/usr/bin/env' '-i' '/bin/echo' 'hello'" {
		t.Fatalf("command=%q", command)
	}
}

func writeExecutable(t *testing.T, content string) string {
	t.Helper()
	return writeNamedExecutable(t, t.TempDir(), "fixture", content)
}

func writeNamedExecutable(t *testing.T, directory, name, content string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte(content), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
