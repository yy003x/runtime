package layout

import (
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
	if paths.Home != home || paths.ConfigDir != filepath.Join(home, "configs") ||
		paths.CommandDir != filepath.Join(home, "commands") ||
		paths.RuntimeConfigFile != filepath.Join(home, "runtime.json") ||
		paths.ResourcesDir != filepath.Join(home, "resources") ||
		paths.RunDBFile != filepath.Join(home, "state", "runtime.db") ||
		paths.ServerBinary != filepath.Join(home, "bin", "sn-server") ||
		paths.ServerPIDFile != filepath.Join(home, "state", "sn-server.pid") ||
		paths.ServerLogFile != filepath.Join(home, "state", "sn-server.log") ||
		paths.ServerLeaseFile != filepath.Join(home, "state", "sn-server.lease.lock") ||
		paths.ServerLockFile != filepath.Join(home, "state", "sn-server.lifecycle.lock") {
		t.Fatalf("unexpected paths: %#v", paths)
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
		paths.BinDir, paths.ConfigDir, paths.CommandDir, paths.ResourcesDir,
		paths.SchemaDir, paths.SessionsDir, paths.StateDir, paths.TmpDir,
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
