package activationgate

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRequireOpen(t *testing.T) {
	t.Run("open", func(t *testing.T) {
		if err := RequireOpen(t.TempDir()); err != nil {
			t.Fatal(err)
		}
	})

	for _, name := range []string{guardFileName, journalFileName} {
		t.Run(name, func(t *testing.T) {
			stateDir := t.TempDir()
			if err := os.WriteFile(
				filepath.Join(stateDir, name), []byte("{}\n"), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			err := RequireOpen(stateDir)
			if err == nil || !strings.Contains(err.Error(), "undergoing activation") {
				t.Fatalf("error=%v", err)
			}
		})
	}

	t.Run("invalid_symlink", func(t *testing.T) {
		stateDir := t.TempDir()
		target := filepath.Join(stateDir, "target")
		if err := os.WriteFile(target, []byte("{}\n"), 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(target, filepath.Join(stateDir, guardFileName)); err != nil {
			t.Fatal(err)
		}
		err := RequireOpen(stateDir)
		if err == nil || !strings.Contains(err.Error(), "barrier is invalid") {
			t.Fatalf("error=%v", err)
		}
	})
}
