package codexauth

import (
	"context"
	"errors"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func fixtureCredential() Credential {
	return Credential{Type: "oauth", AccessToken: "fixture-access", RefreshToken: "fixture-refresh",
		AccountID: "fixture-account", ExpiresAt: time.Now().UTC().Add(time.Hour)}
}

func TestStoreReadOnlyAndPrivateAtomicUpdates(t *testing.T) {
	root := t.TempDir()
	store := Store{StateDir: filepath.Join(root, "state")}
	document, err := store.Read()
	if err != nil || len(document.Providers) != 0 {
		t.Fatalf("read: %v", err)
	}
	if _, err := os.Stat(store.StateDir); !os.IsNotExist(err) {
		t.Fatal("read created state")
	}
	const count = 12
	var workers sync.WaitGroup
	for i := 0; i < count; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			if err := store.Update(context.Background(), func(document *Document) (bool, error) {
				document.Providers[Provider] = fixtureCredential()
				return true, nil
			}); err != nil {
				t.Error(err)
			}
		}()
	}
	workers.Wait()
	document, err = store.Read()
	if err != nil || document.Revision != count || len(document.Providers) != 1 {
		t.Fatalf("revision=%d err=%v", document.Revision, err)
	}
	for name, mode := range map[string]os.FileMode{"": 0o700, "auth.json": 0o600, "auth.lock": 0o600} {
		info, err := os.Stat(filepath.Join(store.StateDir, name))
		if err != nil || info.Mode().Perm() != mode {
			t.Fatalf("mode %s: %v", name, err)
		}
	}
	entries, _ := os.ReadDir(store.StateDir)
	if len(entries) != 2 {
		t.Fatalf("temporary files left: %v", entries)
	}
	before, _ := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
	err = store.Update(context.Background(), func(document *Document) (bool, error) {
		delete(document.Providers, Provider)
		return true, errors.New("fixture failure")
	})
	after, _ := os.ReadFile(filepath.Join(store.StateDir, "auth.json"))
	if err == nil || string(before) != string(after) {
		t.Fatal("failed update changed credentials")
	}
}

func TestStoreRejectsUnsafeFilesWithoutExposingContents(t *testing.T) {
	for _, kind := range []string{"symlink", "hardlink", "directory", "permissions", "invalid", "unknown_field", "missing_revision", "state_symlink"} {
		t.Run(kind, func(t *testing.T) {
			root := t.TempDir()
			state := filepath.Join(root, "state")
			if err := os.Mkdir(state, 0o700); err != nil {
				t.Fatal(err)
			}
			target := filepath.Join(root, "other")
			if err := os.WriteFile(target, []byte("fixture-secret"), 0o600); err != nil {
				t.Fatal(err)
			}
			path := filepath.Join(state, "auth.json")
			switch kind {
			case "symlink":
				if err := os.Symlink(target, path); err != nil {
					t.Fatal(err)
				}
			case "hardlink":
				if err := os.Link(target, path); err != nil {
					t.Fatal(err)
				}
			case "directory":
				if err := os.Mkdir(path, 0o700); err != nil {
					t.Fatal(err)
				}
			case "permissions":
				if err := os.WriteFile(path, []byte("{}"), 0o644); err != nil {
					t.Fatal(err)
				}
				if err := os.Chmod(path, 0o644); err != nil {
					t.Fatal(err)
				}
			case "invalid":
				if err := os.WriteFile(path, []byte("fixture-secret"), 0o600); err != nil {
					t.Fatal(err)
				}
			case "unknown_field":
				if err := os.WriteFile(path, []byte(`{"fixture-secret":"value"}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "missing_revision":
				if err := os.WriteFile(path, []byte(`{"schema_version":1,"providers":{}}`), 0o600); err != nil {
					t.Fatal(err)
				}
			case "state_symlink":
				linked := filepath.Join(root, "linked")
				if err := os.Symlink(state, linked); err != nil {
					t.Fatal(err)
				}
				state = linked
			}
			store := Store{StateDir: state}
			if _, err := store.Read(); err == nil || strings.Contains(err.Error(), "fixture-secret") {
				t.Fatalf("read err=%v", err)
			}
			if err := store.Update(context.Background(), func(*Document) (bool, error) { t.Fatal("unsafe update callback"); return false, nil }); err == nil {
				t.Fatal("unsafe update succeeded")
			}
			data, _ := os.ReadFile(target)
			if string(data) != "fixture-secret" {
				t.Fatal("external file changed")
			}
		})
	}
}

func TestStoreSerializesSeparateProcesses(t *testing.T) {
	store := Store{StateDir: filepath.Join(t.TempDir(), "state")}
	var workers sync.WaitGroup
	for i := 0; i < 3; i++ {
		workers.Add(1)
		go func() {
			defer workers.Done()
			command := exec.Command(os.Args[0], "-test.run=^TestCredentialStoreProcessHelper$")
			command.Env = append(os.Environ(), "SN_AUTH_TEST_STATE="+store.StateDir)
			if data, err := command.CombinedOutput(); err != nil {
				t.Errorf("process failed: %v %s", err, data)
			}
		}()
	}
	workers.Wait()
	document, err := store.Read()
	if err != nil || document.Revision != 12 {
		t.Fatalf("process updates lost: revision=%d err=%v", document.Revision, err)
	}
}

func TestCredentialStoreProcessHelper(t *testing.T) {
	state := os.Getenv("SN_AUTH_TEST_STATE")
	if state == "" {
		return
	}
	store := Store{StateDir: state}
	for i := 0; i < 4; i++ {
		if err := store.Update(context.Background(), func(document *Document) (bool, error) {
			time.Sleep(5 * time.Millisecond)
			document.Providers[Provider] = fixtureCredential()
			return true, nil
		}); err != nil {
			t.Fatal(err)
		}
	}
}

func TestStoreLockIsCancellable(t *testing.T) {
	store := Store{StateDir: filepath.Join(t.TempDir(), "state")}
	entered, release, done := make(chan struct{}), make(chan struct{}), make(chan error, 1)
	go func() {
		done <- store.Update(context.Background(), func(*Document) (bool, error) { close(entered); <-release; return false, nil })
	}()
	<-entered
	ctx, cancel := context.WithTimeout(context.Background(), 50*time.Millisecond)
	defer cancel()
	err := store.Update(ctx, func(*Document) (bool, error) { t.Error("lock bypassed"); return false, nil })
	close(release)
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("err=%v", err)
	}
	if err := <-done; err != nil {
		t.Fatal(err)
	}
}
