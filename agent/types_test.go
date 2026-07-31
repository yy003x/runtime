package agent

import (
	"testing"
	"time"
)

func TestBudgetValidation(t *testing.T) {
	if err := DefaultBudget().Validate(); err != nil {
		t.Fatal(err)
	}
	for name, value := range map[string]Budget{
		"rounds_zero":        {MaxToolCalls: 1, MaxWallTime: time.Second},
		"rounds_too_large":   {MaxRounds: 129, MaxToolCalls: 1, MaxWallTime: time.Second},
		"tools_zero":         {MaxRounds: 1, MaxWallTime: time.Second},
		"tools_too_large":    {MaxRounds: 1, MaxToolCalls: 1025, MaxWallTime: time.Second},
		"tokens_negative":    {MaxRounds: 1, MaxToolCalls: 1, MaxTotalTokens: -1, MaxWallTime: time.Second},
		"wall_time_short":    {MaxRounds: 1, MaxToolCalls: 1, MaxWallTime: time.Millisecond},
		"wall_time_too_long": {MaxRounds: 1, MaxToolCalls: 1, MaxWallTime: 24*time.Hour + time.Second},
	} {
		t.Run(name, func(t *testing.T) {
			if err := value.Validate(); err == nil {
				t.Fatal("invalid budget was accepted")
			}
		})
	}
}
