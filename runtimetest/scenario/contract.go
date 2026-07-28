package scenario

import (
	"encoding/json"
	"fmt"
	"time"

	"github.com/yy003x/runtime/contract"
)

func ToContractRequest(value ModelRequest) (contract.ModelRequest, error) {
	result := contract.ModelRequest{
		System:  value.System,
		Options: contract.GenerateOptions{},
		Trace:   contract.TraceContext{Labels: cloneLabels(value.Trace.Labels)},
	}
	if value.Options.MaxOutputTokens != 0 {
		current := value.Options.MaxOutputTokens
		result.Options.MaxOutputTokens = &current
	}
	if value.Options.Temperature != 0 {
		current := value.Options.Temperature
		result.Options.Temperature = &current
	}
	for _, message := range value.Messages {
		current, err := toContractMessage(message)
		if err != nil {
			return contract.ModelRequest{}, err
		}
		result.Messages = append(result.Messages, current)
	}
	for _, tool := range value.Tools {
		result.Tools = append(result.Tools, contract.ToolSpec{
			Name: tool.Name, Description: tool.Description,
			InputSchema: cloneRaw(tool.InputSchema),
		})
	}
	if err := result.Validate(); err != nil {
		return contract.ModelRequest{}, err
	}
	return result, nil
}

func ToContractEvent(value Event) (contract.Event, error) {
	result := contract.Event{
		Sequence: value.Sequence,
		Type:     contract.EventType(value.Type),
	}
	if value.Time != "" {
		current, err := time.Parse(time.RFC3339Nano, value.Time)
		if err != nil {
			return contract.Event{}, fmt.Errorf("event time: %w", err)
		}
		result.Time = &current
	}
	switch result.Type {
	case contract.EventModelStarted, contract.EventRunSettled:
	case contract.EventContentDelta, contract.EventReasoningDelta:
		result.Model = &contract.ModelEvent{Text: value.Text}
	case contract.EventToolCallArgumentsDelta:
		result.Model = &contract.ModelEvent{
			Text: value.Text, ToolCallID: value.ToolCallID,
		}
	case contract.EventToolCallStarted:
		if value.ToolCall == nil {
			return contract.Event{}, fmt.Errorf("%s requires tool_call", result.Type)
		}
		call, err := toContractToolCall(*value.ToolCall)
		if err != nil {
			return contract.Event{}, err
		}
		result.Model = &contract.ModelEvent{ToolCall: &call}
	case contract.EventModelCompleted:
		result.Model = &contract.ModelEvent{}
		if value.Result != nil {
			current, err := ToContractResult(*value.Result)
			if err != nil {
				return contract.Event{}, err
			}
			result.Model.Result = &current
		}
	case contract.EventRunFailed:
		if value.Error == nil {
			return contract.Event{}, fmt.Errorf("%s requires error", result.Type)
		}
		current, err := ToContractError(*value.Error)
		if err != nil {
			return contract.Event{}, err
		}
		result.Error = &current
	default:
		return contract.Event{}, fmt.Errorf("unsupported event type %q", value.Type)
	}
	return result, nil
}

func ToContractResult(value ModelResult) (contract.ModelResult, error) {
	message, err := toContractMessage(value.Message)
	if err != nil {
		return contract.ModelResult{}, err
	}
	result := contract.ModelResult{
		Message: message, FinishReason: contract.FinishReason(value.FinishReason),
		Usage: contract.Usage{
			InputTokens:     cloneInt64(value.Usage.InputTokens),
			OutputTokens:    cloneInt64(value.Usage.OutputTokens),
			ReasoningTokens: cloneInt64(value.Usage.ReasoningTokens),
			CacheReadTokens: cloneInt64(value.Usage.CacheReadTokens),
			TotalTokens:     cloneInt64(value.Usage.TotalTokens),
			Source:          contract.UsageSource(value.Usage.Source),
			Completeness:    contract.UsageCompleteness(value.Usage.Completeness),
		},
		Provider: contract.ProviderInfo{
			Name: value.Provider.Name, Model: value.Provider.Model,
			RequestID: value.Provider.RequestID,
		},
	}
	if err := result.Validate(); err != nil {
		return contract.ModelResult{}, err
	}
	return result, nil
}

func ToContractError(value RuntimeError) (contract.RuntimeError, error) {
	result := contract.RuntimeError{
		Code: contract.ErrorCode(value.Code), Phase: contract.ErrorPhase(value.Phase),
		Message: value.Message, Retryable: value.Retryable,
		RetryAfterMS: value.RetryAfterMS, HTTPStatus: value.HTTPStatus,
		Provider: value.Provider, RequestID: value.RequestID,
	}
	if err := result.Validate(); err != nil {
		return contract.RuntimeError{}, err
	}
	return result, nil
}

func toContractMessage(value Message) (contract.Message, error) {
	result := contract.Message{
		Role: contract.Role(value.Role), Content: value.Content,
		ToolCallID: value.ToolCallID,
	}
	for _, call := range value.ToolCalls {
		current, err := toContractToolCall(call)
		if err != nil {
			return contract.Message{}, err
		}
		result.ToolCalls = append(result.ToolCalls, current)
	}
	if err := result.Validate(); err != nil {
		return contract.Message{}, err
	}
	return result, nil
}

func toContractToolCall(value ToolCall) (contract.ToolCall, error) {
	result := contract.ToolCall{
		ID: value.ID, Name: value.Name, Arguments: cloneRaw(value.Arguments),
	}
	if err := result.Validate(); err != nil {
		return contract.ToolCall{}, err
	}
	return result, nil
}

func cloneRaw(value json.RawMessage) json.RawMessage {
	return append(json.RawMessage(nil), value...)
}

func cloneInt64(value *int64) *int64 {
	if value == nil {
		return nil
	}
	current := *value
	return &current
}

func cloneLabels(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	result := make(map[string]string, len(values))
	for key, value := range values {
		result[key] = value
	}
	return result
}
