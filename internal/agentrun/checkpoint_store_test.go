package agentrun

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestNormalizeExportedCheckpointMigratesLegacyDigestOnce(t *testing.T) {
	sessionDir := t.TempDir()
	checkpoint := ContextCheckpoint{
		SchemaVersion: SessionSchemaVersion,
		SessionID:     "session-legacy", TurnID: "turn-legacy",
		CreatedAt: time.Now().UTC(), Summary: "legacy",
	}
	reference, err := checkpointRelativeRef(checkpoint.TurnID)
	if err != nil {
		t.Fatal(err)
	}
	path := filepath.Join(sessionDir, filepath.FromSlash(reference))
	if err := writeJSONAtomic(path, checkpoint); err != nil {
		t.Fatal(err)
	}
	legacyDigest, err := digestFile(path)
	if err != nil {
		t.Fatal(err)
	}
	manifest := ContextManifest{
		SessionID: checkpoint.SessionID, TurnID: checkpoint.TurnID,
		SummaryRef: path, SummaryDigest: legacyDigest, Compacted: true,
	}
	normalized, exported := normalizeExportedCheckpoint(sessionDir, manifest)
	if exported == nil || normalized.SummaryRef != reference ||
		normalized.SummaryDigest != digestContextCheckpoint(checkpoint) ||
		normalized.SummaryDigestKind != checkpointDigestStableJSON ||
		normalized.LegacySummaryDigest != legacyDigest {
		t.Fatalf("normalized=%#v exported=%#v", normalized, exported)
	}
	again, exportedAgain := normalizeExportedCheckpoint(sessionDir, normalized)
	if exportedAgain == nil || again.SummaryDigest != normalized.SummaryDigest ||
		again.LegacySummaryDigest != normalized.LegacySummaryDigest {
		t.Fatalf("second normalization drifted: %#v", again)
	}
}

func TestNormalizeExportedCheckpointRejectsOutsideReference(t *testing.T) {
	sessionDir := t.TempDir()
	outside := filepath.Join(t.TempDir(), "turn-outside.json")
	if err := os.WriteFile(outside, []byte("{}"), 0o600); err != nil {
		t.Fatal(err)
	}
	manifest := ContextManifest{
		SessionID: "session-outside", TurnID: "turn-outside",
		SummaryRef: outside, SummaryDigest: "invalid", Compacted: true,
	}
	normalized, checkpoint := normalizeExportedCheckpoint(sessionDir, manifest)
	if checkpoint != nil || normalized.CheckpointError != "checkpoint_ref_outside_session" ||
		normalized.SummaryRef != "" || normalized.Compacted {
		t.Fatalf("normalized=%#v checkpoint=%#v", normalized, checkpoint)
	}
}
