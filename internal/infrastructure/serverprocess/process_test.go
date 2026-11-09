//go:build darwin || linux

package serverprocess

import (
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/yy003x/sn-code/internal/infrastructure/layout"
)

func TestServerRunningRequiresMatchingLeaseAndProcessIdentity(t *testing.T) {
	paths, err := layout.FromHome(filepath.Join(t.TempDir(), ".sn"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	identity, err := processIdentityForPID(os.Getpid())
	if err != nil {
		t.Fatal(err)
	}
	record := PIDRecord{
		SchemaVersion:     serverPIDSchemaVersion,
		PID:               os.Getpid(),
		Binary:            paths.ServerBinary,
		ProcessStart:      identity.StartToken,
		ProcessExecutable: identity.Executable,
		StartedAt:         time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeServerPID(paths.ServerPIDFile, record); err != nil {
		t.Fatal(err)
	}

	running, _, err := Running(paths)
	if err == nil || running {
		t.Fatalf("live pid without a held lease must fail closed: running=%v err=%v", running, err)
	}

	record.ProcessStart += "-stale"
	if err := writeServerPID(paths.ServerPIDFile, record); err != nil {
		t.Fatal(err)
	}
	running, _, err = Running(paths)
	if err != nil || running {
		t.Fatalf("stale pid identity must not report running: running=%v err=%v", running, err)
	}
	record.ProcessStart = identity.StartToken
	if err := writeServerPID(paths.ServerPIDFile, record); err != nil {
		t.Fatal(err)
	}

	lease, acquired, err := tryAcquireFileLock(paths.ServerLeaseFile)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("expected to acquire server lease")
	}
	defer releaseFileLock(lease)

	running, pid, err := Running(paths)
	if err != nil {
		t.Fatal(err)
	}
	if !running || pid != os.Getpid() {
		t.Fatalf("running=%v pid=%d", running, pid)
	}

	record.ProcessStart += "-stale"
	if err := writeServerPID(paths.ServerPIDFile, record); err != nil {
		t.Fatal(err)
	}
	if running, _, err := Running(paths); err == nil || running {
		t.Fatalf("identity mismatch must fail closed: running=%v err=%v", running, err)
	}
}

func TestServerRunningRejectsUnsupportedPIDRecord(t *testing.T) {
	paths, err := layout.FromHome(filepath.Join(t.TempDir(), ".sn"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.ServerPIDFile, []byte("12345\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	if running, _, err := Running(paths); err == nil || running {
		t.Fatalf("unsupported pid record must fail closed: running=%v err=%v", running, err)
	}
}

func TestServerLifecycleLockRejectsConcurrentOwner(t *testing.T) {
	path := filepath.Join(t.TempDir(), "lifecycle.lock")
	first, acquired, err := tryAcquireFileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if !acquired {
		t.Fatal("expected first lifecycle lock")
	}
	defer releaseFileLock(first)

	second, acquired, err := tryAcquireFileLock(path)
	if err != nil {
		t.Fatal(err)
	}
	if acquired {
		releaseFileLock(second)
		t.Fatal("concurrent lifecycle lock unexpectedly succeeded")
	}
}

func TestServerLockRejectsSymlink(t *testing.T) {
	directory := t.TempDir()
	target := filepath.Join(directory, "target")
	if err := os.WriteFile(target, []byte("lock"), 0o600); err != nil {
		t.Fatal(err)
	}
	link := filepath.Join(directory, "lock")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if file, acquired, err := tryAcquireFileLock(link); err == nil {
		if acquired {
			releaseFileLock(file)
		}
		t.Fatal("symlink lock path must be rejected")
	}
}
