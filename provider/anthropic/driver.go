// Package anthropic implements the Runtime vNext Anthropic-compatible wire
// driver. It performs exactly one HTTP attempt and owns no retry or tool loop.
package anthropic

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
	providerName = "anthropic-compatible"

	// Bump manually when execution semantics change in a way not represented by
	// the Profile or Provider-neutral request contract.
	executionImplementation        = "runtime.provider.anthropic-compatible"
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
		Driver:                model.DriverAnthropicCompatible,
		Implementation:        executionImplementation,
		ImplementationVersion: executionImplementationVersion,
	}
}

func (*Driver) Validate(profile model.Profile) error {
	if profile.Driver != model.DriverAnthropicCompatible {
		return fmt.Errorf("anthropic driver cannot serve %q", profile.Driver)
	}
	if err := profile.Validate(); err != nil {
		return err
	}
	if profile.Defaults.MaxTokens == nil {
		return fmt.Errorf("anthropic profile requires defaults.max_tokens")
	}
	return nil
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
	httpRequest.Header.Set("anthropic-version", "2023-06-01")
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
	Model       string           `json:"model"`
	System      string           `json:"system,omitempty"`
	Messages    []messagePayload `json:"messages"`
	Tools       []toolPayload    `json:"tools,omitempty"`
	MaxTokens   int64            `json:"max_tokens"`
	Temperature *float64         `json:"temperature,omitempty"`
	Stream      bool             `json:"stream"`
}

type messagePayload struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type      string          `json:"type"`
	Text      string          `json:"text,omitempty"`
	ID        string          `json:"id,omitempty"`
	Name      string          `json:"name,omitempty"`
	Input     json.RawMessage `json:"input,omitempty"`
	ToolUseID string          `json:"tool_use_id,omitempty"`
	Content   string          `json:"content,omitempty"`
	IsError   bool            `json:"is_error,omitempty"`
}

type toolPayload struct {
	Name        string          `json:"name"`
	Description string          `json:"description,omitempty"`
	InputSchema json.RawMessage `json:"input_schema"`
}

func encodeRequest(resolved model.ResolvedModel, request contract.ModelRequest) ([]byte, error) {
	if request.Options.MaxOutputTokens == nil || *request.Options.MaxOutputTokens <= 0 {
		return nil, fmt.Errorf("max_tokens is required for Anthropic")
	}
	messages := make([]messagePayload, 0, len(request.Messages))
	for _, message := range request.Messages {
		role := string(message.Role)
		parts := make([]contentPart, 0, 1+len(message.ToolCalls))
		if message.Role == contract.RoleTool {
			role = string(contract.RoleUser)
			parts = append(parts, contentPart{
				Type: "tool_result", ToolUseID: message.ToolCallID, Content: message.Content,
				IsError: message.IsError,
			})
		} else {
			if message.Content != "" {
				parts = append(parts, contentPart{Type: "text", Text: message.Content})
			}
			for _, call := range message.ToolCalls {
				parts = append(parts, contentPart{
					Type: "tool_use", ID: call.ID, Name: call.Name,
					Input: append(json.RawMessage(nil), call.Arguments...),
				})
			}
		}
		if len(messages) > 0 && role == string(contract.RoleUser) &&
			messages[len(messages)-1].Role == role && message.Role == contract.RoleTool {
			messages[len(messages)-1].Content = append(messages[len(messages)-1].Content, parts...)
			continue
		}
		messages = append(messages, messagePayload{Role: role, Content: parts})
	}
	tools := make([]toolPayload, 0, len(request.Tools))
	for _, tool := range request.Tools {
		tools = append(tools, toolPayload{
			Name: tool.Name, Description: tool.Description,
			InputSchema: append(json.RawMessage(nil), tool.InputSchema...),
		})
	}
	return json.Marshal(requestBody{
		Model: resolved.Model, System: request.System, Messages: messages, Tools: tools,
		MaxTokens:   *request.Options.MaxOutputTokens,
		Temperature: request.Options.Temperature, Stream: true,
	})
}

