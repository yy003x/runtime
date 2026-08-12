package run

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	runtimecommand "github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
	"github.com/yy003x/runtime/pkg/profile"
	"github.com/yy003x/runtime/pkg/session"
)

type sessionExecutorGenerator struct {
	calls int
}

func (generator *sessionExecutorGenerator) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return generator.GenerateStream(ctx, request, nil)
}

func (generator *sessionExecutorGenerator) GenerateStream(
	_ context.Context,
	_ contract.GenerateRequest,
	_ contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	generator.calls++
	return contract.ModelResult{
		Message: contract.Message{
			Role: contract.RoleAssistant, Content: "done",
		},
		FinishReason: contract.FinishStop,
	}, nil
}

func TestSessionExecutorReconcileRequiresMatchingExecutionEvidence(t *testing.T) {
	generator := &sessionExecutorGenerator{}
	profiles := newSessionExecutorProfiles(t, "https://one.invalid/v1")
	sessions := newSessionExecutorService(t, profiles, generator)
	sessionID := "session_11111111111111111111111111111111"
	runID := "run_11111111111111111111111111111111"
	result, runtimeErr := sessions.Run(context.Background(), session.RunRequest{
		SessionID: sessionID,
		RunID:     runID,
		ProfileID: "api",
		Input:     "hello",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	execution, err := sessions.Execution(sessionID, result.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	executor := &SessionExecutor{Profiles: profiles, Sessions: sessions}
	record := Record{
		ID: runID,
		Request: Request{
			Kind:             KindSession,
			SessionID:        sessionID,
			ProfileID:        "api",
			Input:            "hello",
			RequestDigest:    execution.RequestDigest,
			ConfigDigest:     execution.ConfigDigest,
			BasePromptDigest: execution.BasePromptDigest,
		},
	}
	outcome := executor.Reconcile(context.Background(), record)
	if outcome.State != StateCompleted || outcome.Error != nil {
		t.Fatalf("outcome=%#v", outcome)
	}
	record.Request.ConfigDigest = "sha256:mismatch"
	outcome = executor.Reconcile(context.Background(), record)
	if outcome.State != StateNeedsReconciliation ||
		outcome.Error == nil ||
		outcome.Error.Code != contract.ErrorConflict {
		t.Fatalf("mismatched outcome=%#v", outcome)
	}
}

func TestSessionExecutorRejectsProfileDriftBeforeExecution(t *testing.T) {
	firstProfiles := newSessionExecutorProfiles(
		t, "https://one.invalid/v1",
	)
	firstGenerator := &sessionExecutorGenerator{}
	firstSessions := newSessionExecutorService(
		t, firstProfiles, firstGenerator,
	)
	executor := &SessionExecutor{
		Profiles: firstProfiles, Sessions: firstSessions,
	}
	prepared, err := executor.Prepare(context.Background(), Request{
		Kind:      KindSession,
		SessionID: "session_22222222222222222222222222222222",
		ProfileID: "api",
		Input:     "hello",
	})
	if err != nil {
		t.Fatal(err)
	}
	secondProfiles := newSessionExecutorProfiles(
		t, "https://two.invalid/v1",
	)
	secondGenerator := &sessionExecutorGenerator{}
	executor.Profiles = secondProfiles
	executor.Sessions = newSessionExecutorService(
		t, secondProfiles, secondGenerator,
	)
	outcome := executor.Execute(context.Background(), Record{
		ID:      "run_22222222222222222222222222222222",
		Request: prepared,
	}, nil)
	if outcome.State != StateFailed ||
		outcome.Error == nil ||
		outcome.Error.Code != contract.ErrorConflict {
		t.Fatalf("outcome=%#v", outcome)
	}
	if secondGenerator.calls != 0 {
		t.Fatalf("generator calls=%d", secondGenerator.calls)
	}
}

func TestSessionExecutionOutcomeRequiresReconciliationForRunningFact(
	t *testing.T,
) {
	runtimeErr := &contract.RuntimeError{
		Code: contract.ErrorInternal, Phase: contract.PhaseRun,
		Message: "persist finalization",
	}
	result := session.RunResult{
		SessionID:   "session_44444444444444444444444444444444",
		TurnID:      "turn_44444444444444444444444444444444",
		RunID:       "run_44444444444444444444444444444444",
		ExecutionID: "execution_44444444444444444444444444444444",
		State:       session.TurnRunning,
		Error:       runtimeErr,
	}
	originalErr := &contract.RuntimeError{
		Code: contract.ErrorProviderUnavailable, Phase: contract.PhaseProvider,
		Message: "original provider failure",
	}
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	outcome := sessionExecutionOutcome(result, resultJSON, originalErr)
	if outcome.State != StateNeedsReconciliation ||
		outcome.Error != runtimeErr ||
		string(outcome.Result) != string(resultJSON) {
		t.Fatalf("outcome=%#v", outcome)
	}
}

func TestSessionExecutorPreparePublishesCLIProfileDefaults(t *testing.T) {
	commands, err := runtimecommand.NewCatalog(
		map[string]runtimecommand.Profile{
			"cli": {
				Command: "codex",
				Model:   "gpt-fixture",
				Effort:  runtimecommand.EffortHigh,
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	models, err := model.NewCatalog(map[string]model.Profile{})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewCatalog(commands, models)
	if err != nil {
		t.Fatal(err)
	}
	sessions := newSessionExecutorService(
		t,
		profiles,
		&sessionExecutorGenerator{},
	)
	executor := &SessionExecutor{Profiles: profiles, Sessions: sessions}
	cwd := t.TempDir()

	prepared, err := executor.Prepare(context.Background(), Request{
		Kind:           KindSession,
		SessionID:      "session_33333333333333333333333333333333",
		ProfileID:      "cli",
		Input:          "hello",
		InvocationBase: cwd,
	})
	if err != nil {
		t.Fatal(err)
	}
	if prepared.Model != "gpt-fixture" ||
		prepared.Effort != string(runtimecommand.EffortHigh) ||
		prepared.CWD != cwd ||
		len(prepared.PrivateRequest) == 0 {
		t.Fatalf("prepared=%#v", prepared)
	}
}

func newSessionExecutorProfiles(
	t *testing.T,
	endpoint string,
) *profile.Catalog {
	t.Helper()
	commands, err := runtimecommand.NewCatalog(
		map[string]runtimecommand.Profile{},
	)
	if err != nil {
		t.Fatal(err)
	}
	models, err := model.NewCatalog(map[string]model.Profile{
		"api": {
			Driver:   model.DriverOpenAI,
			Endpoint: endpoint,
			Model:    "fixture",
			Headers: map[string]string{
				"Authorization": "${KEY}",
			},
			Timeout: "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := profile.NewCatalog(commands, models)
	if err != nil {
		t.Fatal(err)
	}
	return result
}

func newSessionExecutorService(
	t *testing.T,
	profiles *profile.Catalog,
	generator *sessionExecutorGenerator,
) *session.Service {
	t.Helper()
	root := t.TempDir()
	store, err := session.NewStore(
		filepath.Join(root, "sessions"),
		filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	result, err := session.NewService(session.ServiceOptions{
		Store: store, Profiles: profiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	return result
}
