package layout

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestResolveUsesSNCLIHome(t *testing.T) {
	home := filepath.Join(t.TempDir(), "custom")
	t.Setenv(HomeEnv, home)
	paths, err := Resolve()
	if err != nil {
		t.Fatal(err)
	}
	home, err = CanonicalHome(home)
	if err != nil {
		t.Fatal(err)
	}
	if paths.Home != home || paths.ConfigDir != filepath.Join(home, "configs") ||
		paths.ToolsDir != filepath.Join(home, "tools") ||
		paths.RuntimeConfigFile != filepath.Join(home, "runtime.json") ||
		paths.ResourcesDir != filepath.Join(home, "resources") ||
		paths.RunDBFile != filepath.Join(home, "state", "runtime.db") ||
		paths.ServerBinary != filepath.Join(home, "bin", "sn-server") ||
		paths.ServerPIDFile != filepath.Join(home, "state", "sn-server.pid") ||
		paths.ServerLogFile != filepath.Join(home, "logs", "sn-server.log") ||
		paths.ServerLeaseFile != filepath.Join(home, "state", "sn-server.lease.lock") ||
		paths.ServerLockFile != filepath.Join(home, "state", "sn-server.lifecycle.lock") ||
		paths.TmuxLockFile != filepath.Join(home, "state", "tmux.lock") ||
		paths.TmuxManifestDir != filepath.Join(home, "tmp", "tmux") ||
		paths.TmuxConfigFile != filepath.Join(home, "resources", "tmux.conf") {
		t.Fatalf("unexpected paths: %#v", paths)
	}
	homeDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(home)))
	if filepath.Dir(paths.TmuxSocketFile) != paths.TmuxSocketDir ||
		paths.TmuxSocketFile != filepath.Join(
			paths.TmuxSocketDir, homeDigest[:16]+".sock",
		) {
		t.Fatalf("unexpected tmux socket paths: %#v", paths)
	}
	if paths.SchemaDir != filepath.Join(home, "resources", "schema") {
		t.Fatalf("unexpected resource paths: %#v", paths)
	}
}

func TestEnsureCreatesPrivateDirectoryTree(t *testing.T) {
	paths, err := FromHome(filepath.Join(t.TempDir(), ".sn"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	for _, dir := range []string{
		paths.BinDir, paths.ConfigDir, paths.ToolsDir, paths.ResourcesDir,
		paths.SchemaDir, paths.SessionsDir, paths.LogsDir, paths.StateDir, paths.TmpDir,
		paths.TmuxManifestDir,
	} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("directory %s is not private: %o", dir, info.Mode().Perm())
		}
	}
}

func TestCanonicalHomeUnifiesParentAliasesAndRejectsHomeSymlink(
	t *testing.T,
) {
	root := t.TempDir()
	realParent := filepath.Join(root, "real")
	if err := os.MkdirAll(realParent, 0o700); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(root, "alias")
	if err := os.Symlink(realParent, alias); err != nil {
		t.Fatal(err)
	}
	throughAlias := filepath.Join(alias, "runtime")
	canonical, err := CanonicalHome(throughAlias)
	if err != nil {
		t.Fatal(err)
	}
	canonicalRealParent, err := filepath.EvalSymlinks(realParent)
	if err != nil {
		t.Fatal(err)
	}
	if canonical != filepath.Join(canonicalRealParent, "runtime") {
		t.Fatalf("canonical home=%s", canonical)
	}
	aliasPaths, err := FromHome(throughAlias)
	if err != nil {
		t.Fatal(err)
	}
	realPaths, err := FromHome(filepath.Join(realParent, "runtime"))
	if err != nil {
		t.Fatal(err)
	}
	if aliasPaths.Home != realPaths.Home ||
		aliasPaths.TmuxSocketFile != realPaths.TmuxSocketFile {
		t.Fatalf(
			"alias paths diverged: %#v %#v", aliasPaths, realPaths,
		)
	}
	if err := os.MkdirAll(realPaths.Home, 0o700); err != nil {
		t.Fatal(err)
	}
	directAlias := filepath.Join(root, "runtime-link")
	if err := os.Symlink(realPaths.Home, directAlias); err != nil {
		t.Fatal(err)
	}
	if _, err := CanonicalHome(directAlias); err == nil {
		t.Fatal("direct Runtime home symlink was accepted")
	}
}
