package openairesponses

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	httpx "github.com/yy003x/runtime/internal/infrastructure/providerhttp"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
)

type responseEnvelope struct {
	ID                string         `json:"id"`
	Model             string         `json:"model"`
	Status            string         `json:"status"`
	Output            []outputItem   `json:"output"`
	Error             *responseError `json:"error"`
	IncompleteDetails *struct {
		Reason string `json:"reason"`
	} `json:"incomplete_details"`
	Usage *usagePayload `json:"usage"`
}

type outputItem struct {
	Type      string `json:"type"`
	Role      string `json:"role"`
	Status    string `json:"status"`
	CallID    string `json:"call_id"`
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
	Content   []struct {
		Type    string `json:"type"`
		Text    string `json:"text"`
		Refusal string `json:"refusal"`
	} `json:"content"`
}

type responseError struct {
	Code    string `json:"code"`
	Message string `json:"message"`
}

func (failure responseError) runtimeError() *contract.RuntimeError {
	message := strings.TrimSpace(failure.Message)
	if message == "" {
		message = "Responses request failed"
	}
	if failure.Code != "" {
		message = failure.Code + ": " + message
	}
	err := httpx.ProtocolError(providerName, message)
	switch failure.Code {
	case "rate_limit_exceeded":
		err.Code, err.Retryable = contract.ErrorRateLimited, true
	case "server_error":
		err.Code, err.Retryable = contract.ErrorProviderUnavailable, true
	case "context_length_exceeded":
		err.Code = contract.ErrorContextOverflow
	}
	return err
}

func (response responseEnvelope) result(resolved model.ResolvedModel, headers http.Header) (contract.ModelResult, *contract.RuntimeError) {
	fail := func(message string) (contract.ModelResult, *contract.RuntimeError) {
		return contract.ModelResult{}, httpx.ProtocolError(providerName, message)
	}
	if response.Error != nil {
		return contract.ModelResult{}, response.Error.runtimeError()
	}
	finish := contract.FinishStop
	switch response.Status {
	case "completed":
		if response.IncompleteDetails != nil {
			return fail("completed response contains incomplete_details")
		}
	case "incomplete":
		if response.IncompleteDetails == nil {
			return fail("incomplete response has no incomplete_details")
		}
		switch response.IncompleteDetails.Reason {
		case "max_output_tokens":
			finish = contract.FinishLength
		case "content_filter":
			finish = contract.FinishContentFilter
		default:
			return fail("unsupported incomplete reason: " + response.IncompleteDetails.Reason)
		}
	default:
		return fail("response has no supported terminal status: " + response.Status)
	}
	var text strings.Builder
	var calls []contract.ToolCall
	for _, item := range response.Output {
		switch item.Type {
		case "message":
			if item.Role != "assistant" || (item.Status != "completed" && !(response.Status == "incomplete" && item.Status == "incomplete")) {
				return fail("output message has invalid role or terminal status")
			}
			for _, part := range item.Content {
				switch part.Type {
				case "output_text":
					text.WriteString(part.Text)
				case "refusal":
					text.WriteString(part.Refusal)
					finish = contract.FinishContentFilter
				default:
					return fail("unsupported output content type: " + part.Type)
				}
			}
		case "function_call":
			// An incomplete call cannot safely be exposed as executable canonical
			// tool input, even when its partial arguments happen to be valid JSON.
			if response.Status != "completed" || (item.Status != "" && item.Status != "completed") {
				return fail("incomplete function call is not supported")
			}
			call := contract.ToolCall{ID: item.CallID, Name: item.Name, Arguments: json.RawMessage(item.Arguments)}
			if err := call.Validate(); err != nil {
				return fail("invalid function call: " + err.Error())
			}
			calls = append(calls, call)
		case "reasoning":
			// Retained in raw/logs, but not projected into canonical history. This
			// is why this driver rejects conversation continuation at ingress.
		default:
			return fail("unsupported output item type: " + item.Type)
		}
	}
	if len(calls) > 0 {
		if finish != contract.FinishStop {
			return fail("function calls conflict with response finish reason")
		}
		finish = contract.FinishToolCall
	}
	result := contract.ModelResult{
		Message:      contract.Message{Role: contract.RoleAssistant, Content: text.String(), ToolCalls: calls},
		FinishReason: finish, Usage: buildUsage(response.Usage),
		Provider: contract.ProviderInfo{Name: providerName, Model: response.Model, RequestID: response.ID},
	}
	if result.Provider.Model == "" {
		result.Provider.Model = resolved.Model
	}
	if result.Provider.RequestID == "" {
		result.Provider.RequestID = headers.Get("X-Request-ID")
	}
	if err := result.Validate(); err != nil {
		return fail(fmt.Sprintf("invalid Responses result: %s", err))
	}
	return result, nil
}

type usagePayload struct {
	InputTokens  *int64 `json:"input_tokens"`
	OutputTokens *int64 `json:"output_tokens"`
	TotalTokens  *int64 `json:"total_tokens"`
	InputDetails *struct {
		CachedTokens     *int64 `json:"cached_tokens"`
		CacheWriteTokens *int64 `json:"cache_write_tokens"`
	} `json:"input_tokens_details"`
	OutputDetails *struct {
		ReasoningTokens *int64 `json:"reasoning_tokens"`
	} `json:"output_tokens_details"`
}

func buildUsage(value *usagePayload) contract.Usage {
	if value == nil {
		return contract.Usage{}
	}
	result := contract.Usage{Source: contract.UsageSourceProvider, InputTokens: value.InputTokens, OutputTokens: value.OutputTokens, TotalTokens: value.TotalTokens}
	if value.InputDetails != nil {
		result.CacheReadTokens, result.CacheWriteTokens = value.InputDetails.CachedTokens, value.InputDetails.CacheWriteTokens
	}
	if value.OutputDetails != nil {
		result.ReasoningTokens = value.OutputDetails.ReasoningTokens
	}
	if result.InputTokens != nil && result.OutputTokens != nil {
		result.Completeness = contract.UsageComplete
	} else if result.InputTokens != nil || result.OutputTokens != nil || result.TotalTokens != nil || result.CacheReadTokens != nil || result.CacheWriteTokens != nil || result.ReasoningTokens != nil {
		result.Completeness = contract.UsagePartial
	}
	return result
}
