package session

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/profile"
)

const (
	mutationTestSessionID   = "session_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	mutationTestTurnID      = "turn_bbbbbbbbbbbbbbbbbbbbbbbbbbbbbbbb"
	mutationTestRunID       = "run_cccccccccccccccccccccccccccccccc"
	mutationTestExecutionID = "execution_dddddddddddddddddddddddddddddddd"
)

func TestSessionMutationRollsBackCallbackError(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	expected := errors.New("fixture mutation failed")
	err := store.withLock(mutationTestSessionID, func() error {
		value, err := store.loadSession(mutationTestSessionID)
		if err != nil {
			return err
		}
		if err := store.appendEvent(&value, mutationTestEvent()); err != nil {
			return err
		}
		return expected
	})
	if !errors.Is(err, expected) {
		t.Fatalf("mutation error=%v", err)
	}
	assertMutationTestSessionCounts(t, store, 0)
	assertMutationJournalAbsent(t, store)
	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err != nil {
		t.Fatalf("reopen store: %v", err)
	}
}

func TestSessionMutationRecoversPreparedAppendAfterProcessExit(
	t *testing.T,
) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)

	runMutationCrashHelper(
		t, root, "append_event", "after_target_write", "events.jsonl",
	)
	if _, err := os.Lstat(
		store.mutationJournalPath(mutationTestSessionID),
	); err != nil {
		t.Fatalf("prepared journal is unavailable: %v", err)
	}

	recovered := newMutationTestStore(t, root)
	assertMutationTestSessionCounts(t, recovered, 0)
	assertMutationJournalAbsent(t, recovered)
	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err != nil {
		t.Fatalf("second recovery was not a no-op: %v", err)
	}
}

func TestSessionMutationKeepsCommittedAppendAfterProcessExit(
	t *testing.T,
) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)

	runMutationCrashHelper(
		t, root, "append_event", "after_commit_marker", "",
	)
	recovered := newMutationTestStore(t, root)
	assertMutationTestSessionCounts(t, recovered, 1)
	assertMutationJournalAbsent(t, recovered)
	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err != nil {
		t.Fatalf("second recovery was not a no-op: %v", err)
	}
}

func TestSessionMutationRecoversTornJSONLAppendAfterProcessExit(
	t *testing.T,
) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	appendMutationTestEvent(t, store)

	runMutationCrashHelper(
		t, root, "append_torn_event", "after_torn_append", "events.jsonl",
	)
	recovered := newMutationTestStore(t, root)
	assertMutationTestSessionCounts(t, recovered, 1)
	assertMutationJournalAbsent(t, recovered)
}

func TestSessionMutationRemovesPartiallyCreatedSessionAfterProcessExit(
	t *testing.T,
) {
	root := t.TempDir()
	_ = newMutationTestStore(t, root)
	runMutationCrashHelper(
		t, root, "create_session", "after_target_write", "session.json",
	)

	recovered := newMutationTestStore(t, root)
	if _, err := os.Lstat(recovered.sessionDir(mutationTestSessionID)); !errors.Is(
		err, os.ErrNotExist,
	) {
		t.Fatalf("partial new Session still exists: %v", err)
	}
	assertMutationJournalAbsent(t, recovered)
}

func TestSessionMutationRecoversNewRootOwnershipBoundaries(t *testing.T) {
	for _, stage := range []string{
		"after_mutation_root_create",
		"after_mutation_owner_write",
	} {
		t.Run(stage, func(t *testing.T) {
			root := t.TempDir()
			store := newMutationTestStore(t, root)
			runMutationCrashHelper(
				t, root, "create_session", stage, "",
			)
			recovered := newMutationTestStore(t, root)
			if _, err := os.Lstat(
				recovered.sessionDir(mutationTestSessionID),
			); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("partial new Session still exists: %v", err)
			}
			assertMutationJournalAbsent(t, store)
		})
	}
}

