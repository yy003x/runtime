package version

import (
	"strings"
	"testing"
)

func TestStringIncludesAvailableBuildMetadata(t *testing.T) {
	originalVersion, originalCommit, originalBuildDate, originalGoVersion := Version, Commit, BuildDate, goVersion
	t.Cleanup(func() {
		Version, Commit, BuildDate, goVersion = originalVersion, originalCommit, originalBuildDate, originalGoVersion
	})
	Version, Commit, BuildDate, goVersion = "1.2.3", "abcdef0", "2026-07-16", "go1.test"
	value := String()
	for _, expected := range []string{"1.2.3", "commit abcdef0", "built 2026-07-16", "go1.test"} {
		if !strings.Contains(value, expected) {
			t.Fatalf("String()=%q missing %q", value, expected)
		}
	}
	Commit, BuildDate, goVersion = "", "", ""
	if value := String(); value != "1.2.3" {
		t.Fatalf("String()=%q", value)
	}
}
