package session

import (
	"context"
	"encoding/json"
	"errors"
	"math"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yy003x/runtime/pkg/agent"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
	"github.com/yy003x/runtime/pkg/profile"
)

type scriptedGenerator struct {
	mu       sync.Mutex
	results  []contract.ModelResult
	requests []contract.GenerateRequest
}

func TestValidateRunRequestRejectsNonFiniteTemperature(t *testing.T) {
	entry := profile.Entry{ID: "api", Kind: profile.KindModel}
	for _, temperature := range []float64{
		math.NaN(), math.Inf(1), math.Inf(-1),
	} {
		err := validateRunRequest(RunRequest{
			ProfileID: "api",
			Input:     "hello",
			ModelOptions: contract.GenerateOptions{
				Temperature: &temperature,
			},
		}, entry)
		if err == nil {
			t.Fatalf("temperature %v was accepted", temperature)
		}
	}
}

func TestValidateRunRequestRejectsInvalidCommonModelOptions(t *testing.T) {
	entry := profile.Entry{
		ID: "api", Kind: profile.KindModel,
		Model: &model.Profile{Driver: model.DriverOpenAI},
	}
	invalidTopP := 1.1
	for name, options := range map[string]contract.GenerateOptions{
		"top_p":          {TopP: &invalidTopP},
		"stop_sequences": {StopSequences: []string{""}},
	} {
		t.Run(name, func(t *testing.T) {
			err := validateRunRequest(RunRequest{
				ProfileID: "api", Input: "hello", ModelOptions: options,
			}, entry)
			if err == nil {
				t.Fatalf("options=%#v were accepted", options)
			}
		})
	}
}

func TestValidateRunRequestRejectsOutputLimitWithoutInputBudget(t *testing.T) {
	maxOutput := int64(32_767)
	entry := profile.Entry{
		ID: "api", Kind: profile.KindModel,
		Model: &model.Profile{Driver: model.DriverAnthropic},
	}
	err := validateRunRequest(RunRequest{
		ProfileID: "api",
		Input:     "hello",
		ModelOptions: contract.GenerateOptions{
			MaxOutputTokens: &maxOutput,
		},
	}, entry)
	if err == nil || !strings.Contains(err.Error(), "fewer than 2 input tokens") {
		t.Fatalf("error=%v", err)
	}
}

