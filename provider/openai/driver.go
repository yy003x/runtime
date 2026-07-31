// Package openai implements the Runtime vNext OpenAI-compatible wire driver.
// It performs exactly one HTTP attempt and owns no retry, Session, or tool loop.
package openai

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/provider/internal/httpx"
)

const (
	providerName = "openai-compatible"

	// Bump manually when execution semantics change in a way not represented by
	// the Profile or Provider-neutral request contract.
	executionImplementation        = "runtime.provider.openai-compatible"
	executionImplementationVersion = 1
)

type Driver struct {
	client *http.Client
}

func New(client *http.Client) *Driver {
	if client == nil {
		client = http.DefaultClient
	}
	return &Driver{client: client}
}

func (*Driver) ExecutionIdentity() model.DriverExecutionIdentity {
	return model.DriverExecutionIdentity{
		Driver:                model.DriverOpenAICompatible,
		Implementation:        executionImplementation,
		ImplementationVersion: executionImplementationVersion,
	}
}

func (*Driver) Validate(profile model.Profile) error {
	if profile.Driver != model.DriverOpenAICompatible {
		return fmt.Errorf("openai driver cannot serve %q", profile.Driver)
	}
	return profile.Validate()
}

func (driver *Driver) Stream(
	ctx context.Context,
	resolved model.ResolvedModel,
	request contract.ModelRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	payload, err := encodeRequest(resolved, request)
	if err != nil {
		return contract.ModelResult{}, httpx.ProtocolError(providerName, err.Error())
	}
	httpRequest, err := http.NewRequestWithContext(
		ctx, http.MethodPost, resolved.Endpoint, bytes.NewReader(payload),
	)
	if err != nil {
		return contract.ModelResult{}, httpx.ProtocolError(providerName, err.Error())
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream, application/json")
	for name, value := range resolved.RequestHeaders() {
		httpRequest.Header.Set(name, value)
	}
	response, err := driver.client.Do(httpRequest)
	if err != nil {
		return contract.ModelResult{}, httpx.NetworkError(providerName, err)
	}
	defer response.Body.Close()
	if response.StatusCode >= 300 {
		body, readErr := httpx.ReadLimited(response.Body, httpx.MaxErrorBytes)
		if readErr != nil {
			return contract.ModelResult{}, httpx.ProtocolError(providerName, readErr.Error())
		}
		return contract.ModelResult{}, httpx.ProviderError(providerName, response.StatusCode, response.Header, body)
	}
	emitter := eventEmitter{sink: sink, sequence: 1}
	if err := emitter.emit(contract.Event{Type: contract.EventModelStarted}); err != nil {
		return contract.ModelResult{}, consumerError(err)
	}
	contentType := strings.ToLower(response.Header.Get("Content-Type"))
	var result contract.ModelResult
	if strings.Contains(contentType, "text/event-stream") {
		result, err = decodeStream(response.Body, resolved, response.Header, &emitter)
	} else {
		result, err = decodeResponse(response.Body, resolved, response.Header, &emitter)
	}
	if err != nil {
		return contract.ModelResult{}, httpx.ProtocolError(providerName, err.Error())
	}
	return result, nil
}

type requestBody struct {
	Model               string           `json:"model"`
	Messages            []messagePayload `json:"messages"`
	Tools               []toolPayload    `json:"tools,omitempty"`
	MaxCompletionTokens *int64           `json:"max_completion_tokens,omitempty"`
	Temperature         *float64         `json:"temperature,omitempty"`
	Stream              bool             `json:"stream"`
	StreamOptions       map[string]bool  `json:"stream_options,omitempty"`
}

type messagePayload struct {
	Role       string            `json:"role"`
	Content    string            `json:"content,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCallPayload `json:"tool_calls,omitempty"`
}

type toolPayload struct {
	Type     string          `json:"type"`
	Function functionPayload `json:"function"`
}

type toolCallPayload struct {
	Index    int             `json:"index,omitempty"`
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type,omitempty"`
	Function functionPayload `json:"function"`
}

type functionPayload struct {
	Name        string          `json:"name,omitempty"`
	Description string          `json:"description,omitempty"`
	Parameters  json.RawMessage `json:"parameters,omitempty"`
	Arguments   string          `json:"arguments,omitempty"`
}

func encodeRequest(resolved model.ResolvedModel, request contract.ModelRequest) ([]byte, error) {
	messages := make([]messagePayload, 0, len(request.Messages)+1)
	if strings.TrimSpace(request.System) != "" {
		messages = append(messages, messagePayload{Role: "system", Content: request.System})
	}
	for _, message := range request.Messages {
		current := messagePayload{
			Role: string(message.Role), Content: message.Content, ToolCallID: message.ToolCallID,
		}
		for _, call := range message.ToolCalls {
			current.ToolCalls = append(current.ToolCalls, toolCallPayload{
				ID: call.ID, Type: "function",
				Function: functionPayload{Name: call.Name, Arguments: string(call.Arguments)},
			})
		}
		messages = append(messages, current)
	}
	tools := make([]toolPayload, 0, len(request.Tools))
	for _, tool := range request.Tools {
		tools = append(tools, toolPayload{
			Type: "function",
			Function: functionPayload{
				Name: tool.Name, Description: tool.Description,
				Parameters: append(json.RawMessage(nil), tool.InputSchema...),
			},
		})
	}
	return json.Marshal(requestBody{
		Model: resolved.Model, Messages: messages, Tools: tools,
		MaxCompletionTokens: request.Options.MaxOutputTokens,
		Temperature:         request.Options.Temperature,
		Stream:              true,
		StreamOptions:       map[string]bool{"include_usage": true},
	})
}

type responseEnvelope struct {
	ID      string `json:"id"`
	Model   string `json:"model"`
	Choices []struct {
		Message struct {
			Content   string            `json:"content"`
			ToolCalls []toolCallPayload `json:"tool_calls"`
		} `json:"message"`
		Delta struct {
			Content   string            `json:"content"`
			ToolCalls []toolCallPayload `json:"tool_calls"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage usagePayload `json:"usage"`
}

type usagePayload struct {
	PromptTokens     int64 `json:"prompt_tokens"`
	CompletionTokens int64 `json:"completion_tokens"`
	TotalTokens      int64 `json:"total_tokens"`
}

type partialCall struct {
	id        string
	name      string
	arguments strings.Builder
	started   bool
}

func decodeStream(
	reader io.Reader,
	resolved model.ResolvedModel,
	headers http.Header,
	emitter *eventEmitter,
) (contract.ModelResult, error) {
	scanner := bufio.NewScanner(io.LimitReader(reader, httpx.MaxResponseBytes+1))
	scanner.Buffer(make([]byte, 64<<10), int(httpx.MaxResponseBytes))
	var text strings.Builder
	calls := map[int]*partialCall{}
	finishReason := ""
	usage := usagePayload{}
	requestID := headers.Get("X-Request-ID")
	responseModel := resolved.Model
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk responseEnvelope
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return contract.ModelResult{}, fmt.Errorf("decode SSE data: %w", err)
		}
		if chunk.ID != "" {
			requestID = chunk.ID
		}
		if chunk.Model != "" {
			responseModel = chunk.Model
		}
		if chunk.Usage != (usagePayload{}) {
			usage = chunk.Usage
		}
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				text.WriteString(choice.Delta.Content)
				if err := emitter.emit(contract.Event{
					Type:  contract.EventContentDelta,
					Model: &contract.ModelEvent{Text: choice.Delta.Content},
				}); err != nil {
					return contract.ModelResult{}, err
				}
			}
			for _, delta := range choice.Delta.ToolCalls {
				call := calls[delta.Index]
				if call == nil {
					call = &partialCall{}
					calls[delta.Index] = call
				}
				if delta.ID != "" {
					call.id = delta.ID
				}
				if delta.Function.Name != "" {
					call.name += delta.Function.Name
				}
				if !call.started && call.id != "" && call.name != "" {
					call.started = true
					if err := emitter.emit(contract.Event{
						Type: contract.EventToolCallStarted,
						Model: &contract.ModelEvent{ToolCall: &contract.ToolCall{
							ID: call.id, Name: call.name, Arguments: json.RawMessage(`{}`),
						}},
					}); err != nil {
						return contract.ModelResult{}, err
					}
				}
				if delta.Function.Arguments != "" {
					call.arguments.WriteString(delta.Function.Arguments)
					if !call.started {
						return contract.ModelResult{}, fmt.Errorf("tool argument delta arrived before tool identity")
					}
					if err := emitter.emit(contract.Event{
						Type: contract.EventToolCallArgumentsDelta,
						Model: &contract.ModelEvent{
							ToolCallID: call.id, Text: delta.Function.Arguments,
						},
					}); err != nil {
						return contract.ModelResult{}, err
					}
				}
			}
			if choice.FinishReason != "" {
				finishReason = choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return contract.ModelResult{}, fmt.Errorf("read SSE: %w", err)
	}
	toolCalls, err := finishCalls(calls)
	if err != nil {
		return contract.ModelResult{}, err
	}
	return buildResult(text.String(), toolCalls, finishReason, usage, responseModel, requestID), nil
}

func decodeResponse(
	reader io.Reader,
	resolved model.ResolvedModel,
	headers http.Header,
	emitter *eventEmitter,
) (contract.ModelResult, error) {
	body, err := httpx.ReadLimited(reader, httpx.MaxResponseBytes)
	if err != nil {
		return contract.ModelResult{}, err
	}
	var response responseEnvelope
	if err := json.Unmarshal(body, &response); err != nil {
		return contract.ModelResult{}, fmt.Errorf("decode response: %w", err)
	}
	if len(response.Choices) == 0 {
		return contract.ModelResult{}, fmt.Errorf("response choices is empty")
	}
	choice := response.Choices[0]
	if choice.Message.Content != "" {
		if err := emitter.emit(contract.Event{
			Type:  contract.EventContentDelta,
			Model: &contract.ModelEvent{Text: choice.Message.Content},
		}); err != nil {
			return contract.ModelResult{}, err
		}
	}
	toolCalls := make([]contract.ToolCall, 0, len(choice.Message.ToolCalls))
	for _, value := range choice.Message.ToolCalls {
		arguments, err := normalizeArguments(value.Function.Arguments)
		if err != nil {
			return contract.ModelResult{}, err
		}
		call := contract.ToolCall{ID: value.ID, Name: value.Function.Name, Arguments: arguments}
		if err := emitter.emit(contract.Event{
			Type:  contract.EventToolCallStarted,
			Model: &contract.ModelEvent{ToolCall: &call},
		}); err != nil {
			return contract.ModelResult{}, err
		}
		if value.Function.Arguments != "" {
			if err := emitter.emit(contract.Event{
				Type: contract.EventToolCallArgumentsDelta,
				Model: &contract.ModelEvent{
					ToolCallID: value.ID, Text: value.Function.Arguments,
				},
			}); err != nil {
				return contract.ModelResult{}, err
			}
		}
		toolCalls = append(toolCalls, call)
	}
	requestID := response.ID
	if requestID == "" {
		requestID = headers.Get("X-Request-ID")
	}
	responseModel := response.Model
	if responseModel == "" {
		responseModel = resolved.Model
	}
	return buildResult(
		choice.Message.Content, toolCalls, choice.FinishReason,
		response.Usage, responseModel, requestID,
	), nil
}

func finishCalls(values map[int]*partialCall) ([]contract.ToolCall, error) {
	indexes := make([]int, 0, len(values))
	for index := range values {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	result := make([]contract.ToolCall, 0, len(indexes))
	for _, index := range indexes {
		value := values[index]
		if !value.started {
			return nil, fmt.Errorf("tool call %d never provided id and name", index)
		}
		arguments, err := normalizeArguments(value.arguments.String())
		if err != nil {
			return nil, fmt.Errorf("tool call %q: %w", value.name, err)
		}
		result = append(result, contract.ToolCall{
			ID: value.id, Name: value.name, Arguments: arguments,
		})
	}
	return result, nil
}

func normalizeArguments(value string) (json.RawMessage, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		value = "{}"
	}
	raw := json.RawMessage(value)
	var object map[string]json.RawMessage
	if !json.Valid(raw) || json.Unmarshal(raw, &object) != nil {
		return nil, fmt.Errorf("tool arguments must be a JSON object")
	}
	return append(json.RawMessage(nil), raw...), nil
}

func buildResult(
	text string,
	toolCalls []contract.ToolCall,
	finish string,
	usage usagePayload,
	modelName, requestID string,
) contract.ModelResult {
	reason := mapFinishReason(finish)
	if len(toolCalls) > 0 {
		reason = contract.FinishToolCall
	}
	return contract.ModelResult{
		Message: contract.Message{
			Role: contract.RoleAssistant, Content: text, ToolCalls: toolCalls,
		},
		FinishReason: reason,
		Usage:        buildUsage(usage),
		Provider: contract.ProviderInfo{
			Name: providerName, Model: modelName, RequestID: requestID,
		},
	}
}

func buildUsage(value usagePayload) contract.Usage {
	result := contract.Usage{Source: contract.UsageSourceProvider}
	if value.PromptTokens > 0 {
		current := value.PromptTokens
		result.InputTokens = &current
	}
	if value.CompletionTokens > 0 {
		current := value.CompletionTokens
		result.OutputTokens = &current
	}
	if value.TotalTokens > 0 {
		current := value.TotalTokens
		result.TotalTokens = &current
	}
	if result.InputTokens != nil && result.OutputTokens != nil {
		result.Completeness = contract.UsageComplete
	} else if result.InputTokens != nil || result.OutputTokens != nil || result.TotalTokens != nil {
		result.Completeness = contract.UsagePartial
	}
	return result
}

func mapFinishReason(value string) contract.FinishReason {
	switch value {
	case "tool_calls", "function_call":
		return contract.FinishToolCall
	case "length":
		return contract.FinishLength
	case "content_filter":
		return contract.FinishContentFilter
	case "cancelled":
		return contract.FinishCancelled
	default:
		return contract.FinishStop
	}
}

type eventEmitter struct {
	sink     contract.EventSink
	sequence uint64
}

func (emitter *eventEmitter) emit(event contract.Event) error {
	event.Sequence = emitter.sequence
	emitter.sequence++
	if emitter.sink == nil {
		return nil
	}
	return emitter.sink(event)
}

func consumerError(err error) *contract.RuntimeError {
	return &contract.RuntimeError{
		Code: contract.ErrorCancelled, Phase: contract.PhaseConsumer,
		Message: err.Error(), Provider: providerName,
	}
}
