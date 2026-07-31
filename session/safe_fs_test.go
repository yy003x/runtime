package session

import (
	"bytes"
	"errors"
	"os"
	"path/filepath"
	"testing"
	"time"

	runtimecommand "github.com/yy003x/runtime/command"
)

func TestSessionMutationRejectsTargetSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)

	outside := filepath.Join(root, "outside-events.jsonl")
	original := []byte("outside sentinel\n")
	if err := os.WriteFile(outside, original, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(
		store.sessionDir(mutationTestSessionID), "events.jsonl",
	)
	swapped := false
	store.mutationFailpoint = func(stage, relativePath string) {
		if swapped ||
			stage != "before_target_open" ||
			relativePath != "events.jsonl" {
			return
		}
		swapped = true
		if err := os.Symlink(outside, target); err != nil {
			t.Fatalf("install target symlink: %v", err)
		}
	}
	err := store.withLock(mutationTestSessionID, func() error {
		value, err := store.loadSession(mutationTestSessionID)
		if err != nil {
			return err
		}
		return store.appendEvent(&value, mutationTestEvent())
	})
	if err == nil || !swapped {
		t.Fatalf("target symlink swap error=%v swapped=%t", err, swapped)
	}
	assertFileBytes(t, outside, original)

	store.mutationFailpoint = nil
	if err := os.Remove(target); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(store.sessionsDir, store.stateDir); err != nil {
		t.Fatalf("recover after restoring target path: %v", err)
	}
	assertFileBytes(t, outside, original)
}

func TestSessionMutationRejectsTurnAncestorSymlinkSwap(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, true)

	outside := filepath.Join(root, "outside-turns")
	outsideTurn := filepath.Join(outside, mutationTestTurnID)
	if err := os.MkdirAll(outsideTurn, 0o700); err != nil {
		t.Fatal(err)
	}
	outsideTarget := filepath.Join(outsideTurn, "turn.json")
	original := []byte("outside sentinel\n")
	if err := os.WriteFile(outsideTarget, original, 0o600); err != nil {
		t.Fatal(err)
	}
	turns := filepath.Join(store.sessionDir(mutationTestSessionID), "turns")
	savedTurns := turns + ".saved"
	relativeTarget := filepath.Join(
		"turns", mutationTestTurnID, "turn.json",
	)
	swapped := false
	store.mutationFailpoint = func(stage, relativePath string) {
		if swapped ||
			stage != "before_target_open" ||
			relativePath != relativeTarget {
			return
		}
		swapped = true
		if err := os.Rename(turns, savedTurns); err != nil {
			t.Fatalf("save turns directory: %v", err)
		}
		if err := os.Symlink(outside, turns); err != nil {
			t.Fatalf("install turns symlink: %v", err)
		}
	}
	err := store.withLock(mutationTestSessionID, func() error {
		value, err := store.loadTurn(
			mutationTestSessionID, mutationTestTurnID,
		)
		if err != nil {
			return err
		}
		value.UpdatedAt = value.UpdatedAt.Add(time.Second)
		return store.writeTurn(value)
	})
	if err == nil || !swapped {
		t.Fatalf("ancestor symlink swap error=%v swapped=%t", err, swapped)
	}
	assertFileBytes(t, outsideTarget, original)

	store.mutationFailpoint = nil
	if err := os.Remove(turns); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(savedTurns, turns); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(store.sessionsDir, store.stateDir); err != nil {
		t.Fatalf("recover after restoring turn parent: %v", err)
	}
	assertFileBytes(t, outsideTarget, original)
}

