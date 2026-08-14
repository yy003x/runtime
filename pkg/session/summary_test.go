package session

import (
	"testing"
	"time"
)

func summaryTestRecord(sessionID string) SummaryRecord {
	return SummaryRecord{
		SummaryVersion:        SummaryRecordVersion,
		SummaryID:             "summary_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa",
		SessionID:             sessionID,
		TurnID:                mutationTestTurnID,
		RunID:                 mutationTestRunID,
		RangeStartSeq:         1,
		RangeEndSeq:           4,
		CompactedRangeDigest:  "sha256:range",
		PolicyKeepRecentTurns: 2,
		CreatedAt:             time.Date(2026, 8, 14, 0, 0, 0, 0, time.UTC),
	}
}

// summaries.jsonl is optional infrastructure in this increment (nothing writes
// it yet outside tests). These tests lock in the append/read/lookup/validation
// contract that B1 will rely on.

func TestSummaryAppendRoundTripAndLookup(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)

	if records, err := store.summaries(mutationTestSessionID); err != nil {
		t.Fatalf("summaries on absent file: %v", err)
	} else if len(records) != 0 {
		t.Fatalf("expected no summaries, got %d", len(records))
	}

	record := summaryTestRecord(mutationTestSessionID)
	if err := store.withLock(mutationTestSessionID, func() error {
		return store.appendSummary(mutationTestSessionID, record)
	}); err != nil {
		t.Fatalf("appendSummary: %v", err)
	}

	records, err := store.summaries(mutationTestSessionID)
	if err != nil {
		t.Fatalf("summaries read: %v", err)
	}
	if len(records) != 1 {
		t.Fatalf("expected 1 summary, got %d", len(records))
	}
	got := records[0]
	if got.SummaryID != record.SummaryID ||
		got.RangeStartSeq != 1 || got.RangeEndSeq != 4 ||
		got.CompactedRangeDigest != record.CompactedRangeDigest {
		t.Fatalf("round-trip lost fields: %#v", got)
	}

	found, err := store.summaryByID(mutationTestSessionID, record.SummaryID)
	if err != nil || found == nil || found.SummaryID != record.SummaryID {
		t.Fatalf("summaryByID hit: %#v err=%v", found, err)
	}
	if _, err := store.summaryByID(
		mutationTestSessionID, "summary_does_not_exist",
	); err == nil {
		t.Fatal("summaryByID missing should error")
	}
}

func TestSummaryLatestByIDWins(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)

	first := summaryTestRecord(mutationTestSessionID)
	second := first
	second.RangeEndSeq = 8
	second.CompactedRangeDigest = "sha256:range-v2"
	for _, record := range []SummaryRecord{first, second} {
		if err := store.withLock(mutationTestSessionID, func() error {
			return store.appendSummary(mutationTestSessionID, record)
		}); err != nil {
			t.Fatalf("appendSummary: %v", err)
		}
	}
	found, err := store.summaryByID(mutationTestSessionID, first.SummaryID)
	if err != nil {
		t.Fatalf("summaryByID: %v", err)
	}
	if found.RangeEndSeq != 8 || found.CompactedRangeDigest != "sha256:range-v2" {
		t.Fatalf("expected latest summary to win: %#v", found)
	}
}

func TestAppendSummaryRejectsInvalidRecord(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)

	bad := summaryTestRecord(mutationTestSessionID)
	bad.SummaryVersion = 5
	err := store.withLock(mutationTestSessionID, func() error {
		return store.appendSummary(mutationTestSessionID, bad)
	})
	if err == nil {
		t.Fatal("appendSummary should reject invalid record")
	}
	if records, _ := store.summaries(mutationTestSessionID); len(records) != 0 {
		t.Fatalf("invalid summary was persisted: %#v", records)
	}
	if err := store.validateSessionRootLayout(mutationTestSessionID); err != nil {
		t.Fatalf("root layout after rejected append: %v", err)
	}
}

func TestSummaryAllowedByLayoutAndValidated(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)

	if err := store.withLock(mutationTestSessionID, func() error {
		return store.appendSummary(mutationTestSessionID, summaryTestRecord(mutationTestSessionID))
	}); err != nil {
		t.Fatalf("appendSummary: %v", err)
	}
	if err := store.validateSessionRootLayout(mutationTestSessionID); err != nil {
		t.Fatalf("summaries.jsonl not allowed by root layout: %v", err)
	}
	if err := store.validateSummaryFacts(mutationTestSessionID); err != nil {
		t.Fatalf("validateSummaryFacts: %v", err)
	}
}

func TestValidateSummaryRecordRejectsBadRecords(t *testing.T) {
	good := summaryTestRecord(mutationTestSessionID)
	cases := []struct {
		name   string
		mutate func(SummaryRecord) SummaryRecord
	}{
		{"wrong version", func(r SummaryRecord) SummaryRecord { r.SummaryVersion = 99; return r }},
		{"empty summary id", func(r SummaryRecord) SummaryRecord { r.SummaryID = ""; return r }},
		{"empty session id", func(r SummaryRecord) SummaryRecord { r.SessionID = ""; return r }},
		{"zero range end", func(r SummaryRecord) SummaryRecord { r.RangeEndSeq = 0; return r }},
		{"inverted range", func(r SummaryRecord) SummaryRecord { r.RangeStartSeq = 10; r.RangeEndSeq = 5; return r }},
		{"missing compacted digest", func(r SummaryRecord) SummaryRecord { r.CompactedRangeDigest = ""; return r }},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			if err := validateSummaryRecord(tc.mutate(good)); err == nil {
				t.Fatal("expected validation error")
			}
		})
	}
	if err := validateSummaryRecord(good); err != nil {
		t.Fatalf("good record rejected: %v", err)
	}
}
