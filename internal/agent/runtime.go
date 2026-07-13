package agent

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agent-arch/internal/llm"
	"agent-arch/internal/memory"
	"agent-arch/internal/persona"
)

type AgentRuntime struct {
	memory                 *memory.Manager
	logger                 TraceLogger
	responseReservedTokens int
}

type RuntimeResult struct {
	Message string
}

func NewAgentRuntime(memoryManager *memory.Manager, logger TraceLogger, responseReservedTokens int) *AgentRuntime {
	return &AgentRuntime{
		memory:                 memoryManager,
		logger:                 logger,
		responseReservedTokens: responseReservedTokens,
	}
}

func (r *AgentRuntime) ExecuteTurn(ctx context.Context, session Session, client llm.Client, userMessage string) (RuntimeResult, error) {
	if err := r.memory.AppendTurn(ctx, session.ID, memory.Turn{
		Role:    "user",
		Content: userMessage,
	}); err != nil {
		return RuntimeResult{}, fmt.Errorf("append user turn: %w", err)
	}

	system := persona.RenderSystem(session.Persona)
	contextMessages, sessionMemory, err := r.memory.BuildContext(ctx, session.ID, userMessage, system)
	if err != nil {
		return RuntimeResult{}, fmt.Errorf("build context: %w", err)
	}
	augmentedSystem, conversationMessages := splitContextMessages(system, contextMessages)

	llmReq := llm.Request{
		Model:           session.Model,
		System:          augmentedSystem,
		Messages:        conversationMessages,
		Temperature:     session.Persona.ModelPolicy.Temperature,
		MaxOutputTokens: session.Persona.ModelPolicy.MaxOutputTokens,
	}
	if llmReq.MaxOutputTokens == 0 {
		llmReq.MaxOutputTokens = r.responseReservedTokens
	}

	turn := turnNumber(sessionMemory)
	startedAt := time.Now().UTC()
	resp, callErr := client.Generate(ctx, llmReq)
	completedAt := time.Now().UTC()

	if err := r.logTrace(ctx, session, llmReq, turn, resp, callErr, startedAt, completedAt); err != nil {
		return RuntimeResult{}, fmt.Errorf("log llm trace: %w", err)
	}
	if callErr != nil {
		return RuntimeResult{}, fmt.Errorf("generate response: %w", callErr)
	}

	if err := r.memory.AppendTurn(ctx, session.ID, memory.Turn{
		Role:    "assistant",
		Content: resp.OutputText,
	}); err != nil {
		return RuntimeResult{}, fmt.Errorf("append assistant turn: %w", err)
	}

	return RuntimeResult{
		Message: resp.OutputText,
	}, nil
}

func splitContextMessages(baseSystem string, messages []llm.Message) (string, []llm.Message) {
	systemParts := []string{baseSystem}
	out := make([]llm.Message, 0, len(messages))
	for _, msg := range messages {
		if msg.Role == "system" {
			if msg.Content != "" {
				systemParts = append(systemParts, msg.Content)
			}
			continue
		}
		out = append(out, msg)
	}
	return strings.Join(systemParts, "\n\n"), out
}

func turnNumber(sessionMemory *memory.SessionMemory) int {
	if sessionMemory == nil {
		return 0
	}
	return (len(sessionMemory.Turns) + 1) / 2
}

func (r *AgentRuntime) logTrace(ctx context.Context, session Session, req llm.Request, turn int, resp llm.Response, callErr error, startedAt, completedAt time.Time) error {
	if r.logger == nil {
		return nil
	}

	entry := LLMTrace{
		TraceID:        fmt.Sprintf("%s-%04d", session.ID, turn),
		SessionID:      session.ID,
		Turn:           turn,
		Provider:       session.Provider,
		Model:          req.Model,
		StartedAt:      startedAt,
		CompletedAt:    completedAt,
		DurationMillis: completedAt.Sub(startedAt).Milliseconds(),
		Request: LLMTraceRequest{
			System:          req.System,
			Messages:        append([]llm.Message(nil), req.Messages...),
			Temperature:     req.Temperature,
			MaxOutputTokens: req.MaxOutputTokens,
		},
	}
	if callErr != nil {
		entry.Error = callErr.Error()
	} else {
		entry.Response = &LLMTraceResponse{
			OutputText:   resp.OutputText,
			InputTokens:  resp.InputTokens,
			OutputTokens: resp.OutputTokens,
		}
	}

	return r.logger.Log(ctx, entry)
}
