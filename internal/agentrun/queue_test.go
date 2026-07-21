package agentrun

import (
	"bytes"
	"context"
	"errors"
	"io"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"agent-runtime/internal/daemon"
)

func TestSubmitExecutesInlineAndListsCompletedRun(t *testing.T) {
	root := t.TempDir()
	writeNativeProfile(t, root, "native", 0)
	service := New(root)
	runID := "task-20260719-000000-submit"
	result, err := service.Submit(context.Background(), RunOptions{RunType: RunTask, RunID: runID, Profile: "native", ProjectID: "demo", Prompt: "hello"})
	if err != nil || result.State != StateDone {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	runs, err := service.ListRuns(RunFilter{State: StateDone, ProjectID: "demo", Profile: "native", Limit: 5})
	if err != nil || len(runs) != 1 || runs[0].RunID != runID || runs[0].CompletedAt.IsZero() {
		t.Fatalf("runs=%#v err=%v", runs, err)
	}
	if SupportedFeatures()["durable_queue"] != 1 {
		t.Fatal("durable_queue feature is missing")
	}
}

func TestDispatchQueueExecutesPersistedWork(t *testing.T) {
	root := t.TempDir()
	writeNativeProfile(t, root, "native", 0)
	service := New(root)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	config := service.DaemonConfig()
	config.IdleTimeout = time.Minute
	serverErr := make(chan error, 1)
	go func() { serverErr <- daemon.NewServer(config).Run(ctx) }()
	deadline := time.Now().Add(3 * time.Second)
	for {
		if _, err := service.DaemonClient().Status(context.Background()); err == nil {
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("daemon did not become ready")
		}
		time.Sleep(20 * time.Millisecond)
	}

	now := time.Now().UTC()
	entry := QueueEntry{
		SchemaVersion: queueSchemaVersion, Sequence: 1, RunID: "task-20260719-000008-dispatch", RunType: RunTask,
		Profile: "native", State: StatePending, Attempt: 1, QueuedAt: now,
		Options: RunOptions{RunType: RunTask, RunID: "task-20260719-000008-dispatch", Profile: "native", Prompt: "hello"},
	}
	seedQueue(t, service, []QueueEntry{entry})
	dispatchErr := make(chan error, 1)
	go func() { dispatchErr <- service.DispatchQueue(ctx) }()
	waitForState(t, service, entry.RunType, entry.RunID, StateDone)
	deadline = time.Now().Add(3 * time.Second)
	for service.QueueBusy() && time.Now().Before(deadline) {
		time.Sleep(20 * time.Millisecond)
	}
	if service.QueueBusy() {
		t.Fatal("queue remained busy after terminal status")
	}
	bad := QueueEntry{
		SchemaVersion: queueSchemaVersion, Sequence: 2, RunID: "task-20260719-000009-invalid", RunType: RunTask,
		Profile: "missing", State: StatePending, Attempt: 1, QueuedAt: time.Now().UTC(),
		Options: RunOptions{RunType: RunTask, RunID: "task-20260719-000009-invalid", Profile: "missing", Prompt: "hello"},
	}
	seedQueue(t, service, []QueueEntry{bad})
	waitForState(t, service, bad.RunType, bad.RunID, StateFailed)
	badStatus, err := service.Status(bad.RunType, bad.RunID)
	if err != nil || badStatus.ErrorCode != "dispatch_error" {
		t.Fatalf("invalid dispatch status=%#v err=%v", badStatus, err)
	}
	cancel()
	if err := <-dispatchErr; err != nil {
		t.Fatal(err)
	}
	if err := <-serverErr; err != nil {
		t.Fatal(err)
	}
}

func TestQueueClaimsFIFOAndExpiresTimedOutEntry(t *testing.T) {
	service := New(t.TempDir())
	now := time.Now().UTC()
	expiredAt := now.Add(-time.Second)
	entries := []QueueEntry{
		{SchemaVersion: queueSchemaVersion, Sequence: 1, RunID: "task-20260719-000001-expired", RunType: RunTask, Profile: "mock", State: StatePending, Attempt: 1, QueuedAt: now.Add(-time.Minute), ExpiresAt: &expiredAt},
		{SchemaVersion: queueSchemaVersion, Sequence: 2, RunID: "task-20260719-000002-first", RunType: RunTask, Profile: "mock", State: StatePending, Attempt: 1, QueuedAt: now},
		{SchemaVersion: queueSchemaVersion, Sequence: 3, RunID: "task-20260719-000003-second", RunType: RunTask, Profile: "mock", State: StatePending, Attempt: 1, QueuedAt: now.Add(time.Second)},
	}
	seedQueue(t, service, entries)

	claimed, found, err := service.claimNext()
	if err != nil || !found {
		t.Fatalf("claimNext found=%v err=%v", found, err)
	}
	if claimed.RunID != entries[1].RunID {
		t.Fatalf("claimed %s, want %s", claimed.RunID, entries[1].RunID)
	}
	expiredStatus, err := service.Status(RunTask, entries[0].RunID)
	if err != nil {
		t.Fatal(err)
	}
	if expiredStatus.State != StateFailed || expiredStatus.ErrorCode != "queue_timeout" || !expiredStatus.Retryable {
		t.Fatalf("expired status=%#v", expiredStatus)
	}
}

func TestCancelQueuedRunWritesTerminalArtifacts(t *testing.T) {
	service := New(t.TempDir())
	queuedAt := time.Now().UTC().Add(-time.Second)
	entry := QueueEntry{
		SchemaVersion: queueSchemaVersion, Sequence: 1, RunID: "task-20260719-000004-cancel", RunType: RunTask,
		Profile: "mock", ProjectID: "demo", State: StatePending, Attempt: 1, QueuedAt: queuedAt,
		Options: RunOptions{RunType: RunTask, RunID: "task-20260719-000004-cancel", Profile: "mock", Caller: "test"},
	}
	seedQueue(t, service, []QueueEntry{entry})

	summary, err := service.Cancel(RunTask, entry.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if summary.State != StateCancelled {
		t.Fatalf("summary=%#v", summary)
	}
	status, err := service.Status(RunTask, entry.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if status.ErrorCode != "cancelled" || status.QueuedAt.IsZero() || status.CompletedAt.IsZero() {
		t.Fatalf("status=%#v", status)
	}
	snapshot, err := service.QueueSnapshot(true)
	if err != nil || len(snapshot.Entries) != 0 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

func TestReconcileOrphanedRunIsDryRunSafeAndNeverRequeuesExecution(t *testing.T) {
	service := New(t.TempDir())
	now := time.Now().UTC()
	entry := QueueEntry{
		SchemaVersion: queueSchemaVersion, Sequence: 1, RunID: "task-20260719-000005-orphan", RunType: RunTask,
		Profile: "mock", State: StateRunning, OwnerPID: 999999999, Attempt: 1, QueuedAt: now.Add(-time.Minute), ClaimedAt: &now,
	}
	seedQueue(t, service, []QueueEntry{entry})
	paths, err := RunPaths(service.RunsDir, entry.RunType, entry.RunID)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	request := Request{
		SchemaVersion: 1, ContractVersion: ContractVersion, RunID: entry.RunID, RunType: entry.RunType,
		Provider: "mock", ProviderProfile: entry.Profile, CreatedAt: entry.QueuedAt, UpdatedAt: now,
	}
	if err := service.store.WriteRequest(paths, request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.WriteStatus(paths, request, StateRunning, "", "running", nil); err != nil {
		t.Fatal(err)
	}

	report, err := service.ReconcileQueue(true)
	if err != nil || len(report.Actions) != 1 || report.Actions[0].Reason != "orphaned" {
		t.Fatalf("dry-run report=%#v err=%v", report, err)
	}
	status, err := service.Status(entry.RunType, entry.RunID)
	if err != nil || status.State != StateRunning {
		t.Fatalf("dry-run status=%#v err=%v", status, err)
	}
	report, err = service.ReconcileQueue(false)
	if err != nil || len(report.Actions) != 1 {
		t.Fatalf("report=%#v err=%v", report, err)
	}
	status, err = service.Status(entry.RunType, entry.RunID)
	if err != nil || status.State != StateFailed || status.ErrorCode != "orphaned" || !status.Retryable {
		t.Fatalf("status=%#v err=%v", status, err)
	}
	snapshot, err := service.QueueSnapshot(true)
	if err != nil || len(snapshot.Entries) != 0 {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
}

func TestWaitCancelsQueuedRunWithContext(t *testing.T) {
	service := New(t.TempDir())
	now := time.Now().UTC()
	entry := QueueEntry{
		SchemaVersion: queueSchemaVersion, Sequence: 1, RunID: "task-20260719-000006-context", RunType: RunTask,
		Profile: "mock", State: StatePending, Attempt: 1, QueuedAt: now,
	}
	seedQueue(t, service, []QueueEntry{entry})
	ctx, cancel := context.WithCancel(context.Background())
	cancel()
	_, err := service.Wait(ctx, entry.RunType, entry.RunID)
	if err == nil {
		t.Fatal("Wait should return the cancelled context")
	}
	status, statusErr := service.Status(entry.RunType, entry.RunID)
	if statusErr != nil || status.State != StateCancelled {
		t.Fatalf("status=%#v err=%v", status, statusErr)
	}
}

func TestFollowStreamsBeforeTerminalAndDrainsLastChunk(t *testing.T) {
	service := New(t.TempDir())
	runID := "task-20260721-180000-follow"
	paths, err := RunPaths(service.RunsDir, RunTask, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := Request{SchemaVersion: 1, RunID: runID, RunType: RunTask, Provider: "cli", CreatedAt: now, UpdatedAt: now}
	if err := service.store.WriteRequest(paths, request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.WriteStatus(paths, request, StateRunning, "", "running", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.OutputLog, []byte("argv=[\"provider\" \"secret-arg\"]\nrunning\n"+outputStreamMarker), 0o644); err != nil {
		t.Fatal(err)
	}
	recorder := newFollowRecorder()
	type followResult struct {
		summary RunSummary
		err     error
	}
	finished := make(chan followResult, 1)
	go func() {
		summary, followErr := service.Follow(context.Background(), RunTask, runID, recorder)
		finished <- followResult{summary: summary, err: followErr}
	}()

	appendFile(t, paths.OutputLog, "[stderr] first chunk\n")
	select {
	case <-recorder.wrote:
	case <-time.After(2 * time.Second):
		t.Fatal("first Provider chunk was not followed before terminal state")
	}
	if got := recorder.String(); !strings.Contains(got, "first chunk") || strings.Contains(got, "secret-arg") {
		t.Fatalf("followed output=%q", got)
	}
	appendFile(t, paths.OutputLog, "[stdout] last chunk")
	if _, err := service.store.WriteStatus(paths, request, StateDone, "", "done", nil); err != nil {
		t.Fatal(err)
	}
	select {
	case result := <-finished:
		if result.err != nil || result.summary.State != StateDone {
			t.Fatalf("summary=%#v err=%v", result.summary, result.err)
		}
	case <-time.After(2 * time.Second):
		t.Fatal("Follow did not return after terminal state")
	}
	if got := recorder.String(); strings.Count(got, "first chunk") != 1 || strings.Count(got, "last chunk") != 1 {
		t.Fatalf("followed output=%q", got)
	}
}

func TestFollowWriterFailureDetachesWithoutCancellingRun(t *testing.T) {
	service := New(t.TempDir())
	runID := "task-20260721-180001-detach"
	paths, err := RunPaths(service.RunsDir, RunTask, runID)
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	request := Request{SchemaVersion: 1, RunID: runID, RunType: RunTask, Provider: "cli", CreatedAt: now, UpdatedAt: now}
	if err := service.store.WriteRequest(paths, request); err != nil {
		t.Fatal(err)
	}
	if _, err := service.store.WriteStatus(paths, request, StateRunning, "", "running", nil); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(paths.OutputLog, []byte("running\n"+outputStreamMarker+"[stdout] hello\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	_, err = service.Follow(context.Background(), RunTask, runID, failingWriter{})
	if err == nil || !errors.Is(err, io.ErrClosedPipe) {
		t.Fatalf("err=%v", err)
	}
	status, statusErr := service.Status(RunTask, runID)
	if statusErr != nil || status.State != StateRunning {
		t.Fatalf("status=%#v err=%v", status, statusErr)
	}
}

type followRecorder struct {
	mu    sync.Mutex
	data  bytes.Buffer
	wrote chan struct{}
}

func newFollowRecorder() *followRecorder {
	return &followRecorder{wrote: make(chan struct{}, 8)}
}

func (r *followRecorder) Write(value []byte) (int, error) {
	r.mu.Lock()
	written, err := r.data.Write(value)
	r.mu.Unlock()
	select {
	case r.wrote <- struct{}{}:
	default:
	}
	return written, err
}

func (r *followRecorder) String() string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return r.data.String()
}

type failingWriter struct{}

func (failingWriter) Write([]byte) (int, error) {
	return 0, io.ErrClosedPipe
}

func appendFile(t *testing.T, path, value string) {
	t.Helper()
	file, err := os.OpenFile(filepath.Clean(path), os.O_WRONLY|os.O_APPEND, 0o644)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := file.WriteString(value); err != nil {
		_ = file.Close()
		t.Fatal(err)
	}
	if err := file.Close(); err != nil {
		t.Fatal(err)
	}
}

func seedQueue(t *testing.T, service *Service, entries []QueueEntry) {
	t.Helper()
	err := service.withQueue(true, func(document *queueDocument) error {
		document.Entries = append([]QueueEntry(nil), entries...)
		document.NextSequence = int64(len(entries) + 1)
		return nil
	})
	if err != nil {
		t.Fatal(err)
	}
	info, err := os.Stat(service.queuePath())
	if err != nil {
		t.Fatal(err)
	}
	if info.Mode().Perm() != 0o600 {
		t.Fatalf("queue mode=%v", info.Mode().Perm())
	}
}
