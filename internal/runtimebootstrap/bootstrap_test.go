package runtimebootstrap

import (
	"context"
	"os"
	"path/filepath"
	"testing"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/internal/layout"
	runtime "github.com/yy003x/runtime/run"
	sqlitestore "github.com/yy003x/runtime/store/sqlite"
)

func TestRunRecoveryLoaderReconcilesAfterCompositionOnly(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	paths, err := layout.FromHome(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{
		paths.ConfigDir, paths.StateDir, paths.SessionsDir,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(paths.ConfigDir, "api.json"),
		[]byte(`{
			"type":"api",
			"driver":"openai",
			"endpoint":"https://example.invalid/v1/chat/completions",
			"model":"fixture",
			"headers":{"Authorization":"${FIXTURE_API_KEY}"},
			"timeout":"1m"
		}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.RuntimeConfigFile, []byte(`{}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	setup, err := LoadServices(paths, home)
	if err != nil {
		t.Fatal(err)
	}
	submitted, runtimeErr := setup.Runs.Submit(ctx, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api",
		Input:       "cancel across process crash",
		AgentBudget: agent.DefaultBudget(),
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	runID := submitted.ID
	if err := setup.Runs.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		paths.RunDBFile,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(ctx, runID); err != nil {
		t.Fatal(err)
	}
	reserved, err := store.RequestCancel(ctx, runID)
	if err != nil || reserved.State != runtime.StateRunning ||
		!reserved.CancelRequested {
		t.Fatalf("reserved=%#v err=%v", reserved, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	query, err := LoadRunQueryServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := query.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != runtime.StateRunning ||
		!unchanged.CancelRequested ||
		unchanged.SettledSequence != 0 {
		t.Fatalf("query loader mutated durable state: %#v", unchanged)
	}
	if err := query.Runs.Close(); err != nil {
		t.Fatal(err)
	}

	maintenance, err := LoadRunMaintenanceServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err = maintenance.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != runtime.StateRunning ||
		!unchanged.CancelRequested ||
		unchanged.SettledSequence != 0 {
		t.Fatalf("maintenance loader mutated durable state: %#v", unchanged)
	}
	if err := maintenance.Runs.Close(); err != nil {
		t.Fatal(err)
	}

	ordinary, err := LoadServices(paths, home)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err = ordinary.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != runtime.StateRunning ||
		!unchanged.CancelRequested ||
		unchanged.SettledSequence != 0 {
		t.Fatalf("ordinary loader mutated durable state: %#v", unchanged)
	}
	if err := ordinary.Runs.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := LoadServicesWithRunRecovery(paths, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Runs.Close() })
	cancelled, err := recovered.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != runtime.StateCancelled ||
		!cancelled.CancelRequested ||
		cancelled.SettledSequence == 0 {
		t.Fatalf("recovery loader did not converge reservation: %#v", cancelled)
	}
}

func TestRunMaintenanceLoaderClosesStoreAfterSessionFailure(t *testing.T) {
	paths, err := layout.FromHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.SessionsDir, []byte("not a directory"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRunMaintenanceServices(paths); err == nil {
		t.Fatal("maintenance loader accepted a non-directory Session root")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(paths.RunDBFile + suffix); !os.IsNotExist(err) {
			t.Fatalf("Run Store sidecar remained after loader error: %s", suffix)
		}
	}
	store, err := sqlitestore.Open(
		paths.RunDBFile,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatalf("reopen Run Store after loader error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunQueryAndMaintenanceLoadersIgnoreExecutionInputsAndClose(
	t *testing.T,
) {
	ctx := context.Background()
	home := t.TempDir()
	paths, err := layout.FromHome(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(paths.ConfigDir, "api.json"),
		[]byte(`{
			"type":"api",
			"driver":"openai",
			"endpoint":"https://example.invalid/v1/chat/completions",
			"model":"fixture",
			"headers":{"Authorization":"${FIXTURE_API_KEY}"},
			"timeout":"1m"
		}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.RuntimeConfigFile, []byte(`{}`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	setup, err := LoadServices(paths, home)
	if err != nil {
		t.Fatal(err)
	}
	submitted, runtimeErr := setup.Runs.Submit(ctx, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "cancel",
		AgentBudget: agent.DefaultBudget(),
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	cancelID := submitted.ID
	if err := setup.Runs.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(paths.ConfigDir, "api.json"),
		[]byte(`{"type":"api","driver":`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.RuntimeConfigFile, []byte(`{"agent":`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		paths.RunDBFile,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	reconcileID := "run_99999999999999999999999999999999"
	if _, err := store.Create(ctx, reconcileID, runtime.Request{
		Kind: runtime.KindSession, ProfileID: "missing", Input: "done",
		SessionID: "session_99999999999999999999999999999999",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(ctx, reconcileID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Settle(
		ctx, reconcileID, runtime.StateCompleted, []byte(`{"ok":true}`), nil,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	query, err := LoadRunQueryServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := query.Runs.Get(ctx, cancelID); err != nil ||
		value.ID != cancelID {
		_ = query.Runs.Close()
		t.Fatalf("value=%#v err=%v", value, err)
	}
	if err := query.Runs.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := query.Runs.Get(ctx, cancelID); err == nil {
		t.Fatal("Run query Store remained open after Close")
	}

	maintenance, err := LoadRunMaintenanceServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := maintenance.Runs.Cancel(ctx, cancelID)
	if err != nil || cancelled.State != runtime.StateCancelled {
		_ = maintenance.Runs.Close()
		t.Fatalf("cancelled=%#v err=%v", cancelled, err)
	}
	reconciled, runtimeErr := maintenance.Runs.ReconcileRun(
		ctx, reconcileID,
	)
	if runtimeErr != nil ||
		reconciled.State != runtime.StateCompleted {
		_ = maintenance.Runs.Close()
		t.Fatalf("reconciled=%#v err=%v", reconciled, runtimeErr)
	}
	if err := maintenance.Runs.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.Runs.Get(ctx, cancelID); err == nil {
		t.Fatal("Run maintenance Store remained open after Close")
	}
}
