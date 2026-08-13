package session

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func TestSessionListUsesIndexAndNewStoreDoesNotScanFacts(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)

	if err := os.WriteFile(
		store.sessionFile(mutationTestSessionID),
		[]byte(`{"corrupt":true}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}

	reopened, err := NewStore(store.sessionsDir, store.stateDir)
	if err != nil {
		t.Fatalf("bounded startup scanned a Session fact: %v", err)
	}
	values, err := reopened.list(ListFilter{})
	if err != nil {
		t.Fatalf("list did not use the Session index: %v", err)
	}
	if len(values) != 1 || values[0].ID != mutationTestSessionID {
		t.Fatalf("indexed Sessions=%#v", values)
	}
	if _, err := reopened.loadSession(mutationTestSessionID); err == nil {
		t.Fatal("targeted Session access accepted a corrupt canonical fact")
	}
	if err := reopened.Validate(); err == nil {
		t.Fatal("explicit full Session validation accepted a corrupt fact")
	}
}

func TestSessionStoreRebuildsMissingIndex(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	indexPath := filepath.Join(store.historyDir, sessionIndexFileName)
	if err := os.Remove(indexPath); err != nil {
		t.Fatal(err)
	}

	rebuilt, err := NewStore(store.sessionsDir, store.stateDir)
	if err != nil {
		t.Fatalf("rebuild missing Session index: %v", err)
	}
	values, err := rebuilt.list(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].ID != mutationTestSessionID {
		t.Fatalf("rebuilt Sessions=%#v", values)
	}
	if err := rebuilt.Validate(); err != nil {
		t.Fatalf("validate rebuilt Session index: %v", err)
	}
}

func TestSessionStoreRejectsCorruptOrUnsupportedIndex(t *testing.T) {
	for _, test := range []struct {
		name string
		data string
		want string
	}{
		{
			name: "corrupt",
			data: `{"schema_version":3,"sessions":[}`,
			want: "decode",
		},
		{
			name: "unsupported_version",
			data: `{"schema_version":999,"sessions":[]}`,
			want: "unsupported Session schema_version 999",
		},
	} {
		t.Run(test.name, func(t *testing.T) {
			root := t.TempDir()
			store := newMutationTestStore(t, root)
			if _, err := store.list(ListFilter{}); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				filepath.Join(store.historyDir, sessionIndexFileName),
				[]byte(test.data),
				0o600,
			); err != nil {
				t.Fatal(err)
			}
			_, err := NewStore(store.sessionsDir, store.stateDir)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v, want substring %q", err, test.want)
			}
		})
	}
}

func TestSessionIndexConcurrentIncrementalUpserts(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	const count = 64
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	errorsBySession := make(chan error, count)
	var group sync.WaitGroup
	for offset := 0; offset < count; offset++ {
		offset := offset
		group.Add(1)
		go func() {
			defer group.Done()
			sessionID := fmt.Sprintf("session_%032x", offset+1)
			now := start.Add(time.Duration(offset) * time.Nanosecond)
			errorsBySession <- store.withLock(sessionID, func() error {
				return store.writeSession(Session{
					SchemaVersion: SchemaVersion,
					ID:            sessionID,
					Interface:     InterfaceManaged,
					State:         SessionIdle,
					Retention:     RetentionStandard,
					CreatedAt:     now,
					UpdatedAt:     now,
				})
			})
		}()
	}
	group.Wait()
	close(errorsBySession)
	for err := range errorsBySession {
		if err != nil {
			t.Fatal(err)
		}
	}
	values, err := store.list(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != count {
		t.Fatalf("Session index count=%d want=%d", len(values), count)
	}
	if err := store.Validate(); err != nil {
		t.Fatalf("validate concurrent Session index: %v", err)
	}
}

func TestSessionIndexLazyInitializationIsSafeWithConcurrentValidation(
	t *testing.T,
) {
	root := t.TempDir()
	store, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	const operations = 64
	errorsByOperation := make(chan error, operations)
	start := make(chan struct{})
	var group sync.WaitGroup
	for offset := 0; offset < operations; offset++ {
		offset := offset
		group.Add(1)
		go func() {
			defer group.Done()
			<-start
			if offset%2 == 0 {
				_, err := store.list(ListFilter{})
				errorsByOperation <- err
				return
			}
			errorsByOperation <- store.Validate()
		}()
	}
	close(start)
	group.Wait()
	close(errorsByOperation)
	for err := range errorsByOperation {
		if err != nil {
			t.Fatal(err)
		}
	}
	if !store.indexReady.Load() {
		t.Fatal("concurrent lazy initialization did not publish index readiness")
	}
}

func TestCommittedMutationRepairsIndexAfterUpdateFailure(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	injected := errors.New("injected Session index update failure")
	store.mutationErrorpoint = func(stage, relativePath string) error {
		if stage == "before_index_update" &&
			relativePath == mutationTestSessionID {
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
		value.UpdatedAt = value.UpdatedAt.Add(time.Second)
		return store.writeSession(value)
	})
	if !errors.Is(err, injected) {
		t.Fatalf("mutation error=%v", err)
	}
	if _, err := os.Lstat(
		store.mutationJournalPath(mutationTestSessionID),
	); err != nil {
		t.Fatalf("committed journal was not retained: %v", err)
	}

	store.mutationErrorpoint = nil
	recovered, err := NewStore(store.sessionsDir, store.stateDir)
	if err != nil {
		t.Fatalf("recover committed index update: %v", err)
	}
	values, err := recovered.list(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].EventCount != 1 {
		t.Fatalf("recovered Session index=%#v", values)
	}
	assertMutationJournalAbsent(t, recovered)
}

func TestCommittedMutationIndexUpdateIsCrashRecoverable(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	runMutationCrashHelper(
		t, root, "append_event", "after_index_update", mutationTestSessionID,
	)
	if _, err := os.Lstat(
		store.mutationJournalPath(mutationTestSessionID),
	); err != nil {
		t.Fatalf("committed journal was not retained after crash: %v", err)
	}
	recovered, err := NewStore(store.sessionsDir, store.stateDir)
	if err != nil {
		t.Fatalf("recover post-index-update crash: %v", err)
	}
	values, err := recovered.list(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].EventCount != 1 {
		t.Fatalf("recovered Session index=%#v", values)
	}
	assertMutationJournalAbsent(t, recovered)
}

func TestExistingStoreRecoversCommittedMutationBeforeList(t *testing.T) {
	root := t.TempDir()
	writer := newMutationTestStore(t, root)
	seedMutationTestSession(t, writer, false)
	reader, err := NewStore(writer.sessionsDir, writer.stateDir)
	if err != nil {
		t.Fatal(err)
	}

	runMutationCrashHelper(
		t, root, "append_event", "after_commit_marker", "",
	)
	values, err := reader.list(ListFilter{})
	if err != nil {
		t.Fatalf("hot-recover committed Session index: %v", err)
	}
	if len(values) != 1 || values[0].EventCount != 1 {
		t.Fatalf("hot-recovered Session index=%#v", values)
	}
	assertMutationJournalAbsent(t, reader)
}

func TestMissingIndexRebuildsBeforeCommittedJournalCleanup(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	runMutationCrashHelper(
		t, root, "append_event", "after_commit_marker", "",
	)
	if err := os.Remove(
		filepath.Join(store.historyDir, sessionIndexFileName),
	); err != nil {
		t.Fatal(err)
	}

	recovered, err := NewStore(store.sessionsDir, store.stateDir)
	if err != nil {
		t.Fatalf("recover committed facts with missing index: %v", err)
	}
	values, err := recovered.list(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 1 || values[0].EventCount != 1 {
		t.Fatalf("rebuilt committed Session index=%#v", values)
	}
	assertMutationJournalAbsent(t, recovered)
}

func TestTrashMoveIndexRemovalIsJournalRecoverable(t *testing.T) {
	root := t.TempDir()
	store := newMutationTestStore(t, root)
	seedMutationTestSession(t, store, false)
	injected := errors.New("injected post-index-remove failure")
	store.mutationErrorpoint = func(stage, relativePath string) error {
		if stage == "after_index_update" &&
			relativePath == mutationTestSessionID {
			return injected
		}
		return nil
	}
	target := filepath.Join(
		store.historyDir, "trash", "20260812T120000.000000000Z",
		mutationTestSessionID,
	)
	if _, err := store.durableMoveSession(
		mutationTestSessionID, target,
	); !errors.Is(err, injected) {
		t.Fatalf("trash move error=%v", err)
	}
	if _, err := os.Lstat(
		filepath.Join(store.moveDir, mutationTestSessionID+".json"),
	); err != nil {
		t.Fatalf("trash move journal was not retained: %v", err)
	}

	store.mutationErrorpoint = nil
	recovered, err := NewStore(store.sessionsDir, store.stateDir)
	if err != nil {
		t.Fatalf("recover trash index removal: %v", err)
	}
	values, err := recovered.list(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("trash-moved Session remains indexed: %#v", values)
	}
	if err := recovered.Validate(); err != nil {
		t.Fatalf("validate recovered trash move: %v", err)
	}
	if _, err := os.Lstat(
		filepath.Join(store.moveDir, mutationTestSessionID+".json"),
	); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed trash move journal remains: %v", err)
	}
}

func TestExistingStoreRecoversPendingTrashMoveBeforeList(t *testing.T) {
	root := t.TempDir()
	writer := newMutationTestStore(t, root)
	seedMutationTestSession(t, writer, false)
	reader, err := NewStore(writer.sessionsDir, writer.stateDir)
	if err != nil {
		t.Fatal(err)
	}
	injected := errors.New("injected Session index removal failure")
	writer.mutationErrorpoint = func(stage, relativePath string) error {
		if stage == "before_index_update" &&
			relativePath == mutationTestSessionID {
			return injected
		}
		return nil
	}
	target := filepath.Join(
		writer.historyDir, "trash", "20260812T120000.000000000Z",
		mutationTestSessionID,
	)
	if _, err := writer.durableMoveSession(
		mutationTestSessionID, target,
	); !errors.Is(err, injected) {
		t.Fatalf("trash move error=%v", err)
	}

	values, err := reader.list(ListFilter{})
	if err != nil {
		t.Fatalf("hot-recover Session trash index removal: %v", err)
	}
	if len(values) != 0 {
		t.Fatalf("trash-moved Session remains indexed: %#v", values)
	}
	if _, err := os.Lstat(
		filepath.Join(reader.moveDir, mutationTestSessionID+".json"),
	); !errors.Is(err, os.ErrNotExist) {
		t.Fatalf("completed trash move journal remains: %v", err)
	}
}

func BenchmarkSessionListFromIndex10000(b *testing.B) {
	root := b.TempDir()
	store, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	)
	if err != nil {
		b.Fatal(err)
	}
	if err := store.ensureSessionIndex(); err != nil {
		b.Fatal(err)
	}
	values := make([]Session, 10_000)
	start := time.Date(2026, 8, 12, 12, 0, 0, 0, time.UTC)
	for offset := range values {
		now := start.Add(time.Duration(offset) * time.Nanosecond)
		values[offset] = Session{
			SchemaVersion: SchemaVersion,
			ID:            fmt.Sprintf("session_%032x", offset+1),
			Interface:     InterfaceManaged,
			State:         SessionIdle,
			Retention:     RetentionStandard,
			CreatedAt:     now,
			UpdatedAt:     now,
		}
	}
	sortSessionSummaries(values)
	if err := store.withIndexLock(func() error {
		return store.writeSessionIndex(sessionIndex{
			SchemaVersion: SchemaVersion,
			Sessions:      values,
		})
	}); err != nil {
		b.Fatal(err)
	}
	b.ReportAllocs()
	b.ResetTimer()
	for iteration := 0; iteration < b.N; iteration++ {
		listed, err := store.list(ListFilter{})
		if err != nil {
			b.Fatal(err)
		}
		if len(listed) != len(values) {
			b.Fatalf("listed=%d want=%d", len(listed), len(values))
		}
	}
}
