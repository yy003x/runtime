package activation

import (
	"os"
	"path/filepath"
	"testing"
)

func TestEnsureCommandLinkCreatesAndPreservesExactLink(t *testing.T) {
	parent := canonicalTestDir(t)
	link := filepath.Join(parent, "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")

	if err := EnsureCommandLink(link, target); err != nil {
		t.Fatalf("create command link: %v", err)
	}
	if err := ValidateCommandLink(link, target); err != nil {
		t.Fatalf("validate command link: %v", err)
	}
	if err := EnsureCommandLink(link, target); err != nil {
		t.Fatalf("preserve command link: %v", err)
	}
	got, err := os.Readlink(link)
	if err != nil {
		t.Fatalf("read command link: %v", err)
	}
	if got != target {
		t.Fatalf("command link target = %q, want %q", got, target)
	}
}

func TestValidateCommandLinkDoesNotCreateMissingLink(t *testing.T) {
	link := filepath.Join(canonicalTestDir(t), "sn-cli")
	target := filepath.Join(t.TempDir(), "bin", "sn-cli")
	if err := ValidateCommandLink(link, target); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(link); !os.IsNotExist(err) {
		t.Fatalf("validation created command link: %v", err)
	}
}

func TestEnsureCommandLinkNeverReplacesExistingEntry(t *testing.T) {
	for _, test := range []struct {
		name  string
		setup func(string) error
	}{
		{
			name: "regular file",
			setup: func(path string) error {
				return os.WriteFile(path, []byte("keep"), 0o600)
			},
		},
		{
			name: "directory",
			setup: func(path string) error {
				return os.Mkdir(path, 0o700)
			},
		},
		{
			name: "different symlink",
			setup: func(path string) error {
				return os.Symlink("/different/runtime/bin/sn-cli", path)
			},
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			parent := canonicalTestDir(t)
			link := filepath.Join(parent, "sn-cli")
			if err := test.setup(link); err != nil {
				t.Fatal(err)
			}
			if err := EnsureCommandLink(
				link, filepath.Join(t.TempDir(), "bin", "sn-cli"),
			); err == nil {
				t.Fatal("expected existing entry rejection")
			}
		})
	}
}

func TestEnsureCommandLinkRejectsSymlinkParent(t *testing.T) {
	parent := canonicalTestDir(t)
	realParent := filepath.Join(parent, "real")
	if err := os.Mkdir(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	linkParent := filepath.Join(parent, "link")
	if err := os.Symlink(realParent, linkParent); err != nil {
		t.Fatal(err)
	}
	err := EnsureCommandLink(
		filepath.Join(linkParent, "sn-cli"),
		filepath.Join(t.TempDir(), "bin", "sn-cli"),
	)
	if err == nil {
		t.Fatal("expected symlink parent rejection")
	}
}

func TestEnsureCommandLinkRejectsSymlinkAncestor(t *testing.T) {
	parent := canonicalTestDir(t)
	realAncestor := filepath.Join(parent, "real")
	realParent := filepath.Join(realAncestor, "bin")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(parent, "alias")
	if err := os.Symlink(realAncestor, alias); err != nil {
		t.Fatal(err)
	}
	err := EnsureCommandLink(
		filepath.Join(alias, "bin", "sn-cli"),
		filepath.Join(t.TempDir(), "bin", "sn-cli"),
	)
	if err == nil {
		t.Fatal("expected symlink ancestor rejection")
	}
	if _, statErr := os.Lstat(
		filepath.Join(realParent, "sn-cli"),
	); !os.IsNotExist(statErr) {
		t.Fatalf("symlink ancestor was followed: %v", statErr)
	}
}

func canonicalTestDir(t *testing.T) string {
	t.Helper()
	value, err := filepath.EvalSymlinks(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	return filepath.Clean(value)
}
