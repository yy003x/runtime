package memory

import (
	"context"
	"fmt"
	"strings"

	"agent-arch/internal/config"
	"agent-arch/internal/llm"
	"agent-arch/internal/token"
)

type Manager struct {
	store     Store
	retriever Retriever
	cfg       config.MemoryConfig
	counter   token.Counter
}

func NewManager(store Store, retriever Retriever, cfg config.MemoryConfig, counter token.Counter) *Manager {
	return &Manager{store: store, retriever: retriever, cfg: cfg, counter: counter}
}

func (m *Manager) InitSession(ctx context.Context, sessionID string) error {
	return m.store.Save(ctx, &SessionMemory{
		SessionID: sessionID,
		Turns:     []Turn{},
		Retrieved: []RetrievedMemory{},
	})
}

func (m *Manager) Get(ctx context.Context, sessionID string) (*SessionMemory, error) {
	return m.store.Get(ctx, sessionID)
}

func (m *Manager) AppendTurn(ctx context.Context, sessionID string, turn Turn) error {
	session, err := m.store.Get(ctx, sessionID)
	if err != nil {
		return fmt.Errorf("load session memory: %w", err)
	}

	session.Turns = append(session.Turns, turn)
	m.compactIfNeeded(session)

	if err := m.store.Save(ctx, session); err != nil {
		return fmt.Errorf("save session memory: %w", err)
	}
	return nil
}

func (m *Manager) BuildContext(ctx context.Context, sessionID, query, system string) ([]llm.Message, *SessionMemory, error) {
	session, err := m.store.Get(ctx, sessionID)
	if err != nil {
		return nil, nil, fmt.Errorf("load session memory: %w", err)
	}

	retrieved, err := m.retriever.Retrieve(ctx, sessionID, query)
	if err != nil {
		return nil, nil, fmt.Errorf("retrieve memory: %w", err)
	}
	session.Retrieved = retrieved

	if err := m.store.Save(ctx, session); err != nil {
		return nil, nil, fmt.Errorf("save retrieved memory: %w", err)
	}

	return m.assembleMessages(system, session), session, nil
}

func (m *Manager) assembleMessages(system string, session *SessionMemory) []llm.Message {
	limit := m.cfg.MaxContextTokens - m.cfg.ResponseReservedTokens - m.cfg.SafetyBufferTokens
	if limit < 0 {
		limit = 0
	}

	used := m.counter.CountText(system)
	var messages []llm.Message

	if session.Summary != nil {
		summaryText := m.renderSummary(*session.Summary)
		summaryTokens := m.counter.CountText(summaryText)
		if used+summaryTokens <= limit {
			messages = append(messages, llm.Message{Role: "system", Content: summaryText})
			used += summaryTokens
		}
	}

	for _, item := range session.Retrieved {
		content := "Retrieved memory [" + item.Source + "]: " + item.Content
		tokens := m.counter.CountText(content)
		if used+tokens > limit {
			break
		}
		messages = append(messages, llm.Message{Role: "system", Content: content})
		used += tokens
	}

	selected := make([]Turn, 0, len(session.Turns))
	for i := len(session.Turns) - 1; i >= 0; i-- {
		turn := session.Turns[i]
		tokens := m.counter.CountText(turn.Content)
		if used+tokens > limit {
			continue
		}
		selected = append(selected, turn)
		used += tokens
	}

	for i := len(selected) - 1; i >= 0; i-- {
		messages = append(messages, llm.Message{
			Role:    selected[i].Role,
			Content: selected[i].Content,
		})
	}

	return messages
}

func (m *Manager) compactIfNeeded(session *SessionMemory) {
	if len(session.Turns) <= m.cfg.KeepRecentTurns {
		return
	}

	currentTokens := 0
	for _, turn := range session.Turns {
		currentTokens += m.counter.CountText(turn.Content)
	}

	thresholdTokens := int(float64(m.cfg.RecentTurnsReserved) * m.cfg.CompressionThreshold)
	if currentTokens < thresholdTokens && len(session.Turns) <= m.cfg.KeepRecentTurns*2 {
		return
	}

	compressCount := len(session.Turns) - m.cfg.KeepRecentTurns
	if compressCount <= 0 {
		return
	}

	compressed := append([]Turn(nil), session.Turns[:compressCount]...)
	session.Turns = append([]Turn(nil), session.Turns[compressCount:]...)

	summary := m.summarize(compressed)
	if session.Summary == nil {
		session.Summary = summary
		return
	}

	session.Summary.CompressedTurns += summary.CompressedTurns
	session.Summary.Bullets = append(session.Summary.Bullets, summary.Bullets...)
}

func (m *Manager) summarize(turns []Turn) *SummaryBlock {
	bullets := make([]string, 0, len(turns))
	for _, turn := range turns {
		text := strings.TrimSpace(turn.Content)
		if text == "" {
			continue
		}
		if len([]rune(text)) > 120 {
			text = string([]rune(text)[:120]) + "..."
		}
		bullets = append(bullets, fmt.Sprintf("%s: %s", turn.Role, text))
	}

	return &SummaryBlock{
		CompressedTurns: len(turns),
		Bullets:         bullets,
	}
}

func (m *Manager) renderSummary(summary SummaryBlock) string {
	var b strings.Builder
	b.WriteString("Rolling summary:\n")
	b.WriteString(fmt.Sprintf("- compressed_turns: %d\n", summary.CompressedTurns))
	for _, bullet := range summary.Bullets {
		b.WriteString("- ")
		b.WriteString(bullet)
		b.WriteString("\n")
	}
	return strings.TrimSpace(b.String())
}
