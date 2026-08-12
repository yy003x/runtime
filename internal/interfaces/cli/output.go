package cli

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"

	"github.com/yy003x/runtime/pkg/contract"
)

const (
	cliOutputSchemaVersion   = 1
	cliOutputContractVersion = 6
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
	value = machineEnvelope(value)
	return output.encodeJSON(value)
}

func (output *cliOutput) encodeJSON(value any) error {
	encoder := json.NewEncoder(output.stdout)
	encoder.SetEscapeHTML(false)
	return encoder.Encode(value)
}

func (output *cliOutput) beginStream() {
	output.streamMode = true
}

func (output *cliOutput) writeEvent(value any) error {
	output.streamStarted = true
	return output.encodeJSON(value)
}

func (output *cliOutput) writeFinal(value any) error {
	return output.writeJSON(value)
}

func machineEnvelope(value any) any {
	if source, ok := value.(map[string]any); ok {
		object := make(map[string]any, len(source)+2)
		for key, current := range source {
			object[key] = current
		}
		object["schema_version"] = cliOutputSchemaVersion
		object["contract_version"] = cliOutputContractVersion
		return object
	}
	data, err := json.Marshal(value)
	if err != nil {
		return value
	}
	var object map[string]any
	if err := json.Unmarshal(data, &object); err != nil || object == nil {
		return map[string]any{
			"schema_version":   cliOutputSchemaVersion,
			"contract_version": cliOutputContractVersion,
			"result":           value,
		}
	}
	object["schema_version"] = cliOutputSchemaVersion
	object["contract_version"] = cliOutputContractVersion
	return object
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
		phase := string(contract.PhaseRequest)
		retryable := false
		retryAfterMS := int64(0)
		httpStatus := 0
		provider := ""
		requestID := ""
		var runtimeErr *contract.RuntimeError
		if errors.As(err, &runtimeErr) {
			if runtimeErr.Code != "" {
				code = string(runtimeErr.Code)
			}
			if runtimeErr.Phase != "" {
				phase = string(runtimeErr.Phase)
			}
			retryable = runtimeErr.Retryable
			retryAfterMS = runtimeErr.RetryAfterMS
			httpStatus = runtimeErr.HTTPStatus
			provider = runtimeErr.Provider
			requestID = runtimeErr.RequestID
		} else {
			var validationErr *cliValidationError
			if errors.As(err, &validationErr) {
				code = string(contract.ErrorInvalidRequest)
				phase = string(contract.PhaseRequest)
			}
		}
		errorValue := map[string]any{
			"code": code, "phase": phase,
			"message": err.Error(), "retryable": retryable,
		}
		if retryAfterMS > 0 {
			errorValue["retry_after_ms"] = retryAfterMS
		}
		if httpStatus > 0 {
			errorValue["http_status"] = httpStatus
		}
		if provider != "" {
			errorValue["provider"] = provider
		}
		if requestID != "" {
			errorValue["request_id"] = requestID
		}
		payload := map[string]any{
			"schema_version":   cliOutputSchemaVersion,
			"contract_version": cliOutputContractVersion,
			"error":            errorValue,
		}
		encoder := json.NewEncoder(output.stderr)
		encoder.SetEscapeHTML(false)
		_ = encoder.Encode(payload)
		return 1
	}
	_, _ = fmt.Fprintf(output.stderr, "error: %v\n", err)
	return 1
}
