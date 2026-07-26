package agentrun

import (
	"strings"
	"testing"

	"github.com/yy003x/runtime/internal/provider"
)

func TestContextProjectionUsesBudgetBeforeProactiveThreshold(t *testing.T) {
	manager := NewSessionManager(New(t.TempDir()))
	base := provider.ContextCapacity{
		ContextWindowTokens: 1000, ReservedOutputTokens: 200,
		InputBudgetTokens: 800, CompactionAtTokens: 560,
		KeepRecentTurns: 2,
	}

	withinBudget := base
	withinBudget.SummaryEnabled = false
	projection, err := manager.projectContext(
		"", "turn-threshold", "test", withinBudget, provider.StaticContextEstimate{},
		strings.Repeat("a", 1600),
	)
	if err != nil || projection.EstimatedInputTokens <= withinBudget.CompactionAtTokens ||
		projection.EstimatedInputTokens > withinBudget.InputBudgetTokens ||
		projection.PressureState != "raw_fallback" {
		t.Fatalf("projection=%#v err=%v", projection, err)
	}

	atBudget := base
	atBudget.SummaryEnabled = true
	projection, err = manager.projectContext(
		"", "turn-budget", "test", atBudget, provider.StaticContextEstimate{},
		strings.Repeat("a", (atBudget.InputBudgetTokens-256)*4),
	)
	if err != nil || projection.EstimatedInputTokens != atBudget.InputBudgetTokens ||
		projection.PressureState != "raw_fallback" {
		t.Fatalf("at budget projection=%#v err=%v", projection, err)
	}

	overflow := base
	overflow.SummaryEnabled = false
	projection, err = manager.projectContext(
		"", "turn-overflow", "test", overflow, provider.StaticContextEstimate{},
		strings.Repeat("a", (overflow.InputBudgetTokens-256)*4+4),
	)
	if err == nil || !strings.Contains(err.Error(), "context_overflow") ||
		projection.PressureState != "overflow_summary_disabled" {
		t.Fatalf("overflow projection=%#v err=%v", projection, err)
	}
}

func TestContextProjectionCountsStaticProviderComponents(t *testing.T) {
	manager := NewSessionManager(New(t.TempDir()))
	capacity := provider.ContextCapacity{
		ContextWindowTokens: 1000, ReservedOutputTokens: 200,
		InputBudgetTokens: 800, CompactionAtTokens: 560,
		KeepRecentTurns: 2, SummaryEnabled: false,
	}
	staticEstimate := provider.StaticContextEstimate{Counted: []provider.ContextEstimateComponent{{
		Category: "system_prompt", ID: "system", EstimatedTokens: 300,
	}}}
	projection, err := manager.projectContext(
		"", "turn-static", "test", capacity, staticEstimate, strings.Repeat("a", 600),
	)
	if err != nil {
		t.Fatal(err)
	}
	if projection.EstimatedInputTokens != 706 || projection.PressureState != "raw_fallback" {
		t.Fatalf("projection=%#v", projection)
	}
}
