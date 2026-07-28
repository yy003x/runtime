// Package cli provides the thin JSON/NDJSON encoder used by the future
// `sn-cli model generate` command. Argument parsing and production routing are
// intentionally not wired in this phase.
package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
)

func Generate(
	ctx context.Context,
	generator model.Generator,
	request contract.GenerateRequest,
	stream bool,
	output io.Writer,
) *contract.RuntimeError {
	if generator == nil {
		return transportError("model generator is required")
	}
	if output == nil {
		return transportError("output writer is required")
	}
	encoder := json.NewEncoder(output)
	encoder.SetEscapeHTML(false)
	if stream {
		_, runtimeErr := generator.GenerateStream(
			ctx,
			request,
			func(event contract.Event) error {
				if err := encoder.Encode(event); err != nil {
					return fmt.Errorf("encode model event: %w", err)
				}
				return nil
			},
		)
		return runtimeErr
	}
	result, runtimeErr := generator.Generate(ctx, request)
	if runtimeErr != nil {
		return runtimeErr
	}
	if err := encoder.Encode(result); err != nil {
		return transportError(fmt.Sprintf("encode model result: %v", err))
	}
	return nil
}

func transportError(message string) *contract.RuntimeError {
	return &contract.RuntimeError{
		Code: contract.ErrorInternal, Phase: contract.PhaseTransport,
		Message: message, Retryable: false,
	}
}
