package contract

import (
	"bytes"
	"encoding/json"
	"fmt"
	"math"
	"strings"
	"unicode/utf8"

	"github.com/yy003x/runtime/internal/profileid"
)

const (
	maxSystemBytes     = 1 << 20
	maxMessageBytes    = 1 << 20
	maxSchemaBytes     = 256 << 10
	maxArgumentsBytes  = 256 << 10
	maxMessages        = 4096
	maxTools           = 256
	maxToolCalls       = 256
	maxLabels          = 32
	maxLabelKeyBytes   = 64
	maxLabelValueBytes = 512
)

func (request GenerateRequest) Validate() error {
	if err := profileid.Validate(request.ModelProfile); err != nil {
		return fmt.Errorf("model_profile: %w", err)
	}
	return request.Input.Validate()
}

func (request ModelRequest) Validate() error {
	if len(request.System) > maxSystemBytes {
		return fmt.Errorf("system exceeds %d bytes", maxSystemBytes)
	}
	if err := validateText(request.System, "system"); err != nil {
		return err
	}
	if len(request.Messages) == 0 {
		return fmt.Errorf("messages are required")
	}
	if len(request.Messages) > maxMessages {
		return fmt.Errorf("messages exceed %d items", maxMessages)
	}
	for index, message := range request.Messages {
		if err := message.Validate(); err != nil {
			return fmt.Errorf("messages[%d]: %w", index, err)
		}
	}
	if len(request.Tools) > maxTools {
		return fmt.Errorf("tools exceed %d items", maxTools)
	}
	toolNames := make(map[string]struct{}, len(request.Tools))
	for index, tool := range request.Tools {
		if err := tool.Validate(); err != nil {
			return fmt.Errorf("tools[%d]: %w", index, err)
		}
		if _, exists := toolNames[tool.Name]; exists {
			return fmt.Errorf("tools[%d]: duplicate tool %q", index, tool.Name)
		}
		toolNames[tool.Name] = struct{}{}
	}
	if request.Options.MaxOutputTokens != nil && *request.Options.MaxOutputTokens <= 0 {
		return fmt.Errorf("options.max_output_tokens must be positive")
	}
	if request.Options.Temperature != nil {
		if err := ValidateTemperature(*request.Options.Temperature); err != nil {
			return fmt.Errorf("options.temperature: %w", err)
		}
	}
	if len(request.Trace.Labels) > maxLabels {
		return fmt.Errorf("trace.labels exceed %d items", maxLabels)
	}
	for key, value := range request.Trace.Labels {
		if err := validateText(key, "trace label key"); err != nil {
			return err
		}
		if err := validateText(value, fmt.Sprintf("trace.labels[%q]", key)); err != nil {
			return err
		}
		if strings.TrimSpace(key) == "" || len(key) > maxLabelKeyBytes {
			return fmt.Errorf("trace.labels contains an invalid key")
		}
		if len(value) > maxLabelValueBytes {
			return fmt.Errorf("trace.labels[%q] exceeds %d bytes", key, maxLabelValueBytes)
		}
	}
	return nil
}

// ValidateTemperature enforces the Provider-neutral sampling range.
func ValidateTemperature(value float64) error {
	if math.IsNaN(value) || math.IsInf(value, 0) || value < 0 || value > 2 {
		return fmt.Errorf("must be a finite number between 0 and 2")
	}
	return nil
}