type streamEnvelope struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		ID    string `json:"id"`
		Model string `json:"model"`
		Usage struct {
			InputTokens int64 `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock contentPart `json:"content_block"`
	Delta        struct {
		Type        string `json:"type"`
		Text        string `json:"text"`
		PartialJSON string `json:"partial_json"`
		StopReason  string `json:"stop_reason"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

type responseEnvelope struct {
	ID         string        `json:"id"`
	Model      string        `json:"model"`
	Content    []contentPart `json:"content"`
	StopReason string        `json:"stop_reason"`
	Usage      struct {
		InputTokens  int64 `json:"input_tokens"`
		OutputTokens int64 `json:"output_tokens"`
	} `json:"usage"`
}

type partialCall struct {
	id        string
	name      string
	arguments strings.Builder
	initial   json.RawMessage
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
	requestID := headers.Get("request-id")
	modelName := resolved.Model
	var inputTokens, outputTokens int64
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || strings.HasPrefix(line, "event:") {
			continue
		}
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event streamEnvelope
		if err := json.Unmarshal(
			[]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))),
			&event,
		); err != nil {
			return contract.ModelResult{}, fmt.Errorf("decode SSE data: %w", err)
		}
		switch event.Type {
		case "message_start":
			if event.Message.ID != "" {
				requestID = event.Message.ID
			}
			if event.Message.Model != "" {
				modelName = event.Message.Model
			}
			inputTokens = event.Message.Usage.InputTokens
		case "content_block_start":
			switch event.ContentBlock.Type {
			case "text":
				if event.ContentBlock.Text != "" {
					text.WriteString(event.ContentBlock.Text)
					if err := emitter.emit(contract.Event{
						Type:  contract.EventContentDelta,
						Model: &contract.ModelEvent{Text: event.ContentBlock.Text},
					}); err != nil {
						return contract.ModelResult{}, err
					}
				}
			case "tool_use":
				call := &partialCall{
					id: event.ContentBlock.ID, name: event.ContentBlock.Name,
					initial: append(json.RawMessage(nil), event.ContentBlock.Input...),
				}
				calls[event.Index] = call
				if err := emitter.emit(contract.Event{
					Type: contract.EventToolCallStarted,
					Model: &contract.ModelEvent{ToolCall: &contract.ToolCall{
						ID: call.id, Name: call.name, Arguments: json.RawMessage(`{}`),
					}},
				}); err != nil {
					return contract.ModelResult{}, err
				}
			}
		case "content_block_delta":
			switch event.Delta.Type {
			case "text_delta":
				text.WriteString(event.Delta.Text)
				if err := emitter.emit(contract.Event{
					Type:  contract.EventContentDelta,
					Model: &contract.ModelEvent{Text: event.Delta.Text},
				}); err != nil {
					return contract.ModelResult{}, err
				}
			case "input_json_delta":
				call := calls[event.Index]
				if call == nil {
					return contract.ModelResult{}, fmt.Errorf("tool argument delta references unknown block %d", event.Index)
				}
				call.arguments.WriteString(event.Delta.PartialJSON)
				if err := emitter.emit(contract.Event{
					Type: contract.EventToolCallArgumentsDelta,
					Model: &contract.ModelEvent{
						ToolCallID: call.id, Text: event.Delta.PartialJSON,
					},
				}); err != nil {
					return contract.ModelResult{}, err
				}
			}
		case "message_delta":
			finishReason = event.Delta.StopReason
			outputTokens = event.Usage.OutputTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return contract.ModelResult{}, fmt.Errorf("read SSE: %w", err)
	}
	toolCalls, err := finishCalls(calls)
	if err != nil {
		return contract.ModelResult{}, err
	}
	return buildResult(
		text.String(), toolCalls, finishReason, inputTokens, outputTokens,
		modelName, requestID,
	), nil
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
	var text strings.Builder
	var toolCalls []contract.ToolCall
	for _, part := range response.Content {
		switch part.Type {
		case "text":
			text.WriteString(part.Text)
			if err := emitter.emit(contract.Event{
				Type:  contract.EventContentDelta,
				Model: &contract.ModelEvent{Text: part.Text},
			}); err != nil {
				return contract.ModelResult{}, err
			}
		case "tool_use":
			arguments, err := normalizeArguments(part.Input)
			if err != nil {
				return contract.ModelResult{}, err
			}
			call := contract.ToolCall{ID: part.ID, Name: part.Name, Arguments: arguments}
			if err := emitter.emit(contract.Event{
				Type:  contract.EventToolCallStarted,
				Model: &contract.ModelEvent{ToolCall: &call},
			}); err != nil {
				return contract.ModelResult{}, err
			}
			toolCalls = append(toolCalls, call)
		}
	}
	requestID := response.ID
	if requestID == "" {
		requestID = headers.Get("request-id")
	}
	modelName := response.Model
	if modelName == "" {
		modelName = resolved.Model
	}
	return buildResult(
		text.String(), toolCalls, response.StopReason,
		response.Usage.InputTokens, response.Usage.OutputTokens, modelName, requestID,
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
		raw := value.initial
		if arguments := strings.TrimSpace(value.arguments.String()); arguments != "" {
			raw = json.RawMessage(arguments)
		}
		normalized, err := normalizeArguments(raw)
		if err != nil {
			return nil, fmt.Errorf("tool call %q: %w", value.name, err)
		}
		result = append(result, contract.ToolCall{
			ID: value.id, Name: value.name, Arguments: normalized,
		})
	}
	return result, nil
}

func normalizeArguments(raw json.RawMessage) (json.RawMessage, error) {
	raw = bytes.TrimSpace(raw)
	if len(raw) == 0 || string(raw) == "null" {
		raw = json.RawMessage(`{}`)
	}
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
	inputTokens, outputTokens int64,
	modelName, requestID string,
) contract.ModelResult {
	reason := mapFinishReason(finish)
	if len(toolCalls) > 0 {
		reason = contract.FinishToolCall
	}
	usage := contract.Usage{Source: contract.UsageSourceProvider}
	if inputTokens > 0 {
		value := inputTokens
		usage.InputTokens = &value
	}
	if outputTokens > 0 {
		value := outputTokens
		usage.OutputTokens = &value
	}
	if usage.InputTokens != nil && usage.OutputTokens != nil {
		total := inputTokens + outputTokens
		usage.TotalTokens = &total
		usage.Completeness = contract.UsageComplete
	} else if usage.InputTokens != nil || usage.OutputTokens != nil {
		usage.Completeness = contract.UsagePartial
	}
	return contract.ModelResult{
		Message: contract.Message{
			Role: contract.RoleAssistant, Content: text, ToolCalls: toolCalls,
		},
		FinishReason: reason,
		Usage:        usage,
		Provider: contract.ProviderInfo{
			Name: providerName, Model: modelName, RequestID: requestID,
		},
	}
}

func mapFinishReason(value string) contract.FinishReason {
	switch value {
	case "tool_use":
		return contract.FinishToolCall
	case "max_tokens":
		return contract.FinishLength
	case "refusal":
		return contract.FinishContentFilter
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