func TestBuildProjectionUsesConservativeContextBudget(t *testing.T) {
	profileDefault := int64(12_000)
	requestLimit := int64(16_000)
	entry := profile.Entry{
		ID: "api", Kind: profile.KindModel,
		Model: &model.Profile{
			Driver: model.DriverAnthropic,
			Parameters: model.Parameters{
				MaxTokens: &profileDefault,
			},
		},
	}
	built, runtimeErr := buildProjection(
		entry,
		"session_77777777777777777777777777777777",
		"turn_77777777777777777777777777777777",
		"run_77777777777777777777777777777777",
		"execution_77777777777777777777777777777777",
		"",
		RunRequest{
			ProfileID: "api", Input: "hello",
			ModelOptions: contract.GenerateOptions{
				MaxOutputTokens: &requestLimit,
			},
		},
		nil,
		time.Unix(1, 0).UTC(),
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	manifest := built.manifest
	if manifest.CapacitySource != "conservative_default" ||
		manifest.ContextWindowTokens != 32_768 ||
		manifest.ReservedOutputTokens != 16_000 ||
		manifest.InputBudgetTokens != 16_768 {
		t.Fatalf("manifest=%#v", manifest)
	}
}

func TestSettleAgentRejectsShortOrForgedCanonicalPrefix(t *testing.T) {
	service := newTestService(t, &scriptedGenerator{}, nil, nil)
	sessionID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	turn, runtimeErr := service.PrepareAgent(RunRequest{
		SessionID: sessionID,
		RunID:     "run_77777777777777777777777777777777",
		ProfileID: "api",
		Input:     "canonical input",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if turn.BaseMessageCount < 1 {
		t.Fatalf("turn=%#v", turn)
	}
	beforeMessages, err := service.Messages(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := service.Events(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	testCases := []struct {
		name     string
		turn     AgentTurn
		messages []contract.Message
	}{
		{
			name: "shorter_than_base",
			turn: turn, messages: cloneMessages(
				turn.Messages[:turn.BaseMessageCount-1],
			),
		},
		{
			name: "forged_runtime_prefix",
			turn: turn,
			messages: func() []contract.Message {
				values := cloneMessages(turn.Messages)
				values[0].Content = "forged runtime prefix"
				return values
			}(),
		},
		{
			name: "forged_turn_and_runtime_prefix",
			turn: func() AgentTurn {
				value := turn
				value.Messages = cloneMessages(turn.Messages)
				value.Messages[0].Content = "forged prepared prefix"
				return value
			}(),
			messages: func() []contract.Message {
				values := cloneMessages(turn.Messages)
				values[0].Content = "forged prepared prefix"
				return values
			}(),
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			_, runtimeErr := service.SettleAgent(
				testCase.turn, testCase.messages,
				agent.Outcome{
					State: agent.StateCompleted,
					Message: &contract.Message{
						Role: contract.RoleAssistant, Content: "must not persist",
					},
				},
			)
			if runtimeErr == nil ||
				runtimeErr.Code != contract.ErrorConflict {
				t.Fatalf("error=%v", runtimeErr)
			}
			afterMessages, err := service.Messages(sessionID, 0)
			if err != nil {
				t.Fatal(err)
			}
			afterEvents, err := service.Events(sessionID, 0)
			if err != nil {
				t.Fatal(err)
			}
			if !reflect.DeepEqual(beforeMessages, afterMessages) ||
				!reflect.DeepEqual(beforeEvents, afterEvents) {
				t.Fatalf(
					"invalid projection mutated Session: messages=%#v events=%#v",
					afterMessages, afterEvents,
				)
			}
		})
	}
}

func TestSettleAgentAndResultForRunExposeOnlyCurrentCompletedMessage(
	t *testing.T,
) {
	testCases := []struct {
		name         string
		outcome      agent.Outcome
		wantState    TurnState
		wantMessage  string
		pausedCallID string
	}{
		{
			name: "completed",
			outcome: agent.Outcome{
				State: agent.StateCompleted, StopReason: "stop",
			},
			wantState: TurnCompleted, wantMessage: "current assistant",
		},
		{
			name: "paused",
			outcome: agent.Outcome{
				State: agent.StatePaused, StopReason: "input_required",
				Pause: &agent.Pause{
					ID: "pause_current", Kind: "approval",
					Prompt: "approve?", ToolCallID: "call_current",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				},
			},
			wantState: TurnRequiresAction, pausedCallID: "call_current",
		},
		{
			name: "failed",
			outcome: agent.Outcome{
				State: agent.StateFailed, StopReason: "model_failed",
				Error: &contract.RuntimeError{
					Code:    contract.ErrorProviderUnavailable,
					Phase:   contract.PhaseProvider,
					Message: "provider failed",
				},
			},
			wantState: TurnFailed,
		},
		{
			name: "cancelled",
			outcome: agent.Outcome{
				State: agent.StateCancelled, StopReason: "cancelled",
				Error: &contract.RuntimeError{
					Code:    contract.ErrorCancelled,
					Phase:   contract.PhaseRun,
					Message: "cancelled",
				},
			},
			wantState: TurnCancelled,
		},
		{
			name: "needs_reconciliation",
			outcome: agent.Outcome{
				State:      agent.StateNeedsReconciliation,
				StopReason: "tool_effect_unknown",
				Error: &contract.RuntimeError{
					Code:    contract.ErrorConflict,
					Phase:   contract.PhaseRun,
					Message: "unknown",
				},
			},
			wantState: TurnRunning,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			service := newTestService(
				t, &scriptedGenerator{}, nil, nil,
			)
			sessionID, err := NewID()
			if err != nil {
				t.Fatal(err)
			}
			historical, runtimeErr := service.PrepareAgent(RunRequest{
				SessionID: sessionID,
				RunID:     "run_11111111111111111111111111111111",
				ProfileID: "api",
				Input:     "historical",
			})
			if runtimeErr != nil {
				t.Fatal(runtimeErr)
			}
			historicalMessages := append(
				cloneMessages(historical.Messages),
				contract.Message{
					Role:    contract.RoleAssistant,
					Content: "historical assistant",
				},
			)
			if _, runtimeErr := service.SettleAgent(
				historical, historicalMessages,
				agent.Outcome{
					State: agent.StateCompleted, StopReason: "stop",
				},
			); runtimeErr != nil {
				t.Fatal(runtimeErr)
			}
			current, runtimeErr := service.PrepareAgent(RunRequest{
				SessionID: sessionID,
				RunID:     "run_22222222222222222222222222222222",
				ProfileID: "api",
				Input:     "current",
			})
			if runtimeErr != nil {
				t.Fatal(runtimeErr)
			}
			currentMessages := cloneMessages(current.Messages)
			if testCase.pausedCallID != "" {
				currentMessages = append(
					currentMessages,
					contract.Message{
						Role: contract.RoleAssistant,
						ToolCalls: []contract.ToolCall{{
							ID:        testCase.pausedCallID,
							Name:      "approval",
							Arguments: json.RawMessage(`{}`),
						}},
					},
				)
			} else {
				currentMessages = append(
					currentMessages,
					contract.Message{
						Role:    contract.RoleAssistant,
						Content: "current assistant",
					},
				)
			}
			immediate, runtimeErr := service.SettleAgent(
				current, currentMessages, testCase.outcome,
			)
			if runtimeErr != nil {
				t.Fatal(runtimeErr)
			}
			persisted, found, err := service.ResultForRun(
				sessionID, current.RunID,
			)
			if err != nil || !found {
				t.Fatalf("persisted=%#v found=%v err=%v", persisted, found, err)
			}
			for label, result := range map[string]RunResult{
				"immediate": immediate,
				"persisted": persisted,
			} {
				if result.State != testCase.wantState {
					t.Fatalf(
						"%s state=%s want=%s",
						label, result.State, testCase.wantState,
					)
				}
				if testCase.wantMessage == "" {
					if result.Message != nil {
						t.Fatalf(
							"%s exposed non-completed message %#v",
							label, result.Message,
						)
					}
				} else if result.Message == nil ||
					result.Message.Content != testCase.wantMessage {
					t.Fatalf(
						"%s message=%#v want=%q",
						label, result.Message, testCase.wantMessage,
					)
				}
			}
		})
	}
}

func TestLookupAgentUsesPersistedProfileFactsAfterCatalogRemoval(t *testing.T) {
	generator := &scriptedGenerator{}
	service := newTestService(t, generator, nil, nil)
	sessionID, err := NewID()
	if err != nil {
		t.Fatal(err)
	}
	prepared, runtimeErr := service.PrepareAgent(RunRequest{
		SessionID: sessionID,
		RunID:     "run_33333333333333333333333333333333",
		ProfileID: "api",
		Input:     "persist profile facts",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	commands, err := runtimecommand.NewCatalog(
		map[string]runtimecommand.Profile{},
	)
	if err != nil {
		t.Fatal(err)
	}
	models, err := model.NewCatalog(map[string]model.Profile{})
	if err != nil {
		t.Fatal(err)
	}
	emptyProfiles, err := profile.NewCatalog(commands, models)
	if err != nil {
		t.Fatal(err)
	}
	recovery, err := NewService(ServiceOptions{
		Store: service.store, Profiles: emptyProfiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	lookedUp, found, runtimeErr := recovery.LookupAgent(RunRequest{
		SessionID: sessionID,
		RunID:     prepared.RunID,
		ProfileID: prepared.ProfileID,
		Input:     "persist profile facts",
	})
	if runtimeErr != nil || !found {
		t.Fatalf(
			"lookedUp=%#v found=%v err=%v",
			lookedUp, found, runtimeErr,
		)
	}
	if lookedUp.ProfileKind != profile.KindModel ||
		lookedUp.RequestDigest == "" ||
		lookedUp.ConfigDigest == "" ||
		lookedUp.RequestDigest != prepared.RequestDigest ||
		lookedUp.ConfigDigest != prepared.ConfigDigest {
		t.Fatalf(
			"persisted Agent facts changed: prepared=%#v lookedUp=%#v",
			prepared, lookedUp,
		)
	}
}

func (generator *scriptedGenerator) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return generator.GenerateStream(ctx, request, nil)
}

func (generator *scriptedGenerator) GenerateStream(
	_ context.Context,
	request contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	generator.mu.Lock()
	defer generator.mu.Unlock()
	generator.requests = append(generator.requests, request)
	if len(generator.results) == 0 {
		return contract.ModelResult{}, &contract.RuntimeError{
			Code: contract.ErrorInternal, Phase: contract.PhaseProvider,
			Message: "script exhausted",
		}
	}
	result := generator.results[0]
	generator.results = generator.results[1:]
	if sink != nil {
		if err := sink(contract.Event{Sequence: 1, Type: contract.EventModelStarted}); err != nil {
			return contract.ModelResult{}, &contract.RuntimeError{
				Code: contract.ErrorCancelled, Phase: contract.PhaseConsumer,
				Message: err.Error(),
			}
		}
		if err := sink(contract.Event{
			Sequence: 2, Type: contract.EventModelCompleted,
			Model: &contract.ModelEvent{Result: &result},
		}); err != nil {
			return contract.ModelResult{}, &contract.RuntimeError{
				Code: contract.ErrorCancelled, Phase: contract.PhaseConsumer,
				Message: err.Error(),
			}
		}
	}
	return result, nil
}

func TestModelSessionPreservesToolRelationsAndIdempotency(t *testing.T) {
	toolCall := contract.ToolCall{
		ID: "call_1", Name: "lookup",
		Arguments: json.RawMessage(`{"key":"value"}`),
	}
	generator := &scriptedGenerator{results: []contract.ModelResult{
		{
			Message: contract.Message{
				Role: contract.RoleAssistant, ToolCalls: []contract.ToolCall{toolCall},
			},
			FinishReason: contract.FinishToolCall,
		},
		{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "done"},
			FinishReason: contract.FinishStop,
		},
	}}
	service := newTestService(t, generator, nil, nil)
	first, runtimeErr := service.Run(context.Background(), RunRequest{
		ProfileID: "api", Input: "first",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if first.State != TurnRequiresAction || len(first.PendingActions) != 1 {
		t.Fatalf("first=%#v", first)
	}
	input := ToolResultInput{
		ToolCallID: "call_1", Content: `{"ok":true}`,
		IdempotencyKey: "idem-1", IsError: true,
	}
	receipt, runtimeErr := service.SubmitToolResult(first.SessionID, first.TurnID, input)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	repeated, runtimeErr := service.SubmitToolResult(first.SessionID, first.TurnID, input)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if repeated != receipt {
		t.Fatalf("repeated=%#v receipt=%#v", repeated, receipt)
	}
	input.Content = `{"ok":false}`
	if _, runtimeErr := service.SubmitToolResult(
		first.SessionID, first.TurnID, input,
	); runtimeErr == nil || runtimeErr.Code != contract.ErrorConflict {
		t.Fatalf("conflict=%v", runtimeErr)
	}
	second, runtimeErr := service.Run(context.Background(), RunRequest{
		SessionID: first.SessionID, ProfileID: "api", Input: "continue",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if second.State != TurnCompleted || second.Message.Content != "done" {
		t.Fatalf("second=%#v", second)
	}
	generator.mu.Lock()
	defer generator.mu.Unlock()
	messages := generator.requests[1].Input.Messages
	if len(messages) != 4 ||
		messages[0].Role != contract.RoleUser ||
		messages[1].Role != contract.RoleAssistant ||
		messages[1].ToolCalls[0].ID != "call_1" ||
		messages[2].Role != contract.RoleTool ||
		messages[2].ToolCallID != "call_1" ||
		!messages[2].IsError ||
		messages[3].Role != contract.RoleUser {
		t.Fatalf("messages=%#v", messages)
	}
}

func TestSessionFinalizationFailureRemainsReconcileable(t *testing.T) {
	tests := []struct {
		name      string
		stage     string
		committed bool
	}{
		{
			name:  "callback rollback",
			stage: "after_mutation_callback",
		},
		{
			name:  "commit marker rollback",
			stage: "before_commit_marker",
		},
		{
			name:      "committed journal cleanup",
			stage:     "before_journal_cleanup",
			committed: true,
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			service := newTestService(t, &scriptedGenerator{}, nil, nil)
			ids := startFinalizationTestTurn(t, service, "api")
			injected := errors.New("injected " + test.stage)
			fired := false
			service.store.mutationErrorpoint = func(
				stage, _ string,
			) error {
				if !fired && stage == test.stage {
					fired = true
					return injected
				}
				return nil
			}

			result, runtimeErr := service.finishModel(ids, contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "done",
				},
				FinishReason: contract.FinishStop,
			})
			if !fired {
				t.Fatalf("errorpoint %q did not fire", test.stage)
			}
			sessionValue, turn, execution := loadFinalizationTestFacts(
				t, service, ids,
			)
			if test.committed {
				if runtimeErr != nil ||
					result.State != TurnCompleted ||
					result.Message == nil ||
					result.Message.Content != "done" ||
					sessionValue.State != SessionIdle ||
					sessionValue.ActiveTurnID != "" ||
					turn.State != TurnCompleted ||
					execution.State != ExecutionSettled ||
					execution.Outcome != OutcomeCompleted {
					t.Fatalf(
						"result=%#v error=%v session=%#v turn=%#v execution=%#v",
						result, runtimeErr, sessionValue, turn, execution,
					)
				}
			} else {
				if runtimeErr == nil ||
					!strings.Contains(runtimeErr.Message, injected.Error()) ||
					result.State != TurnRunning ||
					result.Error == nil ||
					sessionValue.State != SessionBlocked ||
					sessionValue.ActiveTurnID != ids.turn ||
					turn.State != TurnRunning ||
					turn.Error == nil ||
					execution.State != ExecutionRunning ||
					execution.Outcome != "" {
					t.Fatalf(
						"result=%#v error=%v session=%#v turn=%#v execution=%#v",
						result, runtimeErr, sessionValue, turn, execution,
					)
				}
				reconciled, reconcileErr := service.Reconcile(
					context.Background(), ids.session,
					ReconcileOptions{AcknowledgeUnknown: true},
				)
				if reconcileErr != nil ||
					!reconciled.Resolved ||
					reconciled.State != TurnFailed {
					t.Fatalf(
						"reconciled=%#v error=%v",
						reconciled, reconcileErr,
					)
				}
			}
			if _, err := os.Lstat(
				service.store.mutationJournalPath(ids.session),
			); !errors.Is(err, os.ErrNotExist) {
				t.Fatalf("mutation journal still exists: %v", err)
			}
		})
	}
}

func TestManagedCommandFinalizationFailureRemainsReconcileable(t *testing.T) {
	service := newTestService(
		t, &scriptedGenerator{},
		map[string]runtimecommand.Profile{
			"batch": {Command: "codex"},
		},
		nil,
	)
	ids := startFinalizationTestTurn(t, service, "batch")
	fired := false
	service.store.mutationErrorpoint = func(stage, _ string) error {
		if !fired && stage == "after_mutation_callback" {
			fired = true
			return errors.New("injected managed callback failure")
		}
		return nil
	}
	exitCode := 0
	result, runtimeErr := service.finishManagedCommand(ids, managedResult{
		outcome:   OutcomeCompleted,
		assistant: "done",
		exitCode:  &exitCode,
	})
	sessionValue, turn, execution := loadFinalizationTestFacts(
		t, service, ids,
	)
	if !fired ||
		runtimeErr == nil ||
		result.State != TurnRunning ||
		result.Message != nil ||
		result.ExitCode != nil ||
		sessionValue.State != SessionBlocked ||
		sessionValue.ActiveTurnID != ids.turn ||
		turn.State != TurnRunning ||
		execution.State != ExecutionRunning {
		t.Fatalf(
			"result=%#v error=%v session=%#v turn=%#v execution=%#v",
			result, runtimeErr, sessionValue, turn, execution,
		)
	}
}

func TestFailureFinalizationReturnsReconciliationError(t *testing.T) {
	service := newTestService(t, &scriptedGenerator{}, nil, nil)
	ids := startFinalizationTestTurn(t, service, "api")
	fired := false
	service.store.mutationErrorpoint = func(stage, _ string) error {
		if !fired && stage == "after_mutation_callback" {
			fired = true
			return errors.New("injected failure finalization")
		}
		return nil
	}
	original := &contract.RuntimeError{
		Code: contract.ErrorProviderUnavailable, Phase: contract.PhaseProvider,
		Message: "provider unavailable", Retryable: true,
	}
	result, runtimeErr := service.finishFailureResult(ids, original)
	sessionValue, turn, execution := loadFinalizationTestFacts(
		t, service, ids,
	)
	if !fired ||
		result.State != TurnRunning ||
		result.Error == nil ||
		runtimeErr != result.Error ||
		runtimeErr == original ||
		runtimeErr.Code != contract.ErrorInternal ||
		sessionValue.State != SessionBlocked ||
		sessionValue.ActiveTurnID != ids.turn ||
		turn.State != TurnRunning ||
		execution.State != ExecutionRunning {
		t.Fatalf(
			"result=%#v error=%v session=%#v turn=%#v execution=%#v",
			result, runtimeErr, sessionValue, turn, execution,
		)
	}
}

func startFinalizationTestTurn(
	t *testing.T,
	service *Service,
	profileID string,
) executionIDs {
	t.Helper()
	entry, exists := service.profiles.Resolve(profileID)
	if !exists {
		t.Fatalf("profile %q is unavailable", profileID)
	}
	ids, err := service.newExecutionIDs("", "")
	if err != nil {
		t.Fatal(err)
	}
	request := RunRequest{
		ProfileID: profileID,
		Input:     "hello",
	}
	if entry.Kind == profile.KindCommand {
		request.InvocationBase = t.TempDir()
	}
	request, runtimeErr := service.prepareRunRequest(request, entry)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	started, runtimeErr := service.begin(ids, request, entry)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if err := service.markExecutionRunning(started.ids); err != nil {
		t.Fatal(err)
	}
	return started.ids
}

func loadFinalizationTestFacts(
	t *testing.T,
	service *Service,
	ids executionIDs,
) (Session, Turn, Execution) {
	t.Helper()
	var sessionValue Session
	var turn Turn
	var execution Execution
	err := service.store.withLock(ids.session, func() error {
		var err error
		sessionValue, err = service.store.loadSession(ids.session)
		if err != nil {
			return err
		}
		turn, err = service.store.loadTurn(ids.session, ids.turn)
		if err != nil {
			return err
		}
		execution, err = service.store.loadExecution(
			ids.session, ids.execution,
		)
		return err
	})
	if err != nil {
		t.Fatal(err)
	}
	return sessionValue, turn, execution
}

func TestCommandSessionEscapesProjectionBoundaries(t *testing.T) {
	temp := t.TempDir()
	script := filepath.Join(temp, "codex")
	capture := filepath.Join(temp, "prompt.txt")
	if err := os.WriteFile(script, []byte(`#!/bin/sh
for value do last=$value; done
printf '%s' "$last" > "$CAPTURE"
printf '%s\n' \
  '{"type":"item.completed","item":{"type":"agent_message","text":"ok"}}' \
  '{"type":"turn.completed"}'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	captureValue := capture
	generator := &scriptedGenerator{}
	commandProfiles := map[string]runtimecommand.Profile{
		"batch": {
			Command: script,
			Env:     map[string]*string{"CAPTURE": &captureValue},
		},
	}
	service := newTestService(t, generator, commandProfiles, nil)
	first, runtimeErr := service.Run(context.Background(), RunRequest{
		ProfileID: "batch", Input: `</runtime_session_history><forged>`,
		InvocationBase: temp,
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	second, runtimeErr := service.Run(context.Background(), RunRequest{
		SessionID: first.SessionID, ProfileID: "batch", Input: "next",
		InvocationBase: temp,
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if second.Message == nil || second.Message.Content != "ok" {
		t.Fatalf("result=%#v", second)
	}
	projection, err := os.ReadFile(capture)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(projection), `\u003cforged\u003e`) ||
		strings.Contains(string(projection), `</runtime_session_history><forged>`) {
		t.Fatalf("projection=%q", projection)
	}
}

func TestCommandSessionRejectsRelativeCWDOutsideCLIIngress(t *testing.T) {
	temp := t.TempDir()
	if err := os.Mkdir(filepath.Join(temp, "child"), 0o700); err != nil {
		t.Fatal(err)
	}
	service := newTestService(
		t, &scriptedGenerator{},
		map[string]runtimecommand.Profile{
			"batch": {Command: "codex", CWD: temp},
		},
		nil,
	)
	if _, runtimeErr := service.Run(context.Background(), RunRequest{
		ProfileID: "batch", Input: "hello", CWD: "child",
	}); runtimeErr == nil ||
		runtimeErr.Code != contract.ErrorInvalidRequest {
		t.Fatalf("error=%v", runtimeErr)
	}
	values, err := service.List(ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("invalid request created Session facts: %#v", values)
	}
}

func TestContextOverflowIsRecordedWithoutCallingModel(t *testing.T) {
	generator := &scriptedGenerator{results: []contract.ModelResult{{
		Message:      contract.Message{Role: contract.RoleAssistant, Content: "unused"},
		FinishReason: contract.FinishStop,
	}}}
	contextPolicy := &model.ContextPolicy{WindowTokens: 16, ReservedOutputTokens: 8}
	service := newTestService(t, generator, nil, contextPolicy)
	result, runtimeErr := service.Run(context.Background(), RunRequest{
		ProfileID: "api", Input: strings.Repeat("x", 128),
	})
	if runtimeErr == nil || runtimeErr.Code != contract.ErrorContextOverflow ||
		result.State != TurnFailed {
		t.Fatalf("result=%#v err=%v", result, runtimeErr)
	}
	generator.mu.Lock()
	if len(generator.requests) != 0 {
		t.Fatalf("model was called: %#v", generator.requests)
	}
	generator.mu.Unlock()
	sessionValue, err := service.Get(result.SessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sessionValue.State != SessionIdle {
		t.Fatalf("session=%#v", sessionValue)
	}
	events, err := service.Events(result.SessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if events[len(events)-1].Type != "turn.failed" {
		t.Fatalf("events=%#v", events)
	}
	execution, err := service.Execution(result.SessionID, result.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.State != ExecutionSettled ||
		execution.Outcome != OutcomeFailed ||
		execution.Error == nil ||
		execution.Error.Code != contract.ErrorContextOverflow {
		t.Fatalf("execution=%#v", execution)
	}
}

func TestCommandSessionRequiresSuccessfulProcessAndProtocol(t *testing.T) {
	temp := t.TempDir()
	command := filepath.Join(temp, "codex")
	if err := os.WriteFile(command, []byte("#!/bin/sh\nexit 7\n"), 0o700); err != nil {
		t.Fatal(err)
	}
	service := newTestService(t, &scriptedGenerator{}, map[string]runtimecommand.Profile{
		"batch": {Command: command},
	}, nil)
	result, runtimeErr := service.Run(context.Background(), RunRequest{
		ProfileID: "batch", Input: "hello", InvocationBase: temp,
	})
	if runtimeErr == nil || result.State != TurnFailed ||
		result.ExitCode == nil || *result.ExitCode != 7 ||
		result.Message != nil {
		t.Fatalf("result=%#v", result)
	}
}

func TestCommandSessionDoesNotPersistDiagnosticText(t *testing.T) {
	temp := t.TempDir()
	command := filepath.Join(temp, "codex")
	if err := os.WriteFile(command, []byte(`#!/bin/sh
printf '%s\n' 'private-stderr-marker' >&2
printf '%s\n' \
  '{"type":"item.completed","item":{"type":"agent_message","text":"done"}}' \
  '{"type":"turn.completed"}'
`), 0o700); err != nil {
		t.Fatal(err)
	}
	service := newTestService(
		t, &scriptedGenerator{},
		map[string]runtimecommand.Profile{
			"batch": {Command: command},
		},
		nil,
	)
	result, runtimeErr := service.Run(context.Background(), RunRequest{
		ProfileID: "batch", Input: "hello", InvocationBase: temp,
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	execution, err := service.Execution(result.SessionID, result.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.State != ExecutionSettled ||
		execution.Outcome != OutcomeCompleted ||
		execution.Stderr.ObservedBytes == 0 ||
		execution.Stderr.PrefixDigest == "" {
		t.Fatalf("execution=%#v", execution)
	}
	err = filepath.WalkDir(
		service.store.sessionDir(result.SessionID),
		func(path string, entry os.DirEntry, walkErr error) error {
			if walkErr != nil || entry.IsDir() {
				return walkErr
			}
			data, readErr := os.ReadFile(path)
			if readErr != nil {
				return readErr
			}
			if strings.Contains(string(data), "private-stderr-marker") {
				t.Fatalf("diagnostic text leaked into %s", path)
			}
			return nil
		},
	)
	if err != nil {
		t.Fatal(err)
	}
}

func TestCommandSessionCancellationTerminatesManagedProcess(t *testing.T) {
	temp := t.TempDir()
	command := filepath.Join(temp, "codex")
	if err := os.WriteFile(command, []byte(`#!/bin/sh
trap 'exit 0' TERM INT
while :; do
  sleep 1
done
`), 0o700); err != nil {
		t.Fatal(err)
	}
	service := newTestService(
		t, &scriptedGenerator{},
		map[string]runtimecommand.Profile{"batch": {Command: command}},
		nil,
	)
	ctx, cancel := context.WithCancel(context.Background())
	go func() {
		time.Sleep(100 * time.Millisecond)
		cancel()
	}()
	result, runtimeErr := service.Run(ctx, RunRequest{
		ProfileID: "batch", Input: "hello", InvocationBase: temp,
	})
	if runtimeErr == nil ||
		runtimeErr.Code != contract.ErrorCancelled ||
		result.State != TurnCancelled {
		t.Fatalf("result=%#v error=%v", result, runtimeErr)
	}
	execution, err := service.Execution(result.SessionID, result.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.State != ExecutionSettled ||
		execution.Outcome != OutcomeCancelled ||
		execution.Error == nil ||
		execution.Error.Code != contract.ErrorCancelled {
		t.Fatalf("execution=%#v", execution)
	}
}

func TestCommandSessionOutputLimitTerminatesBeforeDecode(t *testing.T) {
	temp := t.TempDir()
	command := filepath.Join(temp, "codex")
	if err := os.WriteFile(command, []byte(`#!/bin/sh
dd if=/dev/zero bs=1024 count=300 1>&2 2>/dev/null
sleep 10
`), 0o700); err != nil {
		t.Fatal(err)
	}
	service := newTestService(
		t, &scriptedGenerator{},
		map[string]runtimecommand.Profile{"batch": {Command: command}},
		nil,
	)
	result, runtimeErr := service.Run(context.Background(), RunRequest{
		ProfileID: "batch", Input: "hello", InvocationBase: temp,
	})
	if runtimeErr == nil ||
		runtimeErr.Code != contract.ErrorContextOverflow ||
		result.State != TurnFailed {
		t.Fatalf("result=%#v error=%v", result, runtimeErr)
	}
	execution, err := service.Execution(result.SessionID, result.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.State != ExecutionSettled ||
		execution.Outcome != OutcomeFailed ||
		!execution.Stderr.Truncated ||
		!execution.Stderr.LimitExceeded ||
		execution.Stderr.ObservedBytes <= maxDiagnosticStderrBytes {
		t.Fatalf("execution=%#v", execution)
	}
}

func TestReconcileRequiresExplicitAcknowledgementForAPIUnknownOutcome(
	t *testing.T,
) {
	service := newTestService(t, &scriptedGenerator{}, nil, nil)
	ids, err := service.newExecutionIDs("", "")
	if err != nil {
		t.Fatal(err)
	}
	now := time.Now().UTC()
	if err := service.store.withLock(ids.session, func() error {
		if err := service.store.writeSession(Session{
			SchemaVersion: SchemaVersion,
			ID:            ids.session,
			Interface:     InterfaceManaged,
			State:         SessionBlocked,
			Retention:     RetentionStandard,
			ActiveTurnID:  ids.turn,
			CreatedAt:     now,
			UpdatedAt:     now,
		}); err != nil {
			return err
		}
		if err := service.store.writeTurn(Turn{
			SchemaVersion: SchemaVersion,
			ID:            ids.turn,
			SessionID:     ids.session,
			RunID:         ids.run,
			ExecutionID:   ids.execution,
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
		return service.store.writeExecution(Execution{
			SchemaVersion: SchemaVersion,
			ID:            ids.execution,
			SessionID:     ids.session,
			TurnID:        ids.turn,
			RunID:         ids.run,
			ProfileID:     "api",
			ProfileKind:   profile.KindModel,
			State:         ExecutionRunning,
			RequestDigest: "sha256:request",
			ConfigDigest:  "sha256:config",
			CreatedAt:     now,
			UpdatedAt:     now,
		})
	}); err != nil {
		t.Fatal(err)
	}
	if _, runtimeErr := service.Reconcile(
		context.Background(), ids.session, ReconcileOptions{},
	); runtimeErr == nil || runtimeErr.Code != contract.ErrorConflict {
		t.Fatalf("default reconcile error=%v", runtimeErr)
	}
	result, runtimeErr := service.Reconcile(
		context.Background(), ids.session,
		ReconcileOptions{AcknowledgeUnknown: true},
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if !result.Resolved || result.State != TurnFailed {
		t.Fatalf("result=%#v", result)
	}
	execution, err := service.Execution(ids.session, ids.execution)
	if err != nil {
		t.Fatal(err)
	}
	if execution.State != ExecutionSettled ||
		execution.Outcome != OutcomeUnknown {
		t.Fatalf("execution=%#v", execution)
	}
	repeated, runtimeErr := service.Reconcile(
		context.Background(), ids.session, ReconcileOptions{},
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if repeated != result {
		t.Fatalf("repeated=%#v result=%#v", repeated, result)
	}
}

func TestStoreRejectsUnsupportedSchema(t *testing.T) {
	root := t.TempDir()
	sessionID := "session_" + strings.Repeat("1", 32)
	sessionDir := filepath.Join(root, "sessions", sessionID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(sessionDir, "session.json"),
		[]byte(`{
  "schema_version": 999,
  "session_id": "`+sessionID+`",
  "state": "idle",
  "retention": "standard",
  "created_at": "2026-07-29T00:00:00Z",
  "updated_at": "2026-07-29T00:00:00Z",
  "message_count": 0,
  "event_count": 0
}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err == nil || !strings.Contains(err.Error(), "unsupported Session schema_version 999") {
		t.Fatalf("error=%v", err)
	}
}

func TestStoreRejectsMixedOrUnknownSessionFacts(t *testing.T) {
	root := t.TempDir()
	sessionID := "session_" + strings.Repeat("2", 32)
	sessionDir := filepath.Join(root, "sessions", sessionID)
	now := time.Now().UTC()
	if err := atomicJSON(filepath.Join(sessionDir, "session.json"), Session{
		SchemaVersion: SchemaVersion,
		ID:            sessionID,
		Interface:     InterfaceManaged,
		State:         SessionIdle,
		Retention:     RetentionStandard,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(sessionDir, "unsupported-execution.json"),
		[]byte(`{"schema_version":999}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	store, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatalf("bounded startup unexpectedly scanned Session facts: %v", err)
	}
	if err := store.Validate(); err == nil ||
		!strings.Contains(err.Error(), "unsupported Session fact") {
		t.Fatalf("error=%v", err)
	}
}

func TestStoreRejectsDuplicateSessionFactFields(t *testing.T) {
	root := t.TempDir()
	sessionID := "session_" + strings.Repeat("8", 32)
	sessionDir := filepath.Join(root, "sessions", sessionID)
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	data := []byte(`{
		"schema_version":3,
		"schema_version":3,
		"id":"` + sessionID + `",
		"interface":"managed",
		"state":"idle",
		"retention":"standard",
		"created_at":"2026-07-30T00:00:00Z",
		"updated_at":"2026-07-30T00:00:00Z"
	}`)
	if err := os.WriteFile(
		filepath.Join(sessionDir, "session.json"), data, 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err == nil || !strings.Contains(err.Error(), "duplicate field") {
		t.Fatalf("duplicate Session fact error=%v", err)
	}
}

func TestStoreIgnoresOwnedAtomicTempFacts(t *testing.T) {
	root := t.TempDir()
	sessionID := "session_" + strings.Repeat("4", 32)
	sessionDir := filepath.Join(root, "sessions", sessionID)
	now := time.Now().UTC()
	if err := atomicJSON(filepath.Join(sessionDir, "session.json"), Session{
		SchemaVersion: SchemaVersion,
		ID:            sessionID,
		Interface:     InterfaceManaged,
		State:         SessionIdle,
		Retention:     RetentionStandard,
		CreatedAt:     now,
		UpdatedAt:     now,
	}, 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(sessionDir, ".runtime-123.tmp"),
		[]byte(`{"partial":true}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err != nil {
		t.Fatal(err)
	}
}

func TestStoreRejectsUnknownSessionState(t *testing.T) {
	root := t.TempDir()
	sessionID := "session_" + strings.Repeat("3", 32)
	now := time.Now().UTC()
	if err := atomicJSON(
		filepath.Join(root, "sessions", sessionID, "session.json"),
		map[string]any{
			"schema_version": SchemaVersion,
			"session_id":     sessionID,
			"interface":      InterfaceManaged,
			"state":          "future_state",
			"retention":      RetentionStandard,
			"created_at":     now,
			"updated_at":     now,
			"message_count":  0,
			"event_count":    0,
		},
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	); err == nil || !strings.Contains(err.Error(), "unsupported Session state") {
		t.Fatalf("error=%v", err)
	}
}

func TestGCOnlyMovesExpiredEphemeralSessions(t *testing.T) {
	now := time.Date(2026, 7, 27, 10, 0, 0, 0, time.UTC)
	generator := &scriptedGenerator{results: []contract.ModelResult{
		{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "one"},
			FinishReason: contract.FinishStop,
		},
		{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "two"},
			FinishReason: contract.FinishStop,
		},
	}}
	service := newTestService(t, generator, nil, nil)
	service.now = func() time.Time { return now }
	ephemeral, runtimeErr := service.Run(context.Background(), RunRequest{
		ProfileID: "api", Input: "one", Retention: RetentionEphemeral,
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	_, runtimeErr = service.Run(context.Background(), RunRequest{
		ProfileID: "api", Input: "two", Retention: RetentionPinned,
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	service.now = func() time.Time { return now.Add(48 * time.Hour) }
	preview, err := service.GC(GCOptions{OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if !preview.DryRun || len(preview.Candidates) != 1 ||
		preview.Candidates[0] != ephemeral.SessionID {
		t.Fatalf("preview=%#v", preview)
	}
	applied, err := service.GC(GCOptions{
		OlderThan: 24 * time.Hour, Apply: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	if len(applied.Moved) != 1 {
		t.Fatalf("applied=%#v", applied)
	}
	if applied.Skipped == nil || len(applied.Skipped) != 0 {
		t.Fatalf("applied skipped=%#v", applied.Skipped)
	}
	data, err := json.Marshal(applied)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"skipped":[]`) {
		t.Fatalf("empty skipped field is not a stable array: %s", data)
	}
}

func TestGCApplyRechecksCandidatesAfterConcurrentChanges(t *testing.T) {
	now := time.Date(2026, 7, 30, 10, 0, 0, 0, time.UTC)
	service := newTestService(t, &scriptedGenerator{}, nil, nil)
	service.now = func() time.Time { return now }

	names := []string{
		"pinned", "active", "blocked", "refreshed", "removed", "eligible",
	}
	sessions := make(map[string]Session, len(names))
	for _, name := range names {
		value, err := service.Create(RetentionEphemeral)
		if err != nil {
			t.Fatal(err)
		}
		err = service.store.withLock(value.ID, func() error {
			value.UpdatedAt = now.Add(-48 * time.Hour)
			return service.store.writeSession(value)
		})
		if err != nil {
			t.Fatal(err)
		}
		sessions[name] = value
	}

	preview, err := service.GC(GCOptions{OlderThan: 24 * time.Hour})
	if err != nil {
		t.Fatal(err)
	}
	if len(preview.Candidates) != len(names) {
		t.Fatalf("preview=%#v", preview)
	}

	// 模拟在 GC 扫描后、应用扫描候选列表前先取得 Session lock 的并发操作。
	if _, err := service.ConfigureRetention(
		sessions["pinned"].ID, RetentionPinned,
	); err != nil {
		t.Fatal(err)
	}
	changeSession := func(name string, state SessionState, updatedAt time.Time) {
		t.Helper()
		sessionID := sessions[name].ID
		if err := service.store.withLock(sessionID, func() error {
			value, err := service.store.loadSession(sessionID)
			if err != nil {
				return err
			}
			value.State = state
			value.UpdatedAt = updatedAt
			return service.store.writeSession(value)
		}); err != nil {
			t.Fatal(err)
		}
	}
	changeSession("active", SessionActive, now.Add(-48*time.Hour))
	changeSession("blocked", SessionBlocked, now.Add(-48*time.Hour))
	changeSession("refreshed", SessionIdle, now)
	if _, err := service.Delete(sessions["removed"].ID); err != nil {
		t.Fatal(err)
	}

	result := GCResult{
		DryRun:     false,
		Candidates: preview.Candidates,
	}
	result.Moved, result.Skipped, err = service.applyGCCandidates(
		preview.Candidates, now.Add(-24*time.Hour),
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.Moved) != 1 || result.Moved[0] != sessions["eligible"].ID {
		t.Fatalf("moved=%v", result.Moved)
	}
	if len(result.Skipped) != 5 {
		t.Fatalf("skipped=%v", result.Skipped)
	}
	skipped := make(map[string]bool, len(result.Skipped))
	for _, sessionID := range result.Skipped {
		skipped[sessionID] = true
	}
	for _, name := range []string{"pinned", "active", "blocked", "refreshed"} {
		sessionID := sessions[name].ID
		if !skipped[sessionID] {
			t.Errorf("%s session %s was not reported as skipped", name, sessionID)
		}
		if _, err := os.Stat(service.store.sessionDir(sessionID)); err != nil {
			t.Errorf("%s session was removed: %v", name, err)
		}
	}
	if !skipped[sessions["removed"].ID] {
		t.Errorf(
			"removed session %s was not reported as skipped",
			sessions["removed"].ID,
		)
	}
	if _, err := os.Stat(
		service.store.sessionDir(sessions["eligible"].ID),
	); !os.IsNotExist(err) {
		t.Fatalf("eligible session still present: %v", err)
	}
	data, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	if !strings.Contains(string(data), `"skipped"`) {
		t.Fatalf("result does not expose skipped candidates: %s", data)
	}
}

func newTestService(
	t *testing.T,
	generator model.Generator,
	commandProfiles map[string]runtimecommand.Profile,
	contextPolicy *model.ContextPolicy,
) *Service {
	t.Helper()
	if commandProfiles == nil {
		commandProfiles = map[string]runtimecommand.Profile{}
	}
	commands, err := runtimecommand.NewCatalog(commandProfiles)
	if err != nil {
		t.Fatal(err)
	}
	modelProfile := model.Profile{
		Driver:   model.DriverOpenAI,
		Endpoint: "https://example.test/v1/chat/completions",
		Model:    "fixture",
		Headers: map[string]string{
			"Authorization": "${FIXTURE_KEY}",
		},
		Timeout: "1m",
	}
	if contextPolicy != nil {
		modelProfile.Context = *contextPolicy
	}
	models, err := model.NewCatalog(map[string]model.Profile{"api": modelProfile})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewCatalog(commands, models)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	t.Setenv("SN_CLI_HOME", root)
	store, err := NewStore(
		filepath.Join(root, "sessions"),
		filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ServiceOptions{
		Store: store, Profiles: profiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	return service
}
