package cli

import (
	"context"
	"errors"
	"fmt"
	"strings"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
)

func runRawModelCall(
	ctx context.Context,
	generator model.Generator,
	request contract.GenerateRequest,
	output *cliOutput,
) error {
	var last *model.Attempt
	ctx = model.WithAttemptObserver(ctx, func(attempt model.Attempt) {
		last = &attempt
	})
	_, runtimeErr := generator.Generate(ctx, request)
	if last == nil {
		if runtimeErr != nil {
			return runtimeErr
		}
		return fmt.Errorf("raw Provider attempt diagnostics are unavailable")
	}
	// Raw output is the final wire request body and response JSON, not a Runtime
	// envelope. The complete retry history belongs to the execution log.
	var response any
	if wire := last.Wire.Response; wire != nil && len(wire.Data) > 0 {
		if wire.Format == "sse" || wire.Format == "" && strings.Contains(strings.ToLower(wire.Headers["Content-Type"]), "text/event-stream") {
			response = wire.Data
		} else {
			response = wire.Data[0]
		}
	}
	writeErr := output.encodeJSON(last.Wire.Request.Body)
	if writeErr == nil {
		writeErr = output.encodeJSON(response)
	}
	if runtimeErr != nil {
		return errors.Join(runtimeErr, writeErr)
	}
	return writeErr
}
