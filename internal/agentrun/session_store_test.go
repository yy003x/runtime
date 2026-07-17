package agentrun

import (
	"os"
	"path/filepath"
	"testing"

	"agent-runtime/internal/provider"
)

func newTestSessionStore(t *testing.T) *SessionStore {
	t.Helper()
	root := t.TempDir()
	return NewSessionStore(filepath.Join(root, "sessions"), filepath.Join(root, "history"), filepath.Join(root, "state", "sessions"))
}

func TestSessionStorePersistsSessionTurnAttemptAndResultRef(t *testing.T) {
	store := newTestSessionStore(t)
	sessionID := "session-20260717-120000-test"
	record, err := store.Create(SessionRecord{SessionID: sessionID, ProjectID: "project", RecordMode: RecordFull,
		Retention: RetentionStandard, CaptureQuality: CaptureStructured})
	if err != nil {
		t.Fatal(err)
	}
	if record.SessionID != sessionID {
		t.Fatalf("session=%#v", record)
	}
	turnID := "turn-20260717-120000-test"
	turn, err := store.AddTurn(sessionID, TurnRecord{TurnID: turnID}, "hello", ContextManifest{
		SchemaVersion: SessionSchemaVersion, SessionID: sessionID, TurnID: turnID,
	})
	if err != nil {
		t.Fatal(err)
	}
	if turn.Sequence != 1 || turn.InputMessageID == "" {
		t.Fatalf("turn=%#v", turn)
	}
	executionID := "execution-20260717-120000-test"
	if err := store.UpsertExecution(sessionID, ExecutionRecord{ExecutionID: executionID, Kind: ExecutionAPI, State: StateRunning}); err != nil {
		t.Fatal(err)
	}
	runID := "task-20260717-120000-test"
	if err := store.AddAttempt(sessionID, RunAttemptRecord{RunID: runID, TurnID: turnID, ExecutionID: executionID, Provider: "api", Profile: "test"}); err != nil {
		t.Fatal(err)
	}
	ref := &ResultRef{RunID: runID, RunType: RunTask, ResultFile: "/tmp/result.json", ResultDigest: "sha256:test"}
	if err := store.CompleteRun(sessionID, turnID, runID, StateDone, "", "world", ref); err != nil {
		t.Fatal(err)
	}
	view, err := store.View(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) != 2 || view.Messages[0].Role != "user" || view.Messages[1].Role != "assistant" {
		t.Fatalf("messages=%#v", view.Messages)
	}
	if len(view.Attempts) != 1 || view.Attempts[0].ResultRef == nil || view.Attempts[0].ResultRef.ResultDigest != "sha256:test" {
		t.Fatalf("attempts=%#v", view.Attempts)
	}
	if view.Session.TurnCount != 1 || view.Session.RunCount != 1 {
		t.Fatalf("session=%#v", view.Session)
	}
	if view.Session.Title != "hello" {
		t.Fatalf("session title=%q", view.Session.Title)
	}
}

func TestSessionStoreIndexIsRebuildableAndDoesNotDeadlock(t *testing.T) {
	store := newTestSessionStore(t)
	for _, id := range []string{"session-20260717-120001-a", "session-20260717-120002-b"} {
		if _, err := store.Create(SessionRecord{SessionID: id, ProjectID: "project", RecordMode: RecordMetadata,
			Retention: RetentionEphemeral, CaptureQuality: CaptureMetadataOnly}); err != nil {
			t.Fatal(err)
		}
	}
	list, err := store.List(SessionFilter{ProjectID: "project"})
	if err != nil || len(list) != 2 {
		t.Fatalf("list=%#v err=%v", list, err)
	}
	if err := os.Remove(store.indexPath()); err != nil {
		t.Fatal(err)
	}
	index, err := store.RebuildIndex()
	if err != nil || len(index.Sessions) != 2 {
		t.Fatalf("index=%#v err=%v", index, err)
	}
}

