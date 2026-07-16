package native

import (
	"encoding/json"
	"fmt"
)

type ContextManager struct{}

func (ContextManager) BuildPrompt(value Context, budget int) ([]Message, error) {
	if budget <= 0 {
		return nil, fmt.Errorf("invalid token budget: %d", budget)
	}
	base := cloneMessages(value.SystemInstructions)
	for _, message := range value.Messages {
		if message.Pinned {
			base = append(base, message)
		}
	}
	if estimate(base) > budget {
		return nil, fmt.Errorf("pinned context exceeds token budget: %d", budget)
	}
	recent := make([]Message, 0, len(value.Messages))
	for index := len(value.Messages) - 1; index >= 0; index-- {
		message := value.Messages[index]
		if message.Pinned {
			continue
		}
		candidate := append(append(append([]Message(nil), base...), message), recent...)
		if estimate(candidate) > budget {
			if len(recent) == 0 {
				return nil, fmt.Errorf("latest context message exceeds token budget: %d", budget)
			}
			break
		}
		recent = append([]Message{message}, recent...)
	}
	return append(base, recent...), nil
}

func (ContextManager) ApplyPatch(value Context, patch ContextPatch) (Context, error) {
	next := Context{
		SystemInstructions: cloneMessages(value.SystemInstructions),
		Messages:           cloneMessages(value.Messages),
	}
	switch patch.Operation {
	case PatchAppend:
		next.SystemInstructions = append(next.SystemInstructions, cloneMessages(patch.SystemInstructions)...)
		next.Messages = append(next.Messages, cloneMessages(patch.Messages)...)
	case PatchReplace:
		if patch.SystemInstructions != nil {
			next.SystemInstructions = cloneMessages(patch.SystemInstructions)
		}
		if patch.Messages != nil {
			next.Messages = cloneMessages(patch.Messages)
		}
	default:
		return Context{}, fmt.Errorf("unsupported patch operation: %s", patch.Operation)
	}
	return next, nil
}

func estimate(messages []Message) int {
	total := 0
	for _, message := range messages {
		total += 4 + len(message.Content)/4
		for _, call := range message.ToolCalls {
			encoded, _ := json.Marshal(call.Arguments)
			total += 8 + (len(call.Name)+len(encoded))/4
		}
	}
	return total
}
