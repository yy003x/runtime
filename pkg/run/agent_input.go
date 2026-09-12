package run

import "github.com/yy003x/runtime/pkg/contract"

func agentUserMessage(request Request) contract.Message {
	if len(request.InputParts) > 0 {
		return contract.Message{
			Role:         contract.RoleUser,
			ContentParts: append([]contract.ContentPart(nil), request.InputParts...),
		}
	}
	return contract.Message{Role: contract.RoleUser, Content: request.Input}
}
