package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
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

func TestMainHelpDocumentsJSONProfileBoundary(t *testing.T) {
	stdout, stderr, exitCode := captureMainOutput(t, []string{"help"})
	if exitCode != 0 || stderr != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	for _, expected := range []string{
		"sn-cli --json <api-profile-id> [options...] [prompt]",
		"stable API Profile/management output; must be first",
		"CLI Profile output remains target-native",
	} {
		if !strings.Contains(stdout, expected) {
			t.Fatalf("help missing %q:\n%s", expected, stdout)
		}
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
	if payload.Error.Message != `unknown profile "system"` {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestTopLevelProfileManagementNamesAreUnknownProfiles(t *testing.T) {
	for _, name := range []string{"list", "show", "check"} {
		t.Run(name, func(t *testing.T) {
			paths := prepareVNextHome(t)
			t.Setenv("SN_CLI_HOME", paths.Home)
			stdout, stderr, exitCode := captureMainOutput(
				t, []string{"--json", name},
			)
			if exitCode == 0 || stdout != "" {
				t.Fatalf(
					"exit=%d stdout=%q stderr=%q",
					exitCode, stdout, stderr,
				)
			}
			var payload struct {
				Error struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(stderr), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error.Message != fmt.Sprintf(
				"unknown profile %q", name,
			) {
				t.Fatalf("payload=%#v", payload)
			}
		})
	}
}

func TestMainImplicitAndExplicitAPIProfileAreEquivalent(t *testing.T) {
	server := httptest.NewTLSServer(http.HandlerFunc(func(
		writer http.ResponseWriter,
		_ *http.Request,
	) {
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{
		  "id":"req-root",
		  "model":"fixture",
		  "choices":[{"message":{"content":"OK"},"finish_reason":"stop"}]
		}`))
	}))
	defer server.Close()
	paths := prepareVNextHome(t)
	writeVNextModel(
		t, paths.ConfigDir, "api-cx",
		server.URL+"/v1/chat/completions",
	)
	t.Setenv("SN_CLI_HOME", paths.Home)
	t.Setenv("MODEL_API_KEY", "secret")
	originalTransport := http.DefaultTransport
	http.DefaultTransport = server.Client().Transport
	t.Cleanup(func() { http.DefaultTransport = originalTransport })

	implicitOut, implicitErr, implicitExit := captureMainOutput(
		t, []string{"api-cx", "reply OK"},
	)
	explicitOut, explicitErr, explicitExit := captureMainOutput(
		t, []string{"profile", "api-cx", "reply OK"},
	)
	if implicitExit != 0 || explicitExit != 0 ||
		implicitErr != "" || explicitErr != "" ||
		implicitOut != explicitOut || implicitOut != "OK\n" {
		t.Fatalf(
			"implicit=(%d,%q,%q) explicit=(%d,%q,%q)",
			implicitExit, implicitOut, implicitErr,
			explicitExit, explicitOut, explicitErr,
		)
	}

	implicitJSON, implicitJSONErr, implicitJSONExit := captureMainOutput(
		t, []string{"--json", "api-cx", "reply OK"},
	)
	explicitJSON, explicitJSONErr, explicitJSONExit := captureMainOutput(
		t, []string{"--json", "profile", "api-cx", "reply OK"},
	)
	if implicitJSONExit != 0 || explicitJSONExit != 0 ||
		implicitJSONErr != "" || explicitJSONErr != "" ||
		implicitJSON != explicitJSON {
		t.Fatalf(
			"implicit JSON=(%d,%q,%q) explicit JSON=(%d,%q,%q)",
			implicitJSONExit, implicitJSON, implicitJSONErr,
			explicitJSONExit, explicitJSON, explicitJSONErr,
		)
	}
	var payload struct {
		SchemaVersion   int    `json:"schema_version"`
		ContractVersion int    `json:"contract_version"`
		State           string `json:"state"`
		Result          struct {
			Message struct {
				Content string `json:"content"`
			} `json:"message"`
			FinishReason string `json:"finish_reason"`
		} `json:"result"`
	}
	if err := json.Unmarshal([]byte(implicitJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SchemaVersion != cliOutputSchemaVersion ||
		payload.ContractVersion != cliOutputContractVersion ||
		payload.State != "completed" ||
		payload.Result.Message.Content != "OK" ||
		payload.Result.FinishReason != "stop" {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestFixedNamespacesAreReservedProfileIDs(t *testing.T) {
	for _, id := range fixedNamespaces {
		t.Run(id, func(t *testing.T) {
			paths := prepareVNextHome(t)
			writeVNextCommand(t, paths.ConfigDir, id)
			_, err := runtimebootstrap.LoadVNext(paths, fixedNamespaces...)
			if err == nil || !strings.Contains(
				err.Error(), "reserved profile ID",
			) {
				t.Fatalf("error=%v", err)
			}
		})
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
