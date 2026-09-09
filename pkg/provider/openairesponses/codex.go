package openairesponses

import (
	"net/http"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
	"github.com/yy003x/runtime/pkg/provider"
)

// NewCodex constructs the separate Codex subscription protocol adapter. It
// shares Responses decoding, but enforces its own endpoint and request grammar.
// OAuth login, credential refresh and retries are owned by the caller.
func NewCodex(client *http.Client) *Driver {
	return &Driver{client: provider.SingleAttemptClient(client), codex: true}
}

func encodeCodexRequest(resolved model.ResolvedModel, request contract.ModelRequest) ([]byte, error) {
	if err := (model.Profile{Driver: model.DriverOpenAICodexResponses}).ValidateSingleCallInput(request); err != nil {
		return nil, err
	}
	// Keep ToolSpec.InputSchema as raw JSON. A map[string]any round trip would
	// coerce large integer schema values to float64 and silently change them.
	if request.System == "" {
		request.System = "You are a helpful assistant."
	}
	resolved.Parameters.Stream = nil // nil selects the required streaming mode.
	return encodeRequest(resolved, request)
}