func TestSessionStoreRejectsIndexPathOutsideSessionsRoot(t *testing.T) {
	store := newTestSessionStore(t)
	sessionID := "session-20260717-120002-unsafe"
	if _, err := store.Create(SessionRecord{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	index, err := store.loadIndex()
	if err != nil {
		t.Fatal(err)
	}
	entry := index.Sessions[sessionID]
	entry.SessionDir = filepath.Join(t.TempDir(), sessionID)
	index.Sessions[sessionID] = entry
	if err := writeJSONAtomic(store.indexPath(), index); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Get(sessionID); err == nil {
		t.Fatal("unsafe history index path was accepted")
	}
}

func TestSessionStoreMetadataModeDoesNotPersistPromptOrOutput(t *testing.T) {
	store := newTestSessionStore(t)
	sessionID := "session-20260717-120003-meta"
	if _, err := store.Create(SessionRecord{SessionID: sessionID, RecordMode: RecordMetadata,
		Retention: RetentionStandard, CaptureQuality: CaptureMetadataOnly}); err != nil {
		t.Fatal(err)
	}
	turnID := "turn-20260717-120003-meta"
	if _, err := store.AddTurn(sessionID, TurnRecord{TurnID: turnID}, "secret prompt", ContextManifest{}); err != nil {
		t.Fatal(err)
	}
	runID := "task-20260717-120003-meta"
	if err := store.AddAttempt(sessionID, RunAttemptRecord{RunID: runID, TurnID: turnID, ExecutionID: "execution-meta"}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRun(sessionID, turnID, runID, StateFailed, "failed", "secret output", nil); err != nil {
		t.Fatal(err)
	}
	messages, err := store.Messages(sessionID, 0, 0)
	if err != nil || len(messages) != 0 {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
}

func TestSessionStoreRecursivelyRedactsSensitiveEventMetadata(t *testing.T) {
	store := newTestSessionStore(t)
	sessionID := "session-20260717-120003-redact"
	if _, err := store.Create(SessionRecord{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	if err := store.AppendEvent(sessionID, SessionEvent{Type: "provider.event", Data: map[string]any{
		"nested": map[string]any{"Authorization": "Bearer secret", "safe": "value"},
	}}); err != nil {
		t.Fatal(err)
	}
	events, err := store.Events(sessionID, 0, 0)
	if err != nil {
		t.Fatal(err)
	}
	nested, _ := events[len(events)-1].Data["nested"].(map[string]any)
	if nested["Authorization"] != "[REDACTED]" || nested["safe"] != "value" {
		t.Fatalf("event data=%#v", events[len(events)-1].Data)
	}
}

func TestSessionStoreExecutionUpdatePreservesRuntimeIdentity(t *testing.T) {
	store := newTestSessionStore(t)
	sessionID := "session-20260717-120004-execution"
	if _, err := store.Create(SessionRecord{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	executionID := "execution-20260717-120004-execution"
	if err := store.UpsertExecution(sessionID, ExecutionRecord{ExecutionID: executionID, Kind: ExecutionTmux,
		Profile: "cx", Provider: "cli", State: StateRunning, CaptureQuality: CaptureTranscriptOnly, TmuxSession: "sn-agent-test"}); err != nil {
		t.Fatal(err)
	}
	if err := store.UpsertExecution(sessionID, ExecutionRecord{ExecutionID: executionID, State: StateDone}); err != nil {
		t.Fatal(err)
	}
	view, err := store.View(sessionID)
	if err != nil || len(view.Executions) != 1 {
		t.Fatalf("view=%#v err=%v", view, err)
	}
	execution := view.Executions[0]
	if execution.Kind != ExecutionTmux || execution.Profile != "cx" || execution.TmuxSession != "sn-agent-test" || execution.CaptureQuality != CaptureTranscriptOnly {
		t.Fatalf("execution identity was lost: %#v", execution)
	}
}

func TestSessionStoreBlockedRunCanResumeWithoutDuplicateCompletion(t *testing.T) {
	store := newTestSessionStore(t)
	sessionID := "session-20260717-120004-resume"
	turnID := "turn-20260717-120004-resume"
	runID := "turn-20260717-120004-run"
	executionID := "execution-20260717-120004-resume"
	if _, err := store.Create(SessionRecord{SessionID: sessionID}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.AddTurn(sessionID, TurnRecord{TurnID: turnID}, "prompt", ContextManifest{}); err != nil {
		t.Fatal(err)
	}
	if err := store.AddAttempt(sessionID, RunAttemptRecord{RunID: runID, RunType: RunTurn, TurnID: turnID, ExecutionID: executionID}); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRun(sessionID, turnID, runID, StateBlocked, "waiting", "waiting", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.CompleteRun(sessionID, turnID, runID, StateBlocked, "waiting", "waiting", nil); err != nil {
		t.Fatal(err)
	}
	if err := store.ResumeRun(sessionID, turnID, runID); err != nil {
		t.Fatal(err)
	}
	view, err := store.View(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if view.Session.State != StateRunning || view.Attempts[0].State != StateRunning || !view.Attempts[0].CompletedAt.IsZero() {
		t.Fatalf("view=%#v", view)
	}
	blockedEvents := 0
	for _, event := range view.Events {
		if event.Type == "run.blocked" {
			blockedEvents++
		}
	}
	if blockedEvents != 1 {
		t.Fatalf("blocked events=%d events=%#v", blockedEvents, view.Events)
	}
}

func TestDecideRecordPolicy(t *testing.T) {
	direct, err := DecideRecordPolicy("cli.profile", RunTask, ExecutionCLIDirect, "", "", "")
	if err != nil || direct.RecordMode != RecordMetadata || direct.CaptureQuality != CaptureMetadataOnly {
		t.Fatalf("direct=%#v err=%v", direct, err)
	}
	oneShot, err := DecideRecordPolicy("cli.task", RunTask, ExecutionCLIManaged, "", "", "")
	if err != nil || oneShot.Retention != RetentionEphemeral {
		t.Fatalf("one-shot=%#v err=%v", oneShot, err)
	}
	managed, err := DecideRecordPolicy("cli.profile", RunTask, ExecutionCLIManaged, "", "", "")
	if err != nil || managed.Retention != RetentionStandard || managed.RecordMode != RecordFull {
		t.Fatalf("managed=%#v err=%v", managed, err)
	}
}

func TestDirectExecutionCreatesMetadataOnlySession(t *testing.T) {
	home := t.TempDir()
	service := New(home)
	manager := NewSessionManager(service)
	record, execution, err := manager.BeginDirectExecution(provider.Config{ID: "cx", Type: provider.TypeCLI}, "project", home, 2)
	if err != nil {
		t.Fatal(err)
	}
	if record.RecordMode != RecordMetadata || record.CaptureQuality != CaptureMetadataOnly || record.Runtime != "cli" || record.Profile != "cx" || execution.Kind != ExecutionCLIDirect {
		t.Fatalf("record=%#v execution=%#v", record, execution)
	}
	if err := manager.CompleteDirectExecution(record.SessionID, execution, 0, nil); err != nil {
		t.Fatal(err)
	}
	view, err := manager.Store().View(record.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if len(view.Messages) != 0 || view.Session.State != StateDone || len(view.Executions) != 1 || view.Executions[0].State != StateDone {
		t.Fatalf("view=%#v", view)
	}
}

func TestServiceImportsLegacyDurableMemoryOnce(t *testing.T) {
	home := t.TempDir()
	legacy := filepath.Join(home, "state", "memory.json")
	if err := os.MkdirAll(filepath.Dir(legacy), 0o700); err != nil {
		t.Fatal(err)
	}
	legacyData := []byte(`[{"id":"legacy","type":"fact","content":"kept","source":"old","created_at":"2026-07-17T00:00:00Z"}]`)
	if err := os.WriteFile(legacy, legacyData, 0o600); err != nil {
		t.Fatal(err)
	}
	service := New(home)
	data, err := os.ReadFile(service.paths.MemoryFile)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != string(legacyData) {
		t.Fatalf("memory import=%s", data)
	}
}

func TestContextManifestIncludesOnlyAuthorizedRuntimeTools(t *testing.T) {
	home := t.TempDir()
	service := New(home)
	manager := NewSessionManager(service)
	decision := RecordDecision{RecordMode: RecordFull, Retention: RetentionStandard, CaptureQuality: CaptureStructured}
	sessionID := "session-20260717-120005-tools"
	if _, err := manager.EnsureSession(sessionID, "project", home, "tools", decision); err != nil {
		t.Fatal(err)
	}
	manifest, err := manager.CompileContext(sessionID, "turn-20260717-120005-tools", home,
		provider.Config{ID: "native", Type: provider.TypeNative}, "hello", []string{"echo", "run-agent"}, nil)
	if err != nil {
		t.Fatal(err)
	}
	if len(manifest.Tools) != 1 || manifest.Tools[0].ID != "echo" {
		t.Fatalf("tools=%#v", manifest.Tools)
	}
}

func TestSessionStoreImportsLegacyMessagesAndEvents(t *testing.T) {
	store := newTestSessionStore(t)
	sessionID := "chat-abcdef123456"
	record, err := store.Import(SessionImport{
		Session: SessionRecord{SessionID: sessionID, ProjectID: "agent", State: StateDone, RecordMode: RecordFull,
			Retention: RetentionStandard, CaptureQuality: CaptureParsed},
		Messages: []SessionMessage{{MessageID: "legacy-user", Role: "user", Kind: "legacy_message", Content: "old prompt"},
			{MessageID: "legacy-assistant", Role: "assistant", Kind: "legacy_message", Content: "old answer"}},
		Events: []SessionEvent{{Type: "legacy.runtime.done"}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if record.SessionID != sessionID {
		t.Fatalf("record=%#v", record)
	}
	view, err := store.View(sessionID)
	if err != nil || len(view.Messages) != 2 || len(view.Events) != 2 {
		t.Fatalf("view=%#v err=%v", view, err)
	}
	if _, err := store.Import(SessionImport{Session: SessionRecord{SessionID: sessionID}}); err == nil {
		t.Fatal("duplicate import was accepted")
	}
}