func (message Message) Validate() error {
	if err := validateText(message.Content, "message content"); err != nil {
		return err
	}
	switch message.Role {
	case RoleUser:
		if message.Content == "" {
			return fmt.Errorf("user content is required")
		}
		if len(message.ToolCalls) != 0 || message.ToolCallID != "" ||
			message.IsError {
			return fmt.Errorf("user message cannot contain tool fields")
		}
	case RoleAssistant:
		if message.Content == "" && len(message.ToolCalls) == 0 {
			return fmt.Errorf("assistant message requires content or tool_calls")
		}
		if message.ToolCallID != "" || message.IsError {
			return fmt.Errorf("assistant message cannot contain tool result fields")
		}
	case RoleTool:
		if message.ToolCallID == "" {
			return fmt.Errorf("tool message requires tool_call_id")
		}
		if len(message.ToolCalls) != 0 {
			return fmt.Errorf("tool message cannot contain tool_calls")
		}
	default:
		return fmt.Errorf("unsupported role %q", message.Role)
	}
	if len(message.Content) > maxMessageBytes {
		return fmt.Errorf("content exceeds %d bytes", maxMessageBytes)
	}
	if len(message.ToolCalls) > maxToolCalls {
		return fmt.Errorf("tool_calls exceed %d items", maxToolCalls)
	}
	callIDs := make(map[string]struct{}, len(message.ToolCalls))
	for index, call := range message.ToolCalls {
		if err := call.Validate(); err != nil {
			return fmt.Errorf("tool_calls[%d]: %w", index, err)
		}
		if _, exists := callIDs[call.ID]; exists {
			return fmt.Errorf("tool_calls[%d]: duplicate call id %q", index, call.ID)
		}
		callIDs[call.ID] = struct{}{}
	}
	return nil
}

func (tool ToolSpec) Validate() error {
	if err := validateName(tool.Name, "tool name"); err != nil {
		return err
	}
	if err := validateText(tool.Description, "tool description"); err != nil {
		return err
	}
	if len(tool.Description) > maxMessageBytes {
		return fmt.Errorf("tool description exceeds %d bytes", maxMessageBytes)
	}
	return validateJSONObject(tool.InputSchema, maxSchemaBytes, "input_schema")
}

func (call ToolCall) Validate() error {
	if err := validateText(call.ID, "tool call id"); err != nil {
		return err
	}
	if strings.TrimSpace(call.ID) == "" {
		return fmt.Errorf("tool call id is required")
	}
	if err := validateName(call.Name, "tool call name"); err != nil {
		return err
	}
	return validateJSONObject(call.Arguments, maxArgumentsBytes, "arguments")
}

