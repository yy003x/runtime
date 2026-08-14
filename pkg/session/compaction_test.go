package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
	"github.com/yy003x/runtime/pkg/profile"
)

func compactionRecords() []MessageRecord {
	var records []MessageRecord
	seq := uint64(0)
	// Four turns, each a user+assistant pair with large content so the full
	// history overflows a small budget but the last turn alone fits.
	for turn := 1; turn <= 4; turn++ {
		turnID := "turn-" + string(rune('a'+turn-1))
		for _, role := range []contract.Role{contract.RoleUser, contract.RoleAssistant} {
			seq++
			records = append(records, MessageRecord{
				Sequence: seq,
				TurnID:   turnID,
				Message:  contract.Message{Role: role, Content: strings.Repeat("x", 900)},
			})
		}
	}
	return records
}

func compactionEntry(enabled bool, keepRecent int) profile.Entry {
	flag := enabled
	return profile.Entry{
		ID: "api", Kind: profile.KindModel,
		Model: &model.Profile{
			Driver: model.DriverAnthropic,
			Context: model.ContextPolicy{
				WindowTokens:         700,
				ReservedOutputTokens: 100, // input budget = 600 tokens
				SummaryEnabled:       &flag,
				KeepRecentTurns:      keepRecent,
			},
		},
	}
}

func TestBuildProjectionCompactsOverflowWhenSummaryEnabled(t *testing.T) {
	records := compactionRecords()
	built, runtimeErr := buildProjection(
		compactionEntry(true, 1),
		"session_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"turn-d", "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"execution_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"", RunRequest{ProfileID: "api", Input: "go"},
		records, time.Unix(1, 0).UTC(),
	)
	if runtimeErr != nil {
		t.Fatalf("buildProjection: %v", runtimeErr)
	}
	if built.summary == nil {
		t.Fatal("expected compaction summary, got nil")
	}
	// Last turn (user+assistant, sequences 7,8) is retained verbatim.
	if len(built.modelMessages) != 2 {
		t.Fatalf("expected 2 compacted messages, got %d", len(built.modelMessages))
	}
	if built.modelMessages[0].Role != contract.RoleUser ||
		built.modelMessages[1].Role != contract.RoleAssistant {
		t.Fatalf("compacted roles=%v %v", built.modelMessages[0].Role, built.modelMessages[1].Role)
	}
	summary := *built.summary
	if summary.RangeStartSeq != 1 || summary.RangeEndSeq != 7 {
		t.Fatalf("summary range=%d..%d, want 1..7", summary.RangeStartSeq, summary.RangeEndSeq)
	}
	if built.manifest.CheckpointRef != summary.SummaryID || built.manifest.CheckpointRef == "" {
		t.Fatalf("manifest checkpoint ref mismatch: %q vs %q", built.manifest.CheckpointRef, summary.SummaryID)
	}
	if built.manifest.MessageSequenceStart != 7 {
		t.Fatalf("manifest start seq=%d, want 7", built.manifest.MessageSequenceStart)
	}
	if built.manifest.PressureState == "overflow" {
		t.Fatalf("compaction left pressure=overflow: %#v", built.manifest)
	}
	// CompactedRangeDigest is reproducible from the dropped canonical messages.
	dropped := recordsToMessages(records[:6])
	droppedJSON, err := json.Marshal(dropped)
	if err != nil {
		t.Fatal(err)
	}
	if summary.CompactedRangeDigest != digest(droppedJSON) {
		t.Fatalf(
			"compacted range digest mismatch: %q != %q",
			summary.CompactedRangeDigest, digest(droppedJSON),
		)
	}
}

func TestBuildProjectionOverflowFailsClosedWhenSummaryDisabled(t *testing.T) {
	_, runtimeErr := buildProjection(
		compactionEntry(false, 1),
		"session_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"turn-d", "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"execution_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"", RunRequest{ProfileID: "api", Input: "go"},
		compactionRecords(), time.Unix(1, 0).UTC(),
	)
	if runtimeErr == nil || runtimeErr.Code != contract.ErrorContextOverflow {
		t.Fatalf("expected context overflow error, got %v", runtimeErr)
	}
}

func TestBuildProjectionDoesNotCompactWhenHistoryFits(t *testing.T) {
	// A single short turn fits the budget; summary_enabled must not trigger.
	short := []MessageRecord{{
		Sequence: 1, TurnID: "turn-a",
		Message: contract.Message{Role: contract.RoleUser, Content: "hi"},
	}}
	built, runtimeErr := buildProjection(
		compactionEntry(true, 1),
		"session_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"turn-a", "run_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"execution_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		"", RunRequest{ProfileID: "api", Input: "hi"},
		short, time.Unix(1, 0).UTC(),
	)
	if runtimeErr != nil {
		t.Fatalf("buildProjection: %v", runtimeErr)
	}
	if built.summary != nil {
		t.Fatalf("expected no compaction when history fits, got summary %#v", *built.summary)
	}
	if built.manifest.CheckpointRef != "" {
		t.Fatalf("expected empty checkpoint ref, got %q", built.manifest.CheckpointRef)
	}
}

