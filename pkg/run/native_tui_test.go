package run_test

import (
	"context"
	"path/filepath"
	"testing"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
	sqlitestore "github.com/yy003x/runtime/pkg/store/sqlite"
)

func TestNativeTUIFailedExecutionIsADurableOutcome(t *testing.T) {
	now := time.Date(2026, 8, 13, 14, 49, 19, 0, time.UTC)
	store, err := sqlitestore.Open(
		filepath.Join(t.TempDir(), "runtime.db"),
		sqlitestore.Options{Now: func() time.Time { return now }},
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := runtime.NewNativeTUIService(runtime.NativeTUIServiceOptions{
		Store: store, Now: func() time.Time { return now },
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if err := service.Close(); err != nil {
			t.Error(err)
		}
	})
	record, running, runtimeErr := service.Begin(
		context.Background(), runtime.NativeTUIBeginRequest{
			SessionID:   "session_11111111111111111111111111111111",
			ExecutionID: "execution_22222222222222222222222222222222",
			ProfileID:   "cc", CWD: t.TempDir(), ConfigDigest: "sha256:fixture",
		},
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	exitCode := 7
	providerErr := &contract.RuntimeError{
		Code: contract.ErrorProviderUnavailable, Phase: contract.PhaseRun,
		Message: "exit status 7",
	}
	terminalExecution := runtime.NewSettledNativeTUIExecution(
		record, "tmux_33333333333333333333333333333333", &exitCode, "",
		runtime.NativeTUIOutcomeFailed, "process_exited", providerErr,
		now.Add(time.Second),
	)
	settled, settleErr := service.Settle(
		context.Background(), terminalExecution,
	)
	if settleErr != nil {
		t.Fatalf("durable failed outcome returned operation error: %v", settleErr)
	}
	if settled.State != runtime.StateFailed || settled.Error == nil ||
		settled.SettledSequence == 0 {
		t.Fatalf("settled Run = %#v", settled)
	}
	decoded, err := runtime.NativeTUIExecutionFromRecord(settled)
	if err != nil {
		t.Fatal(err)
	}
	if running.State != runtime.NativeTUIExecutionRunning ||
		decoded.Outcome != runtime.NativeTUIOutcomeFailed ||
		decoded.ExitCode == nil || *decoded.ExitCode != exitCode ||
		decoded.Error == nil || decoded.Error.Message != providerErr.Message {
		t.Fatalf("running=%#v decoded=%#v", running, decoded)
	}
}
