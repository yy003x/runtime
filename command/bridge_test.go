package command

import (
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yy003x/runtime/contract"
)

func TestBuildCodexCanonicalOverridesSelectorsAndOrdersScopes(t *testing.T) {
	root := t.TempDir()
	commandPath := writeCommandFixture(t, root, "codex")
	model := "gpt-5.6-sol"
	effort := EffortHigh
	invocation, err := Build(BuildRequest{
		Mode:           ModeExec,
		OutputProtocol: OutputCanonical,
		Profile: Profile{
			Command: commandPath,
			Args: []string{
				"--sandbox", "read-only",
				"-c", "model=old",
				"-c", "model_reasoning_effort=low",
				"-c", "model_verbosity=high",
				"exec", "--skip-git-repo-check",
				"--ephemeral", "--json",
			},
			Model: "profile-model", Effort: EffortXHigh,
		},
		Overrides:  Overrides{Model: &model, Effort: &effort},
		ArgvPrompt: stringPointer("reply ok"),
		InheritedEnvironment: []string{
			"PATH=" + root,
			"KEEP=value",
		},
		InvocationBase: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		commandPath,
		"--sandbox", "read-only",
		"-c", "model_verbosity=high",
		"--model", "gpt-5.6-sol",
		"-c", "model_reasoning_effort=high",
		"exec", "--skip-git-repo-check",
		"--ephemeral", "--json",
		"--", "reply ok",
	}
	if !reflect.DeepEqual(invocation.Argv, want) {
		t.Fatalf("argv=%q\nwant=%q", invocation.Argv, want)
	}
}

func TestAdapterRejectsRegisteredOptionAsRequiredValue(t *testing.T) {
	for _, args := range [][]string{
		{"--model", "--json"},
		{"--sandbox", "exec"},
		{"--output-format", "--model"},
	} {
		commandName := "codex"
		if args[0] == "--output-format" {
			commandName = "claude"
		}
		if err := CheckProfile(Profile{
			Command: commandName,
			Args:    args,
		}); err == nil {
			t.Fatalf("accepted args=%#v", args)
		}
	}
}

func TestBuildCodexInteractiveRemovesExecOnlyAndAppendsPrompt(t *testing.T) {
	root := t.TempDir()
	commandPath := writeCommandFixture(t, root, "codex")
	invocation, err := Build(BuildRequest{
		Mode: ModeInteractive, OutputProtocol: OutputNative,
		Profile: Profile{
			Command: commandPath, Exec: true,
			Args: []string{
				"--sandbox", "read-only", "exec",
				"--skip-git-repo-check", "--ephemeral", "--json",
			},
		},
		ArgvPrompt:           stringPointer("typed prompt"),
		InheritedEnvironment: []string{"PATH=" + root},
		InvocationBase:       root,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		commandPath, "--sandbox", "read-only",
		"--", "typed prompt",
	}
	if !reflect.DeepEqual(invocation.Argv, want) {
		t.Fatalf("argv=%q want=%q", invocation.Argv, want)
	}
}

func TestBuildClaudeCanonicalIsStateless(t *testing.T) {
	root := t.TempDir()
	commandPath := writeCommandFixture(t, root, "claude")
	invocation, err := Build(BuildRequest{
		Mode: ModeExec, OutputProtocol: OutputCanonical,
		Profile: Profile{
			Command: commandPath,
			Args: []string{
				"--dangerously-skip-permissions",
				"--model", "old", "--effort", "low",
				"-p", "--no-session-persistence",
				"--output-format", "stream-json",
			},
			Model: "new", Effort: EffortMax,
		},
		ArgvPrompt:           stringPointer("-leading prompt"),
		InheritedEnvironment: []string{"PATH=" + root},
		InvocationBase:       root,
	})
	if err != nil {
		t.Fatal(err)
	}
	want := []string{
		commandPath,
		"--dangerously-skip-permissions",
		"--model", "new", "--effort", "max",
		"--no-session-persistence", "--output-format", "json",
		"-p", "--", "-leading prompt",
	}
	if !reflect.DeepEqual(invocation.Argv, want) {
		t.Fatalf("argv=%q want=%q", invocation.Argv, want)
	}
}

func TestBuildUsesImmutableInheritedSnapshotForArgsEnvAndCWD(t *testing.T) {
	root := t.TempDir()
	commandPath := writeCommandFixture(t, root, "codex")
	work := filepath.Join(root, "work")
	if err := os.Mkdir(work, 0o700); err != nil {
		t.Fatal(err)
	}
	invocation, err := Build(BuildRequest{
		Mode: ModeInteractive, OutputProtocol: OutputNative,
		Profile: Profile{
			Command: commandPath,
			Args:    []string{"--image", "${IMAGE}"},
			Env: map[string]*string{
				"IMAGE": stringPointer("profile-value"),
				"OUT":   stringPointer("${IMAGE}"),
				"DROP":  nil,
			},
			CWD: "${WORK}",
		},
		InheritedEnvironment: []string{
			"PATH=" + root, "IMAGE=inherited-value",
			"WORK=" + work, "DROP=yes",
		},
		InvocationBase: root,
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(
		invocation.Argv,
		[]string{commandPath, "--image", "inherited-value"},
	) {
		t.Fatalf("argv=%q", invocation.Argv)
	}
	env := environmentMap(invocation.Environment)
	if env["IMAGE"] != "profile-value" || env["OUT"] != "inherited-value" {
		t.Fatalf("env=%#v", env)
	}
	if _, exists := env["DROP"]; exists {
		t.Fatalf("DROP reached target: %#v", env)
	}
	if invocation.CWD != work {
		t.Fatalf("cwd=%q want=%q", invocation.CWD, work)
	}
}

func TestBuildRejectsMissingReferenceAndOversizedEnvironmentToken(t *testing.T) {
	root := t.TempDir()
	commandPath := writeCommandFixture(t, root, "codex")
	base := BuildRequest{
		Mode: ModeInteractive, OutputProtocol: OutputNative,
		Profile: Profile{
			Command: commandPath,
			Args:    []string{"--image", "${MISSING}"},
		},
		InheritedEnvironment: []string{"PATH=" + root},
		InvocationBase:       root,
	}
	_, err := Build(base)
	var runtimeErr *contract.RuntimeError
	if err == nil || !strings.Contains(err.Error(), "MISSING") ||
		!errors.As(err, &runtimeErr) ||
		runtimeErr.Code != contract.ErrorInvalidRequest ||
		runtimeErr.Phase != contract.PhaseProfile {
		t.Fatalf("missing reference error=%v", err)
	}
	base.Profile.Args = nil
	base.InheritedEnvironment = append(
		base.InheritedEnvironment,
		"HUGE="+strings.Repeat("x", MaxTokenBytes),
	)
	_, err = Build(base)
	runtimeErr = nil
	if err == nil || !strings.Contains(err.Error(), "env[") ||
		!errors.As(err, &runtimeErr) ||
		runtimeErr.Code != contract.ErrorContextOverflow ||
		runtimeErr.Phase != contract.PhaseRequest {
		t.Fatalf("oversized env error=%v", err)
	}
}

func TestCanonicalDecodersRejectAmbiguousResults(t *testing.T) {
	codex := []byte(strings.Join([]string{
		`{"type":"thread.started"}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"first"}}`,
		`{"type":"item.completed","item":{"type":"agent_message","text":"final"}}`,
		`{"type":"turn.completed"}`,
	}, "\n"))
	result, err := Decode("codex", codex)
	if err != nil || result.Assistant != "final" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if _, err := Decode(
		"codex",
		append(codex, []byte("\n{\"type\":\"turn.completed\"}")...),
	); err == nil {
		t.Fatal("duplicate Codex terminal was accepted")
	} else {
		var runtimeErr *contract.RuntimeError
		if !errors.As(err, &runtimeErr) ||
			runtimeErr.Code != contract.ErrorInvalidProviderResponse ||
			runtimeErr.Phase != contract.PhaseTransport {
			t.Fatalf("decode error=%v", err)
		}
	}
	claude := []byte(`{"type":"result","subtype":"success","is_error":false,"result":"OK"}`)
	result, err = Decode("claude", claude)
	if err != nil || result.Assistant != "OK" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	if _, err := Decode("claude", append(claude, claude...)); err == nil {
		t.Fatal("multiple Claude documents were accepted")
	}
}

func TestResolveAndMergePrompt(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "prompt.txt")
	if err := os.WriteFile(path, []byte("from file"), 0o600); err != nil {
		t.Fatal(err)
	}
	fromFile, err := ResolvePrompt("prompt.txt", root)
	if err != nil || fromFile != "from file" {
		t.Fatalf("prompt=%q error=%v", fromFile, err)
	}
	literal, err := ResolvePrompt("does not exist", root)
	if err != nil || literal != "does not exist" {
		t.Fatalf("prompt=%q error=%v", literal, err)
	}
	merged, err := MergePrompt("base", "", "typed", "stdin", "position")
	if err != nil || merged != "base\ntyped\nstdin\nposition" {
		t.Fatalf("merged=%q error=%v", merged, err)
	}
	link := filepath.Join(root, "link.txt")
	if err := os.Symlink(path, link); err == nil {
		if _, err := ResolvePrompt("link.txt", root); err == nil {
			t.Fatal("prompt symlink was accepted")
		}
	}
	_, err = MergePrompt(
		strings.Repeat("x", MaxTokenBytes),
		"overflow",
	)
	var runtimeErr *contract.RuntimeError
	if err == nil || !errors.As(err, &runtimeErr) ||
		runtimeErr.Code != contract.ErrorContextOverflow ||
		runtimeErr.Phase != contract.PhaseRequest {
		t.Fatalf("merge overflow error=%v", err)
	}
}

func TestResolveExecutableUsesProfileEnvironmentCWDAndPATH(t *testing.T) {
	root := t.TempDir()
	work := filepath.Join(root, "work")
	bin := filepath.Join(work, "bin")
	if err := os.MkdirAll(bin, 0o700); err != nil {
		t.Fatal(err)
	}
	commandPath := filepath.Join(bin, "codex")
	if err := os.WriteFile(commandPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	pathValue := "./bin"
	resolved, err := ResolveExecutable(
		Profile{
			Command: "codex", CWD: "work",
			Env: map[string]*string{"PATH": &pathValue},
		},
		root,
		[]string{"PATH=/definitely/not/used"},
	)
	if err != nil {
		t.Fatal(err)
	}
	if resolved != commandPath {
		t.Fatalf("resolved=%q want=%q", resolved, commandPath)
	}
}

func TestReplaceProcessAppliesCWDAndNullStdin(t *testing.T) {
	const childMarker = "SN_COMMAND_REPLACE_CHILD"
	if os.Getenv(childMarker) == "1" {
		targetCWD := os.Getenv("SN_COMMAND_REPLACE_CWD")
		err := ReplaceProcess(Invocation{
			Path: "/bin/sh",
			Argv: []string{
				"/bin/sh", "-c",
				"pwd; if IFS= read -r value; then echo unexpected; else echo eof; fi",
			},
			Environment: []string{},
			CWD:         targetCWD,
		}, StdinNull)
		panic(err)
	}
	root := t.TempDir()
	child := exec.Command(os.Args[0], "-test.run=^TestReplaceProcessAppliesCWDAndNullStdin$")
	child.Env = append(
		os.Environ(),
		childMarker+"=1",
		"SN_COMMAND_REPLACE_CWD="+root,
	)
	output, err := child.CombinedOutput()
	if err != nil {
		t.Fatalf("child error=%v output=%q", err, output)
	}
	canonicalRoot, err := filepath.EvalSymlinks(root)
	if err != nil {
		t.Fatal(err)
	}
	if string(output) != canonicalRoot+"\neof\n" {
		t.Fatalf("output=%q", output)
	}
}

func writeCommandFixture(t *testing.T, directory, name string) string {
	t.Helper()
	path := filepath.Join(directory, name)
	if err := os.WriteFile(path, []byte("#!/bin/sh\nexit 0\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	return path
}
