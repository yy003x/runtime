package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/runtime/internal/infrastructure/layout"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
	runtimeprofile "github.com/yy003x/runtime/pkg/profile"
)

func TestCLIOutputJSONErrorUsesStableCompactEnvelope(t *testing.T) {
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	output := newCLIOutput(true, &stdout, &stderr)

	exitCode := output.fail(&contract.RuntimeError{
		Code: contract.ErrorRateLimited, Phase: contract.PhaseTransport,
		Message: "retry later", Retryable: true, RetryAfterMS: 250,
		HTTPStatus: 429, Provider: "fixture", RequestID: "request_1",
	})

	if exitCode != 1 || stdout.Len() != 0 {
		t.Fatalf("exit=%d stdout=%q", exitCode, stdout.String())
	}
	var payload struct {
		SchemaVersion   int `json:"schema_version"`
		ContractVersion int `json:"contract_version"`
		Error           struct {
			Code         string `json:"code"`
			Phase        string `json:"phase"`
			Message      string `json:"message"`
			Retryable    bool   `json:"retryable"`
			RetryAfterMS int64  `json:"retry_after_ms"`
			HTTPStatus   int    `json:"http_status"`
			Provider     string `json:"provider"`
			RequestID    string `json:"request_id"`
		} `json:"error"`
	}
	if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
		t.Fatalf("stderr is not one JSON document: %v: %q", err, stderr.String())
	}
	if payload.SchemaVersion != 1 || payload.ContractVersion != 5 ||
		payload.Error.Code != string(contract.ErrorRateLimited) ||
		payload.Error.Phase != string(contract.PhaseTransport) ||
		!payload.Error.Retryable || payload.Error.RetryAfterMS != 250 ||
		payload.Error.HTTPStatus != 429 ||
		payload.Error.Provider != "fixture" ||
		payload.Error.RequestID != "request_1" ||
		!strings.Contains(payload.Error.Message, "retry later") {
		t.Fatalf("payload=%#v", payload)
	}
	if strings.Count(strings.TrimSpace(stderr.String()), "\n") != 0 {
		t.Fatalf("error must be compact: %q", stderr.String())
	}
}

func TestCLIOutputOnlyClassifiesMarkedValidationErrorsAsInvalidRequest(
	t *testing.T,
) {
	for _, test := range []struct {
		name string
		err  error
		code contract.ErrorCode
	}{
		{
			name: "validation",
			err:  cliValidationf("unknown option --bad"),
			code: contract.ErrorInvalidRequest,
		},
		{
			name: "unclassified_io_or_store",
			err:  errors.New("store read failed"),
			code: contract.ErrorInternal,
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			var stderr bytes.Buffer
			output := newCLIOutput(
				true, &bytes.Buffer{}, &stderr,
			)
			if exitCode := output.fail(test.err); exitCode != 1 {
				t.Fatalf("exit=%d", exitCode)
			}
			var payload struct {
				Error contract.RuntimeError `json:"error"`
			}
			if err := json.Unmarshal(stderr.Bytes(), &payload); err != nil {
				t.Fatal(err)
			}
			if payload.Error.Code != test.code ||
				payload.Error.Phase != contract.PhaseRequest {
				t.Fatalf("error=%#v", payload.Error)
			}
		})
	}
}

func TestMachineEnvelopeOverridesDomainSchemaOnlyAtOuterLayer(t *testing.T) {
	payload := machineEnvelope(map[string]any{
		"schema_version":   2,
		"contract_version": 99,
		"session": map[string]any{
			"schema_version": 2,
			"session_id":     "session_1",
		},
	}).(map[string]any)
	if payload["schema_version"] != cliOutputSchemaVersion ||
		payload["contract_version"] != cliOutputContractVersion {
		t.Fatalf("outer payload=%#v", payload)
	}
	sessionValue := payload["session"].(map[string]any)
	if sessionValue["schema_version"] != 2 {
		t.Fatalf("nested session=%#v", sessionValue)
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
	if payload.ContractVersion != 5 ||
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
		[]string{"api", "--stream", "hello"},
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
				"req", "api", "--stream", "--unknown-option",
			},
		},
		{
			name: "agent",
			args: []string{
				"agent", "api", "--stream", "--unknown-option",
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
			if payload.ContractVersion != 5 ||
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
		  "command":"codex"
		}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
	err := runProfileExecutionNamespace(
		paths, []string{"cx", "--stream", "extra"},
		runtimeprofile.KindCommand, runtimecommand.ModeInteractive,
		"direct", output,
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
