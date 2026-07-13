package session

import "testing"

func TestStoreCreatesAndListsSession(t *testing.T) {
	store := Store{Root: t.TempDir()}
	item, err := store.New("fake", "/tmp")
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Append(item.ID, Message{Role: "user", Text: "hello"}); err != nil {
		t.Fatal(err)
	}
	items, err := store.List()
	if err != nil {
		t.Fatal(err)
	}
	if len(items) != 1 || items[0].ID != item.ID {
		t.Fatalf("unexpected sessions: %#v", items)
	}
}
