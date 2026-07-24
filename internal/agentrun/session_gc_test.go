package agentrun

import (
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestSessionGCOnlyTrashesEligibleEphemeralSessions(t *testing.T) {
	root := t.TempDir()
	store := NewSessionManager(New(root)).Store()
	create := func(id, retention, state string) {
		t.Helper()
		if _, err := store.Create(SessionRecord{
			SessionID: id, ProjectID: "project", State: state,
			RecordMode: RecordFull, Retention: retention, CaptureQuality: CaptureStructured,
		}); err != nil {
			t.Fatal(err)
		}
	}
	create("session-20260724-100000-ephemeral-one", RetentionEphemeral, SessionStateIdle)
	create("session-20260724-100001-ephemeral-two", RetentionEphemeral, SessionStateArchived)
	create("session-20260724-100002-active", RetentionEphemeral, SessionStateActive)
	create("session-20260724-100003-standard", RetentionStandard, SessionStateIdle)

	before := time.Now().UTC().Add(time.Hour)
	preview, err := store.GC(SessionGCOptions{Before: before, Limit: 1})
	if err != nil {
		t.Fatal(err)
	}
	if preview.Apply || preview.Candidates != 2 || preview.Processed != 0 ||
		len(preview.Items) != 1 || preview.Items[0].TrashPath != "" {
		t.Fatalf("preview=%#v", preview)
	}

	applied, err := store.GC(SessionGCOptions{Before: before, Limit: 1, Apply: true})
	if err != nil {
		t.Fatal(err)
	}
	if !applied.Apply || applied.Candidates != 2 || applied.Processed != 1 ||
		len(applied.Items) != 1 || applied.Items[0].TrashPath == "" {
		t.Fatalf("applied=%#v", applied)
	}
	if info, err := os.Stat(applied.Items[0].TrashPath); err != nil || !info.IsDir() {
		t.Fatalf("trash path=%q info=%v err=%v", applied.Items[0].TrashPath, info, err)
	}
	trashRoot := filepath.Join(root, "history", "trash")
	relative, err := filepath.Rel(trashRoot, applied.Items[0].TrashPath)
	if err != nil || relative == ".." || filepath.IsAbs(relative) {
		t.Fatalf("trash path escaped root: %q", applied.Items[0].TrashPath)
	}
	for _, id := range []string{"session-20260724-100002-active", "session-20260724-100003-standard"} {
		if _, err := store.Get(id); err != nil {
			t.Fatalf("ineligible session %s was removed: %v", id, err)
		}
	}
	remaining, err := store.List(SessionFilter{Retention: RetentionEphemeral})
	if err != nil {
		t.Fatal(err)
	}
	if len(remaining) != 2 {
		t.Fatalf("remaining ephemeral sessions=%#v", remaining)
	}
}

func TestSessionGCValidatesOptions(t *testing.T) {
	store := NewSessionManager(New(t.TempDir())).Store()
	if _, err := store.GC(SessionGCOptions{}); err == nil {
		t.Fatal("zero cutoff was accepted")
	}
	if _, err := store.GC(SessionGCOptions{Before: time.Now(), Limit: 1001}); err == nil {
		t.Fatal("oversized limit was accepted")
	}
}
