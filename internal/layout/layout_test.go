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
	if paths.Home != home || paths.ConfigDir != filepath.Join(home, "configs") || paths.RunsDir != filepath.Join(home, "runs") {
		t.Fatalf("unexpected paths: %#v", paths)
	}
	if paths.MemoryFile != filepath.Join(home, "memory", "durable.json") || paths.MemoryCandidatesFile != filepath.Join(home, "memory", "candidates.json") || paths.DaemonLog != filepath.Join(home, "logs", "daemon.log") {
		t.Fatalf("unexpected state paths: %#v", paths)
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
	for _, dir := range []string{paths.ConfigDir, paths.RunsDir, paths.SessionsDir, paths.HistoryDir, paths.MemoryDir, paths.DaemonDir, paths.StateDir, paths.SessionStateDir, paths.LogsDir, paths.CacheDir, paths.TmpDir} {
		info, err := os.Stat(dir)
		if err != nil {
			t.Fatal(err)
		}
		if info.Mode().Perm()&0o077 != 0 {
			t.Fatalf("directory %s is not private: %o", dir, info.Mode().Perm())
		}
	}
}
