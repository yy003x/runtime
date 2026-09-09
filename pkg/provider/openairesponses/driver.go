// Package openairesponses implements single-call OpenAI Responses requests.
// The driver performs one HTTP attempt with no local persistence, tool loop or
// retry. Wire diagnostics preserve the consumed upstream JSON independently of
// the canonical result projection.
package openairesponses

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	httpx "github.com/yy003x/runtime/internal/infrastructure/providerhttp"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
	"github.com/yy003x/runtime/pkg/provider"
)

const providerName = "openai-responses"

type Driver struct {
	client *http.Client
	codex  bool
}

func New(client *http.Client) *Driver {
	return &Driver{client: provider.SingleAttemptClient(client)}
}

func (driver *Driver) ExecutionIdentity() model.DriverExecutionIdentity {
	if driver.codex {
		return model.DriverExecutionIdentity{Driver: model.DriverOpenAICodexResponses,
			Implementation: "runtime.provider.openai-codex-responses", ImplementationVersion: 1}
	}
	return model.DriverExecutionIdentity{
		Driver:                model.DriverOpenAIResponses,
		Implementation:        "runtime.provider.openai-responses",
		ImplementationVersion: 1,
	}
}

func (driver *Driver) Validate(profile model.Profile) error {
	if profile.Driver != driver.ExecutionIdentity().Driver {
		return fmt.Errorf("%s driver cannot serve %q", driver.ExecutionIdentity().Driver, profile.Driver)
	}
	return profile.Validate()
}

func (driver *Driver) Stream(
	ctx context.Context,
	resolved model.ResolvedModel,
	request contract.ModelRequest,
	sink contract.EventSink,
) (result contract.ModelResult, attempt provider.Attempt, runtimeErr *contract.RuntimeError) {
	if driver.codex {
		defer func() {
			if runtimeErr != nil {
				runtimeErr.Provider = string(model.DriverOpenAICodexResponses)
			}
		}()
		if resolved.Driver != model.DriverOpenAICodexResponses || resolved.Endpoint != model.OpenAICodexEndpoint {
			return result, attempt, httpx.ProtocolError(string(model.DriverOpenAICodexResponses), "invalid Codex destination")
		}
	}
	payload, err := encodeRequest(resolved, request)
	if driver.codex {
		payload, err = encodeCodexRequest(resolved, request)
	}
	if err != nil {
		return result, attempt, &contract.RuntimeError{
			Code: contract.ErrorInvalidRequest, Phase: contract.PhaseRequest,
			Message: err.Error(), Provider: providerName,
		}
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, resolved.Endpoint, bytes.NewReader(payload))
	if err != nil {
		return result, attempt, httpx.ProtocolError(providerName, err.Error())
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "application/json")
	if resolved.Parameters.StreamingEnabled() {
		httpRequest.Header.Set("Accept", "text/event-stream, application/json")
	}
	for name, value := range resolved.RequestHeaders() {
		httpRequest.Header.Set(name, value)
	}
	if driver.codex {
		httpRequest.Header.Set("Accept", "text/event-stream")
		httpRequest.Header.Set("OpenAI-Beta", "responses=experimental")
		httpRequest.Header.Set("Originator", "sn-cli")
		httpRequest.Header.Set("User-Agent", "sn-runtime")
	}
	attempt = provider.Attempt{Started: true, Request: provider.Request{
		Method: httpRequest.Method, URL: provider.SafeURL(httpRequest.URL.String()),
		Headers: provider.RequestHeaders(httpRequest.Header, resolved.LogRequestHeaders()),
		Body:    append(json.RawMessage(nil), payload...),
	}}
	response, err := driver.client.Do(httpRequest)
	if err != nil {
		return result, attempt, httpx.NetworkError(providerName, provider.SafeNetworkError(err))
	}
	defer response.Body.Close()
	responseFormat := response.Header.Get("Content-Type")
	if driver.codex && response.StatusCode >= 200 && response.StatusCode < 300 {
		// Codex always streams; gateways may omit or mislabel Content-Type.
		// Use the same framing for decoding and capture, without changing wire headers.
		responseFormat = "text/event-stream"
	}
	attempt.Response = &provider.Response{
		Status: response.StatusCode, Headers: provider.SafeHeaders(response.Header),
		Data: make([]json.RawMessage, 0),
	}
	var captured bytes.Buffer
	reader := io.TeeReader(response.Body, &captured)
	defer func() {
		attempt.Response.Data = provider.ResponseData(captured.Bytes(), responseFormat)
		attempt.Response.Format = "json"
		if strings.Contains(strings.ToLower(responseFormat), "text/event-stream") {
			attempt.Response.Format = "sse"
		}
	}()
	if response.StatusCode >= 300 {
		body, err := httpx.ReadLimited(reader, httpx.MaxErrorBytes)
		if err != nil {
			return result, attempt, httpx.ProtocolError(providerName, err.Error())
		}
		return result, attempt, httpx.ProviderError(providerName, response.StatusCode, response.Header, body)
	}
	var envelope responseEnvelope
	if strings.Contains(strings.ToLower(responseFormat), "text/event-stream") {
		envelope, runtimeErr = decodeResponseStream(reader, driver.codex)
	} else {
		var body []byte
		body, err = httpx.ReadLimited(reader, httpx.MaxResponseBytes)
		if err == nil {
			err = json.Unmarshal(body, &envelope)
		}
		if err != nil {
			runtimeErr = httpx.ProtocolError(providerName, "decode response: "+err.Error())
		}
	}
	if runtimeErr != nil {
		if ctx.Err() != nil {
			return result, attempt, httpx.NetworkError(providerName, ctx.Err())
		}
		return result, attempt, runtimeErr
	}
	result, runtimeErr = envelope.result(resolved, response.Header)
	if runtimeErr != nil {
		return contract.ModelResult{}, attempt, runtimeErr
	}
	if driver.codex {
		result.Provider.Name = string(model.DriverOpenAICodexResponses)
	}
	// The terminal Response gates completion. Project completed output once,
	// not a concatenation of deltas and completed item/response snapshots.
	// The enclosing ResilientModel also buffers events until an attempt succeeds.
	if err := emitResult(result, sink); err != nil {
		return contract.ModelResult{}, attempt, &contract.RuntimeError{
			Code: contract.ErrorCancelled, Phase: contract.PhaseConsumer,
			Message: err.Error(), Provider: providerName,
		}
	}
	return result, attempt, nil
}

