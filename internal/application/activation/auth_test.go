package activation

import (
	"os"
	"path/filepath"
	"testing"

	"github.com/yy003x/runtime/internal/infrastructure/layout"
)

func TestRuntimeStateResetPreservesAuthFiles(t *testing.T) {
	paths, err := layout.FromHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"auth.json", "auth.lock"} {
		if err := os.WriteFile(filepath.Join(paths.StateDir, name), []byte("fixture-private-credential"), 0o600); err != nil {
			t.Fatal(err)
		}
	}
	if err := resetRuntimeState(paths.Home); err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"auth.json", "auth.lock"} {
		data, err := os.ReadFile(filepath.Join(paths.StateDir, name))
		if err != nil || string(data) != "fixture-private-credential" {
			t.Fatalf("credential changed during state reset: %s %v", name, err)
		}
	}
}