func (event Event) Validate() error {
	if event.Sequence == 0 {
		return fmt.Errorf("event sequence must be positive")
	}
	switch event.Type {
	case EventModelStarted:
		if event.payloadCount() != 0 {
			return fmt.Errorf("%s cannot contain a payload", event.Type)
		}
	case EventContentDelta, EventReasoningDelta:
		if event.Model == nil || event.Model.Text == "" || event.Model.ToolCall != nil ||
			event.Model.ToolCallID != "" || event.Model.Result != nil ||
			event.payloadCount() != 1 {
			return fmt.Errorf("%s requires only model.text", event.Type)
		}
	case EventToolCallArgumentsDelta:
		if event.Model == nil || event.Model.Text == "" ||
			strings.TrimSpace(event.Model.ToolCallID) == "" ||
			event.Model.ToolCall != nil || event.Model.Result != nil ||
			event.payloadCount() != 1 {
			return fmt.Errorf("%s requires model.text and model.tool_call_id", event.Type)
		}
	case EventToolCallStarted:
		if event.Model == nil || event.Model.ToolCall == nil || event.Model.Text != "" ||
			event.Model.ToolCallID != "" || event.Model.Result != nil ||
			event.payloadCount() != 1 {
			return fmt.Errorf("%s requires only model.tool_call", event.Type)
		}
		if err := event.Model.ToolCall.Validate(); err != nil {
			return fmt.Errorf("%s: %w", event.Type, err)
		}
	case EventModelCompleted:
		if event.Model == nil || event.Model.Result == nil || event.Model.Text != "" ||
			event.Model.ToolCallID != "" || event.Model.ToolCall != nil ||
			event.payloadCount() != 1 {
			return fmt.Errorf("%s requires only model.result", event.Type)
		}
		if err := event.Model.Result.Validate(); err != nil {
			return fmt.Errorf("%s: %w", event.Type, err)
		}
	case EventToolStarted:
		if event.Tool == nil || event.Tool.CallID == "" || event.Tool.Name == "" ||
			event.Tool.IdempotencyKey == "" || event.Tool.Content != "" ||
			event.payloadCount() != 1 {
			return fmt.Errorf("%s requires tool call metadata", event.Type)
		}
	case EventToolCompleted:
		if event.Tool == nil || event.Tool.CallID == "" ||
			event.Tool.IdempotencyKey == "" || event.payloadCount() != 1 {
			return fmt.Errorf("%s requires tool result metadata", event.Type)
		}
	case EventToolFailed:
		if event.Tool == nil || event.Tool.CallID == "" ||
			event.Tool.IdempotencyKey == "" || event.Error == nil ||
			event.payloadCount() != 2 {
			return fmt.Errorf("%s requires tool metadata and error", event.Type)
		}
		if err := event.Error.Validate(); err != nil {
			return fmt.Errorf("%s: %w", event.Type, err)
		}
	case EventAgentPaused:
		if event.Agent == nil || event.Agent.RunID == "" ||
			event.Agent.State != "paused" || event.Agent.PauseID == "" ||
			event.payloadCount() != 1 {
			return fmt.Errorf("%s requires paused agent metadata", event.Type)
		}
	case EventAgentCompleted:
		if event.Agent == nil || event.Agent.RunID == "" ||
			event.Agent.State != "completed" || event.Agent.StopReason == "" ||
			event.payloadCount() != 1 {
			return fmt.Errorf("%s requires completed agent metadata", event.Type)
		}
	case EventCheckpointCommitted:
		if event.Checkpoint == nil || event.Checkpoint.RunID == "" ||
			event.Checkpoint.CheckpointID == "" || event.payloadCount() != 1 {
			return fmt.Errorf("%s requires checkpoint metadata", event.Type)
		}
	case EventRunCompleted, EventRunCancelled:
		wantState := "completed"
		if event.Type == EventRunCancelled {
			wantState = "cancelled"
		}
		if event.Run == nil || event.Run.RunID == "" ||
			event.Run.State != wantState || event.payloadCount() != 1 {
			return fmt.Errorf("%s requires run metadata", event.Type)
		}
	case EventRunFailed:
		if event.Error == nil || event.payloadCount() < 1 ||
			event.payloadCount() > 2 || event.Model != nil || event.Tool != nil ||
			event.Agent != nil || event.Checkpoint != nil {
			return fmt.Errorf("%s requires an error and optional run metadata", event.Type)
		}
		if event.Run != nil &&
			(event.Run.RunID == "" || event.Run.State != "failed") {
			return fmt.Errorf("%s contains invalid run metadata", event.Type)
		}
		if err := event.Error.Validate(); err != nil {
			return fmt.Errorf("%s: %w", event.Type, err)
		}
	case EventRunSettled:
		if event.payloadCount() > 1 ||
			event.Model != nil || event.Tool != nil || event.Agent != nil ||
			event.Checkpoint != nil || event.Error != nil ||
			event.Run != nil && event.Run.RunID == "" {
			return fmt.Errorf("%s accepts only optional run metadata", event.Type)
		}
	default:
		return fmt.Errorf("unsupported event type %q", event.Type)
	}
	return nil
}

func (event Event) payloadCount() int {
	count := 0
	for _, exists := range []bool{
		event.Model != nil,
		event.Tool != nil,
		event.Agent != nil,
		event.Checkpoint != nil,
		event.Run != nil,
		event.Error != nil,
	} {
		if exists {
			count++
		}
	}
	return count
}

