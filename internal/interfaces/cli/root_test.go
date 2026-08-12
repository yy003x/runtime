package cli

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/pkg/contract"
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
	if payload["contract_version"] != float64(6) {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestMainHelpDocumentsPublicNamespacesAndJSONProfileBoundary(t *testing.T) {
	stdout, stderr, exitCode := captureMainOutput(t, []string{"help"})
	if exitCode != 0 || stderr != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	for _, expected := range []string{
		"sn-cli <cli-profile-id> [options...] [input]",
		"sn-cli <cli-profile-id> resume [session-id]",
		"sn-cli exec <cli-profile-id> [options...] [input]",
		"sn-cli req <api-profile-id> [options...] [input]",
		"sn-cli doctor",
		"sn-cli session exec <cli-profile-id> [options...] [input]",
		"sn-cli session req <api-profile-id> [options...] [input]",
		"sn-cli session open <cli-profile-id> [--attach|--detach] [options...] [input]",
		"sn-cli session send|attach|interrupt|close --session-id <id>",
		"sn-cli session close-all",
		"sn-cli session list|show|messages|events|logs|executions|execution",
		"sn-cli session reconcile|configure|export|delete|gc",
		"sn-cli tmux open <cli-profile-id> [options...] [input]",
		"sn-cli tmux list|show|send|attach|interrupt|stop|stop-all",
		"sn-cli agent <api-profile-id> [options...] [input]",
		"sn-cli run get|list|result|trace|events|watch|cancel|resume|retry|reconcile|gc",
		"sn-cli help <topic>",
		"session close-all  close Session-bound native TUI windows",
		"tmux stop-all      close raw windows only",
		"Control audit      <runtime-home>/logs/YYMMDD/audit.jsonl",
		"Server process     <runtime-home>/logs/sn-server.log",
		"stable req/management output; must be first",
		"direct/exec CLI output remains target-native",
		"Tools:               <runtime-home>/tools",
	} {
		if !strings.Contains(stdout, expected) {
			t.Fatalf("help missing %q:\n%s", expected, stdout)
		}
	}
}

func TestMainHelpTopicsHaveHumanAndMachineContracts(t *testing.T) {
	topics := []string{
		"direct", "exec", "req", "doctor", "profile",
		"session", "tmux", "agent", "run", "server",
	}
	for _, topic := range topics {
		t.Run(topic, func(t *testing.T) {
			stdout, stderr, exitCode := captureMainOutput(
				t, []string{"help", topic},
			)
			if exitCode != 0 || stderr != "" ||
				!strings.Contains(stdout, "sn-cli help "+topic) ||
				!strings.Contains(stdout, "Usage:") {
				t.Fatalf(
					"exit=%d stdout=%q stderr=%q",
					exitCode, stdout, stderr,
				)
			}
			stdout, stderr, exitCode = captureMainOutput(
				t, []string{"--json", "help", topic},
			)
			if exitCode != 0 || stderr != "" {
				t.Fatalf(
					"exit=%d stdout=%q stderr=%q",
					exitCode, stdout, stderr,
				)
			}
			var payload struct {
				ContractVersion int          `json:"contract_version"`
				Topic           cliHelpTopic `json:"topic"`
			}
			if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.ContractVersion != cliOutputContractVersion ||
				payload.Topic.Name != topic || len(payload.Topic.Usage) == 0 {
				t.Fatalf("payload=%#v", payload)
			}
		})
	}

	stdout, stderr, exitCode := captureMainOutput(
		t, []string{"--json", "help"},
	)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	var rootPayload struct {
		Topics   []string       `json:"topics"`
		Commands []cliHelpTopic `json:"commands"`
	}
	if err := json.Unmarshal([]byte(stdout), &rootPayload); err != nil {
		t.Fatal(err)
	}
	if len(rootPayload.Topics) != len(topics) ||
		len(rootPayload.Commands) != len(topics) {
		t.Fatalf("payload=%#v", rootPayload)
	}
}

func TestMainHelpAndVersionRejectTrailingArguments(t *testing.T) {
	for _, args := range [][]string{
		{"help", "unexpected"},
		{"help", "session", "open"},
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

func TestMainDoctorIsTopLevelAndWritesAudit(t *testing.T) {
	paths := prepareVNextHome(t)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	stdout, stderr, exitCode := captureMainOutput(
		t, []string{"--json", "doctor"},
	)
	if exitCode != 0 || stderr != "" {
		t.Fatalf("exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr)
	}
	var payload map[string]any
	if err := json.Unmarshal([]byte(stdout), &payload); err != nil {
		t.Fatal(err)
	}
	if payload["ok"] != true || payload["contract_version"] == nil ||
		payload["runtime_home"] != paths.Home ||
		payload["log_root"] != paths.LogsDir ||
		payload["tmux_window_count"] == nil {
		t.Fatalf("payload=%#v", payload)
	}
	day := time.Now().Format("060102")
	audit, err := os.ReadFile(filepath.Join(paths.LogsDir, day, "audit.jsonl"))
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(audit), `"namespace":"doctor"`) ||
		!strings.Contains(string(audit), `"outcome":"succeeded"`) {
		t.Fatalf("audit=%s", audit)
	}
	_, _, serverDoctorExit := captureMainOutput(
		t, []string{"--json", "server", "doctor"},
	)
	if serverDoctorExit == 0 {
		t.Fatal("removed server doctor route was accepted")
	}
}

func TestRuntimeDoctorDoesNotRequireCommandArgumentReferences(t *testing.T) {
	paths := prepareVNextHome(t)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	commandPath := filepath.Join(t.TempDir(), "codex")
	if err := os.WriteFile(commandPath, []byte("#!/bin/sh\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	profile := fmt.Sprintf(
		`{"type":"cli","command":%q,"args":["--image","${WB_RUNTIME_IMAGE_PATH}"]}`,
		commandPath,
	)
	if err := os.WriteFile(
		filepath.Join(paths.ConfigDir, "argument-reference.json"),
		[]byte(profile),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	previous, existed := os.LookupEnv("WB_RUNTIME_IMAGE_PATH")
	if err := os.Unsetenv("WB_RUNTIME_IMAGE_PATH"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if existed {
			_ = os.Setenv("WB_RUNTIME_IMAGE_PATH", previous)
		} else {
			_ = os.Unsetenv("WB_RUNTIME_IMAGE_PATH")
		}
	})
	var stdout bytes.Buffer
	err := runtimeDoctor(
		paths,
		newCLIOutput(false, &stdout, &bytes.Buffer{}),
	)
	if err != nil || !strings.Contains(stdout.String(), "Runtime doctor: OK") {
		t.Fatalf("stdout=%q error=%v", stdout.String(), err)
	}
}

func TestMainMachineArgumentErrorsAreCanonicalInvalidRequests(t *testing.T) {
	paths := prepareVNextHome(t)
	writeVNextCommand(t, paths.ConfigDir, "cx")
	writeVNextModel(
		t, paths.ConfigDir, "api",
		"https://example.invalid/v1/chat/completions",
	)
	t.Setenv("SN_CLI_HOME", paths.Home)
	for _, test := range []struct {
		name string
		args []string
	}{
		{
			name: "session_id",
			args: []string{
				"--json", "session", "show", "--session-id", "bad",
			},
		},
		{
			name: "run_id",
			args: []string{
				"--json", "run", "get", "--run-id", "bad",
			},
		},
		{
			name: "unknown_option",
			args: []string{
				"--json", "session", "show", "--unknown", "value",
			},
		},
		{
			name: "profile_usage",
			args: []string{"--json", "profile"},
		},
		{
			name: "command_profile_unknown_option",
			args: []string{
				"--json", "exec", "cx", "--unknown",
			},
		},
		{
			name: "api_profile_unknown_option",
			args: []string{
				"--json", "req", "api", "--unknown",
			},
		},
		{
			name: "profile_not_found",
			args: []string{
				"--json", "req", "missing", "input",
			},
		},
		{
			name: "removed_profile_execution",
			args: []string{
				"--json", "profile", "cx", "input",
			},
		},
		{
			name: "agent_parse",
			args: []string{
				"--json", "agent", "api", "--unknown",
			},
		},
		{
			name: "agent_profile_not_found",
			args: []string{
				"--json", "agent", "missing", "input",
			},
		},
		{
			name: "removed_run_submit",
			args: []string{
				"--json", "run", "submit", "input",
			},
		},
		{
			name: "tmux_parse",
			args: []string{
				"--json", "tmux", "list", "unexpected",
			},
		},
		{
			name: "tmux_profile_not_found",
			args: []string{
				"--json", "tmux", "open", "missing",
			},
		},
		{
			name: "server_trailing",
			args: []string{
				"--json", "server", "status", "unexpected",
			},
		},
		{
			name: "server_unknown_action",
			args: []string{
				"--json", "server", "unknown",
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			stdout, stderr, exitCode := captureMainOutput(t, test.args)
			if exitCode != 1 || stdout != "" {
				t.Fatalf(
					"exit=%d stdout=%q stderr=%q",
					exitCode, stdout, stderr,
				)
			}
			var payload struct {
				Error contract.RuntimeError `json:"error"`
			}
			if err := json.Unmarshal([]byte(stderr), &payload); err != nil {
				t.Fatalf("stderr=%q error=%v", stderr, err)
			}
			if payload.Error.Code != contract.ErrorInvalidRequest ||
				payload.Error.Phase != contract.PhaseRequest {
				t.Fatalf("error=%#v", payload.Error)
			}
		})
	}
}

func TestMainMachineConfigurationFailureRemainsInternal(t *testing.T) {
	paths := prepareVNextHome(t)
	if err := os.WriteFile(
		paths.ConfigDir+"/broken.json", []byte(`{"type":"cli"`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	t.Setenv("SN_CLI_HOME", paths.Home)
	stdout, stderr, exitCode := captureMainOutput(
		t, []string{"--json", "profile", "list"},
	)
	if exitCode != 1 || stdout != "" {
		t.Fatalf(
			"exit=%d stdout=%q stderr=%q", exitCode, stdout, stderr,
		)
	}
	var payload struct {
		Error contract.RuntimeError `json:"error"`
	}
	if err := json.Unmarshal([]byte(stderr), &payload); err != nil {
		t.Fatalf("stderr=%q error=%v", stderr, err)
	}
	if payload.Error.Code != contract.ErrorInternal {
		t.Fatalf("configuration failure was misclassified: %#v", payload.Error)
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

func TestMainReqIsTheOnlyAPIProfileExecutionNamespace(t *testing.T) {
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

	reqOut, reqErr, reqExit := captureMainOutput(
		t, []string{"req", "api-cx", "reply OK"},
	)
	if reqExit != 0 || reqErr != "" || reqOut != "OK\n" {
		t.Fatalf(
			"req=(%d,%q,%q)", reqExit, reqOut, reqErr,
		)
	}

	reqJSON, reqJSONErr, reqJSONExit := captureMainOutput(
		t, []string{"--json", "req", "api-cx", "reply OK"},
	)
	if reqJSONExit != 0 || reqJSONErr != "" {
		t.Fatalf(
			"req JSON=(%d,%q,%q)", reqJSONExit, reqJSON, reqJSONErr,
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
	if err := json.Unmarshal([]byte(reqJSON), &payload); err != nil {
		t.Fatal(err)
	}
	if payload.SchemaVersion != cliOutputSchemaVersion ||
		payload.ContractVersion != cliOutputContractVersion ||
		payload.State != "completed" ||
		payload.Result.Message.Content != "OK" ||
		payload.Result.FinishReason != "stop" {
		t.Fatalf("payload=%#v", payload)
	}
	for _, args := range [][]string{
		{"api-cx", "reply OK"},
		{"profile", "api-cx", "reply OK"},
	} {
		stdout, stderr, exitCode := captureMainOutput(t, args)
		if exitCode != 1 || stdout != "" || stderr == "" {
			t.Fatalf(
				"removed API route args=%q exit=%d stdout=%q stderr=%q",
				args, exitCode, stdout, stderr,
			)
		}
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
