package toolbuiltin

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"

	"github.com/yy003x/runtime/pkg/agent"
)

func TestGrepFindsLiteralMatches(t *testing.T) {
	root := t.TempDir()
	if err := os.WriteFile(filepath.Join(root, "a.go"), []byte("package main\nfunc Hello() {}\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "b.txt"), []byte("nope\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := Build(Options{Names: []string{"grep"}, Roots: []string{root}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	result, err := registry.Execute(context.Background(), agent.ToolRequest{
		Name:      "grep",
		CallID:    "call_grep",
		Arguments: json.RawMessage(`{"pattern":"Hello","glob":"*.go"}`),
	})
	if err != nil || result.IsError {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if result.Content == "" || !json.Valid([]byte(result.Content)) {
		t.Fatalf("content=%q", result.Content)
	}
}

func TestEditReplacesExactlyOneOccurrence(t *testing.T) {
	root := t.TempDir()
	path := filepath.Join(root, "note.txt")
	if err := os.WriteFile(path, []byte("alpha beta alpha\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	registry, err := Build(Options{Names: []string{"edit"}, Roots: []string{root}, CWD: root})
	if err != nil {
		t.Fatal(err)
	}
	_, err = registry.Execute(context.Background(), agent.ToolRequest{
		Name:      "edit",
		CallID:    "call_edit",
		Arguments: json.RawMessage(`{"path":"note.txt","old_string":"alpha beta alpha","new_string":"gamma"}`),
	})
	if err != nil {
		t.Fatal(err)
	}
	data, err := os.ReadFile(path)
	if err != nil || string(data) != "gamma\n" {
		t.Fatalf("file=%q err=%v", data, err)
	}
	_, err = registry.Execute(context.Background(), agent.ToolRequest{
		Name:      "edit",
		CallID:    "call_edit2",
		Arguments: json.RawMessage(`{"path":"note.txt","old_string":"missing","new_string":"x"}`),
	})
	if err == nil {
		t.Fatal("missing old_string was accepted")
	}
}