func (result ModelResult) Validate() error {
	if err := result.Message.Validate(); err != nil {
		return fmt.Errorf("message: %w", err)
	}
	switch result.FinishReason {
	case FinishStop, FinishToolCall, FinishLength, FinishContentFilter, FinishCancelled:
	default:
		return fmt.Errorf("unsupported finish_reason %q", result.FinishReason)
	}
	if result.FinishReason == FinishToolCall && len(result.Message.ToolCalls) == 0 {
		return fmt.Errorf("tool_call finish_reason requires tool_calls")
	}
	if result.FinishReason != FinishToolCall && len(result.Message.ToolCalls) != 0 {
		return fmt.Errorf("tool_calls require tool_call finish_reason")
	}
	if err := result.Usage.Validate(); err != nil {
		return err
	}
	return nil
}

func (usage Usage) Validate() error {
	for name, value := range map[string]*int64{
		"input_tokens": usage.InputTokens, "output_tokens": usage.OutputTokens,
		"reasoning_tokens": usage.ReasoningTokens, "cache_read_tokens": usage.CacheReadTokens,
		"cache_write_tokens": usage.CacheWriteTokens, "total_tokens": usage.TotalTokens,
	} {
		if value != nil && *value < 0 {
			return fmt.Errorf("usage.%s must not be negative", name)
		}
	}
	switch usage.Source {
	case "", UsageSourceProvider, UsageSourceEstimated:
	default:
		return fmt.Errorf("unsupported usage.source %q", usage.Source)
	}
	switch usage.Completeness {
	case "", UsageComplete, UsagePartial:
	default:
		return fmt.Errorf("unsupported usage.completeness %q", usage.Completeness)
	}
	return nil
}

func (runtimeError RuntimeError) Validate() error {
	switch runtimeError.Code {
	case ErrorInvalidRequest, ErrorAuthenticationFailed, ErrorPermissionDenied,
		ErrorRateLimited, ErrorTimeout, ErrorProviderUnavailable, ErrorProtocol,
		ErrorInvalidProviderResponse, ErrorContextOverflow, ErrorToolFailed,
		ErrorCancelled, ErrorConflict, ErrorNotFound, ErrorInternal:
	default:
		return fmt.Errorf("unsupported error code %q", runtimeError.Code)
	}
	switch runtimeError.Phase {
	case PhaseRequest, PhaseProfile, PhaseProvider, PhaseTransport, PhaseConsumer, PhaseRun:
	default:
		return fmt.Errorf("unsupported error phase %q", runtimeError.Phase)
	}
	if strings.TrimSpace(runtimeError.Message) == "" {
		return fmt.Errorf("error message is required")
	}
	if err := validateText(runtimeError.Message, "error message"); err != nil {
		return err
	}
	if runtimeError.RetryAfterMS < 0 {
		return fmt.Errorf("retry_after_ms must not be negative")
	}
	if runtimeError.HTTPStatus != 0 &&
		(runtimeError.HTTPStatus < 100 || runtimeError.HTTPStatus > 599) {
		return fmt.Errorf("invalid HTTP status %d", runtimeError.HTTPStatus)
	}
	return nil
}

func validateName(value, label string) error {
	if err := validateText(value, label); err != nil {
		return err
	}
	if strings.TrimSpace(value) == "" {
		return fmt.Errorf("%s is required", label)
	}
	if len(value) > 256 {
		return fmt.Errorf("%s exceeds 256 bytes", label)
	}
	return nil
}

func validateText(value, label string) error {
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s must be valid UTF-8 without NUL", label)
	}
	return nil
}

func validateJSONObject(value json.RawMessage, maxBytes int, label string) error {
	value = bytes.TrimSpace(value)
	if len(value) == 0 {
		return fmt.Errorf("%s is required", label)
	}
	if len(value) > maxBytes {
		return fmt.Errorf("%s exceeds %d bytes", label, maxBytes)
	}
	if value[0] != '{' || !json.Valid(value) {
		return fmt.Errorf("%s must be a JSON object", label)
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(value, &object); err != nil {
		return fmt.Errorf("%s must be a JSON object: %w", label, err)
	}
	return nil
}
