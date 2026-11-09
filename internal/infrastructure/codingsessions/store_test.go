package codingsessions

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/sn-code/pkg/contract"
)

func TestCreateAppendReopen(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	created, err := store.Create(time.Unix(1700000000, 0).UTC(), "/tmp/project", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if _, err := created.AppendMessage(contract.Message{Role: contract.RoleUser, Content: "hello"}); err != nil {
		t.Fatalf("append message: %v", err)
	}
	if _, err := created.AppendModelChange("cc"); err != nil {
		t.Fatalf("append model change: %v", err)
	}
	if _, err := created.AppendSessionInfo("my session"); err != nil {
		t.Fatalf("append session info: %v", err)
	}
	if err := created.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	expectedPath := filepath.Join(root, CWDSlug("/tmp/project"))
	matches, err := filepath.Glob(filepath.Join(expectedPath, "*.jsonl"))
	if err != nil || len(matches) != 1 {
		t.Fatalf("session file not created once: %v %v", matches, err)
	}
	reopened, err := store.OpenStoreFile(matches[0])
	if err != nil {
		t.Fatalf("reopen: %v", err)
	}
	defer reopened.Close()
	if reopened.Header.ID != created.Header.ID {
		t.Fatalf("header id mismatch: %s != %s", reopened.Header.ID, created.Header.ID)
	}
	entries := reopened.Entries()
	if len(entries) != 3 {
		t.Fatalf("entry count = %d, want 3", len(entries))
	}
	if entries[0].ParentID != "" || entries[1].ParentID != entries[0].ID || entries[2].ParentID != entries[1].ID {
		t.Fatalf("parent chain broken: %+v", entries)
	}
	path, err := reopened.Path("")
	if err != nil {
		t.Fatalf("path: %v", err)
	}
	if got := len(path); got != 3 {
		t.Fatalf("path length = %d, want 3", got)
	}

	// 重新打开后仍可追加。
	if _, err := reopened.AppendMessage(contract.Message{Role: contract.RoleAssistant, Content: "hi"}); err != nil {
		t.Fatalf("append after reopen: %v", err)
	}
	if err := reopened.Close(); err != nil {
		t.Fatalf("close after reopen: %v", err)
	}
}

func TestListOrdersByTimeAndShowsName(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	older, err := store.Create(time.Unix(1700000000, 0).UTC(), "/tmp/project", "")
	if err != nil {
		t.Fatalf("create older: %v", err)
	}
	if _, err := older.AppendMessage(contract.Message{Role: contract.RoleUser, Content: "first prompt preview"}); err != nil {
		t.Fatalf("append: %v", err)
	}
	if err := older.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	newer, err := store.Create(time.Unix(1800000000, 0).UTC(), "/tmp/project", "")
	if err != nil {
		t.Fatalf("create newer: %v", err)
	}
	if _, err := newer.AppendSessionInfo("named session"); err != nil {
		t.Fatalf("append name: %v", err)
	}
	if err := newer.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}

	summaries, err := store.List("/tmp/project")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(summaries) != 2 {
		t.Fatalf("summary count = %d, want 2", len(summaries))
	}
	if summaries[0].ID != newer.Header.ID {
		t.Fatalf("newest session should be first, got %s", summaries[0].ID)
	}
	if summaries[0].Name != "named session" {
		t.Fatalf("named session name = %q", summaries[0].Name)
	}
	if summaries[1].Name != "first prompt preview" {
		t.Fatalf("preview fallback name = %q", summaries[1].Name)
	}
}

func TestListIgnoresUnrelatedDirectories(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	other, err := store.Create(time.Unix(1700000000, 0).UTC(), "/tmp/other", "")
	if err != nil {
		t.Fatalf("create: %v", err)
	}
	if err := other.Close(); err != nil {
		t.Fatalf("close: %v", err)
	}
	summaries, err := store.List("/tmp/project")
	if err != nil {
		t.Fatalf("list: %v", err)
	}
	if len(summaries) != 0 {
		t.Fatalf("unrelated sessions leaked: %+v", summaries)
	}
}

