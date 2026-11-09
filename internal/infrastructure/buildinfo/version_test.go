package buildinfo

import (
	"strings"
	"testing"
)

func TestStringIncludesAvailableBuildMetadata(t *testing.T) {
	originalVersion, originalCommit, originalBuildDate, originalDirty, originalGoVersion := Version, Commit, BuildDate, Dirty, goVersion
	t.Cleanup(func() {
		Version, Commit, BuildDate, Dirty, goVersion = originalVersion, originalCommit, originalBuildDate, originalDirty, originalGoVersion
	})
	Version, Commit, BuildDate, Dirty, goVersion = "v1.2.3", "abcdef0", "2026-07-16", "true", "go1.test"
	value := String("sn-cli")
	for _, expected := range []string{"sn-cli v1.2.3", "commit abcdef0", "built 2026-07-16", "dirty", "go1.test"} {
		if !strings.Contains(value, expected) {
			t.Fatalf("String()=%q missing %q", value, expected)
		}
	}
	Commit, BuildDate, Dirty, goVersion = "", "", "false", ""
	if value := String("sn-cli"); value != "sn-cli v1.2.3" {
		t.Fatalf("String()=%q", value)
	}
	if value := String("sn-code"); value != "sn-code v1.2.3" {
		t.Fatalf("coding product version=%q", value)
	}
	if Summary() != "v1.2.3" {
		t.Fatalf("protocol version contains product identity: %q", Summary())
	}
}