func TestSessionMutationKeepsNewSessionWhenCommittedRenameLosesAck(
	t *testing.T,
) {
	root := t.TempDir()
	_ = newMutationTestStore(t, root)
	runMutationCrashHelper(
		t, root, "create_session",
		"after_committed_journal_rename_before_directory_sync", "",
	)
	recovered := newMutationTestStore(t, root)
	value, err := recovered.loadSession(mutationTestSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if value.ID != mutationTestSessionID {
		t.Fatalf("session=%#v", value)
	}
	if _, err := os.Lstat(
		recovered.mutationOwnerPath(mutationTestSessionID),
	); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed owner marker remains: %v", err)
	}
	assertMutationJournalAbsent(t, recovered)
}

func TestSessionMutationKeepsNewSessionAfterOwnerCleanupCrash(
	t *testing.T,
) {
	root := t.TempDir()
	_ = newMutationTestStore(t, root)
	runMutationCrashHelper(
		t, root, "create_session", "after_mutation_owner_cleanup", "",
	)
	recovered := newMutationTestStore(t, root)
	value, err := recovered.loadSession(mutationTestSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if value.ID != mutationTestSessionID {
		t.Fatalf("session=%#v", value)
	}
	if _, err := os.Lstat(
		recovered.mutationOwnerPath(mutationTestSessionID),
	); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("committed owner marker remains: %v", err)
	}
	assertMutationJournalAbsent(t, recovered)
	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err != nil {
		t.Fatalf("second recovery was not a no-op: %v", err)
	}
}

func TestSessionMutationRemovesOwnedAtomicTempAfterNewSessionCrash(
	t *testing.T,
) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	runMutationCrashHelper(
		t, root, "create_session_atomic_temp",
		"after_atomic_temp_sync", "session.json",
	)

	entries, err := os.ReadDir(store.sessionDir(mutationTestSessionID))
	if err != nil {
		t.Fatalf("read crashed Session root: %v", err)
	}
	foundTemp := false
	for _, entry := range entries {
		if isAtomicTempFact(entry) {
			foundTemp = true
		}
	}
	if !foundTemp {
		t.Fatal("crash fixture did not leave an owned atomic temp")
	}
	if _, err := os.Lstat(
		store.mutationJournalPath(mutationTestSessionID),
	); err != nil {
		t.Fatalf("prepared journal is unavailable: %v", err)
	}

	recovered := newMutationTestStore(t, root)
	if _, err := os.Lstat(
		recovered.sessionDir(mutationTestSessionID),
	); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("partial new Session still exists: %v", err)
	}
	assertMutationJournalAbsent(t, recovered)
	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err != nil {
		t.Fatalf("second recovery was not a no-op: %v", err)
	}
}

func TestNewStoreWaitsForActiveSessionMutation(t *testing.T) {
	root := t.TempDir()
	writer := newMutationTestStore(t, root)
	seedMutationTestSession(t, writer, false)
	entered := make(chan struct{})
	release := make(chan struct{})
	writer.mutationFailpoint = func(stage, relativePath string) {
		if stage == "after_target_write" && relativePath == "events.jsonl" {
			close(entered)
			<-release
		}
	}
	writerDone := make(chan error, 1)
	go func() {
		writerDone <- writer.withLock(mutationTestSessionID, func() error {
			value, err := writer.loadSession(mutationTestSessionID)
			if err != nil {
				return err
			}
			if err := writer.appendEvent(&value, mutationTestEvent()); err != nil {
				return err
			}
			return writer.writeSession(value)
		})
	}()
	<-entered

	openDone := make(chan error, 1)
	go func() {
		_, err := NewStore(
			filepath.Join(root, "sessions"), filepath.Join(root, "state"),
		)
		openDone <- err
	}()
	select {
	case err := <-openDone:
		t.Fatalf("NewStore observed an active mutation: %v", err)
	case <-time.After(100 * time.Millisecond):
	}
	close(release)
	if err := <-writerDone; err != nil {
		t.Fatal(err)
	}
	if err := <-openDone; err != nil {
		t.Fatal(err)
	}
}