func TestCWDSlugIsCollisionFreeAndListValidatesHeader(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	directories := []string{"/a/b", "/a-b", "/", "/root"}
	for _, cwd := range directories {
		created, err := store.Create(time.Unix(1700000000, 0).UTC(), cwd, "")
		if err != nil {
			t.Fatalf("create %q: %v", cwd, err)
		}
		if err := created.Close(); err != nil {
			t.Fatal(err)
		}
	}
	seen := map[string]string{}
	for _, cwd := range directories {
		slug := CWDSlug(cwd)
		if previous, exists := seen[slug]; exists {
			t.Fatalf("%q and %q share slug %q", previous, cwd, slug)
		}
		seen[slug] = cwd
		summaries, err := store.List(cwd)
		if err != nil || len(summaries) != 1 || summaries[0].CWD != cwd {
			t.Fatalf("List(%q) = %+v, err=%v", cwd, summaries, err)
		}
	}
}

func TestCWDSlugLongPaths(t *testing.T) {
	if got := CWDSlug("/a/b"); got != "2f612f62" {
		t.Fatalf("short path encoding changed: %q", got)
	}
	if got := CWDSlug("/" + strings.Repeat("a", 126)); len(got) != 254 {
		t.Fatalf("127-byte cwd slug length = %d", len(got))
	}
	if got := CWDSlug("/" + strings.Repeat("a", 127)); !strings.HasPrefix(got, "sha256-") || len(got) != 71 {
		t.Fatalf("128-byte cwd slug = %q", got)
	}
	cwd := filepath.Join(t.TempDir(), strings.Repeat("a", 70), strings.Repeat("b", 70))
	if err := os.MkdirAll(cwd, 0o700); err != nil {
		t.Fatal(err)
	}
	store := NewStore(t.TempDir())
	created, err := store.Create(time.Unix(1700000000, 0), cwd, "")
	if err != nil {
		t.Fatalf("create long cwd: %v", err)
	}
	path := created.FilePath()
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	summaries, err := store.List(cwd)
	if err != nil || len(summaries) != 1 || summaries[0].Path != path || summaries[0].CWD != cwd {
		t.Fatalf("List(long cwd) = %+v, err=%v", summaries, err)
	}
	reopened, err := store.OpenStoreFile(path)
	if err != nil {
		t.Fatalf("reopen long cwd: %v", err)
	}
	defer reopened.Close()
	if reopened.Header.CWD != cwd || reopened.Header.ID != created.Header.ID {
		t.Fatalf("reopened header = %+v", reopened.Header)
	}
}

func TestOpenStoreFileRejectsWrongCWDDirectory(t *testing.T) {
	root := t.TempDir()
	store := NewStore(root)
	created, err := store.Create(time.Unix(1700000000, 0).UTC(), "/a/b", "")
	if err != nil {
		t.Fatal(err)
	}
	path := created.FilePath()
	if err := created.Close(); err != nil {
		t.Fatal(err)
	}
	wrongDir := filepath.Join(root, CWDSlug("/a-b"))
	if err := os.MkdirAll(wrongDir, 0o700); err != nil {
		t.Fatal(err)
	}
	wrongPath := filepath.Join(wrongDir, filepath.Base(path))
	raw, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(wrongPath, raw, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := store.OpenStoreFile(wrongPath); err == nil {
		t.Fatal("opened a session from the wrong cwd directory")
	}
}

func TestPreferencesRoundTrip(t *testing.T) {
	path := filepath.Join(t.TempDir(), "state", "preferences.json")
	loaded, err := LoadPreferences(path)
	if err != nil {
		t.Fatalf("load missing preferences: %v", err)
	}
	if loaded.LastProfile != "" {
		t.Fatalf("missing preferences should be zero value")
	}
	if err := SavePreferences(path, Preferences{LastProfile: "cc"}); err != nil {
		t.Fatalf("save: %v", err)
	}
	if _, err := os.Stat(path); err != nil {
		t.Fatalf("preferences file missing: %v", err)
	}
	loaded, err = LoadPreferences(path)
	if err != nil {
		t.Fatalf("load: %v", err)
	}
	if loaded.LastProfile != "cc" {
		t.Fatalf("last profile = %q, want cc", loaded.LastProfile)
	}
}
