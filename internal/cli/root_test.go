package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yy003x/runtime/internal/runtimebootstrap"
)

func TestMainLeadingJSONVersion(t *testing.T) {
	stdout, stderr, exitCode := captureMainOutput(t, []string{"--json", "version"})
	if exitCode != 0 || stderr != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["contract_version"] != float64(3) {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestMainHelpAndVersionRejectTrailingArguments(t *testing.T) {
	for _, args := range [][]string{
		{"help", "unexpected"},
		{"--help", "--json"},
		{"version", "unexpected"},
		{"--version", "--json"},
	} {
		stdout, stderr, exitCode := captureMainOutput(t, args)
		if exitCode == 0 || stdout != "" || stderr == "" {
			t.Fatalf(
				"args=%q exit=%d stdout=%q stderr=%q",
				args, exitCode, stdout, stderr,
			)
		}
	}
}

func TestMainRejectsRemovedSystemNamespace(t *testing.T) {
	paths := prepareVNextHome(t)
	t.Setenv("SN_CLI_HOME", paths.Home)
	stdout, stderr, exitCode := captureMainOutput(t, []string{"--json", "system", "status"})
	if exitCode == 0 || stdout != "" {
		t.Fatalf("exit=%d stdout=%q", exitCode, stdout)
	}
	var payload struct {
		Error struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.Error.Message != `unknown command "system"` {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestSystemCanBeConfiguredAsOrdinaryShortcut(t *testing.T) {
	paths := prepareVNextHome(t)
	writeVNextCommand(t, paths.ConfigDir, "cx")
	if err := os.WriteFile(
		filepath.Join(paths.CommandDir, "system.json"),
		[]byte(`{"profile":"cx"}`+"\n"),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	runtime, err := runtimebootstrap.LoadVNext(paths, fixedNamespaces...)
	if err != nil {
		t.Fatal(err)
	}
	subcommand, exists := runtime.Subcommands.Get("system")
	if !exists || subcommand.Profile != "cx" {
		t.Fatalf("subcommand=%#v exists=%t", subcommand, exists)
	}
}

func captureMainOutput(t *testing.T, args []string) (string, string, int) {
	t.Helper()
	originalStdout := os.Stdout
	originalStderr := os.Stderr
	stdoutRead, stdoutWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	stderrRead, stderrWrite, err := os.Pipe()
	if err != nil {
		t.Fatal(err)
	}
	os.Stdout = stdoutWrite
	os.Stderr = stderrWrite
	defer func() {
		os.Stdout = originalStdout
		os.Stderr = originalStderr
	}()
	exitCode := Main(args)
	_ = stdoutWrite.Close()
	_ = stderrWrite.Close()
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	_, _ = stdout.ReadFrom(stdoutRead)
	_, _ = stderr.ReadFrom(stderrRead)
	_ = stdoutRead.Close()
	_ = stderrRead.Close()
	return stdout.String(), stderr.String(), exitCode
}