func TestCompressProjectionReturnsFalseWhenMinimumTailOverflows(t *testing.T) {
	// Budget too small to hold even one turn: compaction cannot help.
	kept, dropped, ok := compressProjection(compactionRecords(), 1, 10)
	if ok {
		t.Fatalf("expected ok=false, got kept=%d dropped=%d", len(kept), len(dropped))
	}
}

func TestCompressProjectionRespectsKeepRecentTurnsFloor(t *testing.T) {
	records := compactionRecords()
	// keepRecentTurns=3 requires retaining 3 turns (~1440 tokens). A 600-token
	// budget fits only 1 turn, so compaction must refuse rather than drop below
	// the floor.
	if _, _, ok := compressProjection(records, 3, 600); ok {
		t.Fatal("expected ok=false when keep floor cannot be met")
	}
	// A budget that fits 3 turns (but not all 4) keeps exactly 3 turns.
	kept, dropped, ok := compressProjection(records, 3, 1500)
	if !ok {
		t.Fatal("expected ok=true")
	}
	if len(kept) != 6 || len(dropped) != 2 {
		t.Fatalf("expected 6 kept / 2 dropped, got %d / %d", len(kept), len(dropped))
	}
}

func TestVerifyCompactionMatchesOffsetAndRejectsTamperedPrefix(t *testing.T) {
	records := compactionRecords() // sequences 1..8, four turns
	droppedJSON, err := json.Marshal(recordsToMessages(records[:6]))
	if err != nil {
		t.Fatal(err)
	}
	summary := SummaryRecord{
		SummaryVersion:       SummaryRecordVersion,
		SummaryID:            "summary_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		RangeStartSeq:        1,
		RangeEndSeq:          7,
		CompactedRangeDigest: digest(droppedJSON),
	}
	offset, err := verifyCompaction(records, summary)
	if err != nil || offset != 6 {
		t.Fatalf("offset=%d err=%v", offset, err)
	}
	// Tampering with a dropped-prefix message changes its digest -> fail closed.
	tampered := append([]MessageRecord(nil), records...)
	tampered[0] = MessageRecord{
		Sequence: 1, TurnID: records[0].TurnID,
		Message: contract.Message{Role: contract.RoleUser, Content: "tampered"},
	}
	if _, err := verifyCompaction(tampered, summary); err == nil {
		t.Fatal("expected digest mismatch error for tampered prefix")
	}
	// Range end beyond the canonical tail -> fail closed.
	outOfBounds := summary
	outOfBounds.RangeEndSeq = 99
	if _, err := verifyCompaction(records, outOfBounds); err == nil {
		t.Fatal("expected out-of-bounds range error")
	}
}

// TestCompactedAgentTurnSettlesViaOffsetGrounding exercises the full Agent path:
// two session-exec turns build non-overflowing history, a third Agent turn
// overflows the input budget and is compacted at PrepareAgent, then SettleAgent
// must accept the compacted prefix via offset grounding (not the legacy verbatim
// prefix check). Budget is tuned so the first two turns fit and the third
// overflows.
func TestCompactedAgentTurnSettlesViaOffsetGrounding(t *testing.T) {
	content := strings.Repeat("z", 200)
	enabled := true
	policy := &model.ContextPolicy{
		WindowTokens:         300, // input budget = 200 tokens
		ReservedOutputTokens: 100,
		SummaryEnabled:       &enabled,
		KeepRecentTurns:      2,
	}
	generator := &scriptedGenerator{results: []contract.ModelResult{
		{Message: contract.Message{Role: contract.RoleAssistant, Content: content}, FinishReason: contract.FinishStop},
		{Message: contract.Message{Role: contract.RoleAssistant, Content: content}, FinishReason: contract.FinishStop},
	}}
	service := newTestService(t, generator, nil, policy)
	ctx := context.Background()

	first, runtimeErr := service.Run(ctx, RunRequest{ProfileID: "api", Input: content})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	sessionID := first.SessionID
	if _, runtimeErr := service.Run(ctx, RunRequest{
		SessionID: sessionID, ProfileID: "api", Input: content,
	}); runtimeErr != nil {
		t.Fatal(runtimeErr)
	}

	runID, err := identity.New("run")
	if err != nil {
		t.Fatal(err)
	}
	// Third turn: five messages (~285 tokens) overflow the 200-token budget.
	turn, runtimeErr := service.PrepareAgent(RunRequest{
		SessionID: sessionID, RunID: runID, ProfileID: "api", Input: "final",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	summaries, err := service.store.summaries(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(summaries) != 1 {
		t.Fatalf("expected one compaction summary, got %d", len(summaries))
	}
	if turn.BaseMessageCount == 0 || turn.BaseMessageCount == 5 {
		t.Fatalf("expected compacted base, got BaseMessageCount=%d", turn.BaseMessageCount)
	}

	settled, runtimeErr := service.SettleAgent(turn, cloneMessages(turn.Messages), agent.Outcome{
		State:   agent.StateCompleted,
		Message: &contract.Message{Role: contract.RoleAssistant, Content: "ok"},
	})
	if runtimeErr != nil {
		t.Fatalf("SettleAgent failed on compacted prefix: %v", runtimeErr)
	}
	if settled.State != TurnCompleted {
		t.Fatalf("settled state=%v err=%v", settled.State, settled.Error)
	}
}
