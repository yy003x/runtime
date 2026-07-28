package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/yy003x/runtime/contract"
)

const (
	cliOutputSchemaVersion   = 1
	cliOutputContractVersion = 2
)

type cliOutput struct {
	json          bool
	stdout        io.Writer
	stderr        io.Writer
	streamMode    bool
	streamStarted bool
}

func newCLIOutput(jsonOutput bool, stdout, stderr io.Writer) *cliOutput {
	return &cliOutput{
		json: jsonOutput, stdout: stdout, stderr: stderr,
	}
}

func (output *cliOutput) JSON() bool {
	return output != nil && output.json
}

func (output *cliOutput) writeJSON(value any) error {
	encoder := json.NewEncoder(output.stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func (output *cliOutput) beginStream() {
	output.streamMode = true
}

func (output *cliOutput) writeEvent(value any) error {
	output.streamStarted = true
	return output.writeJSON(value)
}

func (output *cliOutput) writeFinal(value any) error {
	return output.writeJSON(value)
}

func (output *cliOutput) line(format string, args ...any) error {
	_, err := fmt.Fprintf(output.stdout, format+"\n", args...)
	return err
}

func (output *cliOutput) text(value string) error {
	if value == "" {
		return nil
	}
	if _, err := io.WriteString(output.stdout, value); err != nil {
		return err
	}
	if value[len(value)-1] != '\n' {
		_, err := io.WriteString(output.stdout, "\n")
		return err
	}
	return nil
}

func (output *cliOutput) diagnostic(value string) error {
	if value == "" {
		return nil
	}
	if _, err := io.WriteString(output.stderr, value); err != nil {
		return err
	}
	if value[len(value)-1] != '\n' {
		_, err := io.WriteString(output.stderr, "\n")
		return err
	}
	return nil
}

func (output *cliOutput) fail(err error) int {
	if err == nil {
		return 0
	}
	if output.JSON() || output.streamMode {
		code := string(contract.ErrorInternal)
		var runtimeErr *contract.RuntimeError
		if errors.As(err, &runtimeErr) && runtimeErr.Code != "" {
			code = string(runtimeErr.Code)
		}
		payload := map[string]any{
			"schema_version":   cliOutputSchemaVersion,
			"contract_version": cliOutputContractVersion,
			"error": map[string]string{
				"code": code, "message": err.Error(),
			},
		}
		encoder := json.NewEncoder(output.stderr)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(payload)
		return 1
	}
	_, _ = fmt.Fprintf(output.stderr, "error: %v\n", err)
	return 1
}
