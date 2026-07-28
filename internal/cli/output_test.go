package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/layout"
)

func TestCLIOutputJSONErrorUsesStableCompactEnvelope(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	output := newCLIOutput(true, &stdout, &stderr)

	exitCode := output.fail(&contract.RuntimeError{
		Code: contract.ErrorRateLimited, Message: "retry later",
	})

	if exitCode != 1 || stdout.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q", exitCode, stdout.String())
	}
	var payload struct {
		SchemaVersion   int `json:"schema_version"`
		ContractVersion int `json:"contract_version"`
		Error           struct {
			Code    string `json:"code"`
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
		t.Fatalf("stderr is not one JSON document: %v: %q", err, stderr.String())
	}
	if payload.SchemaVersion != 1 || payload.ContractVersion != 2 ||
		payload.Error.Code != string(contract.ErrorRateLimited) ||
		!strings.Contains(payload.Error.Message, "retry later") {
		t.Fatalf("payload=%#v", payload)
	}
	if strings.Count(strings.TrimSpace(stderr.String()), "\n") != 0 {
		t.Fatalf("error must be compact: %q", stderr.String())
	}
}

func TestCLIOutputStreamFailureDoesNotWriteFinal(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	output := newCLIOutput(false, &stdout, &stderr)
	output.beginStream()
	if err := output.writeEvent(map[string]string{"type": "started"}); err != nil {
		t.Fatal(err)
	}
	exitCode := output.fail(errors.New("stream failed"))
	if exitCode != 1 {
		t.Fatalf("exit=%d", exitCode)
	}
	if strings.Contains(stdout.String(), `"final"`) {
		t.Fatalf("unexpected final output: %q", stdout.String())
	}
	if !json.Valid(bytes.TrimSpace(stderr.Bytes())) {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestCLIOutputStreamFailureBeforeFirstEventIsJSON(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	output := newCLIOutput(false, &stdout, &stderr)
	output.beginStream()

	exitCode := output.fail(errors.New("failed before first event"))

	if exitCode != 1 || stdout.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q", exitCode, stdout.String())
	}
	var payload struct {
		ContractVersion int `json:"contract_version"`
		Error           struct {
			Message string `json:"message"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
		t.Fatalf("stderr=%q error=%v", stderr.String(), err)
	}
	if payload.ContractVersion != 2 ||
		payload.Error.Message != "failed before first event" {
		t.Fatalf("payload=%#v", payload)
	}
}

func TestRunWatchSelectsMachineErrorsBeforeServiceValidation(t *testing.T) {
	output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
	if err := runRunNamespaceVNext(
		layout.Paths{}, []string{"watch"}, output,
	); err == nil {
		t.Fatal("expected validation failure")
	}
	if !output.streamMode || output.streamStarted {
		t.Fatalf(
			"streamMode=%t streamStarted=%t",
			output.streamMode, output.streamStarted,
		)
	}
}

func TestAgentStreamBeginsAfterSuccessfulParseBeforeServiceFailure(t *testing.T) {
	output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
	if err := runAgentNamespace(
		layout.Paths{},
		[]string{"run", "--profile", "api", "--stream", "hello"},
		output,
	); err == nil {
		t.Fatal("expected service initialization failure")
	}
	if !output.streamMode || output.streamStarted {
		t.Fatalf(
			"streamMode=%t streamStarted=%t",
			output.streamMode, output.streamStarted,
		)
	}
}

func TestSelectedStreamSyntaxErrorsUseMachineErrorWithoutFinal(t *testing.T) {
	paths := prepareVNextHome(t)
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
			name: "profile",
			args: []string{
				"profile", "api", "--stream", "--unknown-option",
			},
		},
		{
			name: "agent",
			args: []string{
				"agent", "run", "--profile", "api", "--stream", "--unknown-option",
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
			lines := strings.Split(strings.TrimSpace(stderr), "\n")
			if len(lines) != 1 {
				t.Fatalf("stderr is not one NDJSON error line: %q", stderr)
			}
			var payload struct {
				ContractVersion int `json:"contract_version"`
				Error           struct {
					Message string `json:"message"`
				} `json:"error"`
			}
			if err := json.Unmarshal([]byte(lines[0]), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.ContractVersion != 2 ||
				!strings.Contains(payload.Error.Message, "unknown") ||
				strings.Contains(stderr, `"final"`) {
				t.Fatalf("payload=%#v stderr=%q", payload, stderr)
			}
		})
	}
}

func TestCLIProfileNativeStreamArgumentDoesNotSelectRuntimeStreamErrors(
	t *testing.T,
) {
	paths := prepareVNextHome(t)
	if err := os.WriteFile(
		filepath.Join(paths.ConfigDir, "cx.json"),
		[]byte(`{
		  "type":"cli",
		  "binary":"codex",
		  "transport":"tty",
		  "prompt_delivery":"argv"
		}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
	err := runVNextProfileNamespace(
		paths, []string{"cx", "--stream", "extra"}, output,
	)
	if err == nil {
		t.Fatal("expected CLI prompt validation failure")
	}
	if output.streamMode || output.streamStarted {
		t.Fatalf(
			"CLI arguments changed Runtime output mode: streamMode=%t streamStarted=%t",
			output.streamMode, output.streamStarted,
		)
	}
}
