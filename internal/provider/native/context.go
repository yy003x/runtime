package native

import "fmt"

type ContextManager struct{}

func (ContextManager) BuildPrompt(value Context, budget int) ([]Message, error) {
	if budget <= 0 {
		return nil, fmt.Errorf("invalid token budget: %d", budget)
	}
	base := append([]Message(nil), value.SystemInstructions...)
	for _, message := range value.Messages {
		if message.Pinned {
			base = append(base, message)
		}
	}
	recent := make([]Message, 0, len(value.Messages))
	for index := len(value.Messages) - 1; index >= 0; index-- {
		message := value.Messages[index]
		if message.Pinned {
			continue
		}
		candidate := append(append(append([]Message(nil), base...), message), recent...)
		if estimate(candidate) > budget {
			break
		}
		recent = append([]Message{message}, recent...)
	}
	return append(base, recent...), nil
}

func (ContextManager) ApplyPatch(value Context, patch ContextPatch) (Context, error) {
	next := Context{
		SystemInstructions: append([]Message(nil), value.SystemInstructions...),
		Messages:           append([]Message(nil), value.Messages...),
	}
	switch patch.Operation {
	case PatchAppend:
		next.SystemInstructions = append(next.SystemInstructions, patch.SystemInstructions...)
		next.Messages = append(next.Messages, patch.Messages...)
	case PatchReplace:
		if patch.SystemInstructions != nil {
			next.SystemInstructions = append([]Message(nil), patch.SystemInstructions...)
		}
		if patch.Messages != nil {
			next.Messages = append([]Message(nil), patch.Messages...)
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
	}
	return total
}
