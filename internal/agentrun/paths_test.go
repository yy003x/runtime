package agentrun

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRunPathsRejectsUnsafeIDs(t *testing.T) {
	for _, id := range []string{"", ".", "..", "../escape", "a/b", `a\\b`, "/absolute", strings.Repeat("a", 129), "bad\x00id"} {
		t.Run(strings.ReplaceAll(id, "/", "_"), func(t *testing.T) {
			if _, err := RunPaths(t.TempDir(), RunTask, id); err == nil {
				t.Fatalf("RunPaths accepted %q", id)
			}
		})
	}
}

func TestLoopOperationsRejectUnsafeIDBeforeFilesystemAccess(t *testing.T) {
	root := t.TempDir()
	sentinel := filepath.Join(root, "runs", "victim", "status.json")
	if err := os.MkdirAll(filepath.Dir(sentinel), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(sentinel, []byte("keep"), 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	if _, err := service.LoopStart(LoopStartOptions{LoopID: "../../victim", Input: "x", Actions: []Action{{Type: "respond", Content: "x"}}, Force: true}); err == nil {
		t.Fatal("LoopStart accepted traversal ID")
	}
	if _, err := service.LoopStep(context.Background(), "../../victim"); err == nil {
		t.Fatal("LoopStep accepted traversal ID")
	}
	if _, err := service.LoopStatus("../../victim"); err == nil {
		t.Fatal("LoopStatus accepted traversal ID")
	}
	if _, err := service.LoopLogs("../../victim", 1); err == nil {
		t.Fatal("LoopLogs accepted traversal ID")
	}
	if _, err := service.LoopCancel("../../victim"); err == nil {
		t.Fatal("LoopCancel accepted traversal ID")
	}
	data, err := os.ReadFile(sentinel)
	if err != nil || string(data) != "keep" {
		t.Fatalf("sentinel changed: data=%q err=%v", data, err)
	}
}
