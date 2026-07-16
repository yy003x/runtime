package native

import (
	"strings"
	"testing"
)

func TestContextManagerPreservesLatestMessageAndRejectsOversizedPinnedContext(t *testing.T) {
	manager := ContextManager{}
	messages, err := manager.BuildPrompt(Context{
		SystemInstructions: []Message{{Role: "system", Content: "system", Pinned: true}},
		Messages: []Message{
			{Role: "user", Content: strings.Repeat("old", 40)},
			{Role: "user", Content: "latest"},
		},
	}, 20)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 2 || messages[1].Content != "latest" {
		t.Fatalf("messages=%#v", messages)
	}
	if _, err := manager.BuildPrompt(Context{SystemInstructions: []Message{{Role: "system", Content: strings.Repeat("x", 100), Pinned: true}}}, 10); err == nil {
		t.Fatal("oversized pinned context was accepted")
	}
	if _, err := manager.BuildPrompt(Context{Messages: []Message{{Role: "user", Content: strings.Repeat("x", 100)}}}, 10); err == nil {
		t.Fatal("oversized latest message was silently dropped")
	}
}