func TestSessionMutationCommitRenameErrorDoesNotRollbackFacts(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	fired := false
	store.mutationErrorpoint = func(stage, _ string) error {
		if !fired &&
			stage == "after_committed_journal_rename_before_directory_sync" {
			fired = true
			return errors.New("injected commit directory sync failure")
		}
		return nil
	}
	err := store.withLock(mutationTestSessionID, func() error {
		value, err := store.loadSession(mutationTestSessionID)
		if err != nil {
			return err
		}
		if err := store.appendEvent(&value, mutationTestEvent()); err != nil {
			return err
		}
		return store.writeSession(value)
	})
	if err != nil {
		t.Fatal(err)
	}
	if !fired {
		t.Fatal("committed journal rename errorpoint did not fire")
	}
	assertMutationTestSessionCounts(t, store, 1)
	assertMutationJournalAbsent(t, store)
}

func TestSessionMutationCommitBeforeRenameErrorRollsBackFacts(
	t *testing.T,
) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	injected := errors.New("injected commit journal rename failure")
	fired := false
	store.mutationErrorpoint = func(stage, _ string) error {
		if !fired && stage == "before_committed_journal_rename" {
			fired = true
			return injected
		}
		return nil
	}
	err := store.withLock(mutationTestSessionID, func() error {
		value, err := store.loadSession(mutationTestSessionID)
		if err != nil {
			return err
		}
		if err := store.appendEvent(&value, mutationTestEvent()); err != nil {
			return err
		}
		return store.writeSession(value)
	})
	if !errors.Is(err, injected) {
		t.Fatalf("commit error=%v", err)
	}
	if !fired {
		t.Fatal("committed journal before-rename errorpoint did not fire")
	}
	assertMutationTestSessionCounts(t, store, 0)
	assertMutationJournalAbsent(t, store)
	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err != nil {
		t.Fatalf("reopen store: %v", err)
	}
}

func TestSessionMutationRollbackCanResumeAfterCleanupError(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	callbackErr := errors.New("callback failed")
	fired := false
	store.mutationErrorpoint = func(stage, relativePath string) error {
		if !fired &&
			stage == "after_rollback_target" &&
			relativePath == "events.jsonl" {
			fired = true
			return errors.New("injected rollback interruption")
		}
		return nil
	}
	err := store.withLock(mutationTestSessionID, func() error {
		value, err := store.loadSession(mutationTestSessionID)
		if err != nil {
			return err
		}
		if err := store.appendEvent(&value, mutationTestEvent()); err != nil {
			return err
		}
		return callbackErr
	})
	if err == nil || !errors.Is(err, callbackErr) || !fired {
		t.Fatalf("rollback error=%v fired=%t", err, fired)
	}
	if _, err := os.Lstat(
		store.mutationJournalPath(mutationTestSessionID),
	); err != nil {
		t.Fatalf("interrupted rollback removed journal: %v", err)
	}
	store.mutationErrorpoint = nil
	recovered := newMutationTestStore(t, root)
	assertMutationTestSessionCounts(t, recovered, 0)
	assertMutationJournalAbsent(t, recovered)
}

