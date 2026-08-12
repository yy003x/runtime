// Package reporoot provides test-only repository discovery without relying on
// a package's depth in the source tree.
package reporoot

import (
	"os"
	"path/filepath"
	"testing"
)

// Root walks upward from the test process working directory until it finds
// the module's go.mod. Tests that consume source fixtures remain stable when a
// package moves between pkg and internal layers.
func Root(t testing.TB) string {
	t.Helper()

	directory, err := os.Getwd()
	if err != nil {
		t.Fatalf("resolve test working directory: %v", err)
	}
	directory, err = filepath.Abs(directory)
	if err != nil {
		t.Fatalf("resolve absolute test working directory: %v", err)
	}

	for {
		manifest := filepath.Join(directory, "go.mod")
		info, statErr := os.Stat(manifest)
		if statErr == nil && !info.IsDir() {
			return directory
		}
		if statErr != nil && !os.IsNotExist(statErr) {
			t.Fatalf("inspect module manifest %s: %v", manifest, statErr)
		}

		parent := filepath.Dir(directory)
		if parent == directory {
			t.Fatalf("find repository root from test working directory")
		}
		directory = parent
	}
}