type requestBody struct {
	Model           string         `json:"model"`
	Instructions    string         `json:"instructions,omitempty"`
	Input           []inputMessage `json:"input"`
	Tools           []functionTool `json:"tools,omitempty"`
	MaxOutputTokens *int64         `json:"max_output_tokens,omitempty"`
	Temperature     *float64       `json:"temperature,omitempty"`
	TopP            *float64       `json:"top_p,omitempty"`
	Stream          bool           `json:"stream"`
	Store           bool           `json:"store"`
}

type inputMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
}

type functionTool struct {
	Type        string          `json:"type"`
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters"`
	Strict      bool            `json:"strict"`
}

func encodeRequest(resolved model.ResolvedModel, request contract.ModelRequest) ([]byte, error) {
	if err := request.Validate(); err != nil {
		return nil, err
	}
	if len(request.Options.StopSequences) > 0 || len(resolved.Parameters.StopSequences) > 0 {
		return nil, fmt.Errorf("openai-responses does not support stop_sequences")
	}
	body := requestBody{
		Model: resolved.Model, Instructions: request.System,
		MaxOutputTokens: request.Options.MaxOutputTokens,
		Temperature:     request.Options.Temperature, TopP: request.Options.TopP,
		Stream: resolved.Parameters.StreamingEnabled(), Store: false,
		Input: make([]inputMessage, 0, len(request.Messages)),
	}
	for _, message := range request.Messages {
		if message.Role != contract.RoleUser {
			return nil, fmt.Errorf("openai-responses single calls accept user messages only; assistant history and tool-result continuation require unsupported Responses context")
		}
		body.Input = append(body.Input, inputMessage{Role: "user", Content: message.Content})
	}
	for _, tool := range request.Tools {
		// Responses otherwise defaults to strict schema normalization, which would
		// silently change the canonical ToolSpec's optional-property semantics.
		body.Tools = append(body.Tools, functionTool{
			Type: "function", Name: tool.Name, Description: tool.Description,
			Parameters: tool.InputSchema, Strict: false,
		})
	}
	return json.Marshal(body)
}

func emitResult(result contract.ModelResult, sink contract.EventSink) error {
	sequence := uint64(0)
	emit := func(event contract.Event) error {
		sequence++
		event.Sequence = sequence
		if sink == nil {
			return nil
		}
		return sink(event)
	}
	if err := emit(contract.Event{Type: contract.EventModelStarted}); err != nil {
		return err
	}
	if result.Message.Content != "" {
		if err := emit(contract.Event{Type: contract.EventContentDelta, Model: &contract.ModelEvent{Text: result.Message.Content}}); err != nil {
			return err
		}
	}
	for _, call := range result.Message.ToolCalls {
		identity := contract.ToolCall{ID: call.ID, Name: call.Name, Arguments: json.RawMessage(`{}`)}
		if err := emit(contract.Event{Type: contract.EventToolCallStarted, Model: &contract.ModelEvent{ToolCall: &identity}}); err != nil {
			return err
		}
		if err := emit(contract.Event{Type: contract.EventToolCallArgumentsDelta, Model: &contract.ModelEvent{ToolCallID: call.ID, Text: string(call.Arguments)}}); err != nil {
			return err
		}
	}
	// Model Service validates and emits model.completed from the returned result.
	return nil
}