func TestSessionMutationRestoresMultipleFactsAfterProcessExit(
	t *testing.T,
) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, true)
	turnPath := store.turnFile(mutationTestSessionID, mutationTestTurnID)
	executionPath := filepath.Join(
		store.sessionDir(mutationTestSessionID),
		"executions", mutationTestExecutionID+".json",
	)
	expectedTurn, err := os.ReadFile(turnPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedExecution, err := os.ReadFile(executionPath)
	if err != nil {
		t.Fatal(err)
	}

	runMutationCrashHelper(
		t, root, "settle_facts", "after_target_write",
		filepath.Join("turns", mutationTestTurnID, "turn.json"),
	)
	recovered := newMutationTestStore(t, root)
	actualTurn, err := os.ReadFile(turnPath)
	if err != nil {
		t.Fatal(err)
	}
	actualExecution, err := os.ReadFile(executionPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(actualTurn) != string(expectedTurn) {
		t.Fatalf("Turn preimage was not restored")
	}
	if string(actualExecution) != string(expectedExecution) {
		t.Fatalf("Execution preimage was not restored")
	}
	assertMutationJournalAbsent(t, recovered)
}

func TestStoreRejectsUnsafeSessionMutationJournal(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	outside := filepath.Join(root, "outside.json")
	if err := os.WriteFile(outside, []byte("unchanged"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := sessionMutationJournal{
		MutationVersion: sessionMutationVersion,
		SessionID:       mutationTestSessionID,
		Nonce:           strings.Repeat("1", 32),
		State:           mutationPrepared,
		SessionExisted:  true,
		Entries: []sessionMutationBackup{{
			RelativePath: "../outside.json",
			Kind:         mutationReplace,
		}},
	}
	if err := atomicJSON(
		store.mutationJournalPath(mutationTestSessionID), journal, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err == nil || !strings.Contains(
		err.Error(), "escapes Session root",
	) {
		t.Fatalf("unsafe journal error=%v", err)
	}
	data, err := os.ReadFile(outside)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != "unchanged" {
		t.Fatalf("unsafe journal modified an out-of-scope file")
	}
}

func TestStoreRejectsForgedNewSessionJournalWithoutDeletingExistingFacts(
	t *testing.T,
) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	appendMutationTestEvent(t, store)

	sessionPath := store.sessionFile(mutationTestSessionID)
	eventsPath := filepath.Join(
		store.sessionDir(mutationTestSessionID), "events.jsonl",
	)
	expectedSession, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	expectedEvents, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	tempPath := filepath.Join(
		store.sessionDir(mutationTestSessionID), ".runtime-forged.tmp",
	)
	if err := os.WriteFile(tempPath, []byte("owned temp"), 0o600); err != nil {
		t.Fatal(err)
	}
	journal := sessionMutationJournal{
		MutationVersion: sessionMutationVersion,
		SessionID:       mutationTestSessionID,
		Nonce:           strings.Repeat("2", 32),
		State:           mutationPrepared,
		SessionExisted:  false,
		Entries: []sessionMutationBackup{{
			RelativePath: "session.json",
			Kind:         mutationReplace,
		}},
	}
	if err := atomicJSON(
		store.mutationJournalPath(mutationTestSessionID), journal, 0o600,
	); err != nil {
		t.Fatal(err)
	}

	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err == nil || !strings.Contains(err.Error(), "owner is missing") {
		t.Fatalf("forged new-Session journal error=%v", err)
	}
	actualSession, err := os.ReadFile(sessionPath)
	if err != nil {
		t.Fatal(err)
	}
	actualEvents, err := os.ReadFile(eventsPath)
	if err != nil {
		t.Fatal(err)
	}
	if string(actualSession) != string(expectedSession) {
		t.Fatal("forged new-Session journal modified session.json")
	}
	if string(actualEvents) != string(expectedEvents) {
		t.Fatal("forged new-Session journal modified events.jsonl")
	}
	tempData, err := os.ReadFile(tempPath)
	if err != nil {
		t.Fatalf("failed recovery removed owned temp before full validation: %v", err)
	}
	if string(tempData) != "owned temp" {
		t.Fatalf("failed recovery modified owned temp: %q", tempData)
	}
	if _, err := os.Lstat(
		store.mutationJournalPath(mutationTestSessionID),
	); err != nil {
		t.Fatalf("failed recovery removed forensic journal: %v", err)
	}
}

func TestStoreRejectsForgedNewSessionJournalThatEnumeratesAllFacts(
	t *testing.T,
) {
	for _, withFacts := range []bool{false, true} {
		name := "minimal"
		if withFacts {
			name = "all_facts"
		}
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			store := newMutationTestStore(t, root)
			seedMutationTestSession(t, store, withFacts)
			sessionPath := store.sessionFile(mutationTestSessionID)
			expectedSession, err := os.ReadFile(sessionPath)
			if err != nil {
				t.Fatal(err)
			}
			entries := []sessionMutationBackup{{
				RelativePath: "session.json",
				Kind:         mutationReplace,
			}}
			if withFacts {
				entries = append(entries,
					sessionMutationBackup{
						RelativePath: filepath.Join(
							"turns", mutationTestTurnID, "turn.json",
						),
						Kind: mutationReplace,
					},
					sessionMutationBackup{
						RelativePath: filepath.Join(
							"executions", mutationTestExecutionID+".json",
						),
						Kind: mutationReplace,
					},
				)
			}
			journal := sessionMutationJournal{
				MutationVersion: sessionMutationVersion,
				SessionID:       mutationTestSessionID,
				Nonce:           strings.Repeat("3", 32),
				State:           mutationPrepared,
				SessionExisted:  false,
				Entries:         entries,
			}
			if err := atomicJSON(
				store.mutationJournalPath(mutationTestSessionID),
				journal, 0o600,
			); err != nil {
				t.Fatal(err)
			}
			if _, err := NewStore(
				filepath.Join(root, "sessions"),
				filepath.Join(root, "state"),
			); err == nil || !strings.Contains(
				err.Error(), "owner is missing",
			) {
				t.Fatalf("forged journal error=%v", err)
			}
			actualSession, err := os.ReadFile(sessionPath)
			if err != nil {
				t.Fatal(err)
			}
			if string(actualSession) != string(expectedSession) {
				t.Fatal("forged journal modified session.json")
			}
			if _, err := os.Lstat(
				store.mutationJournalPath(mutationTestSessionID),
			); err != nil {
				t.Fatalf("forged journal was removed: %v", err)
			}
		})
	}
}

func TestStoreRejectsMismatchedNewSessionMutationOwner(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	journal := sessionMutationJournal{
		MutationVersion: sessionMutationVersion,
		SessionID:       mutationTestSessionID,
		Nonce:           strings.Repeat("4", 32),
		State:           mutationPrepared,
		SessionExisted:  false,
		Entries: []sessionMutationBackup{{
			RelativePath: "session.json",
			Kind:         mutationReplace,
		}},
	}
	if err := atomicJSON(
		store.mutationJournalPath(mutationTestSessionID),
		journal, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := atomicJSON(
		store.mutationOwnerPath(mutationTestSessionID),
		sessionMutationOwner{
			MutationVersion: sessionMutationVersion,
			SessionID:       mutationTestSessionID,
			Nonce:           strings.Repeat("5", 32),
		},
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err == nil || !strings.Contains(err.Error(), "does not match") {
		t.Fatalf("mismatched owner error=%v", err)
	}
	if _, err := os.Lstat(store.sessionFile(mutationTestSessionID)); err != nil {
		t.Fatalf("mismatched owner removed Session fact: %v", err)
	}
	if _, err := os.Lstat(
		store.mutationJournalPath(mutationTestSessionID),
	); err != nil {
		t.Fatalf("mismatched owner removed journal: %v", err)
	}
}

func TestSessionMutationCrashHelper(t *testing.T) {
	root := os.Getenv("SN_TEST_SESSION_MUTATION_ROOT")
	if root == "" {
		t.Skip("subprocess-only crash helper")
	}
	store, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	wantStage := os.Getenv("SN_TEST_SESSION_MUTATION_STAGE")
	wantTarget := os.Getenv("SN_TEST_SESSION_MUTATION_TARGET")
	store.mutationFailpoint = func(stage, relativePath string) {
		if stage == wantStage &&
			(wantTarget == "" || relativePath == wantTarget) {
			os.Exit(86)
		}
	}
	switch os.Getenv("SN_TEST_SESSION_MUTATION_OPERATION") {
	case "append_event":
		err = store.withLock(mutationTestSessionID, func() error {
			value, err := store.loadSession(mutationTestSessionID)
			if err != nil {
				return err
			}
			if err := store.appendEvent(&value, mutationTestEvent()); err != nil {
				return err
			}
			value.UpdatedAt = time.Now().UTC()
			return store.writeSession(value)
		})
	case "append_torn_event":
		err = store.withLock(mutationTestSessionID, func() error {
			path := filepath.Join(
				store.sessionDir(mutationTestSessionID), "events.jsonl",
			)
			relativePath, err := store.prepareAppend(
				mutationTestSessionID, path,
			)
			if err != nil {
				return err
			}
			file, err := os.OpenFile(path, os.O_APPEND|os.O_WRONLY, 0)
			if err != nil {
				return err
			}
			if _, err := file.Write([]byte(`{"sequence":2`)); err != nil {
				file.Close()
				return err
			}
			if err := file.Sync(); err != nil {
				file.Close()
				return err
			}
			if err := file.Close(); err != nil {
				return err
			}
			store.hitMutationFailpoint("after_torn_append", relativePath)
			return nil
		})
	case "create_session":
		now := time.Now().UTC()
		err = store.withLock(mutationTestSessionID, func() error {
			return store.writeSession(Session{
				SchemaVersion: SchemaVersion,
				ID:            mutationTestSessionID,
				State:         SessionIdle,
				Retention:     RetentionStandard,
				CreatedAt:     now,
				UpdatedAt:     now,
			})
		})
	case "create_session_atomic_temp":
		err = store.withLock(mutationTestSessionID, func() error {
			path := store.sessionFile(mutationTestSessionID)
			relativePath, err := store.prepareReplace(
				mutationTestSessionID, path,
			)
			if err != nil {
				return err
			}
			parent := filepath.Dir(path)
			if err := ensureDirectory(parent); err != nil {
				return err
			}
			temp, err := os.CreateTemp(parent, ".runtime-*.tmp")
			if err != nil {
				return err
			}
			if err := temp.Chmod(0o600); err != nil {
				temp.Close()
				return err
			}
			if _, err := temp.Write([]byte(`{"partial":true}`)); err != nil {
				temp.Close()
				return err
			}
			if err := temp.Sync(); err != nil {
				temp.Close()
				return err
			}
			if err := temp.Close(); err != nil {
				return err
			}
			store.hitMutationFailpoint(
				"after_atomic_temp_sync", relativePath,
			)
			return fmt.Errorf("atomic temp crash fixture did not exit")
		})
	case "settle_facts":
		err = store.withLock(mutationTestSessionID, func() error {
			execution, err := store.loadExecution(
				mutationTestSessionID, mutationTestExecutionID,
			)
			if err != nil {
				return err
			}
			execution.State = ExecutionSettled
			execution.Outcome = OutcomeFailed
			execution.Error = &contract.RuntimeError{
				Code: contract.ErrorInternal, Message: "fixture",
			}
			execution.UpdatedAt = time.Now().UTC()
			if err := store.writeExecution(execution); err != nil {
				return err
			}
			turn, err := store.loadTurn(
				mutationTestSessionID, mutationTestTurnID,
			)
			if err != nil {
				return err
			}
			turn.State = TurnFailed
			turn.Error = execution.Error
			turn.UpdatedAt = execution.UpdatedAt
			if err := store.writeTurn(turn); err != nil {
				return err
			}
			value, err := store.loadSession(mutationTestSessionID)
			if err != nil {
				return err
			}
			value.State = SessionIdle
			value.ActiveTurnID = ""
			value.UpdatedAt = execution.UpdatedAt
			return store.writeSession(value)
		})
	default:
		t.Fatalf(
			"unknown mutation helper operation %q",
			os.Getenv("SN_TEST_SESSION_MUTATION_OPERATION"),
		)
	}
	if err != nil {
		t.Fatal(err)
	}
	t.Fatalf(
		"mutation helper did not reach failpoint stage=%q target=%q",
		wantStage, wantTarget,
	)
}

func newMutationTestStore(t *testing.T, root string) *Store {
	t.Helper()
	store, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	return store
}

func seedMutationTestSession(
	t *testing.T,
	store *Store,
	withFacts bool,
) {
	t.Helper()
	now := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	err := store.withLock(mutationTestSessionID, func() error {
		value := Session{
			SchemaVersion: SchemaVersion,
			ID:            mutationTestSessionID,
			State:         SessionIdle,
			Retention:     RetentionStandard,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
		if withFacts {
			value.State = SessionActive
			value.ActiveTurnID = mutationTestTurnID
		}
		if err := store.writeSession(value); err != nil {
			return err
		}
		if !withFacts {
			return nil
		}
		if err := store.writeTurn(Turn{
			SchemaVersion: SchemaVersion,
			ID:            mutationTestTurnID,
			SessionID:     mutationTestSessionID,
			RunID:         mutationTestRunID,
			ExecutionID:   mutationTestExecutionID,
			ProfileID:     "api",
			ProfileKind:   profile.KindModel,
			State:         TurnRunning,
			RequestDigest: "sha256:request",
			ConfigDigest:  "sha256:config",
			CreatedAt:     now,
			UpdatedAt:     now,
		}); err != nil {
			return err
		}
		return store.writeExecution(Execution{
			SchemaVersion: SchemaVersion,
			ID:            mutationTestExecutionID,
			SessionID:     mutationTestSessionID,
			TurnID:        mutationTestTurnID,
			RunID:         mutationTestRunID,
			ProfileID:     "api",
			ProfileKind:   profile.KindModel,
			State:         ExecutionRunning,
			RequestDigest: "sha256:request",
			ConfigDigest:  "sha256:config",
			CreatedAt:     now,
			UpdatedAt:     now,
		})
	})
	if err != nil {
		t.Fatal(err)
	}
}

func mutationTestEvent() EventRecord {
	return EventRecord{
		Time:        time.Now().UTC(),
		Type:        "fixture.event",
		TurnID:      mutationTestTurnID,
		RunID:       mutationTestRunID,
		ExecutionID: mutationTestExecutionID,
	}
}

func appendMutationTestEvent(t *testing.T, store *Store) {
	t.Helper()
	if err := store.withLock(mutationTestSessionID, func() error {
		value, err := store.loadSession(mutationTestSessionID)
		if err != nil {
			return err
		}
		if err := store.appendEvent(&value, mutationTestEvent()); err != nil {
			return err
		}
		return store.writeSession(value)
	}); err != nil {
		t.Fatal(err)
	}
}

func runMutationCrashHelper(
	t *testing.T,
	root, operation, stage, target string,
) {
	t.Helper()
	command := exec.CommandContext(
		context.Background(),
		os.Args[0],
		"-test.run=^TestSessionMutationCrashHelper$",
	)
	command.Env = append(
		os.Environ(),
		"SN_TEST_SESSION_MUTATION_ROOT="+root,
		"SN_TEST_SESSION_MUTATION_OPERATION="+operation,
		"SN_TEST_SESSION_MUTATION_STAGE="+stage,
		"SN_TEST_SESSION_MUTATION_TARGET="+target,
	)
	output, err := command.CombinedOutput()
	var exitErr *exec.ExitError
	if !errors.As(err, &exitErr) || exitErr.ExitCode() != 86 {
		t.Fatalf(
			"crash helper error=%v output=%s", err, strings.TrimSpace(string(output)),
		)
	}
}

func assertMutationTestSessionCounts(
	t *testing.T,
	store *Store,
	eventCount uint64,
) {
	t.Helper()
	value, err := store.loadSession(mutationTestSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if value.EventCount != eventCount {
		t.Fatalf("event_count=%d want=%d", value.EventCount, eventCount)
	}
	events, err := store.events(mutationTestSessionID)
	if err != nil {
		t.Fatal(err)
	}
	if uint64(len(events)) != eventCount {
		t.Fatalf("event facts=%d want=%d", len(events), eventCount)
	}
}

func assertMutationJournalAbsent(t *testing.T, store *Store) {
	t.Helper()
	if _, err := os.Lstat(
		store.mutationJournalPath(mutationTestSessionID),
	); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("mutation journal still exists: %v", err)
	}
}