func TestSessionStoreRejectsRuntimeAncestorSymlinkSwap(t *testing.T) {
	base := t.TempDir()
	anchor := filepath.Join(base, "anchor")
	store, err := NewStore(
		filepath.Join(anchor, "sessions"),
		filepath.Join(anchor, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	seedMutationTestSession(t, store, false)

	savedAnchor := anchor + ".saved"
	outside := filepath.Join(base, "outside")
	if err := os.MkdirAll(
		filepath.Join(outside, "sessions", mutationTestSessionID),
		0o700,
	); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(outside, "sentinel")
	original := []byte("outside sentinel\n")
	if err := os.WriteFile(sentinel, original, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(anchor, savedAnchor); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(outside, anchor); err != nil {
		t.Fatal(err)
	}

	if _, err := store.loadSession(mutationTestSessionID); err == nil {
		t.Fatal("ancestor symlink replacement was accepted")
	}
	assertFileBytes(t, sentinel, original)

	if err := os.Remove(anchor); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(savedAnchor, anchor); err != nil {
		t.Fatal(err)
	}
}

func TestNewStoreRejectsInitialProjectAncestorSymlink(t *testing.T) {
	base := t.TempDir()
	realRoot := filepath.Join(base, "real")
	if err := os.Mkdir(realRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(realRoot, "sentinel")
	original := []byte("outside sentinel\n")
	if err := os.WriteFile(sentinel, original, 0o600); err != nil {
		t.Fatal(err)
	}
	alias := filepath.Join(base, "alias")
	if err := os.Symlink(realRoot, alias); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(
		filepath.Join(alias, "sessions"),
		filepath.Join(alias, "state"),
	); err == nil {
		t.Fatal("initial ancestor symlink was accepted")
	}
	assertFileBytes(t, sentinel, original)
}

func TestSessionStoreRejectsSameKindSessionRootReplacement(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)

	sessionRoot := store.sessionDir(mutationTestSessionID)
	savedRoot := sessionRoot + ".saved"
	if err := os.Rename(sessionRoot, savedRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(sessionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinel := filepath.Join(sessionRoot, "sentinel")
	original := []byte("replacement sentinel\n")
	if err := os.WriteFile(sentinel, original, 0o600); err != nil {
		t.Fatal(err)
	}

	if _, err := store.loadSession(mutationTestSessionID); err == nil {
		t.Fatal("same-kind Session root replacement was accepted")
	}
	assertFileBytes(t, sentinel, original)

	if err := os.Remove(sentinel); err != nil {
		t.Fatal(err)
	}
	if err := os.Remove(sessionRoot); err != nil {
		t.Fatal(err)
	}
	if err := os.Rename(savedRoot, sessionRoot); err != nil {
		t.Fatal(err)
	}
}

func TestSessionAppendRejectsExternalHardlink(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)

	outside := filepath.Join(root, "outside-hardlink")
	original := []byte("outside sentinel\n")
	if err := os.WriteFile(outside, original, 0o600); err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(
		store.sessionDir(mutationTestSessionID), "events.jsonl",
	)
	if err := os.Link(outside, target); err != nil {
		t.Fatal(err)
	}
	err := store.withLock(mutationTestSessionID, func() error {
		value, err := store.loadSession(mutationTestSessionID)
		if err != nil {
			return err
		}
		return store.appendEvent(&value, mutationTestEvent())
	})
	if err == nil {
		t.Fatal("external hardlink append was accepted")
	}
	assertFileBytes(t, outside, original)
}

func TestSessionTrashMoveResumesAfterRenameBeforeSyncFailure(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	firstTarget := filepath.Join(
		store.historyDir,
		"trash",
		"20260730T120000.000000000Z",
		mutationTestSessionID,
	)
	injected := errors.New("injected trash barrier failure")
	fired := false
	store.mutationErrorpoint = func(stage, _ string) error {
		if !fired && stage == "after_trash_rename_before_sync" {
			fired = true
			return injected
		}
		return nil
	}
	if _, err := store.durableMoveSession(
		mutationTestSessionID, firstTarget,
	); !errors.Is(err, injected) {
		t.Fatalf("trash move error=%v", err)
	}
	if !fired {
		t.Fatal("trash move failure point did not fire")
	}
	store.mutationErrorpoint = nil
	secondTarget := filepath.Join(
		store.historyDir,
		"trash",
		"20260730T130000.000000000Z",
		mutationTestSessionID,
	)
	actualTarget, err := store.durableMoveSession(
		mutationTestSessionID, secondTarget,
	)
	if err != nil {
		t.Fatalf("resume pending trash move: %v", err)
	}
	if actualTarget != firstTarget {
		t.Fatalf("resumed target=%s want=%s", actualTarget, firstTarget)
	}
	if _, err := os.Lstat(secondTarget); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("retry created a second trash target: %v", err)
	}
	if err := store.rebuildIndex(); err != nil {
		t.Fatal(err)
	}
	if err := store.completeTrashMove(mutationTestSessionID); err != nil {
		t.Fatal(err)
	}
	if _, err := os.Lstat(firstTarget); err != nil {
		t.Fatalf("durable trash target missing: %v", err)
	}
	if _, err := os.Lstat(
		filepath.Join(store.moveDir, mutationTestSessionID+".json"),
	); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed trash journal remains: %v", err)
	}
}

func TestAtomicWriteMissingDoesNotReplaceLastInstructionInsertion(t *testing.T) {
	root, err := canonicalStorePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory, err := openSafeDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.close()
	target := filepath.Join(root, "target.json")
	sentinel := []byte("foreign sentinel\n")
	_, err = directory.atomicWrite(
		"target.json", []byte("runtime\n"), 0o600, true, nil, nil,
		func() error {
			return os.WriteFile(target, sentinel, 0o600)
		},
		nil,
	)
	if err == nil {
		t.Fatal("missing-target publication replaced a last-instruction insertion")
	}
	assertFileBytes(t, target, sentinel)
}

func TestAtomicWriteExistingRestoresLastInstructionReplacement(t *testing.T) {
	root, err := canonicalStorePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte("runtime-old\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := openSafeDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.close()
	entry, err := directory.statEntry("target.json")
	if err != nil {
		t.Fatal(err)
	}
	expected := entry.identity()
	saved := target + ".saved"
	sentinel := []byte("foreign sentinel\n")
	_, err = directory.atomicWrite(
		"target.json", []byte("runtime-new\n"), 0o600, false, &expected, nil,
		func() error {
			if err := os.Rename(target, saved); err != nil {
				return err
			}
			return os.WriteFile(target, sentinel, 0o600)
		},
		nil,
	)
	if err == nil {
		t.Fatal("existing-target publication accepted a last-instruction replacement")
	}
	assertFileBytes(t, target, sentinel)
}

func TestRemoveRegularRestoresLastInstructionReplacement(t *testing.T) {
	root, err := canonicalStorePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	target := filepath.Join(root, "target.json")
	if err := os.WriteFile(target, []byte("runtime\n"), 0o600); err != nil {
		t.Fatal(err)
	}
	directory, err := openSafeDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.close()
	entry, err := directory.statEntry("target.json")
	if err != nil {
		t.Fatal(err)
	}
	expected := entry.identity()
	saved := target + ".saved"
	sentinel := []byte("foreign sentinel\n")
	err = directory.removeRegular(
		"target.json", &expected,
		func() error {
			if err := os.Rename(target, saved); err != nil {
				return err
			}
			return os.WriteFile(target, sentinel, 0o600)
		},
	)
	if err == nil {
		t.Fatal("conditional removal accepted a last-instruction replacement")
	}
	assertFileBytes(t, target, sentinel)
}

func TestOpenDirectoryCreateDoesNotAdoptLastInstructionInsertion(t *testing.T) {
	root, err := canonicalStorePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory, err := openSafeDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.close()
	child := filepath.Join(root, "child")
	sentinel := filepath.Join(child, "sentinel")
	opened, err := directory.openDirectoryWithCreateHook(
		"child", true,
		func() error {
			if err := os.Mkdir(child, 0o700); err != nil {
				return err
			}
			return os.WriteFile(sentinel, []byte("foreign\n"), 0o600)
		},
	)
	if opened != nil {
		opened.close()
	}
	if err == nil {
		t.Fatal("directory creation adopted a last-instruction insertion")
	}
	assertFileBytes(t, sentinel, []byte("foreign\n"))
}

func TestOpenDirectoryCreateRejectsReplacementBeforeOpen(t *testing.T) {
	root, err := canonicalStorePath(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	directory, err := openSafeDirectory(root)
	if err != nil {
		t.Fatal(err)
	}
	defer directory.close()
	child := filepath.Join(root, "child")
	saved := child + ".saved"
	sentinel := filepath.Join(child, "sentinel")
	opened, err := directory.openDirectoryWithCreateHook(
		"child", true, nil,
		func() error {
			if err := os.Rename(child, saved); err != nil {
				return err
			}
			if err := os.Mkdir(child, 0o700); err != nil {
				return err
			}
			return os.WriteFile(sentinel, []byte("foreign\n"), 0o600)
		},
	)
	if opened != nil {
		opened.close()
	}
	if err == nil {
		t.Fatal("directory creation adopted a replacement before open")
	}
	assertFileBytes(t, sentinel, []byte("foreign\n"))
}

func TestSessionLockRejectsVisibleInodeReplacementAfterFlock(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	lockPath := filepath.Join(
		store.lockDir, mutationTestSessionID+".lock",
	)
	saved := lockPath + ".saved"
	sentinel := []byte("foreign lock\n")
	fired := false
	store.mutationFailpoint = func(stage, relativePath string) {
		if fired ||
			stage != "after_session_lock_acquired" ||
			relativePath != mutationTestSessionID {
			return
		}
		fired = true
		if err := os.Rename(lockPath, saved); err != nil {
			t.Fatalf("save lock: %v", err)
		}
		if err := os.WriteFile(lockPath, sentinel, 0o600); err != nil {
			t.Fatalf("replace lock: %v", err)
		}
	}
	err := store.withSessionFileLock(mutationTestSessionID, func() error {
		t.Fatal("callback ran with a replaced visible lock")
		return nil
	})
	if err == nil || !fired {
		t.Fatalf("lock replacement error=%v fired=%t", err, fired)
	}
	assertFileBytes(t, lockPath, sentinel)
}

func TestCreateReportsIndexRebuildFailureAfterFactsCommit(t *testing.T) {
	service := newTestService(t, &scriptedGenerator{}, nil, nil)
	lockPath := filepath.Join(service.store.lockDir, "index.lock")
	saved := lockPath + ".saved"
	sentinel := []byte("foreign index lock\n")
	fired := false
	service.store.mutationFailpoint = func(stage, _ string) {
		if fired || stage != "after_index_lock_acquired" {
			return
		}
		fired = true
		if err := os.Rename(lockPath, saved); err != nil {
			t.Fatalf("save index lock: %v", err)
		}
		if err := os.WriteFile(lockPath, sentinel, 0o600); err != nil {
			t.Fatalf("replace index lock: %v", err)
		}
	}
	value, err := service.Create(RetentionStandard)
	if err == nil || !fired {
		t.Fatalf("Create error=%v fired=%t", err, fired)
	}
	if value.ID == "" {
		t.Fatal("Create lost the committed Session identity")
	}
	service.store.mutationFailpoint = nil
	if _, loadErr := service.store.loadSession(value.ID); loadErr != nil {
		t.Fatalf("committed Session facts are unavailable: %v", loadErr)
	}
	assertFileBytes(t, lockPath, sentinel)
}

func TestInvocationManifestRejectsDirectoryReplacement(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	service := &Service{store: store, now: time.Now}
	manifest := writeTestInvocationManifest(t, service)
	saved := store.invocationDir + ".saved"
	if err := os.Rename(store.invocationDir, saved); err != nil {
		t.Fatal(err)
	}
	if err := os.Mkdir(store.invocationDir, 0o700); err != nil {
		t.Fatal(err)
	}
	sentinelPath := filepath.Join(store.invocationDir, manifest.name)
	sentinel := []byte("foreign manifest\n")
	if err := os.WriteFile(sentinelPath, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := consumeInvocationManifest(
		manifest.path,
		manifest.directoryIdentity,
		manifest.fileIdentity,
	); err == nil {
		t.Fatal("helper accepted a replaced invocation directory")
	}
	assertFileBytes(t, sentinelPath, sentinel)
}

func TestInvocationManifestRejectsSameNameReplacement(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	service := &Service{store: store, now: time.Now}
	manifest := writeTestInvocationManifest(t, service)
	saved := manifest.path + ".saved"
	if err := os.Rename(manifest.path, saved); err != nil {
		t.Fatal(err)
	}
	sentinel := []byte("foreign manifest\n")
	if err := os.WriteFile(manifest.path, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := consumeInvocationManifest(
		manifest.path,
		manifest.directoryIdentity,
		manifest.fileIdentity,
	); err == nil {
		t.Fatal("helper accepted a same-name manifest replacement")
	}
	assertFileBytes(t, manifest.path, sentinel)
}

func TestInvocationManifestRejectsHardlinkAndSymlink(t *testing.T) {
	t.Run("hardlink", func(t *testing.T) {
		root := t.TempDir()
		store := newMutationTestStore(t, root)
		service := &Service{store: store, now: time.Now}
		manifest := writeTestInvocationManifest(t, service)
		outside := filepath.Join(root, "outside-hardlink")
		if err := os.Link(manifest.path, outside); err != nil {
			t.Fatal(err)
		}
		original, err := os.ReadFile(outside)
		if err != nil {
			t.Fatal(err)
		}
		if _, err := consumeInvocationManifest(
			manifest.path,
			manifest.directoryIdentity,
			manifest.fileIdentity,
		); err == nil {
			t.Fatal("helper accepted a hardlinked manifest")
		}
		assertFileBytes(t, outside, original)
	})
	t.Run("symlink", func(t *testing.T) {
		root := t.TempDir()
		store := newMutationTestStore(t, root)
		service := &Service{store: store, now: time.Now}
		manifest := writeTestInvocationManifest(t, service)
		saved := manifest.path + ".saved"
		if err := os.Rename(manifest.path, saved); err != nil {
			t.Fatal(err)
		}
		outside := filepath.Join(root, "outside-sentinel")
		sentinel := []byte("outside sentinel\n")
		if err := os.WriteFile(outside, sentinel, 0o600); err != nil {
			t.Fatal(err)
		}
		if err := os.Symlink(outside, manifest.path); err != nil {
			t.Fatal(err)
		}
		if _, err := consumeInvocationManifest(
			manifest.path,
			manifest.directoryIdentity,
			manifest.fileIdentity,
		); err == nil {
			t.Fatal("helper accepted a symlink manifest")
		}
		assertFileBytes(t, outside, sentinel)
	})
}

func TestInvocationManifestConsumeDoesNotDeleteExternalSentinel(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	service := &Service{store: store, now: time.Now}
	manifest := writeTestInvocationManifest(t, service)
	outside := filepath.Join(root, "outside-sentinel")
	sentinel := []byte("outside sentinel\n")
	if err := os.WriteFile(outside, sentinel, 0o600); err != nil {
		t.Fatal(err)
	}
	value, err := consumeInvocationManifest(
		manifest.path,
		manifest.directoryIdentity,
		manifest.fileIdentity,
	)
	if err != nil {
		t.Fatal(err)
	}
	if value.Path != "/bin/echo" {
		t.Fatalf("manifest path=%q", value.Path)
	}
	if _, err := os.Lstat(manifest.path); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("consumed manifest remains: %v", err)
	}
	assertFileBytes(t, outside, sentinel)
}

func TestSessionTrashMoveRestoresLastInstructionSourceReplacement(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	source := store.sessionDir(mutationTestSessionID)
	saved := source + ".saved"
	target := filepath.Join(
		store.historyDir, "trash", "20260730T140000.000000000Z",
		mutationTestSessionID,
	)
	sentinelPath := filepath.Join(source, "sentinel")
	sentinel := []byte("foreign session\n")
	fired := false
	store.mutationFailpoint = func(stage, _ string) {
		if fired || stage != "before_trash_rename" {
			return
		}
		fired = true
		if err := os.Rename(source, saved); err != nil {
			t.Fatalf("save source: %v", err)
		}
		if err := os.Mkdir(source, 0o700); err != nil {
			t.Fatalf("replace source: %v", err)
		}
		if err := os.WriteFile(sentinelPath, sentinel, 0o600); err != nil {
			t.Fatalf("write replacement sentinel: %v", err)
		}
	}
	if _, err := store.durableMoveSession(
		mutationTestSessionID, target,
	); err == nil {
		t.Fatal("trash move accepted a last-instruction source replacement")
	}
	if !fired {
		t.Fatal("trash source replacement hook did not fire")
	}
	assertFileBytes(t, sentinelPath, sentinel)
}

func TestSessionTrashMoveDoesNotReplaceLastInstructionTarget(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	source := store.sessionDir(mutationTestSessionID)
	target := filepath.Join(
		store.historyDir, "trash", "20260730T150000.000000000Z",
		mutationTestSessionID,
	)
	sentinelPath := filepath.Join(target, "sentinel")
	sentinel := []byte("foreign target\n")
	fired := false
	store.mutationFailpoint = func(stage, _ string) {
		if fired || stage != "before_trash_rename" {
			return
		}
		fired = true
		if err := os.Mkdir(target, 0o700); err != nil {
			t.Fatalf("insert target: %v", err)
		}
		if err := os.WriteFile(sentinelPath, sentinel, 0o600); err != nil {
			t.Fatalf("write target sentinel: %v", err)
		}
	}
	if _, err := store.durableMoveSession(
		mutationTestSessionID, target,
	); err == nil {
		t.Fatal("trash move replaced a last-instruction target")
	}
	if !fired {
		t.Fatal("trash target insertion hook did not fire")
	}
	if _, err := os.Lstat(source); err != nil {
		t.Fatalf("trash source was moved despite target insertion: %v", err)
	}
	assertFileBytes(t, sentinelPath, sentinel)
}

func TestRemoveNewSessionRootRestoresLastInstructionReplacement(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	if err := store.ensure(); err != nil {
		t.Fatal(err)
	}
	sessionRoot := store.sessionDir(mutationTestSessionID)
	if err := os.Mkdir(sessionRoot, 0o700); err != nil {
		t.Fatal(err)
	}
	directory, err := openSafeDirectory(sessionRoot)
	if err != nil {
		t.Fatal(err)
	}
	rootIdentity, err := directory.identity()
	directory.close()
	if err != nil {
		t.Fatal(err)
	}
	saved := sessionRoot + ".saved"
	sentinelPath := filepath.Join(sessionRoot, "sentinel")
	sentinel := []byte("foreign session\n")
	fired := false
	store.mutationFailpoint = func(stage, _ string) {
		if fired || stage != "before_new_session_root_unpublish" {
			return
		}
		fired = true
		if err := os.Rename(sessionRoot, saved); err != nil {
			t.Fatalf("save root: %v", err)
		}
		if err := os.Mkdir(sessionRoot, 0o700); err != nil {
			t.Fatalf("replace root: %v", err)
		}
		if err := os.WriteFile(sentinelPath, sentinel, 0o600); err != nil {
			t.Fatalf("write replacement sentinel: %v", err)
		}
	}
	err = store.removeNewSessionRoot(sessionMutationJournal{
		SessionID:  mutationTestSessionID,
		RootDevice: rootIdentity.dev,
		RootInode:  rootIdentity.ino,
	})
	if err == nil || !fired {
		t.Fatalf("root removal error=%v fired=%t", err, fired)
	}
	assertFileBytes(t, sentinelPath, sentinel)
}

func TestCreateSessionRootRejectsReplacementBeforeOpen(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	if err := store.ensure(); err != nil {
		t.Fatal(err)
	}
	nonce := "0123456789abcdef0123456789abcdef"
	tempName := mutationRootTempName(nonce)
	tempPath := filepath.Join(store.sessionsDir, tempName)
	saved := tempPath + ".saved"
	sentinelPath := filepath.Join(tempPath, "sentinel")
	sentinel := []byte("foreign root\n")
	fired := false
	store.mutationFailpoint = func(stage, _ string) {
		if fired || stage != "after_mutation_root_mkdir_before_open" {
			return
		}
		fired = true
		if err := os.Rename(tempPath, saved); err != nil {
			t.Fatalf("save temporary root: %v", err)
		}
		if err := os.Mkdir(tempPath, 0o700); err != nil {
			t.Fatalf("replace temporary root: %v", err)
		}
		if err := os.WriteFile(sentinelPath, sentinel, 0o600); err != nil {
			t.Fatalf("write temporary-root sentinel: %v", err)
		}
	}
	created, _, err := store.createSessionRoot(
		mutationTestSessionID, nonce,
	)
	if created != nil {
		created.close()
	}
	if err == nil || !fired {
		t.Fatalf("temporary root replacement error=%v fired=%t", err, fired)
	}
	assertFileBytes(t, sentinelPath, sentinel)
}

func TestPublishSessionRootRestoresLastInstructionSourceReplacement(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	if err := store.ensure(); err != nil {
		t.Fatal(err)
	}
	nonce := "fedcba9876543210fedcba9876543210"
	created, tempName, err := store.createSessionRoot(
		mutationTestSessionID, nonce,
	)
	if err != nil {
		t.Fatal(err)
	}
	expected, err := created.identity()
	if closeErr := created.close(); err == nil {
		err = closeErr
	}
	if err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(store.sessionsDir, tempName)
	saved := tempPath + ".saved"
	sentinelPath := filepath.Join(tempPath, "sentinel")
	sentinel := []byte("foreign root\n")
	fired := false
	store.mutationFailpoint = func(stage, _ string) {
		if fired || stage != "before_mutation_root_publish" {
			return
		}
		fired = true
		if err := os.Rename(tempPath, saved); err != nil {
			t.Fatalf("save temporary root: %v", err)
		}
		if err := os.Mkdir(tempPath, 0o700); err != nil {
			t.Fatalf("replace temporary root: %v", err)
		}
		if err := os.WriteFile(sentinelPath, sentinel, 0o600); err != nil {
			t.Fatalf("write replacement sentinel: %v", err)
		}
	}
	err = store.publishSessionRoot(
		mutationTestSessionID, tempName, expected,
	)
	if err == nil || !fired {
		t.Fatalf("root publication error=%v fired=%t", err, fired)
	}
	assertFileBytes(t, sentinelPath, sentinel)
}

func writeTestInvocationManifest(
	t *testing.T,
	service *Service,
) invocationManifestHandle {
	t.Helper()
	manifest, err := service.writeInvocationManifest(
		runtimecommand.Invocation{
			Path:        "/bin/echo",
			Argv:        []string{"/bin/echo", "ok"},
			Environment: []string{"PATH=/usr/bin:/bin"},
			CWD:         t.TempDir(),
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	return manifest
}

func assertFileBytes(t *testing.T, path string, expected []byte) {
	t.Helper()
	actual, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if !bytes.Equal(actual, expected) {
		t.Fatalf("%s=%q want=%q", path, actual, expected)
	}
}
