package codingsessions

import (
	"encoding/json"
	"testing"
	"time"

	"github.com/yy003x/sn-code/pkg/contract"
)

func userEntry(id, parent, text string) Entry {
	return Entry{
		Type:      EntryTypeMessage,
		ID:        id,
		ParentID:  parent,
		Timestamp: time.Unix(1000, 0).UTC(),
		Message:   &contract.Message{Role: contract.RoleUser, Content: text},
	}
}

func TestDecodeLineRoundTrip(t *testing.T) {
	entry := userEntry("e1", "", "hello")
	encoded, err := json.Marshal(entry)
	if err != nil {
		t.Fatalf("marshal entry: %v", err)
	}
	_, decoded, err := DecodeLine(encoded)
	if err != nil {
		t.Fatalf("decode entry: %v", err)
	}
	if decoded == nil || decoded.Message == nil || decoded.Message.Content != "hello" {
		t.Fatalf("unexpected decoded entry %+v", decoded)
	}
	header := Header{Type: "session", Version: Version, ID: "s1", Timestamp: time.Unix(0, 0).UTC(), CWD: "/tmp"}
	encoded, err = json.Marshal(header)
	if err != nil {
		t.Fatalf("marshal header: %v", err)
	}
	decodedHeader, decodedEntry, err := DecodeLine(encoded)
	if err != nil || decodedEntry != nil || decodedHeader == nil || decodedHeader.ID != "s1" {
		t.Fatalf("unexpected header decode: %+v %v %v", decodedHeader, decodedEntry, err)
	}
}

func TestDecodeLineSkipsUnknownType(t *testing.T) {
	line := []byte(`{"type":"future_entry","id":"x","parentId":"","timestamp":"2026-01-01T00:00:00Z","payload":{"a":1}}`)
	header, entry, err := DecodeLine(line)
	if err != nil || header != nil || entry != nil {
		t.Fatalf("unknown type must be skipped: %v %v %v", header, entry, err)
	}
}

func TestDecodeLineRejectsMissingPayload(t *testing.T) {
	line := []byte(`{"type":"message","id":"e1","parentId":"","timestamp":"2026-01-01T00:00:00Z"}`)
	if _, _, err := DecodeLine(line); err == nil {
		t.Fatal("message entry without payload must fail")
	}
}

func TestPathWalksParentChain(t *testing.T) {
	entries := []Entry{
		userEntry("e1", "", "first"),
		userEntry("e2", "e1", "second"),
		userEntry("b1", "e1", "branch"),
		userEntry("e3", "e2", "third"),
	}
	path, err := Path(entries, "b1")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if got := len(path); got != 2 {
		t.Fatalf("branch path length = %d, want 2", got)
	}
	if path[0].ID != "e1" || path[1].ID != "b1" {
		t.Fatalf("branch path = [%s %s], want [e1 b1]", path[0].ID, path[1].ID)
	}
	defaultPath, err := Path(entries, "")
	if err != nil {
		t.Fatalf("default path: %v", err)
	}
	if defaultPath[len(defaultPath)-1].ID != "e3" {
		t.Fatalf("default leaf = %s, want e3", defaultPath[len(defaultPath)-1].ID)
	}
}

func compactionEntry(id, parent, firstKept, summary string) Entry {
	return Entry{
		Type:       EntryTypeCompaction,
		ID:         id,
		ParentID:   parent,
		Timestamp:  time.Unix(1000, 0).UTC(),
		Compaction: &Compaction{Summary: summary, FirstKeptEntryID: firstKept, TokensBefore: 100},
	}
}

func TestContextEntriesAppliesLatestCompaction(t *testing.T) {
	entries := []Entry{
		userEntry("e1", "", "old1"),
		userEntry("e2", "e1", "old2"),
		userEntry("e3", "e2", "kept1"),
		compactionEntry("c1", "e3", "e3", "summary"),
		userEntry("e4", "c1", "new1"),
	}
	path, err := Path(entries, "")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	context := ContextEntries(path)
	if len(context) != 3 {
		t.Fatalf("context length = %d, want 3 (compaction, kept, after)", len(context))
	}
	if context[0].ID != "c1" || context[1].ID != "e3" || context[2].ID != "e4" {
		t.Fatalf("unexpected context ids: %v", SortedEntryIDs(context))
	}
	messages := ContextMessages(context)
	if len(messages) != 3 {
		t.Fatalf("messages length = %d, want 3", len(messages))
	}
	if messages[0].Role != contract.RoleUser || messages[0].Content != "The conversation history before this point was compacted into the following summary:\n<summary>\nsummary\n</summary>" {
		t.Fatalf("unexpected compaction message %+v", messages[0])
	}
	if messages[1].Content != "kept1" || messages[2].Content != "new1" {
		t.Fatalf("unexpected kept messages %+v", messages[1:])
	}
}

func TestContextEntriesWithoutCompactionKeepsAll(t *testing.T) {
	entries := []Entry{
		userEntry("e1", "", "first"),
		userEntry("e2", "e1", "second"),
	}
	path, err := Path(entries, "")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	context := ContextEntries(path)
	if len(context) != 2 {
		t.Fatalf("context length = %d, want 2", len(context))
	}
}

func TestCurrentProfileAndSessionName(t *testing.T) {
	entries := []Entry{
		userEntry("e1", "", "first"),
		{
			Type: EntryTypeModelChange, ID: "e2", ParentID: "e1", Timestamp: time.Unix(1000, 0).UTC(),
			ModelChange: &ModelChange{Profile: "cc"},
		},
		{
			Type: EntryTypeModelChange, ID: "e3", ParentID: "e2", Timestamp: time.Unix(1000, 0).UTC(),
			ModelChange: &ModelChange{Profile: "cx-core"},
		},
		{
			Type: EntryTypeSessionInfo, ID: "e4", ParentID: "e3", Timestamp: time.Unix(1000, 0).UTC(),
			SessionInfo: &SessionInfo{Name: "my session"},
		},
	}
	path, err := Path(entries, "")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if profile := CurrentProfile(path); profile != "cx-core" {
		t.Fatalf("current profile = %q, want cx-core", profile)
	}
	if name := SessionName(path); name != "my session" {
		t.Fatalf("session name = %q, want my session", name)
	}
}
