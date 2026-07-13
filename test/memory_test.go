package test

import (
	"context"
	"strings"
	"testing"

	"agent-arch/internal/config"
	"agent-arch/internal/memory"
	"agent-arch/internal/token"
)

func TestMemoryCompactionThreshold(t *testing.T) {
	t.Parallel()

	cfg := config.MemoryConfig{
		MaxContextTokens:       1024,
		ResponseReservedTokens: 128,
		SafetyBufferTokens:     64,
		RecentTurnsReserved:    80,
		CompressionThreshold:   0.5,
		KeepRecentTurns:        2,
	}

	manager := memory.NewManager(memory.NewInMemoryStore(), memory.StubRetriever{}, cfg, token.NewApproxCounter())
	ctx := context.Background()
	sessionID := "sess_compact"

	if err := manager.InitSession(ctx, sessionID); err != nil {
		t.Fatalf("init session: %v", err)
	}

	for i := 0; i < 6; i++ {
		if err := manager.AppendTurn(ctx, sessionID, memory.Turn{
			Role:    "user",
			Content: strings.Repeat("token ", 20),
		}); err != nil {
			t.Fatalf("append turn %d: %v", i, err)
		}
	}

	mem, err := manager.Get(ctx, sessionID)
	if err != nil {
		t.Fatalf("get memory: %v", err)
	}

	if mem.Summary == nil || mem.Summary.CompressedTurns == 0 {
		t.Fatal("expected summary after compaction")
	}
	if len(mem.Turns) > cfg.KeepRecentTurns {
		t.Fatalf("expected no more than %d recent turns, got %d", cfg.KeepRecentTurns, len(mem.Turns))
	}
}

func TestTokenBudgetTrimming(t *testing.T) {
	t.Parallel()

	cfg := config.MemoryConfig{
		MaxContextTokens:       120,
		ResponseReservedTokens: 20,
		SafetyBufferTokens:     10,
		RecentTurnsReserved:    60,
		CompressionThreshold:   0.5,
		KeepRecentTurns:        4,
	}

	counter := token.NewApproxCounter()
	manager := memory.NewManager(memory.NewInMemoryStore(), memory.StubRetriever{}, cfg, counter)
	ctx := context.Background()
	sessionID := "sess_trim"

	if err := manager.InitSession(ctx, sessionID); err != nil {
		t.Fatalf("init session: %v", err)
	}

	for i := 0; i < 10; i++ {
		if err := manager.AppendTurn(ctx, sessionID, memory.Turn{
			Role:    "user",
			Content: strings.Repeat("abcdef ", 10),
		}); err != nil {
			t.Fatalf("append turn: %v", err)
		}
	}

	system := strings.Repeat("sys ", 10)
	messages, _, err := manager.BuildContext(ctx, sessionID, "question", system)
	if err != nil {
		t.Fatalf("build context: %v", err)
	}

	total := counter.CountText(system)
	for _, msg := range messages {
		total += counter.CountText(msg.Content)
	}

	limit := cfg.MaxContextTokens - cfg.ResponseReservedTokens - cfg.SafetyBufferTokens
	if total > limit {
		t.Fatalf("expected total tokens <= %d, got %d", limit, total)
	}
}
