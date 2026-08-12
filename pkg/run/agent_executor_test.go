package run_test

import (
	"bytes"
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
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
	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
	sqlitestore "github.com/yy003x/runtime/pkg/store/sqlite"
)

type agentModel struct {
	mu            sync.Mutex
	results       []contract.ModelResult
	requests      []contract.GenerateRequest
	afterGenerate func()
}

type terminalAgentModel struct {
	mu         sync.Mutex
	result     contract.ModelResult
	runtimeErr *contract.RuntimeError
	requests   []contract.GenerateRequest
}

type failToolStartedEventStore struct {
	runtime.Store

	mu             sync.Mutex
	failedEvent    bool
	failNextSettle bool
}

type mutateAfterPrepareStore struct {
	runtime.Store

	once   sync.Once
	mutate func()
}

type agentExecutionCrash string

type terminalCheckpointCrashStore struct {
	runtime.Store

	crashOnce sync.Once
}

func newTerminalCheckpointCrashStore(
	store runtime.Store,
) *terminalCheckpointCrashStore {
	return &terminalCheckpointCrashStore{Store: store}
}

func (store *terminalCheckpointCrashStore) SaveCheckpoint(
	ctx context.Context,
	checkpoint runtime.Checkpoint,
) error {
	if err := store.Store.SaveCheckpoint(ctx, checkpoint); err != nil {
		return err
	}
	var state agent.LoopState
	if json.Unmarshal(checkpoint.State, &state) == nil &&
		state.TerminalOutcome != nil {
		store.crashOnce.Do(func() {
			panic(agentExecutionCrash("after terminal checkpoint commit"))
		})
	}
	return nil
}

type completedToolCrashStore struct {
	runtime.Store

	crashOnce sync.Once
}

func newCompletedToolCrashStore(
	store runtime.Store,
) *completedToolCrashStore {
	return &completedToolCrashStore{Store: store}
}

func (store *completedToolCrashStore) CompleteToolEffect(
	ctx context.Context,
	effect runtime.ToolEffect,
) error {
	if err := store.Store.CompleteToolEffect(ctx, effect); err != nil {
		return err
	}
	store.crashOnce.Do(func() {
		panic(agentExecutionCrash("after completed tool commit"))
	})
	return nil
}

func (store *mutateAfterPrepareStore) PrepareToolEffect(
	ctx context.Context,
	effect runtime.ToolEffect,
	checkpoint runtime.Checkpoint,
) error {
	if err := store.Store.PrepareToolEffect(
		ctx, effect, checkpoint,
	); err != nil {
		return err
	}
	store.once.Do(store.mutate)
	return nil
}

type failPreparedToolStartStore struct {
	runtime.Store

	mu         sync.Mutex
	callID     string
	failedOnce bool
}

type modelCallAckLossStore struct {
	runtime.Store

	mu           sync.Mutex
	startCommit  bool
	finishCommit bool
	failStart    bool
	failFinish   bool
}

type orphanToolEffectStore struct {
	runtime.Store
	effect runtime.ToolEffect
}

type checkpointAckLossStore struct {
	runtime.Store
	commit bool
}

func (store *checkpointAckLossStore) SaveCheckpoint(
	ctx context.Context,
	checkpoint runtime.Checkpoint,
) error {
	if store.commit {
		if err := store.Store.SaveCheckpoint(
			ctx, checkpoint,
		); err != nil {
			return err
		}
	}
	return errors.New("fixture lost checkpoint acknowledgement")
}

func (store *orphanToolEffectStore) ToolEffects(
	context.Context,
	string,
) ([]runtime.ToolEffect, error) {
	return []runtime.ToolEffect{store.effect}, nil
}

func (store *modelCallAckLossStore) StartModelCall(
	ctx context.Context,
	call runtime.ModelCall,
) error {
	store.mu.Lock()
	fail := store.failStart
	if fail {
		store.failStart = false
	}
	commit := store.startCommit
	store.mu.Unlock()
	if !fail {
		return store.Store.StartModelCall(ctx, call)
	}
	if commit {
		if err := store.Store.StartModelCall(ctx, call); err != nil {
			return err
		}
	}
	return errors.New("fixture lost model call start acknowledgement")
}

func (store *modelCallAckLossStore) FinishModelCall(
	ctx context.Context,
	call runtime.ModelCall,
) error {
	store.mu.Lock()
	fail := store.failFinish
	if fail {
		store.failFinish = false
	}
	commit := store.finishCommit
	store.mu.Unlock()
	if !fail {
		return store.Store.FinishModelCall(ctx, call)
	}
	if commit {
		if err := store.Store.FinishModelCall(ctx, call); err != nil {
			return err
		}
	}
	return errors.New("fixture lost model call finish acknowledgement")
}

func (store *failPreparedToolStartStore) PrepareToolEffect(
	ctx context.Context,
	effect runtime.ToolEffect,
	checkpoint runtime.Checkpoint,
) error {
	store.mu.Lock()
	if effect.CallID == store.callID && !store.failedOnce {
		store.failedOnce = true
		store.mu.Unlock()
		if err := store.Store.PrepareToolEffect(
			ctx, effect, checkpoint,
		); err != nil {
			return err
		}
		return errors.New("fixture lost prepared tool acknowledgement")
	}
	store.mu.Unlock()
	return store.Store.PrepareToolEffect(ctx, effect, checkpoint)
}

func (store *failToolStartedEventStore) AppendEvent(
	ctx context.Context,
	runID string,
	event contract.Event,
) (contract.Event, error) {
	store.mu.Lock()
	if event.Type == contract.EventToolStarted && !store.failedEvent {
		store.failedEvent = true
		store.mu.Unlock()
		return contract.Event{}, errors.New("fixture lost tool.started event")
	}
	store.mu.Unlock()
	return store.Store.AppendEvent(ctx, runID, event)
}

func (store *failToolStartedEventStore) Settle(
	ctx context.Context,
	runID string,
	state runtime.State,
	result json.RawMessage,
	runtimeErr *contract.RuntimeError,
) (runtime.Record, error) {
	store.mu.Lock()
	if store.failNextSettle {
		store.failNextSettle = false
		store.mu.Unlock()
		return runtime.Record{}, errors.New("fixture terminal publish failed")
	}
	store.mu.Unlock()
	return store.Store.Settle(ctx, runID, state, result, runtimeErr)
}

func (store *failToolStartedEventStore) failTerminalPublishOnce() {
	store.mu.Lock()
	defer store.mu.Unlock()
	store.failNextSettle = true
}

func (value *agentModel) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return value.GenerateStream(ctx, request, nil)
}

func (*agentModel) ExecutionSnapshot(
	profileID string,
) (model.ExecutionSnapshot, error) {
	return testAgentModelExecutionSnapshot(profileID)
}

func (value *agentModel) GenerateStream(
	_ context.Context,
	request contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	value.mu.Lock()
	defer value.mu.Unlock()
	value.requests = append(value.requests, request)
	result := value.results[0]
	value.results = value.results[1:]
	if sink != nil {
		if err := sink(contract.Event{
			Sequence: 1, Type: contract.EventModelStarted,
		}); err != nil {
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
	if value.afterGenerate != nil {
		value.afterGenerate()
	}
	return result, nil
}

func (value *terminalAgentModel) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return value.GenerateStream(ctx, request, nil)
}

func (*terminalAgentModel) ExecutionSnapshot(
	profileID string,
) (model.ExecutionSnapshot, error) {
	return testAgentModelExecutionSnapshot(profileID)
}

func (value *terminalAgentModel) GenerateStream(
	_ context.Context,
	request contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	value.mu.Lock()
	defer value.mu.Unlock()
	value.requests = append(value.requests, request)
	if sink != nil {
		if err := sink(contract.Event{
			Type: contract.EventModelStarted,
		}); err != nil {
			return contract.ModelResult{}, &contract.RuntimeError{
				Code: contract.ErrorCancelled, Phase: contract.PhaseConsumer,
				Message: err.Error(),
			}
		}
		if value.runtimeErr == nil {
			if err := sink(contract.Event{
				Type: contract.EventModelCompleted,
				Model: &contract.ModelEvent{
					Result: &value.result,
				},
			}); err != nil {
				return contract.ModelResult{}, &contract.RuntimeError{
					Code:    contract.ErrorCancelled,
					Phase:   contract.PhaseConsumer,
					Message: err.Error(),
				}
			}
		}
	}
	return value.result, value.runtimeErr
}

func TestDurableAgentRecoversToolEffectWithoutUnsafeReplay(t *testing.T) {
	testCases := []struct {
		name              string
		effectState       string
		wantRunState      runtime.State
		wantHandlerCalls  int
		wantModelCalls    int
		wantToolResult    string
		wantCheckpointEvt int
		wantStartedEvt    int
		wantCompletedEvt  int
		wantFailedEvt     int
	}{
		{
			name: "prepared", effectState: "prepared",
			wantRunState:     runtime.StateCompleted,
			wantHandlerCalls: 1, wantModelCalls: 1,
			wantToolResult:    "handler-result",
			wantCheckpointEvt: 1, wantStartedEvt: 1, wantCompletedEvt: 1,
		},
		{
			name: "completed", effectState: "completed",
			wantRunState:   runtime.StateCompleted,
			wantModelCalls: 1, wantToolResult: "durable-result",
			wantCheckpointEvt: 1, wantStartedEvt: 1, wantCompletedEvt: 1,
		},
		{
			name:              "completed_without_started_event",
			effectState:       "completed_no_started",
			wantRunState:      runtime.StateNeedsReconciliation,
			wantCheckpointEvt: 1,
		},
		{
			name:           "completed_without_checkpoint_event",
			effectState:    "completed_no_checkpoint",
			wantRunState:   runtime.StateNeedsReconciliation,
			wantStartedEvt: 1,
		},
		{
			name:              "completed_with_checkpoint_after_started",
			effectState:       "completed_reverse_checkpoint",
			wantRunState:      runtime.StateNeedsReconciliation,
			wantCheckpointEvt: 1, wantStartedEvt: 1,
		},
		{
			name: "failed", effectState: "failed",
			wantRunState:      runtime.StateFailed,
			wantCheckpointEvt: 1, wantStartedEvt: 1, wantFailedEvt: 1,
		},
		{
			name: "failed_event_persisted", effectState: "failed_persisted",
			wantRunState:      runtime.StateFailed,
			wantCheckpointEvt: 1, wantStartedEvt: 1, wantFailedEvt: 1,
		},
		{
			name: "started", effectState: "started",
			wantRunState:      runtime.StateNeedsReconciliation,
			wantCheckpointEvt: 1, wantStartedEvt: 1,
		},
		{
			name: "tampered_request", effectState: "tampered",
			wantRunState: runtime.StateNeedsReconciliation,
		},
		{
			name:         "tampered_checkpoint_binding",
			effectState:  "tampered_checkpoint",
			wantRunState: runtime.StateNeedsReconciliation,
		},
		{
			name: "missing_effect", effectState: "missing",
			wantRunState: runtime.StateNeedsReconciliation,
		},
		{
			name: "tampered_profile", effectState: "tampered_profile",
			wantRunState: runtime.StateNeedsReconciliation,
		},
		{
			name: "round_zero", effectState: "round_zero",
			wantRunState: runtime.StateNeedsReconciliation,
		},
		{
			name: "unsupported_loop_schema", effectState: "schema_unsupported",
			wantRunState: runtime.StateNeedsReconciliation,
		},
		{
			name: "pending_call_not_seen", effectState: "seen_missing",
			wantRunState: runtime.StateNeedsReconciliation,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			databasePath := filepath.Join(root, "runtime.db")
			fixedNow := func() time.Time {
				return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
			}
			store, err := sqlitestore.Open(
				databasePath, sqlitestore.Options{
					Now: fixedNow, SkipReconcile: true,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			runID := "run_" + strings.Repeat("7", 32)
			echoDefinition := contract.ToolSpec{
				Name: "echo",
				InputSchema: json.RawMessage(`{
					"type":"object",
					"required":["value"],
					"properties":{"value":{"type":"string"}},
					"additionalProperties":false
				}`),
			}
			storedRequest := prepareStoredAgentRequest(
				t, store,
				runtime.Request{
					Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
					AgentBudget: agent.DefaultBudget(),
				},
				[]contract.ToolSpec{echoDefinition}, nil,
			)
			record, err := store.Create(ctx, runID, storedRequest)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Start(ctx, record.ID); err != nil {
				t.Fatal(err)
			}
			call := contract.ToolCall{
				ID: "call_recovery", Name: "echo",
				Arguments: json.RawMessage(`{"value":"persisted"}`),
			}
			toolRequest := agent.ToolRequest{
				RunID: runID, CallID: call.ID, Name: call.Name,
				IdempotencyKey: testToolIdempotencyKey(runID, call),
				Arguments:      json.RawMessage(`{"value":"persisted"}`),
			}
			checkpointID := "checkpoint_" + strings.Repeat("8", 32)
			toolRequest.CheckpointID = checkpointID
			assistantMessage := contract.Message{
				Role:      contract.RoleAssistant,
				ToolCalls: []contract.ToolCall{call},
			}
			modelCall := runtime.ModelCall{
				ID:    "model_call_" + strings.Repeat("6", 32),
				RunID: runID, Sequence: 1,
				RequestDigest: "sha256:" + strings.Repeat("0", 64),
			}
			setTestModelRequest(
				t, &modelCall, runID,
				[]contract.Message{{
					Role: contract.RoleUser, Content: "start",
				}},
				[]contract.ToolSpec{echoDefinition},
			)
			if err := store.StartModelCall(ctx, modelCall); err != nil {
				t.Fatal(err)
			}
			modelCall.State = "completed"
			modelResult := contract.ModelResult{
				Message:      assistantMessage,
				FinishReason: contract.FinishToolCall,
			}
			setTestModelResult(t, &modelCall, modelResult)
			if err := store.FinishModelCall(ctx, modelCall); err != nil {
				t.Fatal(err)
			}
			if _, err := store.AppendEvent(
				ctx, runID,
				contract.Event{Type: contract.EventModelStarted},
			); err != nil {
				t.Fatal(err)
			}
			if _, err := store.AppendEvent(
				ctx, runID, contract.Event{
					Type:  contract.EventModelCompleted,
					Model: &contract.ModelEvent{Result: &modelResult},
				},
			); err != nil {
				t.Fatal(err)
			}
			stateJSON, err := json.Marshal(agent.LoopState{
				SchemaVersion: 2, RunID: runID, ModelProfile: "api",
				Messages: []contract.Message{
					{Role: contract.RoleUser, Content: "start"},
					assistantMessage,
				},
				BaseMessageCount: 1, Round: 1, ToolCallCount: 1,
				SeenToolCallIDs:           []string{call.ID},
				PendingToolCalls:          []contract.ToolCall{call},
				PendingEffectCheckpointID: checkpointID,
				NextEventSequence:         2,
			})
			if err != nil {
				t.Fatal(err)
			}
			requestJSON, err := json.Marshal(toolRequest)
			if err != nil {
				t.Fatal(err)
			}
			if err := store.PrepareToolEffect(
				ctx,
				runtime.ToolEffect{
					RunID: runID, CallID: call.ID,
					IdempotencyKey: toolRequest.IdempotencyKey,
					Name:           toolRequest.Name,
					Request:        requestJSON,
				},
				runtime.Checkpoint{
					ID: checkpointID, RunID: runID,
					Sequence: 2, State: stateJSON,
				},
			); err != nil {
				t.Fatal(err)
			}
			if testCase.effectState == "completed" ||
				testCase.effectState == "completed_no_started" ||
				testCase.effectState == "completed_no_checkpoint" ||
				testCase.effectState == "completed_reverse_checkpoint" ||
				testCase.effectState == "failed" ||
				testCase.effectState == "failed_persisted" ||
				testCase.effectState == "started" {
				if testCase.effectState != "completed_no_checkpoint" &&
					testCase.effectState != "completed_reverse_checkpoint" {
					if _, err := store.AppendEvent(ctx, runID, contract.Event{
						Type: contract.EventCheckpointCommitted,
						Checkpoint: &contract.CheckpointEvent{
							RunID: runID, CheckpointID: checkpointID,
						},
					}); err != nil {
						t.Fatal(err)
					}
				}
				if err := store.StartToolEffect(ctx, runID, call.ID); err != nil {
					t.Fatal(err)
				}
				if testCase.effectState != "completed_no_started" {
					if _, err := store.AppendEvent(ctx, runID, contract.Event{
						Type: contract.EventToolStarted,
						Tool: &contract.ToolEvent{
							CallID: call.ID, Name: call.Name,
							IdempotencyKey: toolRequest.IdempotencyKey,
						},
					}); err != nil {
						t.Fatal(err)
					}
				}
				if testCase.effectState == "completed_reverse_checkpoint" {
					if _, err := store.AppendEvent(ctx, runID, contract.Event{
						Type: contract.EventCheckpointCommitted,
						Checkpoint: &contract.CheckpointEvent{
							RunID: runID, CheckpointID: checkpointID,
						},
					}); err != nil {
						t.Fatal(err)
					}
				}
			}
			switch testCase.effectState {
			case "completed", "completed_no_started",
				"completed_no_checkpoint",
				"completed_reverse_checkpoint":
				resultJSON, err := json.Marshal(agent.ToolResult{
					Content: "durable-result",
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.CompleteToolEffect(ctx, runtime.ToolEffect{
					RunID: runID, CallID: call.ID, Result: resultJSON,
				}); err != nil {
					t.Fatal(err)
				}
			case "failed", "failed_persisted":
				if err := store.FailToolEffect(ctx, runtime.ToolEffect{
					RunID: runID, CallID: call.ID,
					Error: &contract.RuntimeError{
						Code:    contract.ErrorToolFailed,
						Phase:   contract.PhaseRun,
						Message: "durable tool failure",
					},
				}); err != nil {
					t.Fatal(err)
				}
				if testCase.effectState == "failed_persisted" {
					if _, err := store.AppendEvent(
						ctx, runID, contract.Event{
							Type: contract.EventToolFailed,
							Tool: &contract.ToolEvent{
								CallID: call.ID, Name: call.Name,
								IdempotencyKey: toolRequest.IdempotencyKey,
							},
							Error: &contract.RuntimeError{
								Code:    contract.ErrorToolFailed,
								Phase:   contract.PhaseRun,
								Message: "durable tool failure",
							},
						},
					); err != nil {
						t.Fatal(err)
					}
					var savedState agent.LoopState
					if err := json.Unmarshal(stateJSON, &savedState); err != nil {
						t.Fatal(err)
					}
					savedState.NextEventSequence = 5
					savedJSON, err := json.Marshal(savedState)
					if err != nil {
						t.Fatal(err)
					}
					if err := store.SaveCheckpoint(
						ctx, runtime.Checkpoint{
							ID:    "checkpoint_" + strings.Repeat("9", 32),
							RunID: runID, Sequence: 5, State: savedJSON,
						},
					); err != nil {
						t.Fatal(err)
					}
				}
			case "tampered", "tampered_checkpoint":
				tampered := toolRequest
				if testCase.effectState == "tampered" {
					tampered.Arguments = json.RawMessage(
						`{"value":"tampered"}`,
					)
				} else {
					tampered.CheckpointID =
						"checkpoint_" + strings.Repeat("4", 32)
				}
				tamperedJSON, err := json.Marshal(tampered)
				if err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", databasePath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(
					`UPDATE tool_effects
					    SET request_json = ?
					  WHERE run_id = ? AND call_id = ?`,
					tamperedJSON, runID, call.ID,
				); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			case "missing":
				db, err := sql.Open("sqlite", databasePath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(
					`DELETE FROM tool_effects
					  WHERE run_id = ? AND call_id = ?`,
					runID, call.ID,
				); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			case "tampered_profile", "round_zero", "schema_unsupported",
				"seen_missing":
				var tamperedState agent.LoopState
				if err := json.Unmarshal(stateJSON, &tamperedState); err != nil {
					t.Fatal(err)
				}
				switch testCase.effectState {
				case "tampered_profile":
					tamperedState.ModelProfile = "other"
				case "round_zero":
					tamperedState.Round = 0
				case "schema_unsupported":
					tamperedState.SchemaVersion = 999
				case "seen_missing":
					tamperedState.ToolCallCount = 0
					tamperedState.SeenToolCallIDs = nil
				}
				tamperedJSON, err := json.Marshal(tamperedState)
				if err != nil {
					t.Fatal(err)
				}
				db, err := sql.Open("sqlite", databasePath)
				if err != nil {
					t.Fatal(err)
				}
				if _, err := db.Exec(
					`UPDATE checkpoints
					    SET state_json = ?
					  WHERE checkpoint_id = ?`,
					tamperedJSON, checkpointID,
				); err != nil {
					db.Close()
					t.Fatal(err)
				}
				if err := db.Close(); err != nil {
					t.Fatal(err)
				}
			}
			if testCase.effectState == "started" {
				outcome := (&runtime.AgentExecutor{
					Store: store,
				}).Execute(ctx, record, nil)
				if outcome.State != runtime.StateNeedsReconciliation ||
					outcome.Error == nil ||
					outcome.Error.Code != contract.ErrorConflict ||
					!strings.Contains(
						outcome.Error.Message,
						"tool effect outcome is unknown",
					) {
					t.Fatalf(
						"nil-dependency started recovery=%#v",
						outcome,
					)
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			store, err = sqlitestore.Open(databasePath, sqlitestore.Options{
				Now: fixedNow,
			})
			if err != nil {
				t.Fatal(err)
			}
			handlerCalls := 0
			registry, err := agent.NewRegistry(agent.RegisteredTool{
				Definition: echoDefinition,
				Handler: func(
					_ context.Context,
					request agent.ToolRequest,
				) (agent.ToolResult, error) {
					handlerCalls++
					if string(request.Arguments) != `{"value":"persisted"}` {
						t.Fatalf(
							"handler used checkpoint request: %s",
							request.Arguments,
						)
					}
					return agent.ToolResult{Content: "handler-result"}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			generator := &agentModel{results: []contract.ModelResult{{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "done",
				},
				FinishReason: contract.FinishStop,
			}}}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: buildAgentProfiles(t, false),
						Model:    generator, Tools: registry, Store: store,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { runs.Close() })
			current, err := runs.Get(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if testCase.effectState == "started" {
				if current.State != runtime.StateNeedsReconciliation ||
					current.Error == nil {
					t.Fatalf("started recovery=%#v", current)
				}
			} else {
				if current.State != runtime.StateQueued {
					t.Fatalf("requeued=%#v", current)
				}
				current, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
				if runtimeErr != nil &&
					testCase.wantRunState != runtime.StateFailed &&
					testCase.wantRunState != runtime.StateNeedsReconciliation {
					t.Fatal(runtimeErr)
				}
				if !found {
					t.Fatal("recovered Run was not queued")
				}
				if current.State != testCase.wantRunState {
					t.Fatalf("current=%#v err=%v", current, runtimeErr)
				}
			}
			if handlerCalls != testCase.wantHandlerCalls {
				t.Fatalf(
					"handlerCalls=%d want=%d",
					handlerCalls, testCase.wantHandlerCalls,
				)
			}
			if len(generator.requests) != testCase.wantModelCalls {
				t.Fatalf(
					"modelCalls=%d want=%d",
					len(generator.requests), testCase.wantModelCalls,
				)
			}
			if testCase.wantToolResult != "" {
				messages := generator.requests[0].Input.Messages
				last := messages[len(messages)-1]
				if last.Role != contract.RoleTool ||
					last.ToolCallID != call.ID ||
					last.Content != testCase.wantToolResult {
					t.Fatalf("recovered messages=%#v", messages)
				}
			}
			events, err := runs.Events(ctx, runID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[contract.EventType]int{}
			for index, event := range events {
				if event.Sequence != uint64(index+1) {
					t.Fatalf("events=%#v", events)
				}
				counts[event.Type]++
			}
			if counts[contract.EventCheckpointCommitted] !=
				testCase.wantCheckpointEvt ||
				counts[contract.EventToolStarted] !=
					testCase.wantStartedEvt ||
				counts[contract.EventToolCompleted] !=
					testCase.wantCompletedEvt ||
				counts[contract.EventToolFailed] !=
					testCase.wantFailedEvt {
				t.Fatalf("event counts=%#v events=%#v", counts, events)
			}
		})
	}
}

func TestDurableAgentEventSinkFailureNeedsReconciliation(t *testing.T) {
	testCases := []struct {
		name       string
		result     contract.ModelResult
		tool       *agent.RegisteredTool
		rejectType contract.EventType
		callID     string
	}{
		{
			name: "agent_completed",
			result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "done",
				},
				FinishReason: contract.FinishStop,
			},
			rejectType: contract.EventAgentCompleted,
		},
		{
			name: "tool_completed",
			result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant,
					ToolCalls: []contract.ToolCall{{
						ID: "call_sink_completed", Name: "echo",
						Arguments: json.RawMessage(`{}`),
					}},
				},
				FinishReason: contract.FinishToolCall,
			},
			tool: &agent.RegisteredTool{
				Definition: contract.ToolSpec{
					Name:        "echo",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				},
				Handler: func(
					context.Context,
					agent.ToolRequest,
				) (agent.ToolResult, error) {
					return agent.ToolResult{Content: "durable"}, nil
				},
			},
			rejectType: contract.EventToolCompleted,
			callID:     "call_sink_completed",
		},
		{
			name: "agent_paused",
			result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant,
					ToolCalls: []contract.ToolCall{{
						ID: "call_sink_pause", Name: "approval",
						Arguments: json.RawMessage(`{}`),
					}},
				},
				FinishReason: contract.FinishToolCall,
			},
			tool: &agent.RegisteredTool{
				Definition: contract.ToolSpec{
					Name:        "approval",
					InputSchema: json.RawMessage(`{"type":"object"}`),
				},
				Handler: func(
					context.Context,
					agent.ToolRequest,
				) (agent.ToolResult, error) {
					return agent.ToolResult{Pause: &agent.Pause{
						ID: "pause_sink", Kind: "approval",
						Prompt:      "approve?",
						InputSchema: json.RawMessage(`{"type":"boolean"}`),
					}}, nil
				},
			},
			rejectType: contract.EventAgentPaused,
			callID:     "call_sink_pause",
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlitestore.Open(
				filepath.Join(t.TempDir(), "runtime.db"),
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { store.Close() })
			runID := "run_" + strings.Repeat("2", 32)
			var definitions []contract.ToolSpec
			if testCase.tool != nil {
				definitions = append(definitions, testCase.tool.Definition)
			}
			storedRequest := prepareStoredAgentRequest(
				t, store,
				runtime.Request{
					Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
					AgentBudget: agent.DefaultBudget(),
				},
				definitions, nil,
			)
			record, err := store.Create(ctx, runID, storedRequest)
			if err != nil {
				t.Fatal(err)
			}
			record, err = store.Start(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			var registrations []agent.RegisteredTool
			if testCase.tool != nil {
				registrations = append(registrations, *testCase.tool)
			}
			registry, err := agent.NewRegistry(registrations...)
			if err != nil {
				t.Fatal(err)
			}
			executor := &runtime.AgentExecutor{
				Profiles: buildAgentProfiles(t, false),
				Model: &agentModel{
					results: []contract.ModelResult{testCase.result},
				},
				Tools: registry, Store: store,
			}
			outcome := executor.Execute(
				ctx, record,
				func(event contract.Event) error {
					if event.Type == testCase.rejectType {
						return errors.New("durable event sink failed")
					}
					_, err := store.AppendEvent(ctx, runID, event)
					return err
				},
			)
			if outcome.State != runtime.StateNeedsReconciliation ||
				outcome.Error == nil {
				t.Fatalf("outcome=%#v", outcome)
			}
			modelCalls, err := store.ModelCalls(ctx, runID)
			if err != nil || len(modelCalls) != 1 ||
				modelCalls[0].State != "completed" {
				t.Fatalf("modelCalls=%#v err=%v", modelCalls, err)
			}
			if _, exists, err := store.LatestCheckpoint(
				ctx, runID,
			); err != nil || !exists {
				t.Fatalf("checkpoint exists=%v err=%v", exists, err)
			}
			if testCase.callID != "" {
				effect, exists, err := store.ToolEffect(
					ctx, runID, testCase.callID,
				)
				if err != nil || !exists ||
					effect.State != "completed" {
					t.Fatalf(
						"effect=%#v exists=%v err=%v",
						effect, exists, err,
					)
				}
			}
		})
	}
}

func TestDurableAgentModelCrashWindowsFailClosed(t *testing.T) {
	testCases := []struct {
		name            string
		modelState      string
		checkpointRound int
		modelSequence   int
		wantRunState    runtime.State
		wantErrorCode   contract.ErrorCode
	}{
		{
			name:       "running_without_checkpoint",
			modelState: "running", modelSequence: 1,
			wantRunState:  runtime.StateNeedsReconciliation,
			wantErrorCode: contract.ErrorConflict,
		},
		{
			name:       "completed_without_checkpoint",
			modelState: "completed", modelSequence: 1,
			wantRunState:  runtime.StateNeedsReconciliation,
			wantErrorCode: contract.ErrorConflict,
		},
		{
			name:       "failed_without_checkpoint",
			modelState: "failed", modelSequence: 1,
			wantRunState:  runtime.StateFailed,
			wantErrorCode: contract.ErrorInternal,
		},
		{
			name:       "cancelled_without_checkpoint",
			modelState: "cancelled", modelSequence: 1,
			wantRunState:  runtime.StateCancelled,
			wantErrorCode: contract.ErrorCancelled,
		},
		{
			name: "completed_ahead_of_checkpoint", modelState: "completed",
			checkpointRound: 1, modelSequence: 2,
			wantRunState:  runtime.StateNeedsReconciliation,
			wantErrorCode: contract.ErrorConflict,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			databasePath := filepath.Join(t.TempDir(), "runtime.db")
			fixedNow := func() time.Time {
				return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
			}
			store, err := sqlitestore.Open(
				databasePath, sqlitestore.Options{
					Now: fixedNow, SkipReconcile: true,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			runID := "run_" + strings.Repeat("a", 32)
			storedRequest := prepareStoredAgentRequest(
				t, store,
				runtime.Request{
					Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
					AgentBudget: agent.DefaultBudget(),
				},
				nil, nil,
			)
			record, err := store.Create(ctx, runID, storedRequest)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := store.Start(ctx, record.ID); err != nil {
				t.Fatal(err)
			}
			if testCase.checkpointRound > 0 {
				stateJSON, err := json.Marshal(agent.LoopState{
					SchemaVersion: 2, RunID: runID, ModelProfile: "api",
					Messages: []contract.Message{
						{Role: contract.RoleUser, Content: "start"},
						{Role: contract.RoleAssistant, Content: "stale"},
					},
					BaseMessageCount: 1, Round: testCase.checkpointRound,
					TerminalOutcome: &agent.Outcome{
						State: agent.StateCompleted, StopReason: "stop",
						Message: &contract.Message{
							Role: contract.RoleAssistant, Content: "stale",
						},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				if err := store.SaveCheckpoint(ctx, runtime.Checkpoint{
					ID:    "checkpoint_" + strings.Repeat("b", 32),
					RunID: runID, State: stateJSON,
				}); err != nil {
					t.Fatal(err)
				}
			}
			for sequence := 1; sequence <= testCase.modelSequence; sequence++ {
				call := runtime.ModelCall{
					ID:    "model_call_" + fmt.Sprintf("%032x", sequence),
					RunID: runID, Sequence: sequence,
					RequestDigest: "sha256:" + strings.Repeat("0", 64),
				}
				setTestModelRequest(
					t, &call, runID,
					[]contract.Message{{
						Role: contract.RoleUser, Content: "start",
					}},
					nil,
				)
				if err := store.StartModelCall(ctx, call); err != nil {
					t.Fatal(err)
				}
				state := "completed"
				if sequence == testCase.modelSequence {
					state = testCase.modelState
				}
				if state != "running" {
					call.State = state
					if state == "completed" {
						setTestModelResult(t, &call, contract.ModelResult{
							Message: contract.Message{
								Role:    contract.RoleAssistant,
								Content: fmt.Sprintf("result-%d", sequence),
							},
							FinishReason: contract.FinishStop,
						})
					} else {
						code := contract.ErrorInternal
						if state == "cancelled" {
							code = contract.ErrorCancelled
						}
						call.Error = &contract.RuntimeError{
							Code: code, Phase: contract.PhaseProvider,
							Message: "fixture model failure",
						}
					}
					if err := store.FinishModelCall(ctx, call); err != nil {
						t.Fatal(err)
					}
				}
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}
			store, err = sqlitestore.Open(databasePath, sqlitestore.Options{
				Now: fixedNow,
			})
			if err != nil {
				t.Fatal(err)
			}
			generator := &agentModel{}
			registry, err := agent.NewRegistry()
			if err != nil {
				t.Fatal(err)
			}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: buildAgentProfiles(t, false),
						Model:    generator, Tools: registry, Store: store,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { runs.Close() })
			current, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
			if !found || runtimeErr == nil ||
				runtimeErr.Code != testCase.wantErrorCode ||
				current.State != testCase.wantRunState {
				t.Fatalf(
					"current=%#v found=%v err=%v",
					current, found, runtimeErr,
				)
			}
			if len(generator.requests) != 0 {
				t.Fatalf("provider was replayed: %#v", generator.requests)
			}
			if testCase.wantRunState == runtime.StateFailed ||
				testCase.wantRunState == runtime.StateCancelled {
				if current.SettledSequence == 0 ||
					len(current.Result) == 0 {
					t.Fatalf("terminal recovery was not durably settled: %#v", current)
				}
				checkpoint, exists, err := store.LatestCheckpoint(ctx, runID)
				if err != nil || !exists {
					t.Fatalf(
						"terminal checkpoint=%#v exists=%v err=%v",
						checkpoint, exists, err,
					)
				}
				var state agent.LoopState
				if err := json.Unmarshal(checkpoint.State, &state); err != nil {
					t.Fatal(err)
				}
				if state.TerminalOutcome == nil ||
					(testCase.wantRunState == runtime.StateFailed &&
						state.TerminalOutcome.State != agent.StateFailed) ||
					(testCase.wantRunState == runtime.StateCancelled &&
						state.TerminalOutcome.State != agent.StateCancelled) {
					t.Fatalf("terminal checkpoint state=%#v", state)
				}
			}
		})
	}
}

func TestRecordingModelAcknowledgementLossUsesDurableTerminalFacts(
	t *testing.T,
) {
	testCases := []struct {
		name           string
		failStart      bool
		startCommit    bool
		failFinish     bool
		finishCommit   bool
		modelError     *contract.RuntimeError
		wantState      runtime.State
		wantModelCalls int
	}{
		{
			name: "start_not_committed", failStart: true,
			wantState: runtime.StateCompleted, wantModelCalls: 1,
		},
		{
			name:      "start_committed_ack_lost",
			failStart: true, startCommit: true,
			wantState: runtime.StateCompleted, wantModelCalls: 1,
		},
		{
			name:       "finish_not_committed",
			failFinish: true,
			modelError: &contract.RuntimeError{
				Code: contract.ErrorInternal, Phase: contract.PhaseProvider,
				Message: "provider failed",
			},
			wantState:      runtime.StateFailed,
			wantModelCalls: 1,
		},
		{
			name:       "failed_finish_committed_ack_lost",
			failFinish: true, finishCommit: true,
			modelError: &contract.RuntimeError{
				Code:  contract.ErrorProviderUnavailable,
				Phase: contract.PhaseProvider, Message: "provider failed",
			},
			wantState:      runtime.StateFailed,
			wantModelCalls: 1,
		},
		{
			name:       "cancelled_finish_committed_ack_lost",
			failFinish: true, finishCommit: true,
			modelError: &contract.RuntimeError{
				Code: contract.ErrorTimeout, Phase: contract.PhaseProvider,
				Message: "provider timed out",
			},
			wantState:      runtime.StateCancelled,
			wantModelCalls: 1,
		},
		{
			name:       "completed_finish_committed_ack_lost",
			failFinish: true, finishCommit: true,
			wantState:      runtime.StateCompleted,
			wantModelCalls: 1,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlitestore.Open(
				filepath.Join(t.TempDir(), "runtime.db"),
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			storedRequest := prepareStoredAgentRequest(
				t, store,
				runtime.Request{
					Kind: runtime.KindAgent, ProfileID: "api",
					Input: "start", AgentBudget: agent.DefaultBudget(),
				},
				nil, nil,
			)
			record, err := store.Create(
				ctx,
				"run_1234567890abcdef1234567890abcdef",
				storedRequest,
			)
			if err != nil {
				t.Fatal(err)
			}
			record, err = store.Start(ctx, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := agent.NewRegistry()
			if err != nil {
				t.Fatal(err)
			}
			generator := &terminalAgentModel{
				result: contract.ModelResult{
					Message: contract.Message{
						Role: contract.RoleAssistant, Content: "completed",
					},
					FinishReason: contract.FinishStop,
				},
				runtimeErr: testCase.modelError,
			}
			ackStore := &modelCallAckLossStore{
				Store:     store,
				failStart: testCase.failStart, startCommit: testCase.startCommit,
				failFinish: testCase.failFinish, finishCommit: testCase.finishCommit,
			}
			executor := &runtime.AgentExecutor{
				Profiles: buildAgentProfiles(t, false),
				Model:    generator, Tools: registry, Store: ackStore,
			}
			first := executor.Execute(
				ctx, record,
				func(event contract.Event) error {
					_, err := store.AppendEvent(ctx, record.ID, event)
					return err
				},
			)
			if first.State != testCase.wantState ||
				len(generator.requests) != testCase.wantModelCalls {
				t.Fatalf("first=%#v", first)
			}
			if testCase.wantState == runtime.StateFailed ||
				testCase.wantState == runtime.StateCancelled {
				if first.Error == nil ||
					!reflect.DeepEqual(first.Error, testCase.modelError) {
					t.Fatalf("terminal outcome=%#v", first)
				}
			} else if first.Error != nil {
				t.Fatalf("completed outcome=%#v", first)
			}
			checkpoint, exists, err := store.LatestCheckpoint(
				ctx, record.ID,
			)
			if err != nil || !exists {
				t.Fatalf(
					"checkpoint=%#v exists=%v err=%v",
					checkpoint, exists, err,
				)
			}
			var state agent.LoopState
			if err := json.Unmarshal(checkpoint.State, &state); err != nil {
				t.Fatal(err)
			}
			if state.TerminalOutcome == nil ||
				(testCase.wantState == runtime.StateCompleted &&
					state.TerminalOutcome.State != agent.StateCompleted) ||
				(testCase.wantState == runtime.StateFailed &&
					state.TerminalOutcome.State != agent.StateFailed) ||
				(testCase.wantState == runtime.StateCancelled &&
					state.TerminalOutcome.State != agent.StateCancelled) {
				t.Fatalf("terminal state=%#v", state)
			}
		})
	}
}

func TestTerminalCheckpointAcknowledgementLossPreservesKnownOutcome(
	t *testing.T,
) {
	testCases := []struct {
		name         string
		commit       bool
		model        *terminalAgentModel
		wantState    runtime.State
		wantTerminal agent.State
	}{
		{
			name: "completed_committed", commit: true,
			model: &terminalAgentModel{result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "completed",
				},
				FinishReason: contract.FinishStop,
			}},
			wantState: runtime.StateCompleted, wantTerminal: agent.StateCompleted,
		},
		{
			name: "failed_committed", commit: true,
			model: &terminalAgentModel{runtimeErr: &contract.RuntimeError{
				Code:  contract.ErrorProviderUnavailable,
				Phase: contract.PhaseProvider, Message: "provider failed",
			}},
			wantState: runtime.StateFailed, wantTerminal: agent.StateFailed,
		},
		{
			name: "cancelled_committed", commit: true,
			model: &terminalAgentModel{runtimeErr: &contract.RuntimeError{
				Code: contract.ErrorTimeout, Phase: contract.PhaseProvider,
				Message: "provider timed out",
			}},
			wantState:    runtime.StateCancelled,
			wantTerminal: agent.StateCancelled,
		},
		{
			name: "completed_not_committed",
			model: &terminalAgentModel{result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "completed",
				},
				FinishReason: contract.FinishStop,
			}},
			wantState: runtime.StateNeedsReconciliation,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			store, err := sqlitestore.Open(
				filepath.Join(t.TempDir(), "runtime.db"),
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = store.Close() })
			storedRequest := prepareStoredAgentRequest(
				t, store,
				runtime.Request{
					Kind: runtime.KindAgent, ProfileID: "api",
					Input: "start", AgentBudget: agent.DefaultBudget(),
				},
				nil, nil,
			)
			record, err := store.Create(
				ctx,
				"run_fedcbafedcbafedcbafedcbafedcbafe",
				storedRequest,
			)
			if err != nil {
				t.Fatal(err)
			}
			record, err = store.Start(ctx, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := agent.NewRegistry()
			if err != nil {
				t.Fatal(err)
			}
			faultStore := &checkpointAckLossStore{
				Store: store, commit: testCase.commit,
			}
			outcome := (&runtime.AgentExecutor{
				Profiles: buildAgentProfiles(t, false),
				Model:    testCase.model, Tools: registry,
				Store: faultStore,
			}).Execute(
				ctx, record,
				func(event contract.Event) error {
					_, err := store.AppendEvent(ctx, record.ID, event)
					return err
				},
			)
			if outcome.State != testCase.wantState ||
				len(testCase.model.requests) != 1 {
				t.Fatalf(
					"outcome=%#v requests=%d",
					outcome, len(testCase.model.requests),
				)
			}
			checkpoint, exists, err := store.LatestCheckpoint(
				ctx, record.ID,
			)
			if err != nil || exists != testCase.commit {
				t.Fatalf(
					"checkpoint=%#v exists=%v err=%v",
					checkpoint, exists, err,
				)
			}
			if testCase.commit {
				var state agent.LoopState
				if err := json.Unmarshal(
					checkpoint.State, &state,
				); err != nil {
					t.Fatal(err)
				}
				if state.TerminalOutcome == nil ||
					state.TerminalOutcome.State !=
						testCase.wantTerminal {
					t.Fatalf("state=%#v", state)
				}
			} else if outcome.Error == nil ||
				outcome.Error.Code != contract.ErrorInternal {
				t.Fatalf("uncommitted outcome=%#v", outcome)
			}
		})
	}
}

func TestRecordingModelAckLossSettlesThroughRunService(t *testing.T) {
	testCases := []struct {
		name         string
		failStart    bool
		startCommit  bool
		failFinish   bool
		finishCommit bool
		model        *terminalAgentModel
		wantState    runtime.State
	}{
		{
			name:      "start_committed",
			failStart: true, startCommit: true,
			model: &terminalAgentModel{result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "completed",
				},
				FinishReason: contract.FinishStop,
			}},
			wantState: runtime.StateCompleted,
		},
		{
			name:       "completed_finish_committed",
			failFinish: true, finishCommit: true,
			model: &terminalAgentModel{result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "completed",
				},
				FinishReason: contract.FinishStop,
			}},
			wantState: runtime.StateCompleted,
		},
		{
			name:       "failed_finish_committed",
			failFinish: true, finishCommit: true,
			model: &terminalAgentModel{runtimeErr: &contract.RuntimeError{
				Code:  contract.ErrorProviderUnavailable,
				Phase: contract.PhaseProvider, Message: "provider failed",
			}},
			wantState: runtime.StateFailed,
		},
		{
			name:       "cancelled_finish_committed",
			failFinish: true, finishCommit: true,
			model: &terminalAgentModel{runtimeErr: &contract.RuntimeError{
				Code: contract.ErrorTimeout, Phase: contract.PhaseProvider,
				Message: "provider timed out",
			}},
			wantState: runtime.StateCancelled,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			sqliteStore, err := sqlitestore.Open(
				filepath.Join(t.TempDir(), "runtime.db"),
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			store := &modelCallAckLossStore{
				Store:     sqliteStore,
				failStart: testCase.failStart, startCommit: testCase.startCommit,
				failFinish: testCase.failFinish, finishCommit: testCase.finishCommit,
			}
			registry, err := agent.NewRegistry()
			if err != nil {
				t.Fatal(err)
			}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: buildAgentProfiles(t, false),
						Model:    testCase.model, Tools: registry,
						Store: store,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runs.Close() })
			record, runtimeErr := runs.RunNow(ctx, runtime.Request{
				Kind: runtime.KindAgent, ProfileID: "api",
				Input: "ack loss",
			}, nil)
			if record.State != testCase.wantState ||
				record.State == runtime.StateNeedsReconciliation ||
				record.SettledSequence == 0 ||
				len(testCase.model.requests) != 1 {
				t.Fatalf(
					"record=%#v error=%#v requests=%d",
					record, runtimeErr, len(testCase.model.requests),
				)
			}
			if testCase.wantState == runtime.StateCompleted {
				if runtimeErr != nil || record.Error != nil {
					t.Fatalf("completed=%#v error=%#v", record, runtimeErr)
				}
			} else if !reflect.DeepEqual(
				runtimeErr, testCase.model.runtimeErr,
			) || !reflect.DeepEqual(
				record.Error, testCase.model.runtimeErr,
			) {
				t.Fatalf("terminal=%#v error=%#v", record, runtimeErr)
			}
		})
	}
}

func TestFinishCancelledSettlesBoundAndUnboundAgentRuns(t *testing.T) {
	for _, bound := range []bool{false, true} {
		name := "unbound"
		if bound {
			name = "bound"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			profiles := buildAgentProfiles(t, false)
			generator := &terminalAgentModel{result: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "partial",
				},
				FinishReason: contract.FinishCancelled,
			}}
			registry, err := agent.NewRegistry()
			if err != nil {
				t.Fatal(err)
			}
			store, err := sqlitestore.Open(
				filepath.Join(root, "runtime.db"),
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			var sessions *session.Service
			sessionID := ""
			if bound {
				sessionStore, err := session.NewStore(
					filepath.Join(root, "sessions"),
					filepath.Join(root, "session-state"),
				)
				if err != nil {
					t.Fatal(err)
				}
				sessions, err = session.NewService(session.ServiceOptions{
					Store: sessionStore, Profiles: profiles, Models: generator,
				})
				if err != nil {
					t.Fatal(err)
				}
				sessionID, err = session.NewID()
				if err != nil {
					t.Fatal(err)
				}
			}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: profiles, Model: generator,
						Tools: registry, Store: store, Sessions: sessions,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runs.Close() })
			cancelled, runtimeErr := runs.RunNow(ctx, runtime.Request{
				Kind: runtime.KindAgent, ProfileID: "api",
				Input: "cancel generation", SessionID: sessionID,
			}, nil)
			expectedError := &contract.RuntimeError{
				Code: contract.ErrorCancelled, Phase: contract.PhaseRun,
				Message: "model generation was cancelled",
			}
			if cancelled.State != runtime.StateCancelled ||
				!reflect.DeepEqual(runtimeErr, expectedError) ||
				!reflect.DeepEqual(cancelled.Error, expectedError) ||
				cancelled.SettledSequence == 0 ||
				len(generator.requests) != 1 {
				t.Fatalf(
					"cancelled=%#v error=%#v requests=%d",
					cancelled, runtimeErr, len(generator.requests),
				)
			}
			modelCalls, err := store.ModelCalls(ctx, cancelled.ID)
			if err != nil || len(modelCalls) != 1 ||
				modelCalls[0].State != "completed" {
				t.Fatalf("modelCalls=%#v err=%v", modelCalls, err)
			}
			var durableResult contract.ModelResult
			if err := json.Unmarshal(
				modelCalls[0].Result, &durableResult,
			); err != nil {
				t.Fatal(err)
			}
			if durableResult.FinishReason != contract.FinishCancelled {
				t.Fatalf("durableResult=%#v", durableResult)
			}
			events, err := store.Events(ctx, cancelled.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			for _, event := range events {
				if event.Type == contract.EventAgentCompleted ||
					event.Type == contract.EventRunCompleted {
					t.Fatalf(
						"FinishCancelled emitted completion: %#v",
						events,
					)
				}
			}
			if bound {
				result, found, err := sessions.ResultForRun(
					sessionID, cancelled.ID,
				)
				if err != nil || !found ||
					result.State != session.TurnCancelled ||
					!reflect.DeepEqual(result.Error, expectedError) {
					t.Fatalf(
						"session result=%#v found=%v err=%v",
						result, found, err,
					)
				}
				execution, err := sessions.Execution(
					sessionID, result.ExecutionID,
				)
				if err != nil ||
					execution.Outcome != session.OutcomeCancelled ||
					!reflect.DeepEqual(execution.Error, expectedError) {
					t.Fatalf("execution=%#v err=%v", execution, err)
				}
			}
		})
	}
}

func TestTerminalCheckpointRecoversAfterSessionSettledBeforeRun(
	t *testing.T,
) {
	testCases := []struct {
		name             string
		modelResult      contract.ModelResult
		modelError       *contract.RuntimeError
		wantRunState     runtime.State
		wantTurnState    session.TurnState
		wantSessionState session.ExecutionOutcome
	}{
		{
			name: "completed",
			modelResult: contract.ModelResult{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "completed",
				},
				FinishReason: contract.FinishStop,
			},
			wantRunState:     runtime.StateCompleted,
			wantTurnState:    session.TurnCompleted,
			wantSessionState: session.OutcomeCompleted,
		},
		{
			name: "failed",
			modelError: &contract.RuntimeError{
				Code:  contract.ErrorProviderUnavailable,
				Phase: contract.PhaseProvider, Message: "provider failed",
			},
			wantRunState:     runtime.StateFailed,
			wantTurnState:    session.TurnFailed,
			wantSessionState: session.OutcomeFailed,
		},
		{
			name: "cancelled",
			modelError: &contract.RuntimeError{
				Code: contract.ErrorTimeout, Phase: contract.PhaseProvider,
				Message: "provider timed out",
			},
			wantRunState:     runtime.StateCancelled,
			wantTurnState:    session.TurnCancelled,
			wantSessionState: session.OutcomeCancelled,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			databasePath := filepath.Join(root, "runtime.db")
			profiles := buildAgentProfiles(t, false)
			generator := &terminalAgentModel{
				result: testCase.modelResult, runtimeErr: testCase.modelError,
			}
			registry, err := agent.NewRegistry()
			if err != nil {
				t.Fatal(err)
			}
			sessionStore, err := session.NewStore(
				filepath.Join(root, "sessions"),
				filepath.Join(root, "session-state"),
			)
			if err != nil {
				t.Fatal(err)
			}
			sessions, err := session.NewService(session.ServiceOptions{
				Store: sessionStore, Profiles: profiles, Models: generator,
			})
			if err != nil {
				t.Fatal(err)
			}
			sessionID, err := session.NewID()
			if err != nil {
				t.Fatal(err)
			}
			sqliteStore, err := sqlitestore.Open(
				databasePath,
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			store := &terminalSettleAckStore{
				Store: sqliteStore, failures: 2,
			}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: profiles, Model: generator,
						Tools: registry, Store: store, Sessions: sessions,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			first, runtimeErr := runs.RunNow(ctx, runtime.Request{
				Kind: runtime.KindAgent, ProfileID: "api",
				Input: "terminal crash window", SessionID: sessionID,
			}, nil)
			if runtimeErr == nil ||
				runtimeErr.Code != contract.ErrorInternal ||
				first.ID == "" ||
				first.State != runtime.StateRunning {
				t.Fatalf("first=%#v error=%#v", first, runtimeErr)
			}
			runID := first.ID
			sessionResult, found, err := sessions.ResultForRun(
				sessionID, runID,
			)
			if err != nil || !found ||
				sessionResult.State != testCase.wantTurnState ||
				!reflect.DeepEqual(
					sessionResult.Error, testCase.modelError,
				) {
				t.Fatalf(
					"pre-recovery Session=%#v found=%v err=%v",
					sessionResult, found, err,
				)
			}
			reserved, err := runs.Cancel(ctx, runID)
			if err != nil ||
				reserved.State != runtime.StateRunning ||
				!reserved.CancelRequested {
				t.Fatalf("reserved=%#v err=%v", reserved, err)
			}
			if err := runs.Close(); err != nil {
				t.Fatal(err)
			}

			sqliteStore, err = sqlitestore.Open(
				databasePath, sqlitestore.Options{},
			)
			if err != nil {
				t.Fatal(err)
			}
			runs, err = runtime.NewService(runtime.ServiceOptions{
				Store: sqliteStore,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: profiles, Model: generator,
						Tools: registry, Store: sqliteStore,
						Sessions: sessions,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runs.Close() })
			if err := runs.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			recovered, err := runs.Get(ctx, runID)
			if err != nil ||
				recovered.State != testCase.wantRunState ||
				!reflect.DeepEqual(recovered.Error, testCase.modelError) ||
				recovered.SettledSequence == 0 ||
				len(generator.requests) != 1 {
				t.Fatalf(
					"recovered=%#v error=%v requests=%d",
					recovered, err, len(generator.requests),
				)
			}
			recoveredResult, found, err := sessions.ResultForRun(
				sessionID, runID,
			)
			if err != nil || !found ||
				recoveredResult.State != testCase.wantTurnState ||
				!reflect.DeepEqual(
					recoveredResult.Error, testCase.modelError,
				) {
				t.Fatalf(
					"recovered Session=%#v found=%v err=%v",
					recoveredResult, found, err,
				)
			}
			execution, err := sessions.Execution(
				sessionID, recoveredResult.ExecutionID,
			)
			if err != nil ||
				execution.Outcome != testCase.wantSessionState ||
				!reflect.DeepEqual(execution.Error, testCase.modelError) {
				t.Fatalf("execution=%#v err=%v", execution, err)
			}
			sessionValue, err := sessions.Get(sessionID)
			if err != nil ||
				sessionValue.State != session.SessionIdle ||
				sessionValue.ActiveTurnID != "" {
				t.Fatalf("Session=%#v err=%v", sessionValue, err)
			}
			settledSequence := recovered.SettledSequence
			if err := runs.Reconcile(ctx); err != nil {
				t.Fatal(err)
			}
			repeated, err := runs.Get(ctx, runID)
			if err != nil ||
				repeated.State != testCase.wantRunState ||
				repeated.SettledSequence != settledSequence {
				t.Fatalf("repeated=%#v err=%v", repeated, err)
			}
		})
	}
}

func TestDurableAgentDoesNotReplayUnprovenStartedToolOutcome(t *testing.T) {
	testCases := []struct {
		name string
		run  func() (agent.ToolResult, error)
	}{
		{
			name: "handler_cancelled_after_side_effect",
			run: func() (agent.ToolResult, error) {
				return agent.ToolResult{}, context.Canceled
			},
		},
		{
			name: "ordinary_handler_error_after_side_effect",
			run: func() (agent.ToolResult, error) {
				return agent.ToolResult{}, errors.New("outcome is not proven")
			},
		},
		{
			name: "invalid_result_after_side_effect",
			run: func() (agent.ToolResult, error) {
				return agent.ToolResult{}, nil
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			databasePath := filepath.Join(t.TempDir(), "runtime.db")
			store, err := sqlitestore.Open(
				databasePath, sqlitestore.Options{},
			)
			if err != nil {
				t.Fatal(err)
			}
			call := contract.ToolCall{
				ID: "call_unknown", Name: "effect",
				Arguments: json.RawMessage(`{"value":"apply"}`),
			}
			sideEffects := 0
			registry, err := agent.NewRegistry(agent.RegisteredTool{
				Definition: contract.ToolSpec{
					Name: "effect",
					InputSchema: json.RawMessage(`{
						"type":"object",
						"required":["value"],
						"properties":{"value":{"type":"string"}},
						"additionalProperties":false
					}`),
				},
				Handler: func(
					context.Context,
					agent.ToolRequest,
				) (agent.ToolResult, error) {
					sideEffects++
					return testCase.run()
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			generator := &agentModel{results: []contract.ModelResult{{
				Message: contract.Message{
					Role:      contract.RoleAssistant,
					ToolCalls: []contract.ToolCall{call},
				},
				FinishReason: contract.FinishToolCall,
			}}}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: buildAgentProfiles(t, false),
						Model:    generator,
						Tools:    registry,
						Store:    store,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			record, runtimeErr := runs.RunNow(ctx, runtime.Request{
				Kind: runtime.KindAgent, ProfileID: "api", Input: "apply",
			}, nil)
			if runtimeErr == nil ||
				record.State != runtime.StateNeedsReconciliation ||
				record.Error == nil {
				t.Fatalf("record=%#v error=%v", record, runtimeErr)
			}
			if sideEffects != 1 {
				t.Fatalf("side effects=%d", sideEffects)
			}
			effect, exists, err := store.ToolEffect(
				ctx, record.ID, call.ID,
			)
			if err != nil || !exists || effect.State != "started" ||
				effect.Error != nil || len(effect.Result) != 0 {
				t.Fatalf(
					"effect=%#v exists=%v error=%v",
					effect, exists, err,
				)
			}
			if err := runs.Close(); err != nil {
				t.Fatal(err)
			}

			store, err = sqlitestore.Open(
				databasePath, sqlitestore.Options{},
			)
			if err != nil {
				t.Fatal(err)
			}
			replayRegistry, err := agent.NewRegistry(agent.RegisteredTool{
				Definition: contract.ToolSpec{
					Name: "effect",
					InputSchema: json.RawMessage(`{
						"type":"object",
						"required":["value"],
						"properties":{"value":{"type":"string"}},
						"additionalProperties":false
					}`),
				},
				Handler: func(
					context.Context,
					agent.ToolRequest,
				) (agent.ToolResult, error) {
					sideEffects++
					return agent.ToolResult{Content: "replayed"}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			runs, err = runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: buildAgentProfiles(t, false),
						Model:    &agentModel{},
						Tools:    replayRegistry,
						Store:    store,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runs.Close() })
			recovered, err := runs.Get(ctx, record.ID)
			if err != nil ||
				recovered.State != runtime.StateNeedsReconciliation {
				t.Fatalf("recovered=%#v error=%v", recovered, err)
			}
			_, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
			if runtimeErr != nil || found {
				t.Fatalf("found=%v error=%v", found, runtimeErr)
			}
			if sideEffects != 1 {
				t.Fatalf("started effect replayed: %d", sideEffects)
			}
		})
	}
}

func TestDurableAgentSessionModelCrashFailsClosedAndReconciles(
	t *testing.T,
) {
	ctx := context.Background()
	root := t.TempDir()
	databasePath := filepath.Join(root, "runtime.db")
	fixedNow := func() time.Time {
		return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	}
	profiles := buildAgentProfiles(t, false)
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "session-state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: &agentModel{},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_" + strings.Repeat("d", 32)
	turn, runtimeErr := sessions.PrepareAgent(session.RunRequest{
		SessionID: sessionID, RunID: runID,
		ProfileID: "api", Input: "start",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	store, err := sqlitestore.Open(databasePath, sqlitestore.Options{
		Now: fixedNow, SkipReconcile: true,
	})
	if err != nil {
		t.Fatal(err)
	}
	storedRequest := prepareStoredAgentRequest(
		t, store,
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
			SessionID: sessionID, AgentBudget: agent.DefaultBudget(),
		},
		nil, sessions,
	)
	record, err := store.Create(ctx, runID, storedRequest)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(ctx, record.ID); err != nil {
		t.Fatal(err)
	}
	modelCall := runtime.ModelCall{
		ID:    "model_call_" + strings.Repeat("d", 32),
		RunID: runID, Sequence: 1,
		RequestDigest: "sha256:" + strings.Repeat("0", 64),
	}
	setTestModelRequest(t, &modelCall, runID, turn.Messages, nil)
	if err := store.StartModelCall(ctx, modelCall); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlitestore.Open(databasePath, sqlitestore.Options{
		Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	generator := &agentModel{}
	registry, err := agent.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Profiles: profiles, Model: generator, Tools: registry,
				Store: store, Sessions: sessions,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runs.Close() })
	unknown, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
	if !found || runtimeErr == nil ||
		runtimeErr.Code != contract.ErrorConflict ||
		unknown.State != runtime.StateNeedsReconciliation {
		t.Fatalf(
			"unknown=%#v found=%v err=%v",
			unknown, found, runtimeErr,
		)
	}
	if len(generator.requests) != 0 {
		t.Fatalf("provider was replayed: %#v", generator.requests)
	}
	sessionValue, err := sessions.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sessionValue.State != session.SessionActive ||
		sessionValue.ActiveTurnID != turn.TurnID {
		t.Fatalf("session=%#v", sessionValue)
	}
	runningResult, resultFound, err := sessions.ResultForRun(sessionID, runID)
	if err != nil || !resultFound ||
		runningResult.State != session.TurnRunning {
		t.Fatalf(
			"runningResult=%#v found=%v err=%v",
			runningResult, resultFound, err,
		)
	}
	runningExecution, err := sessions.Execution(
		sessionID, runningResult.ExecutionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if runningExecution.State != session.ExecutionRunning {
		t.Fatalf("runningExecution=%#v", runningExecution)
	}
	reconciled, runtimeErr := runs.ReconcileRun(ctx, runID)
	if runtimeErr != nil ||
		reconciled.State != runtime.StateFailed {
		t.Fatalf("reconciled=%#v err=%v", reconciled, runtimeErr)
	}
	repeated, runtimeErr := runs.ReconcileRun(ctx, runID)
	if runtimeErr != nil ||
		repeated.ID != reconciled.ID ||
		repeated.SettledSequence != reconciled.SettledSequence {
		t.Fatalf("repeated=%#v err=%v", repeated, runtimeErr)
	}
	failedResult, resultFound, err := sessions.ResultForRun(sessionID, runID)
	if err != nil || !resultFound ||
		failedResult.State != session.TurnFailed {
		t.Fatalf(
			"failedResult=%#v found=%v err=%v",
			failedResult, resultFound, err,
		)
	}
	settledExecution, err := sessions.Execution(
		sessionID, failedResult.ExecutionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if settledExecution.State != session.ExecutionSettled ||
		settledExecution.Outcome != session.OutcomeUnknown {
		t.Fatalf("settledExecution=%#v", settledExecution)
	}
}

func TestDurableAgentTerminalCheckpointDoesNotReplayModel(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	databasePath := filepath.Join(root, "runtime.db")
	fixedNow := func() time.Time {
		return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	}
	profiles := buildAgentProfiles(t, false)
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "session-state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: &agentModel{},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		databasePath, sqlitestore.Options{
			Now: fixedNow, SkipReconcile: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_" + strings.Repeat("c", 32)
	storedRequest := prepareStoredAgentRequest(
		t, store,
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
			SessionID: sessionID, AgentBudget: agent.DefaultBudget(),
		},
		nil, sessions,
	)
	record, err := store.Create(ctx, runID, storedRequest)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Start(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstModel := &agentModel{results: []contract.ModelResult{{
		Message: contract.Message{
			Role: contract.RoleAssistant, Content: "durable final",
		},
		FinishReason: contract.FinishStop,
	}}}
	registry, err := agent.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	executor := &runtime.AgentExecutor{
		Profiles: profiles, Model: firstModel, Tools: registry,
		Store: store, Sessions: sessions,
	}
	outcome := executor.Execute(ctx, record, func(event contract.Event) error {
		_, err := store.AppendEvent(ctx, runID, event)
		return err
	})
	if outcome.State != runtime.StateCompleted {
		t.Fatalf("outcome=%#v", outcome)
	}
	firstResult := append(json.RawMessage(nil), outcome.Result...)
	sessionEventsBefore, err := sessions.Events(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	sessionResultBefore, resultFound, err := sessions.ResultForRun(
		sessionID, runID,
	)
	if err != nil || !resultFound ||
		sessionResultBefore.State != session.TurnCompleted {
		t.Fatalf(
			"sessionResultBefore=%#v found=%v err=%v",
			sessionResultBefore, resultFound, err,
		)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = sqlitestore.Open(databasePath, sqlitestore.Options{
		Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Store: store, Sessions: sessions,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runs.Close() })
	completed, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
	if runtimeErr != nil || !found ||
		completed.State != runtime.StateCompleted {
		t.Fatalf(
			"completed=%#v found=%v err=%v",
			completed, found, runtimeErr,
		)
	}
	if string(completed.Result) != string(firstResult) {
		t.Fatalf(
			"recovered result changed schema or value:\nfirst=%s\nrecovered=%s",
			firstResult, completed.Result,
		)
	}
	sessionEventsAfter, err := sessions.Events(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	sessionResultAfter, resultFound, err := sessions.ResultForRun(
		sessionID, runID,
	)
	if err != nil || !resultFound ||
		sessionResultAfter.TurnID != sessionResultBefore.TurnID ||
		sessionResultAfter.ExecutionID != sessionResultBefore.ExecutionID ||
		len(sessionEventsAfter) != len(sessionEventsBefore) {
		t.Fatalf(
			"sessionResultAfter=%#v eventsBefore=%d eventsAfter=%d found=%v err=%v",
			sessionResultAfter, len(sessionEventsBefore),
			len(sessionEventsAfter), resultFound, err,
		)
	}
	events, err := runs.Events(ctx, runID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[contract.EventType]int{}
	for index, event := range events {
		if event.Sequence != uint64(index+1) {
			t.Fatalf("events=%#v", events)
		}
		counts[event.Type]++
	}
	if counts[contract.EventModelStarted] != 1 ||
		counts[contract.EventModelCompleted] != 1 ||
		counts[contract.EventAgentCompleted] != 1 ||
		counts[contract.EventRunSettled] != 1 {
		t.Fatalf("event counts=%#v events=%#v", counts, events)
	}
}

func TestAgentSessionDigestForgeryIsRejectedByFrozenSnapshot(
	t *testing.T,
) {
	testCases := []struct {
		name         string
		field        string
		synchronized bool
	}{
		{name: "turn_request_only", field: "request"},
		{name: "turn_config_only", field: "config"},
		{
			name:  "turn_execution_request",
			field: "request", synchronized: true,
		},
		{
			name:  "turn_execution_config",
			field: "config", synchronized: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			sessionsRoot := filepath.Join(root, "sessions")
			profiles := buildAgentProfiles(t, false)
			generator := &agentModel{results: []contract.ModelResult{{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "done",
				},
				FinishReason: contract.FinishStop,
			}}}
			sessionStore, err := session.NewStore(
				sessionsRoot, filepath.Join(root, "session-state"),
			)
			if err != nil {
				t.Fatal(err)
			}
			sessions, err := session.NewService(session.ServiceOptions{
				Store: sessionStore, Profiles: profiles, Models: generator,
			})
			if err != nil {
				t.Fatal(err)
			}
			sessionID, err := session.NewID()
			if err != nil {
				t.Fatal(err)
			}
			store, err := sqlitestore.Open(
				filepath.Join(root, "runtime.db"),
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			registry, err := agent.NewRegistry()
			if err != nil {
				t.Fatal(err)
			}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: profiles, Model: generator,
						Tools: registry, Store: store,
						Sessions: sessions,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runs.Close() })
			completed, runtimeErr := runs.RunNow(ctx, runtime.Request{
				Kind: runtime.KindAgent, ProfileID: "api",
				Input: "digest-bound", SessionID: sessionID,
			}, nil)
			if runtimeErr != nil ||
				completed.State != runtime.StateCompleted {
				t.Fatalf("completed=%#v err=%v", completed, runtimeErr)
			}
			result, found, err := sessions.ResultForRun(
				sessionID, completed.ID,
			)
			if err != nil || !found {
				t.Fatalf("result=%#v found=%v err=%v", result, found, err)
			}
			turnPath := filepath.Join(
				sessionsRoot, sessionID, "turns",
				result.TurnID, "turn.json",
			)
			executionPath := filepath.Join(
				sessionsRoot, sessionID, "executions",
				result.ExecutionID+".json",
			)
			turnJSON, err := os.ReadFile(turnPath)
			if err != nil {
				t.Fatal(err)
			}
			executionJSON, err := os.ReadFile(executionPath)
			if err != nil {
				t.Fatal(err)
			}
			var turn session.Turn
			if err := json.Unmarshal(turnJSON, &turn); err != nil {
				t.Fatal(err)
			}
			var execution session.Execution
			if err := json.Unmarshal(
				executionJSON, &execution,
			); err != nil {
				t.Fatal(err)
			}
			forged := "sha256:" + strings.Repeat("f", 64)
			switch testCase.field {
			case "request":
				turn.RequestDigest = forged
				if testCase.synchronized {
					execution.RequestDigest = forged
				}
			case "config":
				turn.ConfigDigest = forged
				if testCase.synchronized {
					execution.ConfigDigest = forged
				}
			default:
				t.Fatalf("unknown field %q", testCase.field)
			}
			turnJSON, err = json.Marshal(turn)
			if err != nil {
				t.Fatal(err)
			}
			executionJSON, err = json.Marshal(execution)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(turnPath, turnJSON, 0o600); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				executionPath, executionJSON, 0o600,
			); err != nil {
				t.Fatal(err)
			}
			eventsBefore, err := sessions.Events(sessionID, 0)
			if err != nil {
				t.Fatal(err)
			}
			modelCallsBefore, err := store.ModelCalls(
				ctx, completed.ID,
			)
			if err != nil {
				t.Fatal(err)
			}
			outcome := (&runtime.AgentExecutor{
				Store: store, Sessions: sessions,
			}).Execute(ctx, completed, nil)
			if outcome.State != runtime.StateNeedsReconciliation ||
				outcome.Error == nil ||
				outcome.Error.Code != contract.ErrorConflict {
				t.Fatalf("outcome=%#v", outcome)
			}
			if testCase.synchronized &&
				!strings.Contains(
					outcome.Error.Message, "frozen execution snapshot",
				) {
				t.Fatalf(
					"synchronized forgery bypassed frozen snapshot: %v",
					outcome.Error,
				)
			}
			actualTurn, err := os.ReadFile(turnPath)
			if err != nil {
				t.Fatal(err)
			}
			actualExecution, err := os.ReadFile(executionPath)
			if err != nil {
				t.Fatal(err)
			}
			eventsAfter, err := sessions.Events(sessionID, 0)
			if err != nil {
				t.Fatal(err)
			}
			modelCallsAfter, err := store.ModelCalls(ctx, completed.ID)
			if err != nil {
				t.Fatal(err)
			}
			current, err := runs.Get(ctx, completed.ID)
			if err != nil {
				t.Fatal(err)
			}
			if !bytes.Equal(actualTurn, turnJSON) ||
				!bytes.Equal(actualExecution, executionJSON) ||
				len(eventsAfter) != len(eventsBefore) ||
				!reflect.DeepEqual(modelCallsAfter, modelCallsBefore) ||
				current.State != runtime.StateCompleted ||
				current.SettledSequence != completed.SettledSequence ||
				len(generator.requests) != 1 {
				t.Fatalf(
					"turn_changed=%t execution_changed=%t events=%d/%d "+
						"modelCalls=%d/%d current=%#v provider=%d",
					!bytes.Equal(actualTurn, turnJSON),
					!bytes.Equal(actualExecution, executionJSON),
					len(eventsBefore), len(eventsAfter),
					len(modelCallsBefore), len(modelCallsAfter),
					current, len(generator.requests),
				)
			}
		})
	}
}

func TestAgentRecoveryDoesNotCreateMissingSessionTurn(t *testing.T) {
	ctx := context.Background()
	root := t.TempDir()
	profiles := buildAgentProfiles(t, false)
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "session-state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: &agentModel{},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		filepath.Join(root, "runtime.db"),
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_" + strings.Repeat("d", 32)
	storedRequest := prepareStoredAgentRequest(
		t, store,
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
			SessionID: sessionID, AgentBudget: agent.DefaultBudget(),
		},
		nil, sessions,
	)
	record, err := store.Create(ctx, runID, storedRequest)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Start(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	finalMessage := contract.Message{
		Role: contract.RoleAssistant, Content: "durable final",
	}
	modelCall := runtime.ModelCall{
		ID:    "model_call_" + strings.Repeat("d", 32),
		RunID: runID, Sequence: 1,
		RequestDigest: "sha256:" + strings.Repeat("0", 64),
	}
	setTestModelRequest(
		t, &modelCall, runID,
		[]contract.Message{{
			Role: contract.RoleUser, Content: "start",
		}},
		nil,
	)
	if err := store.StartModelCall(ctx, modelCall); err != nil {
		t.Fatal(err)
	}
	modelCall.State = "completed"
	setTestModelResult(t, &modelCall, contract.ModelResult{
		Message: finalMessage, FinishReason: contract.FinishStop,
	})
	if err := store.FinishModelCall(ctx, modelCall); err != nil {
		t.Fatal(err)
	}
	stateJSON, err := json.Marshal(agent.LoopState{
		SchemaVersion: 2, RunID: runID, ModelProfile: "api",
		Messages: []contract.Message{
			{Role: contract.RoleUser, Content: "start"},
			finalMessage,
		},
		BaseMessageCount: 1, Round: 1,
		TerminalOutcome: &agent.Outcome{
			State: agent.StateCompleted, StopReason: "stop",
			Message: &finalMessage,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.SaveCheckpoint(ctx, runtime.Checkpoint{
		ID:    "checkpoint_" + strings.Repeat("d", 32),
		RunID: runID, State: stateJSON,
	}); err != nil {
		t.Fatal(err)
	}
	registry, err := agent.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	outcome := (&runtime.AgentExecutor{
		Profiles: profiles, Model: &agentModel{}, Tools: registry,
		Store: store, Sessions: sessions,
	}).Execute(ctx, record, nil)
	if outcome.State != runtime.StateNeedsReconciliation ||
		outcome.Error == nil ||
		!strings.Contains(
			outcome.Error.Message, "no matching Session Turn",
		) {
		t.Fatalf("outcome=%#v error=%v", outcome, outcome.Error)
	}
	if _, err := sessions.Get(sessionID); err == nil {
		t.Fatal("Agent recovery silently created a replacement Session")
	}
}

func TestDurableAgentTerminalCheckpointWithoutSessionDoesNotReplayModel(
	t *testing.T,
) {
	ctx := context.Background()
	databasePath := filepath.Join(t.TempDir(), "runtime.db")
	fixedNow := func() time.Time {
		return time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	}
	store, err := sqlitestore.Open(
		databasePath, sqlitestore.Options{
			Now: fixedNow, SkipReconcile: true,
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_" + strings.Repeat("e", 32)
	storedRequest := prepareStoredAgentRequest(
		t, store,
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
			AgentBudget: agent.DefaultBudget(),
		},
		nil, nil,
	)
	record, err := store.Create(ctx, runID, storedRequest)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Start(ctx, record.ID)
	if err != nil {
		t.Fatal(err)
	}
	firstModel := &agentModel{results: []contract.ModelResult{{
		Message: contract.Message{
			Role: contract.RoleAssistant, Content: "durable final",
		},
		FinishReason: contract.FinishStop,
	}}}
	registry, err := agent.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	profiles := buildAgentProfiles(t, false)
	executor := &runtime.AgentExecutor{
		Profiles: profiles, Model: firstModel, Tools: registry, Store: store,
	}
	outcome := executor.Execute(ctx, record, func(event contract.Event) error {
		_, err := store.AppendEvent(ctx, runID, event)
		return err
	})
	if outcome.State != runtime.StateCompleted {
		t.Fatalf("outcome=%#v", outcome)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	store, err = sqlitestore.Open(databasePath, sqlitestore.Options{
		Now: fixedNow,
	})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Store: store,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runs.Close() })
	completed, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
	if runtimeErr != nil || !found ||
		completed.State != runtime.StateCompleted {
		t.Fatalf(
			"completed=%#v found=%v err=%v",
			completed, found, runtimeErr,
		)
	}
	events, err := runs.Events(ctx, runID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := map[contract.EventType]int{}
	for _, event := range events {
		counts[event.Type]++
	}
	if counts[contract.EventModelStarted] != 1 ||
		counts[contract.EventModelCompleted] != 1 ||
		counts[contract.EventAgentCompleted] != 1 {
		t.Fatalf("event counts=%#v events=%#v", counts, events)
	}
}

func TestDurableAgentPersistsToolLoopAndSessionProjection(t *testing.T) {
	call := contract.ToolCall{
		ID: "call_1", Name: "echo", Arguments: json.RawMessage(`{"value":"ok"}`),
	}
	generator := &agentModel{results: []contract.ModelResult{
		{
			Message: contract.Message{
				Role: contract.RoleAssistant, ToolCalls: []contract.ToolCall{call},
			},
			FinishReason: contract.FinishToolCall,
		},
		{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "done"},
			FinishReason: contract.FinishStop,
		},
	}}
	profiles := buildAgentProfiles(t, true)
	registry, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Handler: func(context.Context, agent.ToolRequest) (agent.ToolResult, error) {
			return agent.ToolResult{Content: `{"value":"ok"}`}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	runStore, err := sqlitestore.Open(
		filepath.Join(root, "state", "runtime.db"), sqlitestore.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	executor := &runtime.AgentExecutor{
		Profiles: profiles, Model: generator, Tools: registry,
		Store: runStore, Sessions: sessions,
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: runStore,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: executor,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runs.Close() })
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	record, runtimeErr := runs.RunNow(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api",
		Input: "start", SessionID: sessionID,
	}, nil)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if record.State != runtime.StateCompleted || record.SettledSequence != 10 {
		t.Fatalf("record=%#v", record)
	}
	events, err := runs.Events(context.Background(), record.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	if len(events) != int(record.SettledSequence) ||
		events[len(events)-1].Type != contract.EventRunSettled {
		t.Fatalf("events=%#v", events)
	}
	messages, err := sessions.Messages(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(messages) != 4 ||
		messages[1].Message.ToolCalls[0].ID != "call_1" ||
		messages[2].Message.Role != contract.RoleTool ||
		messages[2].Message.ToolCallID != "call_1" ||
		messages[3].Message.Content != "done" {
		t.Fatalf("messages=%#v", messages)
	}
}

func TestDurableAgentPauseResumeKeepsRunIdentity(t *testing.T) {
	call := contract.ToolCall{
		ID: "call_pause", Name: "approval", Arguments: json.RawMessage(`{}`),
	}
	generator := &agentModel{results: []contract.ModelResult{
		{
			Message: contract.Message{
				Role: contract.RoleAssistant, ToolCalls: []contract.ToolCall{call},
			},
			FinishReason: contract.FinishToolCall,
		},
		{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "resumed"},
			FinishReason: contract.FinishStop,
		},
	}}
	registry, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "approval", InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Handler: func(context.Context, agent.ToolRequest) (agent.ToolResult, error) {
			return agent.ToolResult{Pause: &agent.Pause{
				ID: "pause_1", Kind: "approval", Prompt: "approve?",
				InputSchema: json.RawMessage(
					`{"type":"object","required":["approved"]}`,
				),
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles := buildAgentProfiles(t, false)
	root := t.TempDir()
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		filepath.Join(root, "state", "runtime.db"), sqlitestore.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Profiles: profiles, Model: generator, Tools: registry,
				Store: store, Sessions: sessions,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runs.Close() })
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	paused, runtimeErr := runs.RunNow(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api",
		Input: "start", SessionID: sessionID,
	}, nil)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if paused.State != runtime.StatePaused || len(paused.Pause) == 0 {
		t.Fatalf("paused=%#v", paused)
	}
	sessionValue, err := sessions.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sessionValue.State != session.SessionBlocked ||
		sessionValue.ActiveTurnID != "" {
		t.Fatalf("paused session=%#v", sessionValue)
	}
	sessionResult, found, err := sessions.ResultForRun(sessionID, paused.ID)
	if err != nil || !found ||
		sessionResult.State != session.TurnRequiresAction {
		t.Fatalf("paused result=%#v found=%v err=%v", sessionResult, found, err)
	}
	resumed, err := runs.Resume(context.Background(), paused.ID, json.RawMessage(
		`{"pause_id":"pause_1","input":{"approved":true}}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	if resumed.State != runtime.StateQueued {
		t.Fatalf("resumed=%#v", resumed)
	}
	completed, found, runtimeErr := runs.ExecuteNext(
		context.Background(), "worker-1",
	)
	if runtimeErr != nil || !found ||
		completed.ID != paused.ID ||
		completed.State != runtime.StateCompleted {
		t.Fatalf("completed=%#v found=%v err=%v", completed, found, runtimeErr)
	}
	sessionValue, err = sessions.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sessionValue.State != session.SessionIdle ||
		sessionValue.ActiveTurnID != "" {
		t.Fatalf("resumed session=%#v", sessionValue)
	}
	sessionResult, found, err = sessions.ResultForRun(sessionID, paused.ID)
	if err != nil || !found || sessionResult.State != session.TurnCompleted {
		t.Fatalf("completed result=%#v found=%v err=%v", sessionResult, found, err)
	}
}

func TestDurableAgentResumeUsesStoreAcceptanceTimeForExpiry(t *testing.T) {
	ctx := context.Background()
	before := time.Date(2026, 7, 30, 12, 0, 0, 0, time.UTC)
	expiresAt := before.Add(time.Second)
	after := expiresAt.Add(time.Hour)
	var clockMu sync.Mutex
	clock := before
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	setClock := func(value time.Time) {
		clockMu.Lock()
		clock = value
		clockMu.Unlock()
	}
	call := contract.ToolCall{
		ID: "call_expiring_pause", Name: "approval",
		Arguments: json.RawMessage(`{}`),
	}
	generator := &agentModel{results: []contract.ModelResult{
		{
			Message: contract.Message{
				Role:      contract.RoleAssistant,
				ToolCalls: []contract.ToolCall{call},
			},
			FinishReason: contract.FinishToolCall,
		},
		{
			Message: contract.Message{
				Role: contract.RoleAssistant, Content: "accepted in time",
			},
			FinishReason: contract.FinishStop,
		},
	}}
	registry, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "approval",
			InputSchema: json.RawMessage(
				`{"type":"object","additionalProperties":false}`,
			),
		},
		Handler: func(
			context.Context,
			agent.ToolRequest,
		) (agent.ToolResult, error) {
			return agent.ToolResult{Pause: &agent.Pause{
				ID: "pause_expiring", Kind: "approval",
				Prompt: "approve before expiry?", ExpiresAt: &expiresAt,
				InputSchema: json.RawMessage(`{
					"type":"object",
					"required":["approved"],
					"properties":{"approved":{"type":"boolean"}},
					"additionalProperties":false
				}`),
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles := buildAgentProfiles(t, false)
	store, err := sqlitestore.Open(
		filepath.Join(t.TempDir(), "runtime.db"),
		sqlitestore.Options{Now: now, SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	executor := &runtime.AgentExecutor{
		Profiles: profiles, Model: generator, Tools: registry,
		Store: store, Now: now,
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: executor,
		},
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runs.Close() })
	paused, runtimeErr := runs.RunNow(ctx, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
	}, nil)
	if runtimeErr != nil || paused.State != runtime.StatePaused {
		t.Fatalf("paused=%#v err=%v", paused, runtimeErr)
	}
	setClock(expiresAt)
	resumed, err := runs.Resume(
		ctx, paused.ID,
		json.RawMessage(
			`{"pause_id":"pause_expiring","input":{"approved":true}}`,
		),
	)
	if err != nil || resumed.State != runtime.StateQueued ||
		resumed.ResumeAcceptedAt == nil ||
		!resumed.ResumeAcceptedAt.Equal(expiresAt) {
		t.Fatalf("resumed=%#v err=%v", resumed, err)
	}
	setClock(after)
	completed, found, runtimeErr := runs.ExecuteNext(ctx, "late-worker")
	if runtimeErr != nil || !found ||
		completed.State != runtime.StateCompleted {
		t.Fatalf(
			"completed=%#v found=%v err=%v",
			completed, found, runtimeErr,
		)
	}
	if len(generator.requests) != 2 {
		t.Fatalf("provider requests=%d", len(generator.requests))
	}
	resumes, err := store.Resumes(ctx, paused.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumes) != 1 || !resumes[0].AcceptedAt.Equal(expiresAt) {
		t.Fatalf("resume journal=%#v", resumes)
	}
}

func TestDurableAgentResumeRejectsSnapshotDriftBeforeStoreMutation(
	t *testing.T,
) {
	ctx := context.Background()
	call := contract.ToolCall{
		ID: "call_resume_snapshot", Name: "approval",
		Arguments: json.RawMessage(`{}`),
	}
	generator := &agentModel{results: []contract.ModelResult{{
		Message: contract.Message{
			Role:      contract.RoleAssistant,
			ToolCalls: []contract.ToolCall{call},
		},
		FinishReason: contract.FinishToolCall,
	}}}
	registry, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "approval",
			InputSchema: json.RawMessage(
				`{"type":"object","additionalProperties":false}`,
			),
		},
		Handler: func(
			context.Context,
			agent.ToolRequest,
		) (agent.ToolResult, error) {
			return agent.ToolResult{Pause: &agent.Pause{
				ID: "pause_resume_snapshot", Kind: "approval",
				Prompt: "approve?",
				InputSchema: json.RawMessage(
					`{"type":"object","required":["approved"]}`,
				),
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		filepath.Join(t.TempDir(), "runtime.db"),
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Profiles: buildAgentProfiles(t, false),
				Model:    generator, Tools: registry, Store: store,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runs.Close() })
	paused, runtimeErr := runs.RunNow(ctx, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
	}, nil)
	if runtimeErr != nil || paused.State != runtime.StatePaused {
		t.Fatalf("paused=%#v err=%v", paused, runtimeErr)
	}
	if err := registry.Register(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "drifted",
			InputSchema: json.RawMessage(
				`{"type":"object","additionalProperties":false}`,
			),
		},
		Handler: func(
			context.Context,
			agent.ToolRequest,
		) (agent.ToolResult, error) {
			return agent.ToolResult{Content: "must not run"}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	_, err = runs.Resume(
		ctx, paused.ID,
		json.RawMessage(
			`{"pause_id":"pause_resume_snapshot","input":{"approved":true}}`,
		),
	)
	var changed *contract.RuntimeError
	if !errors.As(err, &changed) ||
		changed.Code != contract.ErrorConflict ||
		changed.Phase != contract.PhaseProfile {
		t.Fatalf("resume error=%v", err)
	}
	current, err := runs.Get(ctx, paused.ID)
	if err != nil {
		t.Fatal(err)
	}
	if current.State != runtime.StatePaused ||
		current.ResumeAcceptedAt != nil ||
		!bytes.Equal(current.Pause, paused.Pause) {
		t.Fatalf("current=%#v", current)
	}
	resumes, err := store.Resumes(ctx, paused.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumes) != 0 || len(generator.requests) != 1 {
		t.Fatalf(
			"resume journal=%#v model requests=%d",
			resumes, len(generator.requests),
		)
	}
}

func TestAcceptedAgentResumeDriftRestoresOriginalPauseBeforeActivation(
	t *testing.T,
) {
	ctx := context.Background()
	root := t.TempDir()
	call := contract.ToolCall{
		ID: "call_resume_accepted_drift", Name: "approval",
		Arguments: json.RawMessage(`{}`),
	}
	generator := &agentModel{results: []contract.ModelResult{
		{
			Message: contract.Message{
				Role:      contract.RoleAssistant,
				ToolCalls: []contract.ToolCall{call},
			},
			FinishReason: contract.FinishToolCall,
		},
		{
			Message: contract.Message{
				Role: contract.RoleAssistant, Content: "must not resume",
			},
			FinishReason: contract.FinishStop,
		},
	}}
	registry, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "approval",
			InputSchema: json.RawMessage(
				`{"type":"object","additionalProperties":false}`,
			),
		},
		Handler: func(
			context.Context,
			agent.ToolRequest,
		) (agent.ToolResult, error) {
			return agent.ToolResult{Pause: &agent.Pause{
				ID: "pause_resume_accepted_drift", Kind: "approval",
				Prompt: "approve?",
				InputSchema: json.RawMessage(
					`{"type":"object","required":["approved"]}`,
				),
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles := buildAgentProfiles(t, false)
	sessionsRoot := filepath.Join(root, "sessions")
	sessionStore, err := session.NewStore(
		sessionsRoot,
		filepath.Join(root, "session-state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		filepath.Join(root, "runtime.db"),
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Profiles: profiles, Model: generator, Tools: registry,
				Store: store, Sessions: sessions,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runs.Close() })
	paused, runtimeErr := runs.RunNow(ctx, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api",
		Input: "pause", SessionID: sessionID,
	}, nil)
	if runtimeErr != nil || paused.State != runtime.StatePaused {
		t.Fatalf("paused=%#v err=%v", paused, runtimeErr)
	}
	originalPause := append(json.RawMessage(nil), paused.Pause...)
	queued, err := runs.Resume(
		ctx, paused.ID,
		json.RawMessage(
			`{"pause_id":"pause_resume_accepted_drift","input":{"approved":true}}`,
		),
	)
	if err != nil || queued.State != runtime.StateQueued ||
		queued.ResumeAcceptedAt == nil {
		t.Fatalf("queued=%#v err=%v", queued, err)
	}
	if err := registry.Register(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "drifted",
			InputSchema: json.RawMessage(
				`{"type":"object","additionalProperties":false}`,
			),
		},
		Handler: func(
			context.Context,
			agent.ToolRequest,
		) (agent.ToolResult, error) {
			t.Fatal("drifted handler executed")
			return agent.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	restored, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
	if runtimeErr != nil || !found ||
		restored.State != runtime.StatePaused ||
		!bytes.Equal(restored.Pause, originalPause) ||
		len(generator.requests) != 1 {
		t.Fatalf(
			"restored=%#v found=%v err=%v requests=%d",
			restored, found, runtimeErr, len(generator.requests),
		)
	}
	sessionValue, err := sessions.Get(sessionID)
	if err != nil ||
		sessionValue.State != session.SessionBlocked ||
		sessionValue.ActiveTurnID != "" {
		t.Fatalf("Session=%#v err=%v", sessionValue, err)
	}
	result, resultFound, err := sessions.ResultForRun(
		sessionID, paused.ID,
	)
	if err != nil || !resultFound ||
		result.State != session.TurnRequiresAction {
		t.Fatalf(
			"Session result=%#v found=%v err=%v",
			result, resultFound, err,
		)
	}
	resumes, err := store.Resumes(ctx, paused.ID)
	if err != nil || len(resumes) != 1 {
		t.Fatalf("resume journal=%#v err=%v", resumes, err)
	}
}

func TestAcceptedAgentResumeDriftRestoresPauseAfterActivationCrash(
	t *testing.T,
) {
	ctx := context.Background()
	root := t.TempDir()
	call := contract.ToolCall{
		ID: "call_resume_activation_crash", Name: "approval",
		Arguments: json.RawMessage(`{}`),
	}
	handlerCalls := 0
	generator := &agentModel{results: []contract.ModelResult{
		{
			Message: contract.Message{
				Role:      contract.RoleAssistant,
				ToolCalls: []contract.ToolCall{call},
			},
			FinishReason: contract.FinishToolCall,
		},
		{
			Message: contract.Message{
				Role: contract.RoleAssistant, Content: "must not resume",
			},
			FinishReason: contract.FinishStop,
		},
	}}
	registry, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "approval",
			InputSchema: json.RawMessage(
				`{"type":"object","additionalProperties":false}`,
			),
		},
		Handler: func(
			context.Context,
			agent.ToolRequest,
		) (agent.ToolResult, error) {
			handlerCalls++
			return agent.ToolResult{Pause: &agent.Pause{
				ID: "pause_resume_activation_crash", Kind: "approval",
				Prompt: "approve?",
				InputSchema: json.RawMessage(
					`{"type":"object","required":["approved"]}`,
				),
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles := buildAgentProfiles(t, false)
	sessionsRoot := filepath.Join(root, "sessions")
	sessionStore, err := session.NewStore(
		sessionsRoot,
		filepath.Join(root, "session-state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		filepath.Join(root, "runtime.db"),
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Profiles: profiles, Model: generator, Tools: registry,
				Store: store, Sessions: sessions,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runs.Close() })
	paused, runtimeErr := runs.RunNow(ctx, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api",
		Input: "pause", SessionID: sessionID,
	}, nil)
	if runtimeErr != nil || paused.State != runtime.StatePaused {
		t.Fatalf("paused=%#v err=%v", paused, runtimeErr)
	}
	originalPause := append(json.RawMessage(nil), paused.Pause...)
	queued, err := runs.Resume(
		ctx, paused.ID,
		json.RawMessage(
			`{"pause_id":"pause_resume_activation_crash","input":{"approved":true}}`,
		),
	)
	if err != nil || queued.State != runtime.StateQueued {
		t.Fatalf("queued=%#v err=%v", queued, err)
	}
	turn, found, lookupErr := sessions.LookupAgent(session.RunRequest{
		SessionID: sessionID, RunID: paused.ID,
		ProfileID: "api", Input: "pause",
	})
	if lookupErr != nil || !found || turn.ProjectedPause == nil {
		t.Fatalf(
			"turn=%#v found=%v err=%v",
			turn, found, lookupErr,
		)
	}
	if runtimeErr := sessions.ActivateAgentResume(turn); runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	active, err := sessions.Get(sessionID)
	if err != nil || active.State != session.SessionActive ||
		active.ActiveTurnID != turn.TurnID {
		t.Fatalf("active=%#v err=%v", active, err)
	}
	pausedResult, resultFound, err := sessions.ResultForRun(
		sessionID, paused.ID,
	)
	if err != nil || !resultFound {
		t.Fatalf(
			"paused result=%#v found=%v err=%v",
			pausedResult, resultFound, err,
		)
	}
	executionPath := filepath.Join(
		sessionsRoot, sessionID, "executions",
		pausedResult.ExecutionID+".json",
	)
	executionJSON, err := os.ReadFile(executionPath)
	if err != nil {
		t.Fatal(err)
	}
	var crashedExecution session.Execution
	if err := json.Unmarshal(
		executionJSON, &crashedExecution,
	); err != nil {
		t.Fatal(err)
	}
	crashedExecution.Process = nil
	executionJSON, err = json.Marshal(crashedExecution)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(executionPath, executionJSON, 0o600); err != nil {
		t.Fatal(err)
	}
	restartedSessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	crashRecovered, err := restartedSessions.Get(sessionID)
	if err != nil ||
		crashRecovered.State != session.SessionBlocked ||
		crashRecovered.ActiveTurnID != turn.TurnID {
		t.Fatalf(
			"crash-recovered Session=%#v err=%v",
			crashRecovered, err,
		)
	}
	running, found, err := restartedSessions.ResultForRun(
		sessionID, paused.ID,
	)
	if err != nil || !found || running.State != session.TurnRunning {
		t.Fatalf(
			"running=%#v found=%v err=%v",
			running, found, err,
		)
	}
	eventsBefore, err := restartedSessions.Events(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "drifted",
			InputSchema: json.RawMessage(
				`{"type":"object","additionalProperties":false}`,
			),
		},
		Handler: func(
			context.Context,
			agent.ToolRequest,
		) (agent.ToolResult, error) {
			t.Fatal("drifted handler executed")
			return agent.ToolResult{}, nil
		},
	}); err != nil {
		t.Fatal(err)
	}
	restored, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
	if runtimeErr != nil || !found ||
		restored.State != runtime.StatePaused ||
		!bytes.Equal(restored.Pause, originalPause) ||
		len(generator.requests) != 1 ||
		handlerCalls != 1 {
		t.Fatalf(
			"restored=%#v found=%v err=%v requests=%d handlers=%d",
			restored, found, runtimeErr,
			len(generator.requests), handlerCalls,
		)
	}
	sessionValue, err := restartedSessions.Get(sessionID)
	if err != nil ||
		sessionValue.State != session.SessionBlocked ||
		sessionValue.ActiveTurnID != "" {
		t.Fatalf("Session=%#v err=%v", sessionValue, err)
	}
	result, resultFound, err := restartedSessions.ResultForRun(
		sessionID, paused.ID,
	)
	if err != nil || !resultFound ||
		result.State != session.TurnRequiresAction {
		t.Fatalf(
			"Session result=%#v found=%v err=%v",
			result, resultFound, err,
		)
	}
	execution, err := restartedSessions.Execution(
		sessionID, result.ExecutionID,
	)
	if err != nil ||
		execution.State != session.ExecutionSettled ||
		execution.Outcome != session.OutcomeCompleted {
		t.Fatalf("execution=%#v err=%v", execution, err)
	}
	eventsAfter, err := restartedSessions.Events(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if len(eventsAfter) != len(eventsBefore) {
		t.Fatalf(
			"pause restoration duplicated events: before=%d after=%d",
			len(eventsBefore), len(eventsAfter),
		)
	}
}

func TestAcceptedAgentResumeRejectsTamperedSessionSuffixBeforeEffects(
	t *testing.T,
) {
	for _, snapshotDrift := range []bool{false, true} {
		name := "without_snapshot_drift"
		if snapshotDrift {
			name = "with_snapshot_drift"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			call := contract.ToolCall{
				ID: "call_resume_suffix_tamper", Name: "approval",
				Arguments: json.RawMessage(`{}`),
			}
			handlerCalls := 0
			generator := &agentModel{results: []contract.ModelResult{
				{
					Message: contract.Message{
						Role:      contract.RoleAssistant,
						ToolCalls: []contract.ToolCall{call},
					},
					FinishReason: contract.FinishToolCall,
				},
				{
					Message: contract.Message{
						Role:    contract.RoleAssistant,
						Content: "must not resume after Session tampering",
					},
					FinishReason: contract.FinishStop,
				},
			}}
			registry, err := agent.NewRegistry(agent.RegisteredTool{
				Definition: contract.ToolSpec{
					Name: "approval",
					InputSchema: json.RawMessage(
						`{"type":"object","additionalProperties":false}`,
					),
				},
				Handler: func(
					context.Context,
					agent.ToolRequest,
				) (agent.ToolResult, error) {
					handlerCalls++
					return agent.ToolResult{Pause: &agent.Pause{
						ID:   "pause_resume_suffix_tamper",
						Kind: "approval", Prompt: "approve?",
						InputSchema: json.RawMessage(
							`{"type":"object","required":["approved"]}`,
						),
					}}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			profiles := buildAgentProfiles(t, false)
			sessionsRoot := filepath.Join(root, "sessions")
			sessionStore, err := session.NewStore(
				sessionsRoot,
				filepath.Join(root, "session-state"),
			)
			if err != nil {
				t.Fatal(err)
			}
			sessions, err := session.NewService(session.ServiceOptions{
				Store: sessionStore, Profiles: profiles, Models: generator,
			})
			if err != nil {
				t.Fatal(err)
			}
			sessionID, err := session.NewID()
			if err != nil {
				t.Fatal(err)
			}
			store, err := sqlitestore.Open(
				filepath.Join(root, "runtime.db"),
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			executor := &runtime.AgentExecutor{
				Profiles: profiles, Model: generator, Tools: registry,
				Store: store, Sessions: sessions,
			}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: executor,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runs.Close() })
			paused, runtimeErr := runs.RunNow(ctx, runtime.Request{
				Kind: runtime.KindAgent, ProfileID: "api",
				Input: "pause", SessionID: sessionID,
			}, nil)
			if runtimeErr != nil || paused.State != runtime.StatePaused {
				t.Fatalf("paused=%#v err=%v", paused, runtimeErr)
			}
			queued, err := runs.Resume(
				ctx, paused.ID,
				json.RawMessage(
					`{"pause_id":"pause_resume_suffix_tamper","input":{"approved":true}}`,
				),
			)
			if err != nil || queued.State != runtime.StateQueued {
				t.Fatalf("queued=%#v err=%v", queued, err)
			}
			turn, found, lookupErr := sessions.LookupAgent(
				session.RunRequest{
					SessionID: sessionID, RunID: paused.ID,
					ProfileID: "api", Input: "pause",
				},
			)
			if lookupErr != nil || !found || turn.ProjectedPause == nil {
				t.Fatalf(
					"turn=%#v found=%v err=%v",
					turn, found, lookupErr,
				)
			}
			if runtimeErr := sessions.ActivateAgentResume(
				turn,
			); runtimeErr != nil {
				t.Fatal(runtimeErr)
			}
			executionPath := filepath.Join(
				sessionsRoot, sessionID, "executions",
				turn.ExecutionID+".json",
			)
			executionJSON, err := os.ReadFile(executionPath)
			if err != nil {
				t.Fatal(err)
			}
			var crashedExecution session.Execution
			if err := json.Unmarshal(
				executionJSON, &crashedExecution,
			); err != nil {
				t.Fatal(err)
			}
			crashedExecution.Process = nil
			executionJSON, err = json.Marshal(crashedExecution)
			if err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				executionPath, executionJSON, 0o600,
			); err != nil {
				t.Fatal(err)
			}
			restartedSessions, err := session.NewService(
				session.ServiceOptions{
					Store: sessionStore, Profiles: profiles,
					Models: generator,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			executor.Sessions = restartedSessions
			recoveredSession, err := restartedSessions.Get(sessionID)
			if err != nil ||
				recoveredSession.State != session.SessionBlocked ||
				recoveredSession.ActiveTurnID != turn.TurnID {
				t.Fatalf(
					"recovered Session=%#v err=%v",
					recoveredSession, err,
				)
			}
			appendTamperedAgentSessionSuffix(
				t, sessionsRoot, recoveredSession, turn,
			)
			if snapshotDrift {
				if err := registry.Register(agent.RegisteredTool{
					Definition: contract.ToolSpec{
						Name: "drifted",
						InputSchema: json.RawMessage(
							`{"type":"object","additionalProperties":false}`,
						),
					},
					Handler: func(
						context.Context,
						agent.ToolRequest,
					) (agent.ToolResult, error) {
						t.Fatal("drifted handler executed")
						return agent.ToolResult{}, nil
					},
				}); err != nil {
					t.Fatal(err)
				}
			}
			current, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
			if runtimeErr == nil || !found ||
				current.State != runtime.StateNeedsReconciliation ||
				!strings.Contains(
					runtimeErr.Message, "provider-safe",
				) {
				t.Fatalf(
					"current=%#v found=%v err=%v",
					current, found, runtimeErr,
				)
			}
			if len(generator.requests) != 1 || handlerCalls != 1 {
				t.Fatalf(
					"side effects replayed: requests=%d handlers=%d",
					len(generator.requests), handlerCalls,
				)
			}
			sessionValue, err := restartedSessions.Get(sessionID)
			if err != nil ||
				sessionValue.State != session.SessionBlocked ||
				sessionValue.ActiveTurnID != turn.TurnID {
				t.Fatalf(
					"Session was incorrectly restored: %#v err=%v",
					sessionValue, err,
				)
			}
			result, resultFound, err :=
				restartedSessions.ResultForRun(sessionID, paused.ID)
			if err != nil || !resultFound ||
				result.State != session.TurnRunning {
				t.Fatalf(
					"Session result=%#v found=%v err=%v",
					result, resultFound, err,
				)
			}
		})
	}
}

func appendTamperedAgentSessionSuffix(
	t *testing.T,
	sessionsRoot string,
	sessionValue session.Session,
	turn session.AgentTurn,
) {
	t.Helper()
	path := filepath.Join(
		sessionsRoot, sessionValue.ID, "messages.jsonl",
	)
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if len(data) != 0 && data[len(data)-1] != '\n' {
		data = append(data, '\n')
	}
	record, err := json.Marshal(session.MessageRecord{
		Sequence:    sessionValue.MessageCount + 1,
		Time:        time.Now().UTC(),
		TurnID:      turn.TurnID,
		RunID:       turn.RunID,
		ExecutionID: turn.ExecutionID,
		ProfileID:   turn.ProfileID,
		Message: contract.Message{
			Role:    contract.RoleAssistant,
			Content: "tampered provider-unsafe suffix",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	data = append(data, record...)
	data = append(data, '\n')
	if err := os.WriteFile(path, data, 0o600); err != nil {
		t.Fatal(err)
	}
}

func TestDurableAgentExecutionSnapshotDriftFailsBeforeNewEffects(
	t *testing.T,
) {
	for _, preparedSession := range []bool{false, true} {
		name := "fresh"
		if preparedSession {
			name = "recover_after_session_prepare"
		}
		t.Run(name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			profiles := buildAgentProfiles(t, false)
			generator := &agentModel{results: []contract.ModelResult{{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "must not execute",
				},
				FinishReason: contract.FinishStop,
			}}}
			registry, err := agent.NewRegistry()
			if err != nil {
				t.Fatal(err)
			}
			store, err := sqlitestore.Open(
				filepath.Join(root, "runtime.db"),
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			var sessions *session.Service
			sessionID := ""
			if preparedSession {
				sessionStore, err := session.NewStore(
					filepath.Join(root, "sessions"),
					filepath.Join(root, "session-state"),
				)
				if err != nil {
					t.Fatal(err)
				}
				sessions, err = session.NewService(
					session.ServiceOptions{
						Store: sessionStore, Profiles: profiles,
						Models: generator,
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				sessionID, err = session.NewID()
				if err != nil {
					t.Fatal(err)
				}
			}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: profiles, Model: generator,
						Tools: registry, Store: store,
						Sessions: sessions,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runs.Close() })
			submitted, runtimeErr := runs.Submit(ctx, runtime.Request{
				Kind: runtime.KindAgent, ProfileID: "api",
				Input: "snapshot-bound", SessionID: sessionID,
			})
			if runtimeErr != nil {
				t.Fatal(runtimeErr)
			}
			if preparedSession {
				turn, runtimeErr := sessions.PrepareAgent(
					session.RunRequest{
						SessionID: sessionID, RunID: submitted.ID,
						ProfileID: "api", Input: "snapshot-bound",
					},
				)
				if runtimeErr != nil ||
					turn.RequestDigest == "" ||
					turn.ConfigDigest == "" ||
					turn.RequestDigest == turn.ConfigDigest {
					t.Fatalf("turn=%#v err=%v", turn, runtimeErr)
				}
			}
			if err := registry.Register(agent.RegisteredTool{
				Definition: contract.ToolSpec{
					Name: "drifted",
					InputSchema: json.RawMessage(
						`{"type":"object","additionalProperties":false}`,
					),
				},
				Handler: func(
					context.Context,
					agent.ToolRequest,
				) (agent.ToolResult, error) {
					t.Fatal("drifted tool handler executed")
					return agent.ToolResult{}, nil
				},
			}); err != nil {
				t.Fatal(err)
			}
			current, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
			if !found || runtimeErr == nil ||
				runtimeErr.Code != contract.ErrorConflict ||
				runtimeErr.Phase != contract.PhaseProfile ||
				current.State != runtime.StateFailed ||
				!bytes.Contains(
					current.Result,
					[]byte(`"stop_reason":"execution_snapshot_changed"`),
				) {
				t.Fatalf(
					"current=%#v found=%v err=%v",
					current, found, runtimeErr,
				)
			}
			modelCalls, err := store.ModelCalls(ctx, submitted.ID)
			if err != nil {
				t.Fatal(err)
			}
			effects, err := store.ToolEffects(ctx, submitted.ID)
			if err != nil {
				t.Fatal(err)
			}
			if len(generator.requests) != 0 ||
				len(modelCalls) != 0 || len(effects) != 0 {
				t.Fatalf(
					"provider=%d modelCalls=%#v effects=%#v",
					len(generator.requests), modelCalls, effects,
				)
			}
			if preparedSession {
				result, found, err := sessions.ResultForRun(
					sessionID, submitted.ID,
				)
				if err != nil || !found ||
					result.State != session.TurnFailed ||
					result.Error == nil ||
					result.Error.Code != contract.ErrorConflict {
					t.Fatalf(
						"Session result=%#v found=%v err=%v",
						result, found, err,
					)
				}
				sessionValue, err := sessions.Get(sessionID)
				if err != nil ||
					sessionValue.State != session.SessionIdle ||
					sessionValue.ActiveTurnID != "" {
					t.Fatalf(
						"Session=%#v err=%v",
						sessionValue, err,
					)
				}
			}
		})
	}
}

func TestAgentToolSnapshotDriftEffectBarrier(t *testing.T) {
	testCases := []struct {
		name               string
		driftAfterPrepare  bool
		wantEffects        int
		wantCheckpoint     int
		wantToolFailed     int
		wantEffectTerminal bool
	}{
		{
			name: "after_model_before_prepare",
		},
		{
			name:               "after_prepare_before_start",
			driftAfterPrepare:  true,
			wantEffects:        1,
			wantCheckpoint:     1,
			wantToolFailed:     1,
			wantEffectTerminal: true,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			call := contract.ToolCall{
				ID: "call_tool_snapshot_barrier", Name: "echo",
				Arguments: json.RawMessage(`{"value":"ok"}`),
			}
			handlerCalls := 0
			registry, err := agent.NewRegistry(agent.RegisteredTool{
				Definition: contract.ToolSpec{
					Name: "echo",
					InputSchema: json.RawMessage(`{
						"type":"object",
						"required":["value"],
						"properties":{"value":{"type":"string"}},
						"additionalProperties":false
					}`),
				},
				Handler: func(
					context.Context,
					agent.ToolRequest,
				) (agent.ToolResult, error) {
					handlerCalls++
					return agent.ToolResult{Content: "must not run"}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			drift := func() {
				if err := registry.Register(agent.RegisteredTool{
					Definition: contract.ToolSpec{
						Name: "drifted",
						InputSchema: json.RawMessage(
							`{"type":"object","additionalProperties":false}`,
						),
					},
					Handler: func(
						context.Context,
						agent.ToolRequest,
					) (agent.ToolResult, error) {
						t.Fatal("drifted handler executed")
						return agent.ToolResult{}, nil
					},
				}); err != nil {
					t.Fatal(err)
				}
			}
			generator := &agentModel{results: []contract.ModelResult{{
				Message: contract.Message{
					Role:      contract.RoleAssistant,
					ToolCalls: []contract.ToolCall{call},
				},
				FinishReason: contract.FinishToolCall,
			}}}
			store, err := sqlitestore.Open(
				filepath.Join(t.TempDir(), "runtime.db"),
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			var executionStore runtime.Store = store
			if testCase.driftAfterPrepare {
				executionStore = &mutateAfterPrepareStore{
					Store: store, mutate: drift,
				}
			} else {
				generator.afterGenerate = drift
			}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: executionStore,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: buildAgentProfiles(t, false),
						Model:    generator, Tools: registry,
						Store: executionStore,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runs.Close() })
			current, runtimeErr := runs.RunNow(ctx, runtime.Request{
				Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
			}, nil)
			if runtimeErr == nil ||
				runtimeErr.Code != contract.ErrorConflict ||
				runtimeErr.Phase != contract.PhaseProfile ||
				current.State != runtime.StateFailed ||
				!bytes.Contains(
					current.Result,
					[]byte(`"stop_reason":"execution_snapshot_changed"`),
				) {
				t.Fatalf("current=%#v err=%v", current, runtimeErr)
			}
			if handlerCalls != 0 || len(generator.requests) != 1 {
				t.Fatalf(
					"handler=%d provider=%d",
					handlerCalls, len(generator.requests),
				)
			}
			modelCalls, err := store.ModelCalls(ctx, current.ID)
			if err != nil || len(modelCalls) != 1 ||
				modelCalls[0].State != "completed" {
				t.Fatalf("model calls=%#v err=%v", modelCalls, err)
			}
			effects, err := store.ToolEffects(ctx, current.ID)
			if err != nil || len(effects) != testCase.wantEffects {
				t.Fatalf("effects=%#v err=%v", effects, err)
			}
			if testCase.wantEffectTerminal &&
				(effects[0].State != "failed" ||
					effects[0].Error == nil ||
					effects[0].Error.Code != contract.ErrorConflict ||
					effects[0].Error.Phase != contract.PhaseProfile) {
				t.Fatalf("effect=%#v", effects[0])
			}
			events, err := store.Events(ctx, current.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			counts := map[contract.EventType]int{}
			for _, event := range events {
				counts[event.Type]++
			}
			if counts[contract.EventCheckpointCommitted] !=
				testCase.wantCheckpoint ||
				counts[contract.EventToolStarted] != 0 ||
				counts[contract.EventToolCompleted] != 0 ||
				counts[contract.EventToolFailed] !=
					testCase.wantToolFailed {
				t.Fatalf("event counts=%#v events=%#v", counts, events)
			}
		})
	}
}

func TestExpiredDurableAgentResumeDoesNotMutateRunOrSession(t *testing.T) {
	ctx := context.Background()
	before := time.Date(2026, 7, 30, 13, 0, 0, 0, time.UTC)
	expiresAt := before.Add(time.Second)
	after := expiresAt.Add(time.Nanosecond)
	var clockMu sync.Mutex
	clock := before
	now := func() time.Time {
		clockMu.Lock()
		defer clockMu.Unlock()
		return clock
	}
	setClock := func(value time.Time) {
		clockMu.Lock()
		clock = value
		clockMu.Unlock()
	}
	call := contract.ToolCall{
		ID: "call_expired_session_pause", Name: "approval",
		Arguments: json.RawMessage(`{}`),
	}
	generator := &agentModel{results: []contract.ModelResult{{
		Message: contract.Message{
			Role:      contract.RoleAssistant,
			ToolCalls: []contract.ToolCall{call},
		},
		FinishReason: contract.FinishToolCall,
	}}}
	registry, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "approval",
			InputSchema: json.RawMessage(
				`{"type":"object","additionalProperties":false}`,
			),
		},
		Handler: func(
			context.Context,
			agent.ToolRequest,
		) (agent.ToolResult, error) {
			return agent.ToolResult{Pause: &agent.Pause{
				ID: "pause_expired_session", Kind: "approval",
				Prompt: "approve?", ExpiresAt: &expiresAt,
				InputSchema: json.RawMessage(`{
					"type":"object",
					"required":["approved"],
					"properties":{"approved":{"type":"boolean"}},
					"additionalProperties":false
				}`),
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles := buildAgentProfiles(t, false)
	root := t.TempDir()
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"),
		filepath.Join(root, "session-state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		filepath.Join(root, "runtime.db"),
		sqlitestore.Options{Now: now, SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Profiles: profiles, Model: generator, Tools: registry,
				Store: store, Sessions: sessions, Now: now,
			},
		},
		Now: now,
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runs.Close() })
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	paused, runtimeErr := runs.RunNow(ctx, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api",
		Input: "start", SessionID: sessionID,
	}, nil)
	if runtimeErr != nil || paused.State != runtime.StatePaused {
		t.Fatalf("paused=%#v err=%v", paused, runtimeErr)
	}
	beforeSession, err := sessions.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	beforeMessages, err := sessions.Messages(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	beforeEvents, err := sessions.Events(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	setClock(after)
	if _, err := runs.Resume(
		ctx, paused.ID,
		json.RawMessage(
			`{"pause_id":"pause_expired_session","input":{"approved":true}}`,
		),
	); !errors.Is(err, runtime.ErrConflict) {
		t.Fatalf("resume error=%v", err)
	}
	afterRun, err := runs.Get(ctx, paused.ID)
	if err != nil {
		t.Fatal(err)
	}
	afterSession, err := sessions.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	afterMessages, err := sessions.Messages(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	afterEvents, err := sessions.Events(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	if afterRun.State != runtime.StatePaused ||
		len(afterRun.Request.Resume) != 0 ||
		afterRun.ResumeAcceptedAt != nil ||
		!reflect.DeepEqual(beforeSession, afterSession) ||
		!reflect.DeepEqual(beforeMessages, afterMessages) ||
		!reflect.DeepEqual(beforeEvents, afterEvents) {
		t.Fatalf(
			"expired resume mutated facts: run=%#v session=%#v",
			afterRun, afterSession,
		)
	}
	resumes, err := store.Resumes(ctx, paused.ID)
	if err != nil {
		t.Fatal(err)
	}
	if len(resumes) != 0 || len(generator.requests) != 1 {
		t.Fatalf(
			"resumes=%#v provider_requests=%d",
			resumes, len(generator.requests),
		)
	}
}

func TestDurableAgentRejectsTamperedResumeAcceptanceTime(t *testing.T) {
	for _, target := range []string{
		"run_latest_acceptance",
		"resume_journal_acceptance",
	} {
		t.Run(target, func(t *testing.T) {
			ctx := context.Background()
			fixedNow := time.Date(
				2026, 7, 30, 14, 0, 0, 0, time.UTC,
			)
			call := contract.ToolCall{
				ID: "call_resume_time", Name: "approval",
				Arguments: json.RawMessage(`{}`),
			}
			generator := &agentModel{results: []contract.ModelResult{
				{
					Message: contract.Message{
						Role:      contract.RoleAssistant,
						ToolCalls: []contract.ToolCall{call},
					},
					FinishReason: contract.FinishToolCall,
				},
				{
					Message: contract.Message{
						Role:    contract.RoleAssistant,
						Content: "must not replay",
					},
					FinishReason: contract.FinishStop,
				},
			}}
			registry, err := agent.NewRegistry(agent.RegisteredTool{
				Definition: contract.ToolSpec{
					Name: "approval",
					InputSchema: json.RawMessage(
						`{"type":"object","additionalProperties":false}`,
					),
				},
				Handler: func(
					context.Context,
					agent.ToolRequest,
				) (agent.ToolResult, error) {
					return agent.ToolResult{Pause: &agent.Pause{
						ID: "pause_resume_time", Kind: "approval",
						Prompt: "approve?",
						InputSchema: json.RawMessage(`{
							"type":"object",
							"required":["approved"],
							"properties":{"approved":{"type":"boolean"}},
							"additionalProperties":false
						}`),
					}}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			databasePath := filepath.Join(t.TempDir(), "runtime.db")
			store, err := sqlitestore.Open(
				databasePath,
				sqlitestore.Options{
					Now:           func() time.Time { return fixedNow },
					SkipReconcile: true,
				},
			)
			if err != nil {
				t.Fatal(err)
			}
			profiles := buildAgentProfiles(t, false)
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: profiles, Model: generator,
						Tools: registry, Store: store,
					},
				},
				Now: func() time.Time { return fixedNow },
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runs.Close() })
			paused, runtimeErr := runs.RunNow(ctx, runtime.Request{
				Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
			}, nil)
			if runtimeErr != nil || paused.State != runtime.StatePaused {
				t.Fatalf("paused=%#v err=%v", paused, runtimeErr)
			}
			if _, err := runs.Resume(
				ctx, paused.ID,
				json.RawMessage(
					`{"pause_id":"pause_resume_time","input":{"approved":true}}`,
				),
			); err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", databasePath)
			if err != nil {
				t.Fatal(err)
			}
			tampered := fixedNow.Add(time.Second).Format(
				time.RFC3339Nano,
			)
			switch target {
			case "run_latest_acceptance":
				_, err = db.ExecContext(
					ctx,
					`UPDATE runs
					    SET resume_accepted_at = ?
					  WHERE run_id = ?`,
					tampered, paused.ID,
				)
			case "resume_journal_acceptance":
				_, err = db.ExecContext(
					ctx,
					`UPDATE resumes
					    SET accepted_at = ?
					  WHERE run_id = ? AND sequence = 1`,
					tampered, paused.ID,
				)
			}
			if closeErr := db.Close(); err == nil {
				err = closeErr
			}
			if err != nil {
				t.Fatal(err)
			}
			unknown, found, runtimeErr := runs.ExecuteNext(
				ctx, "worker",
			)
			if !found || runtimeErr == nil ||
				runtimeErr.Code != contract.ErrorConflict ||
				unknown.State != runtime.StateNeedsReconciliation {
				t.Fatalf(
					"unknown=%#v found=%v err=%v",
					unknown, found, runtimeErr,
				)
			}
			if len(generator.requests) != 1 {
				t.Fatalf(
					"provider was replayed: requests=%d",
					len(generator.requests),
				)
			}
		})
	}
}

func TestDurableAgentRecoversPreparedSiblingAfterPauseResume(
	t *testing.T,
) {
	testCases := []struct {
		name  string
		calls []contract.ToolCall
	}{
		{
			name: "first_sibling_pauses",
			calls: []contract.ToolCall{
				{
					ID: "call_pause_first", Name: "approval",
					Arguments: json.RawMessage(`{}`),
				},
				{
					ID: "call_after_first_pause", Name: "echo",
					Arguments: json.RawMessage(`{"value":"after"}`),
				},
			},
		},
		{
			name: "middle_sibling_pauses",
			calls: []contract.ToolCall{
				{
					ID: "call_before_middle_pause", Name: "echo",
					Arguments: json.RawMessage(`{"value":"before"}`),
				},
				{
					ID: "call_pause_middle", Name: "approval",
					Arguments: json.RawMessage(`{}`),
				},
				{
					ID: "call_after_middle_pause", Name: "echo",
					Arguments: json.RawMessage(`{"value":"after"}`),
				},
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			ctx := context.Background()
			root := t.TempDir()
			databasePath := filepath.Join(root, "runtime.db")
			profiles := buildAgentProfiles(t, false)
			initialModel := &agentModel{results: []contract.ModelResult{{
				Message: contract.Message{
					Role: contract.RoleAssistant,
					ToolCalls: append(
						[]contract.ToolCall(nil), testCase.calls...,
					),
				},
				FinishReason: contract.FinishToolCall,
			}}}
			executions := make(map[string]int)
			buildRegistry := func() *agent.Registry {
				registry, err := agent.NewRegistry(
					agent.RegisteredTool{
						Definition: contract.ToolSpec{
							Name: "approval",
							InputSchema: json.RawMessage(
								`{"type":"object","additionalProperties":false}`,
							),
						},
						Handler: func(
							context.Context,
							agent.ToolRequest,
						) (agent.ToolResult, error) {
							executions["approval"]++
							return agent.ToolResult{
								Pause: &agent.Pause{
									ID: "pause_sibling", Kind: "approval",
									Prompt: "approve sibling continuation?",
									InputSchema: json.RawMessage(`{
										"type":"object",
										"required":["approved"],
										"properties":{"approved":{"type":"boolean"}},
										"additionalProperties":false
									}`),
								},
							}, nil
						},
					},
					agent.RegisteredTool{
						Definition: contract.ToolSpec{
							Name: "echo",
							InputSchema: json.RawMessage(`{
								"type":"object",
								"required":["value"],
								"properties":{"value":{"type":"string"}},
								"additionalProperties":false
							}`),
						},
						Handler: func(
							_ context.Context,
							request agent.ToolRequest,
						) (agent.ToolResult, error) {
							executions[request.CallID]++
							return agent.ToolResult{
								Content: string(request.Arguments),
							}, nil
						},
					},
				)
				if err != nil {
					t.Fatal(err)
				}
				return registry
			}
			sqliteStore, err := sqlitestore.Open(
				databasePath,
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			failingStore := &failPreparedToolStartStore{
				Store:  sqliteStore,
				callID: testCase.calls[len(testCase.calls)-1].ID,
			}
			registry := buildRegistry()
			executor := &runtime.AgentExecutor{
				Profiles: profiles, Model: initialModel,
				Tools: registry, Store: failingStore,
			}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: failingStore,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: executor,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			paused, runtimeErr := runs.RunNow(ctx, runtime.Request{
				Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
			}, nil)
			if runtimeErr != nil || paused.State != runtime.StatePaused {
				t.Fatalf("paused=%#v err=%v", paused, runtimeErr)
			}
			if executions["approval"] != 1 {
				t.Fatalf("approval executions=%d", executions["approval"])
			}
			resumed, err := runs.Resume(
				ctx, paused.ID,
				json.RawMessage(
					`{"pause_id":"pause_sibling","input":{"approved":true}}`,
				),
			)
			if err != nil || resumed.State != runtime.StateQueued {
				t.Fatalf("resumed=%#v err=%v", resumed, err)
			}
			running, err := failingStore.Start(ctx, paused.ID)
			if err != nil {
				t.Fatal(err)
			}
			outcome := executor.Execute(
				ctx, running,
				func(event contract.Event) error {
					_, err := failingStore.AppendEvent(
						ctx, running.ID, event,
					)
					return err
				},
			)
			if outcome.State != runtime.StateNeedsReconciliation ||
				outcome.Error == nil ||
				!strings.Contains(
					outcome.Error.Message,
					"prepare tool checkpoint",
				) {
				t.Fatalf("pre-crash outcome=%#v", outcome)
			}
			if executions[testCase.calls[len(testCase.calls)-1].ID] != 0 {
				t.Fatal("prepared sibling executed before simulated crash")
			}
			if err := runs.Close(); err != nil {
				t.Fatal(err)
			}

			sqliteStore, err = sqlitestore.Open(
				databasePath, sqlitestore.Options{},
			)
			if err != nil {
				t.Fatal(err)
			}
			recoveryModel := &agentModel{results: []contract.ModelResult{{
				Message: contract.Message{
					Role: contract.RoleAssistant, Content: "done",
				},
				FinishReason: contract.FinishStop,
			}}}
			recoveryExecutor := &runtime.AgentExecutor{
				Profiles: profiles, Model: recoveryModel,
				Tools: buildRegistry(), Store: sqliteStore,
			}
			runs, err = runtime.NewService(runtime.ServiceOptions{
				Store: sqliteStore,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: recoveryExecutor,
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { _ = runs.Close() })
			completed, found, runtimeErr := runs.ExecuteNext(
				ctx, "recovery-worker",
			)
			if runtimeErr != nil || !found ||
				completed.ID != paused.ID ||
				completed.State != runtime.StateCompleted {
				t.Fatalf(
					"completed=%#v found=%v err=%v",
					completed, found, runtimeErr,
				)
			}
			if executions["approval"] != 1 {
				t.Fatalf(
					"approval was replayed: executions=%d",
					executions["approval"],
				)
			}
			for index, call := range testCase.calls {
				if call.Name != "echo" {
					continue
				}
				if executions[call.ID] != 1 {
					t.Fatalf(
						"echo sibling %d executions=%d",
						index, executions[call.ID],
					)
				}
			}
			events, err := runs.Events(ctx, paused.ID, 0, 100)
			if err != nil {
				t.Fatal(err)
			}
			counts := make(map[contract.EventType]int)
			for _, event := range events {
				counts[event.Type]++
			}
			if counts[contract.EventAgentPaused] != 1 ||
				counts[contract.EventCheckpointCommitted] !=
					len(testCase.calls) ||
				counts[contract.EventToolStarted] != len(testCase.calls) {
				t.Fatalf("lifecycle events=%#v", events)
			}
		})
	}
}

func TestDurableAgentRecoversFinalCheckpointAfterPauseResume(
	t *testing.T,
) {
	ctx := context.Background()
	call := contract.ToolCall{
		ID: "call_pause_recovery", Name: "approval",
		Arguments: json.RawMessage(`{}`),
	}
	generator := &agentModel{results: []contract.ModelResult{
		{
			Message: contract.Message{
				Role:      contract.RoleAssistant,
				ToolCalls: []contract.ToolCall{call},
			},
			FinishReason: contract.FinishToolCall,
		},
		{
			Message: contract.Message{
				Role: contract.RoleAssistant, Content: "resumed",
			},
			FinishReason: contract.FinishStop,
		},
	}}
	registry, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "approval",
			InputSchema: json.RawMessage(
				`{"type":"object","additionalProperties":false}`,
			),
		},
		Handler: func(
			context.Context,
			agent.ToolRequest,
		) (agent.ToolResult, error) {
			return agent.ToolResult{Pause: &agent.Pause{
				ID: "pause_recovery", Kind: "approval", Prompt: "approve?",
				InputSchema: json.RawMessage(`{
					"type":"object",
					"required":["approved"],
					"properties":{"approved":{"type":"boolean"}},
					"additionalProperties":false
				}`),
			}}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles := buildAgentProfiles(t, false)
	databasePath := filepath.Join(t.TempDir(), "runtime.db")
	store, err := sqlitestore.Open(
		databasePath, sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	executor := &runtime.AgentExecutor{
		Profiles: profiles, Model: generator, Tools: registry, Store: store,
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: executor,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	paused, runtimeErr := runs.RunNow(ctx, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
	}, nil)
	if runtimeErr != nil || paused.State != runtime.StatePaused {
		t.Fatalf("paused=%#v err=%v", paused, runtimeErr)
	}
	if _, err := runs.Resume(
		ctx, paused.ID,
		json.RawMessage(
			`{"pause_id":"pause_recovery","input":{"approved":true}}`,
		),
	); err != nil {
		t.Fatal(err)
	}
	running, err := store.Start(ctx, paused.ID)
	if err != nil {
		t.Fatal(err)
	}
	outcome := executor.Execute(
		ctx, running,
		func(event contract.Event) error {
			_, err := store.AppendEvent(ctx, running.ID, event)
			return err
		},
	)
	if outcome.State != runtime.StateCompleted {
		t.Fatalf("outcome=%#v", outcome)
	}
	if len(generator.requests) != 2 {
		t.Fatalf("model requests=%d", len(generator.requests))
	}
	runs.Close()
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = sqlitestore.Open(databasePath, sqlitestore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	replayModel := &agentModel{}
	runs, err = runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Profiles: profiles, Model: replayModel,
				Tools: registry, Store: store,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runs.Close() })
	completed, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
	if runtimeErr != nil || !found ||
		completed.ID != paused.ID ||
		completed.State != runtime.StateCompleted {
		t.Fatalf(
			"completed=%#v found=%v err=%v",
			completed, found, runtimeErr,
		)
	}
	if len(replayModel.requests) != 0 {
		t.Fatalf("provider was replayed: %#v", replayModel.requests)
	}
	events, err := runs.Events(ctx, paused.ID, 0, 100)
	if err != nil {
		t.Fatal(err)
	}
	counts := make(map[contract.EventType]int)
	for _, event := range events {
		counts[event.Type]++
	}
	if counts[contract.EventAgentPaused] != 1 ||
		counts[contract.EventAgentCompleted] != 1 {
		t.Fatalf("events=%#v", events)
	}
}

func TestDurableAgentRejectsForgedHistoricalJournal(t *testing.T) {
	testCases := []string{
		"extra_assistant_message",
		"unknown_tool_message",
		"tampered_preparation_checkpoint",
	}
	for _, testCase := range testCases {
		t.Run(testCase, func(t *testing.T) {
			ctx := context.Background()
			call := contract.ToolCall{
				ID: "call_historical", Name: "echo",
				Arguments: json.RawMessage(`{"value":"ok"}`),
			}
			generator := &agentModel{results: []contract.ModelResult{
				{
					Message: contract.Message{
						Role:      contract.RoleAssistant,
						ToolCalls: []contract.ToolCall{call},
					},
					FinishReason: contract.FinishToolCall,
				},
				{
					Message: contract.Message{
						Role: contract.RoleAssistant, Content: "done",
					},
					FinishReason: contract.FinishStop,
				},
			}}
			registry, err := agent.NewRegistry(agent.RegisteredTool{
				Definition: contract.ToolSpec{
					Name: "echo",
					InputSchema: json.RawMessage(`{
						"type":"object",
						"required":["value"],
						"properties":{"value":{"type":"string"}},
						"additionalProperties":false
					}`),
				},
				Handler: func(
					context.Context,
					agent.ToolRequest,
				) (agent.ToolResult, error) {
					return agent.ToolResult{Content: "tool-result"}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			databasePath := filepath.Join(t.TempDir(), "runtime.db")
			store, err := sqlitestore.Open(
				databasePath,
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			runID := "run_" + strings.Repeat("f", 32)
			storedRequest := prepareStoredAgentRequest(
				t, store,
				runtime.Request{
					Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
					AgentBudget: agent.DefaultBudget(),
				},
				registry.Definitions(), nil,
			)
			record, err := store.Create(ctx, runID, storedRequest)
			if err != nil {
				t.Fatal(err)
			}
			record, err = store.Start(ctx, record.ID)
			if err != nil {
				t.Fatal(err)
			}
			executor := &runtime.AgentExecutor{
				Profiles: buildAgentProfiles(t, false),
				Model:    generator, Tools: registry, Store: store,
			}
			outcome := executor.Execute(
				ctx, record,
				func(event contract.Event) error {
					_, err := store.AppendEvent(ctx, runID, event)
					return err
				},
			)
			if outcome.State != runtime.StateCompleted {
				t.Fatalf("outcome=%#v", outcome)
			}
			latest, exists, err := store.LatestCheckpoint(ctx, runID)
			if err != nil || !exists {
				t.Fatalf("latest=%#v exists=%v err=%v", latest, exists, err)
			}
			targetCheckpointID := latest.ID
			var tampered agent.LoopState
			if err := json.Unmarshal(latest.State, &tampered); err != nil {
				t.Fatal(err)
			}
			switch testCase {
			case "extra_assistant_message":
				tampered.Messages = append(
					append(
						[]contract.Message(nil),
						tampered.Messages[:tampered.BaseMessageCount]...,
					),
					append(
						[]contract.Message{{
							Role:    contract.RoleAssistant,
							Content: "forged",
						}},
						tampered.Messages[tampered.BaseMessageCount:]...,
					)...,
				)
			case "unknown_tool_message":
				finalIndex := len(tampered.Messages) - 1
				tampered.Messages = append(
					append(
						[]contract.Message(nil),
						tampered.Messages[:finalIndex]...,
					),
					append(
						[]contract.Message{{
							Role:       contract.RoleTool,
							ToolCallID: "call_unknown",
							Content:    "forged",
						}},
						tampered.Messages[finalIndex:]...,
					)...,
				)
			case "tampered_preparation_checkpoint":
				effect, exists, err := store.ToolEffect(
					ctx, runID, call.ID,
				)
				if err != nil || !exists {
					t.Fatalf(
						"effect=%#v exists=%v err=%v",
						effect, exists, err,
					)
				}
				var request agent.ToolRequest
				if err := json.Unmarshal(effect.Request, &request); err != nil {
					t.Fatal(err)
				}
				prepared, exists, err := store.Checkpoint(
					ctx, request.CheckpointID,
				)
				if err != nil || !exists {
					t.Fatalf(
						"prepared=%#v exists=%v err=%v",
						prepared, exists, err,
					)
				}
				if err := json.Unmarshal(prepared.State, &tampered); err != nil {
					t.Fatal(err)
				}
				tampered.ModelProfile = "forged"
				targetCheckpointID = prepared.ID
			}
			tamperedJSON, err := json.Marshal(tampered)
			if err != nil {
				t.Fatal(err)
			}
			db, err := sql.Open("sqlite", databasePath)
			if err != nil {
				t.Fatal(err)
			}
			if _, err := db.Exec(
				`UPDATE checkpoints
				    SET state_json = ?
				  WHERE checkpoint_id = ?`,
				tamperedJSON, targetCheckpointID,
			); err != nil {
				db.Close()
				t.Fatal(err)
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			store, err = sqlitestore.Open(
				databasePath, sqlitestore.Options{},
			)
			if err != nil {
				t.Fatal(err)
			}
			replayModel := &agentModel{}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: buildAgentProfiles(t, false),
						Model:    replayModel, Tools: registry, Store: store,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { runs.Close() })
			current, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
			if !found || runtimeErr == nil ||
				current.State != runtime.StateNeedsReconciliation {
				t.Fatalf(
					"current=%#v found=%v err=%v",
					current, found, runtimeErr,
				)
			}
			if len(replayModel.requests) != 0 {
				t.Fatalf("provider was replayed: %#v", replayModel.requests)
			}
		})
	}
}

func TestDurableAgentRejectsTamperedPauseEvidence(t *testing.T) {
	testCases := []string{
		"pause_id",
		"pause_state",
		"pause_tool_call_id",
	}
	for _, testCase := range testCases {
		t.Run(testCase, func(t *testing.T) {
			ctx := context.Background()
			call := contract.ToolCall{
				ID: "call_pause_tamper", Name: "approval",
				Arguments: json.RawMessage(`{}`),
			}
			generator := &agentModel{results: []contract.ModelResult{{
				Message: contract.Message{
					Role:      contract.RoleAssistant,
					ToolCalls: []contract.ToolCall{call},
				},
				FinishReason: contract.FinishToolCall,
			}}}
			registry, err := agent.NewRegistry(agent.RegisteredTool{
				Definition: contract.ToolSpec{
					Name: "approval",
					InputSchema: json.RawMessage(
						`{"type":"object","additionalProperties":false}`,
					),
				},
				Handler: func(
					context.Context,
					agent.ToolRequest,
				) (agent.ToolResult, error) {
					return agent.ToolResult{Pause: &agent.Pause{
						ID: "pause_tamper", Kind: "approval",
						Prompt: "approve?",
						InputSchema: json.RawMessage(`{
							"type":"object",
							"required":["approved"],
							"properties":{"approved":{"type":"boolean"}},
							"additionalProperties":false
						}`),
					}}, nil
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			databasePath := filepath.Join(t.TempDir(), "runtime.db")
			store, err := sqlitestore.Open(
				databasePath,
				sqlitestore.Options{SkipReconcile: true},
			)
			if err != nil {
				t.Fatal(err)
			}
			runID := "run_" + strings.Repeat("1", 32)
			storedRequest := prepareStoredAgentRequest(
				t, store,
				runtime.Request{
					Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
					AgentBudget: agent.DefaultBudget(),
				},
				registry.Definitions(), nil,
			)
			record, err := store.Create(ctx, runID, storedRequest)
			if err != nil {
				t.Fatal(err)
			}
			record, err = store.Start(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			profiles := buildAgentProfiles(t, false)
			executor := &runtime.AgentExecutor{
				Profiles: profiles, Model: generator,
				Tools: registry, Store: store,
			}
			outcome := executor.Execute(
				ctx, record,
				func(event contract.Event) error {
					_, err := store.AppendEvent(ctx, runID, event)
					return err
				},
			)
			if outcome.State != runtime.StatePaused {
				t.Fatalf("outcome=%#v", outcome)
			}
			db, err := sql.Open("sqlite", databasePath)
			if err != nil {
				t.Fatal(err)
			}
			switch testCase {
			case "pause_id", "pause_state":
				events, err := store.Events(ctx, runID, 0, 100)
				if err != nil {
					db.Close()
					t.Fatal(err)
				}
				var paused contract.Event
				for _, event := range events {
					if event.Type == contract.EventAgentPaused {
						paused = event
						break
					}
				}
				if paused.Agent == nil {
					db.Close()
					t.Fatal("agent.paused event is missing")
				}
				if testCase == "pause_id" {
					paused.Agent.PauseID = "pause_forged"
				} else {
					paused.Agent.State = "completed"
				}
				eventJSON, err := json.Marshal(paused)
				if err != nil {
					db.Close()
					t.Fatal(err)
				}
				if _, err := db.Exec(
					`UPDATE events SET event_json = ?
					  WHERE run_id = ? AND sequence = ?`,
					eventJSON, runID, paused.Sequence,
				); err != nil {
					db.Close()
					t.Fatal(err)
				}
			case "pause_tool_call_id":
				effect, exists, err := store.ToolEffect(
					ctx, runID, call.ID,
				)
				if err != nil || !exists {
					db.Close()
					t.Fatalf(
						"effect=%#v exists=%v err=%v",
						effect, exists, err,
					)
				}
				var result agent.ToolResult
				if err := json.Unmarshal(effect.Result, &result); err != nil {
					db.Close()
					t.Fatal(err)
				}
				result.Pause.ToolCallID = "call_forged"
				resultJSON, err := json.Marshal(result)
				if err != nil {
					db.Close()
					t.Fatal(err)
				}
				if _, err := db.Exec(
					`UPDATE tool_effects SET result_json = ?
					  WHERE run_id = ? AND call_id = ?`,
					resultJSON, runID, call.ID,
				); err != nil {
					db.Close()
					t.Fatal(err)
				}
			}
			if err := db.Close(); err != nil {
				t.Fatal(err)
			}
			if err := store.Close(); err != nil {
				t.Fatal(err)
			}

			store, err = sqlitestore.Open(
				databasePath, sqlitestore.Options{},
			)
			if err != nil {
				t.Fatal(err)
			}
			replayModel := &agentModel{}
			runs, err := runtime.NewService(runtime.ServiceOptions{
				Store: store,
				Executors: map[runtime.Kind]runtime.Executor{
					runtime.KindAgent: &runtime.AgentExecutor{
						Profiles: profiles, Model: replayModel,
						Tools: registry, Store: store,
					},
				},
			})
			if err != nil {
				t.Fatal(err)
			}
			t.Cleanup(func() { runs.Close() })
			current, err := runs.Get(ctx, runID)
			if err != nil {
				t.Fatal(err)
			}
			if current.State == runtime.StateNeedsReconciliation {
				if len(replayModel.requests) != 0 {
					t.Fatalf(
						"provider was replayed: %#v",
						replayModel.requests,
					)
				}
				return
			}
			current, found, runtimeErr := runs.ExecuteNext(ctx, "worker")
			if !found || runtimeErr == nil ||
				current.State != runtime.StateNeedsReconciliation {
				t.Fatalf(
					"current=%#v found=%v err=%v",
					current, found, runtimeErr,
				)
			}
			if len(replayModel.requests) != 0 {
				t.Fatalf("provider was replayed: %#v", replayModel.requests)
			}
		})
	}
}

func TestDurableAgentSessionUnknownEffectReconcilesIdempotently(t *testing.T) {
	call := contract.ToolCall{
		ID: "call_unknown", Name: "echo",
		Arguments: json.RawMessage(`{"value":"maybe"}`),
	}
	generator := &agentModel{results: []contract.ModelResult{
		{
			Message: contract.Message{
				Role: contract.RoleAssistant, ToolCalls: []contract.ToolCall{call},
			},
			FinishReason: contract.FinishToolCall,
		},
		{
			Message: contract.Message{
				Role: contract.RoleAssistant, Content: "after reconciliation",
			},
			FinishReason: contract.FinishStop,
		},
	}}
	toolExecutions := 0
	registry, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "echo", InputSchema: json.RawMessage(`{"type":"object"}`),
		},
		Handler: func(context.Context, agent.ToolRequest) (agent.ToolResult, error) {
			toolExecutions++
			return agent.ToolResult{Content: `{"value":"maybe"}`}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles := buildAgentProfiles(t, false)
	root := t.TempDir()
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	databasePath := filepath.Join(root, "state", "runtime.db")
	sqliteStore, err := sqlitestore.Open(
		databasePath, sqlitestore.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	store := &failToolStartedEventStore{Store: sqliteStore}
	executor := &runtime.AgentExecutor{
		Profiles: profiles, Model: generator, Tools: registry,
		Store: store, Sessions: sessions,
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: executor,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runs.Close() })
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	unknown, runtimeErr := runs.RunNow(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api",
		Input: "start", SessionID: sessionID,
	}, nil)
	if runtimeErr == nil || unknown.State != runtime.StateNeedsReconciliation {
		t.Fatalf("unknown=%#v err=%v", unknown, runtimeErr)
	}
	if toolExecutions != 0 {
		t.Fatalf("tool executed after its started event was lost: %d", toolExecutions)
	}
	sessionValue, err := sessions.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sessionValue.State != session.SessionBlocked ||
		sessionValue.ActiveTurnID == "" {
		t.Fatalf("session=%#v", sessionValue)
	}
	sessionResult, found, err := sessions.ResultForRun(sessionID, unknown.ID)
	if err != nil || !found ||
		sessionResult.State != session.TurnRunning ||
		sessionResult.Error == nil {
		t.Fatalf("result=%#v found=%v err=%v", sessionResult, found, err)
	}
	execution, err := sessions.Execution(
		sessionID, sessionResult.ExecutionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if execution.State != session.ExecutionSettled ||
		execution.Outcome != session.OutcomeUnknown {
		t.Fatalf("execution=%#v", execution)
	}
	if _, directErr := sessions.Reconcile(
		context.Background(), sessionID,
		session.ReconcileOptions{AcknowledgeUnknown: true},
	); directErr == nil ||
		directErr.Message !=
			"Agent-backed Session must be reconciled through run reconcile" {
		t.Fatalf("direct Session reconcile error=%v", directErr)
	}
	if _, blockedErr := sessions.PrepareAgent(session.RunRequest{
		SessionID: sessionID,
		RunID:     "run_22222222222222222222222222222222",
		ProfileID: "api",
		Input:     "must stay blocked",
	}); blockedErr == nil ||
		blockedErr.Code != contract.ErrorConflict {
		t.Fatalf("blocked PrepareAgent error=%v", blockedErr)
	}
	db, err := sql.Open("sqlite", databasePath)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	assertToolEffectState := func(want string) {
		t.Helper()
		var state string
		if err := db.QueryRow(
			`SELECT state FROM tool_effects WHERE run_id = ? AND call_id = ?`,
			unknown.ID, call.ID,
		).Scan(&state); err != nil {
			t.Fatal(err)
		}
		if state != want {
			t.Fatalf("tool effect state=%q want=%q", state, want)
		}
	}
	assertToolEffectState("started")

	store.failTerminalPublishOnce()
	if _, runtimeErr := runs.ReconcileRun(
		context.Background(), unknown.ID,
	); runtimeErr == nil || runtimeErr.Code != contract.ErrorInternal {
		t.Fatalf("first reconciliation error=%v", runtimeErr)
	}
	stillUnknown, getErr := runs.Get(context.Background(), unknown.ID)
	if getErr != nil ||
		stillUnknown.State != runtime.StateNeedsReconciliation {
		t.Fatalf("stillUnknown=%#v err=%v", stillUnknown, getErr)
	}
	nextTurn, sessionRuntimeErr := sessions.PrepareAgent(session.RunRequest{
		SessionID: sessionID,
		RunID:     "run_66666666666666666666666666666666",
		ProfileID: "api",
		Input:     "new Turn between the two Store commits",
	})
	if sessionRuntimeErr != nil {
		t.Fatal(sessionRuntimeErr)
	}
	reconciled, runtimeErr := runs.ReconcileRun(
		context.Background(), unknown.ID,
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if reconciled.State != runtime.StateFailed ||
		reconciled.Error == nil ||
		reconciled.SettledSequence == 0 {
		t.Fatalf("reconciled=%#v", reconciled)
	}
	repeated, runtimeErr := runs.ReconcileRun(
		context.Background(), unknown.ID,
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if repeated.ID != reconciled.ID ||
		repeated.State != reconciled.State ||
		repeated.SettledSequence != reconciled.SettledSequence ||
		string(repeated.Result) != string(reconciled.Result) {
		t.Fatalf("repeated=%#v reconciled=%#v", repeated, reconciled)
	}
	repeatedSession, sessionRuntimeErr := sessions.ReconcileAgent(
		context.Background(), sessionID, unknown.ID, unknown.Error,
	)
	if sessionRuntimeErr != nil ||
		!repeatedSession.Resolved ||
		repeatedSession.RunID != unknown.ID ||
		repeatedSession.State != session.TurnFailed {
		t.Fatalf(
			"repeated Session reconciliation=%#v err=%v",
			repeatedSession, sessionRuntimeErr,
		)
	}
	assertToolEffectState("started")
	sessionValue, err = sessions.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sessionValue.State != session.SessionActive ||
		sessionValue.ActiveTurnID != nextTurn.TurnID {
		t.Fatalf("new active Turn was overwritten: %#v", sessionValue)
	}
	sessionResult, found, err = sessions.ResultForRun(sessionID, unknown.ID)
	if err != nil || !found ||
		sessionResult.State != session.TurnFailed ||
		sessionResult.Error == nil {
		t.Fatalf(
			"reconciled result=%#v found=%v err=%v",
			sessionResult, found, err,
		)
	}
	execution, err = sessions.Execution(
		sessionID, sessionResult.ExecutionID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if execution.Outcome != session.OutcomeUnknown {
		t.Fatalf("reconciled execution=%#v", execution)
	}
	laterCall := contract.ToolCall{
		ID: "call_later_pause", Name: "approval",
		Arguments: json.RawMessage(`{}`),
	}
	laterMessages := append(
		append([]contract.Message(nil), nextTurn.Messages...),
		contract.Message{
			Role:      contract.RoleAssistant,
			ToolCalls: []contract.ToolCall{laterCall},
		},
	)
	if _, sessionRuntimeErr := sessions.SettleAgent(
		nextTurn, laterMessages,
		agent.Outcome{
			State: agent.StatePaused, StopReason: "input_required",
			Pause: &agent.Pause{
				ID: "pause_later", Kind: "approval",
				Prompt:      "preserve later blocked Turn",
				ToolCallID:  laterCall.ID,
				InputSchema: json.RawMessage(`{"type":"object"}`),
			},
		},
	); sessionRuntimeErr != nil {
		t.Fatal(sessionRuntimeErr)
	}
	if _, sessionRuntimeErr := sessions.ReconcileAgent(
		context.Background(), sessionID, unknown.ID, unknown.Error,
	); sessionRuntimeErr != nil {
		t.Fatal(sessionRuntimeErr)
	}
	sessionValue, err = sessions.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sessionValue.State != session.SessionBlocked ||
		sessionValue.ActiveTurnID != "" {
		t.Fatalf("later blocked Turn was overwritten: %#v", sessionValue)
	}
	resumedTurn, sessionRuntimeErr := sessions.RecoverAgent(session.RunRequest{
		SessionID: sessionID, RunID: nextTurn.RunID,
		ProfileID: "api", Input: "new Turn between the two Store commits",
	})
	if sessionRuntimeErr != nil {
		t.Fatal(sessionRuntimeErr)
	}
	if sessionRuntimeErr := sessions.ActivateAgentResume(resumedTurn); sessionRuntimeErr != nil {
		t.Fatal(sessionRuntimeErr)
	}
	nextMessages := append(
		append([]contract.Message(nil), resumedTurn.Messages...),
		contract.Message{
			Role: contract.RoleAssistant, Content: "new Turn preserved",
		},
	)
	if _, sessionRuntimeErr := sessions.SettleAgent(
		resumedTurn, nextMessages,
		agent.Outcome{
			State: agent.StateCompleted,
			Message: &contract.Message{
				Role: contract.RoleAssistant, Content: "new Turn preserved",
			},
		},
	); sessionRuntimeErr != nil {
		t.Fatal(sessionRuntimeErr)
	}
	next, runtimeErr := runs.RunNow(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api",
		Input: "continue after reconciliation", SessionID: sessionID,
	}, nil)
	if runtimeErr != nil || next.State != runtime.StateCompleted {
		t.Fatalf("next=%#v err=%v", next, runtimeErr)
	}
}

func TestReservedCancellationConvergesAfterSessionSettledBeforeRunCrash(
	t *testing.T,
) {
	ctx := context.Background()
	root := t.TempDir()
	databasePath := filepath.Join(root, "runtime.db")
	profiles := buildAgentProfiles(t, false)
	generator := &agentModel{}
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"),
		filepath.Join(root, "session-state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_55555555555555555555555555555555"
	turn, runtimeErr := sessions.PrepareAgent(session.RunRequest{
		SessionID: sessionID, RunID: runID,
		ProfileID: "api", Input: "cancel before execution",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	cancelledError := &contract.RuntimeError{
		Code: contract.ErrorCancelled, Phase: contract.PhaseRun,
		Message: "run was cancelled",
	}
	sessionResult, sessionErr := sessions.SettleAgent(
		turn, turn.Messages,
		agent.Outcome{
			State: agent.StateCancelled, StopReason: "cancelled",
			Error: cancelledError,
		},
	)
	if sessionErr != nil ||
		sessionResult.State != session.TurnCancelled {
		t.Fatalf("session result=%#v err=%v", sessionResult, sessionErr)
	}
	store, err := sqlitestore.Open(
		databasePath, sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	storedRequest := prepareStoredAgentRequest(
		t, store,
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api",
			Input: "cancel before execution", SessionID: sessionID,
			AgentBudget: agent.DefaultBudget(),
		},
		nil, sessions,
	)
	if _, err := store.Create(ctx, runID, storedRequest); err != nil {
		t.Fatal(err)
	}
	reserved, err := store.RequestCancel(ctx, runID)
	if err != nil || !reserved.CancelRequested ||
		reserved.State != runtime.StateQueued {
		t.Fatalf("reserved=%#v err=%v", reserved, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	store, err = sqlitestore.Open(
		databasePath, sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agent.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Profiles: profiles, Model: generator, Tools: registry,
				Store: store, Sessions: sessions,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runs.Close() })
	if err := runs.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	cancelled, err := runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != runtime.StateCancelled ||
		!cancelled.CancelRequested ||
		cancelled.SettledSequence == 0 ||
		cancelled.Error == nil ||
		cancelled.Error.Code != contract.ErrorCancelled {
		t.Fatalf("cancelled=%#v", cancelled)
	}
	persistedSession, found, err := sessions.ResultForRun(
		sessionID, runID,
	)
	if err != nil || !found ||
		persistedSession.State != session.TurnCancelled ||
		persistedSession.Message != nil {
		t.Fatalf(
			"session result=%#v found=%v err=%v",
			persistedSession, found, err,
		)
	}
	if len(generator.requests) != 0 {
		t.Fatalf("provider was replayed: %#v", generator.requests)
	}
	if err := runs.Reconcile(ctx); err != nil {
		t.Fatal(err)
	}
	repeated, err := runs.Get(ctx, runID)
	if err != nil ||
		repeated.SettledSequence != cancelled.SettledSequence {
		t.Fatalf("repeated=%#v err=%v", repeated, err)
	}
}

func TestAgentCancellationProjectsTerminalCheckpointAfterSessionLag(
	t *testing.T,
) {
	ctx := context.Background()
	root := t.TempDir()
	profiles := buildAgentProfiles(t, false)
	generator := &agentModel{results: []contract.ModelResult{{
		Message: contract.Message{
			Role: contract.RoleAssistant, Content: "durable terminal",
		},
		FinishReason: contract.FinishStop,
	}}}
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"),
		filepath.Join(root, "session-state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	sqliteStore, err := sqlitestore.Open(
		filepath.Join(root, "runtime.db"),
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	crashStore := newTerminalCheckpointCrashStore(sqliteStore)
	registry, err := agent.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_" + strings.Repeat("d", 32)
	storedRequest := prepareStoredAgentRequest(
		t, crashStore,
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api",
			Input: "terminal before Session settle", SessionID: sessionID,
			AgentBudget: agent.DefaultBudget(),
		},
		nil, sessions,
	)
	record, err := crashStore.Create(ctx, runID, storedRequest)
	if err != nil {
		t.Fatal(err)
	}
	record, err = crashStore.Start(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	executor := &runtime.AgentExecutor{
		Profiles: profiles, Model: generator, Tools: registry,
		Store: crashStore, Sessions: sessions,
	}
	executionDone := make(chan any, 1)
	go func() {
		defer func() { executionDone <- recover() }()
		_ = executor.Execute(
			ctx, record,
			func(event contract.Event) error {
				_, err := crashStore.AppendEvent(ctx, runID, event)
				return err
			},
		)
	}()
	select {
	case crashed := <-executionDone:
		if crashed != agentExecutionCrash(
			"after terminal checkpoint commit",
		) {
			t.Fatalf("unexpected execution crash=%#v", crashed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("terminal checkpoint crash was not reached")
	}
	reserved, err := crashStore.RequestCancel(ctx, runID)
	if err != nil || !reserved.CancelRequested ||
		reserved.State != runtime.StateRunning {
		t.Fatalf("reserved=%#v err=%v", reserved, err)
	}
	turn, found, runtimeErr := sessions.LookupAgent(session.RunRequest{
		SessionID: sessionID, RunID: runID,
		ProfileID: "api", Input: "terminal before Session settle",
	})
	if runtimeErr != nil || !found || turn.ExistingResult != nil ||
		len(turn.Messages) != turn.BaseMessageCount {
		t.Fatalf(
			"lagging turn=%#v found=%v err=%v",
			turn, found, runtimeErr,
		)
	}
	finalized := (&runtime.AgentExecutor{
		Store: crashStore, Sessions: sessions,
	}).FinalizeCancellation(ctx, reserved)
	if finalized.State != runtime.StateCompleted ||
		finalized.Error != nil {
		t.Fatalf(
			"finalized=%#v error=%v",
			finalized, finalized.Error,
		)
	}
	result, resultFound, err := sessions.ResultForRun(sessionID, runID)
	if err != nil || !resultFound ||
		result.State != session.TurnCompleted ||
		result.Message == nil ||
		result.Message.Content != "durable terminal" {
		t.Fatalf(
			"Session result=%#v found=%v err=%v",
			result, resultFound, err,
		)
	}
	if len(generator.requests) != 1 {
		t.Fatalf("provider replayed: %d", len(generator.requests))
	}
}

func TestAgentCancellationProjectsCompletedToolRoundAfterSessionLag(
	t *testing.T,
) {
	ctx := context.Background()
	root := t.TempDir()
	profiles := buildAgentProfiles(t, false)
	call := contract.ToolCall{
		ID: "call_cancel_completed_lag", Name: "echo",
		Arguments: json.RawMessage(`{"value":"done"}`),
	}
	generator := &agentModel{results: []contract.ModelResult{
		{
			Message: contract.Message{
				Role:      contract.RoleAssistant,
				ToolCalls: []contract.ToolCall{call},
			},
			FinishReason: contract.FinishToolCall,
		},
		{
			Message: contract.Message{
				Role: contract.RoleAssistant, Content: "must not replay",
			},
			FinishReason: contract.FinishStop,
		},
	}}
	handlerCalls := 0
	registry, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: "echo",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"required":["value"],
				"properties":{"value":{"type":"string"}},
				"additionalProperties":false
			}`),
		},
		Handler: func(
			context.Context,
			agent.ToolRequest,
		) (agent.ToolResult, error) {
			handlerCalls++
			return agent.ToolResult{Content: "durable tool result"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"),
		filepath.Join(root, "session-state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: generator,
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	sqliteStore, err := sqlitestore.Open(
		filepath.Join(root, "runtime.db"),
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	crashStore := newCompletedToolCrashStore(sqliteStore)
	runID := "run_" + strings.Repeat("e", 32)
	storedRequest := prepareStoredAgentRequest(
		t, crashStore,
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api",
			Input:     "completed tool before Session settle",
			SessionID: sessionID, AgentBudget: agent.DefaultBudget(),
		},
		registry.Definitions(), sessions,
	)
	record, err := crashStore.Create(ctx, runID, storedRequest)
	if err != nil {
		t.Fatal(err)
	}
	record, err = crashStore.Start(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	executor := &runtime.AgentExecutor{
		Profiles: profiles, Model: generator, Tools: registry,
		Store: crashStore, Sessions: sessions,
	}
	executionDone := make(chan any, 1)
	go func() {
		defer func() { executionDone <- recover() }()
		_ = executor.Execute(
			ctx, record,
			func(event contract.Event) error {
				_, err := crashStore.AppendEvent(ctx, runID, event)
				return err
			},
		)
	}()
	select {
	case crashed := <-executionDone:
		if crashed != agentExecutionCrash(
			"after completed tool commit",
		) {
			t.Fatalf("unexpected execution crash=%#v", crashed)
		}
	case <-time.After(3 * time.Second):
		t.Fatal("completed tool crash was not reached")
	}
	reserved, err := crashStore.RequestCancel(ctx, runID)
	if err != nil || !reserved.CancelRequested ||
		reserved.State != runtime.StateRunning {
		t.Fatalf("reserved=%#v err=%v", reserved, err)
	}
	turn, found, runtimeErr := sessions.LookupAgent(session.RunRequest{
		SessionID: sessionID, RunID: runID,
		ProfileID: "api", Input: "completed tool before Session settle",
	})
	if runtimeErr != nil || !found || turn.ExistingResult != nil ||
		len(turn.Messages) != turn.BaseMessageCount {
		t.Fatalf(
			"lagging turn=%#v found=%v err=%v",
			turn, found, runtimeErr,
		)
	}
	finalized := (&runtime.AgentExecutor{
		Store: crashStore, Sessions: sessions,
	}).FinalizeCancellation(ctx, reserved)
	if finalized.State != runtime.StateCancelled ||
		finalized.Error == nil ||
		finalized.Error.Code != contract.ErrorCancelled {
		t.Fatalf(
			"finalized=%#v error=%v",
			finalized, finalized.Error,
		)
	}
	result, resultFound, err := sessions.ResultForRun(sessionID, runID)
	if err != nil || !resultFound ||
		result.State != session.TurnCancelled {
		t.Fatalf(
			"Session result=%#v found=%v err=%v",
			result, resultFound, err,
		)
	}
	messages, err := sessions.Messages(sessionID, 0)
	if err != nil || len(messages) != 3 ||
		messages[1].Message.Role != contract.RoleAssistant ||
		messages[2].Message.Role != contract.RoleTool ||
		messages[2].Message.Content != "durable tool result" {
		t.Fatalf("messages=%#v err=%v", messages, err)
	}
	if len(generator.requests) != 1 || handlerCalls != 1 {
		t.Fatalf(
			"side effects replayed: provider=%d handler=%d",
			len(generator.requests), handlerCalls,
		)
	}
}

func TestPausedAgentCancellationIsBoundedAndDoesNotReplay(
	t *testing.T,
) {
	for _, bound := range []bool{false, true} {
		for _, resumeAccepted := range []bool{false, true} {
			name := "unbound"
			if bound {
				name = "bound"
			}
			if resumeAccepted {
				name += "_resume_accepted"
			} else {
				name += "_paused"
			}
			t.Run(name, func(t *testing.T) {
				ctx := context.Background()
				root := t.TempDir()
				profiles := buildAgentProfiles(t, false)
				call := contract.ToolCall{
					ID: "call_cancel_pause", Name: "approval",
					Arguments: json.RawMessage(`{}`),
				}
				generator := &agentModel{
					results: []contract.ModelResult{{
						Message: contract.Message{
							Role:      contract.RoleAssistant,
							ToolCalls: []contract.ToolCall{call},
						},
						FinishReason: contract.FinishToolCall,
					}},
				}
				registry, err := agent.NewRegistry(agent.RegisteredTool{
					Definition: contract.ToolSpec{
						Name: "approval",
						InputSchema: json.RawMessage(
							`{"type":"object","additionalProperties":false}`,
						),
					},
					Handler: func(
						context.Context,
						agent.ToolRequest,
					) (agent.ToolResult, error) {
						return agent.ToolResult{Pause: &agent.Pause{
							ID: "pause_cancel", Kind: "approval",
							Prompt:      "approve?",
							InputSchema: json.RawMessage(`{"type":"boolean"}`),
						}}, nil
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				store, err := sqlitestore.Open(
					filepath.Join(root, "runtime.db"),
					sqlitestore.Options{SkipReconcile: true},
				)
				if err != nil {
					t.Fatal(err)
				}
				var sessions *session.Service
				sessionID := ""
				if bound {
					sessionStore, err := session.NewStore(
						filepath.Join(root, "sessions"),
						filepath.Join(root, "session-state"),
					)
					if err != nil {
						t.Fatal(err)
					}
					sessions, err = session.NewService(
						session.ServiceOptions{
							Store: sessionStore, Profiles: profiles,
							Models: generator,
						},
					)
					if err != nil {
						t.Fatal(err)
					}
					sessionID, err = session.NewID()
					if err != nil {
						t.Fatal(err)
					}
				}
				runs, err := runtime.NewService(runtime.ServiceOptions{
					Store: store,
					Executors: map[runtime.Kind]runtime.Executor{
						runtime.KindAgent: &runtime.AgentExecutor{
							Profiles: profiles, Model: generator,
							Tools: registry, Store: store,
							Sessions: sessions,
						},
					},
				})
				if err != nil {
					t.Fatal(err)
				}
				t.Cleanup(func() { _ = runs.Close() })
				paused, runtimeErr := runs.RunNow(ctx, runtime.Request{
					Kind: runtime.KindAgent, ProfileID: "api",
					Input: "pause before cancel", SessionID: sessionID,
				}, nil)
				if runtimeErr != nil ||
					paused.State != runtime.StatePaused ||
					len(generator.requests) != 1 {
					t.Fatalf(
						"paused=%#v error=%#v requests=%d",
						paused, runtimeErr, len(generator.requests),
					)
				}
				if resumeAccepted {
					paused, err = runs.Resume(
						ctx, paused.ID,
						json.RawMessage(
							`{"pause_id":"pause_cancel","input":true}`,
						),
					)
					if err != nil ||
						paused.State != runtime.StateQueued ||
						paused.ResumeAcceptedAt == nil {
						t.Fatalf("resumed=%#v err=%v", paused, err)
					}
				}
				cancelled, err := runs.Cancel(ctx, paused.ID)
				if err != nil ||
					cancelled.State != runtime.StateCancelled ||
					cancelled.Error == nil ||
					cancelled.Error.Code != contract.ErrorCancelled ||
					cancelled.SettledSequence == 0 ||
					len(generator.requests) != 1 {
					t.Fatalf(
						"cancelled=%#v err=%v requests=%d",
						cancelled, err, len(generator.requests),
					)
				}
				repeated, err := runs.Cancel(ctx, paused.ID)
				if err != nil ||
					repeated.State != runtime.StateCancelled ||
					repeated.SettledSequence !=
						cancelled.SettledSequence ||
					len(generator.requests) != 1 {
					t.Fatalf(
						"repeated=%#v err=%v requests=%d",
						repeated, err, len(generator.requests),
					)
				}
				if bound {
					result, found, err := sessions.ResultForRun(
						sessionID, paused.ID,
					)
					if err != nil || !found ||
						result.State != session.TurnCancelled ||
						result.Error == nil ||
						result.Error.Code != contract.ErrorCancelled {
						t.Fatalf(
							"Session result=%#v found=%v err=%v",
							result, found, err,
						)
					}
					sessionValue, err := sessions.Get(sessionID)
					if err != nil ||
						sessionValue.State != session.SessionIdle ||
						sessionValue.ActiveTurnID != "" {
						t.Fatalf(
							"Session=%#v err=%v",
							sessionValue, err,
						)
					}
				}
			})
		}
	}
}

func TestCancellationCheckpointAndOrphanEffectFailClosed(
	t *testing.T,
) {
	ctx := context.Background()
	profiles := buildAgentProfiles(t, false)
	generator := &agentModel{}
	registry, err := agent.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		filepath.Join(t.TempDir(), "runtime.db"),
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Profiles: profiles, Model: generator,
				Tools: registry, Store: store,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = runs.Close() })
	submitted, runtimeErr := runs.Submit(ctx, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "cancel queued",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	cancelled, err := runs.Cancel(ctx, submitted.ID)
	if err != nil ||
		cancelled.State != runtime.StateCancelled ||
		len(cancelled.Result) == 0 {
		t.Fatalf("cancelled=%#v err=%v", cancelled, err)
	}
	checkpoint, exists, err := store.LatestCheckpoint(ctx, submitted.ID)
	if err != nil || !exists {
		t.Fatalf(
			"checkpoint=%#v exists=%v err=%v",
			checkpoint, exists, err,
		)
	}
	var state agent.LoopState
	if err := json.Unmarshal(checkpoint.State, &state); err != nil {
		t.Fatal(err)
	}
	if state.TerminalOutcome == nil ||
		state.TerminalOutcome.State != agent.StateCancelled ||
		state.TerminalOutcome.Error == nil ||
		state.TerminalOutcome.Error.Code != contract.ErrorCancelled {
		t.Fatalf("checkpoint state=%#v", state)
	}

	orphanRequest := prepareStoredAgentRequest(
		t, store,
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api",
			Input: "orphan effect",
		},
		nil, nil,
	)
	orphanRecord := runtime.Record{
		ID:      "run_abcdefabcdefabcdefabcdefabcdefab",
		Request: orphanRequest,
		State:   runtime.StateQueued, CancelRequested: true,
	}
	orphanStore := &orphanToolEffectStore{
		Store: store,
		effect: runtime.ToolEffect{
			RunID: orphanRecord.ID, CallID: "call_orphan",
			State: "prepared",
		},
	}
	outcome := (&runtime.AgentExecutor{
		Profiles: profiles, Model: generator,
		Tools: registry, Store: orphanStore,
	}).FinalizeCancellation(ctx, orphanRecord)
	if outcome.State != runtime.StateNeedsReconciliation ||
		outcome.Error == nil ||
		!strings.Contains(
			outcome.Error.Message,
			"tool evidence has no cancellation checkpoint",
		) {
		t.Fatalf("orphan outcome=%#v", outcome)
	}
}

func TestAgentReconcileWithoutSessionAcknowledgesUnknownEffect(t *testing.T) {
	store, err := sqlitestore.Open(
		filepath.Join(t.TempDir(), "runtime.db"),
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	request := prepareStoredAgentRequest(
		t, store,
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
		},
		nil, nil,
	)
	original := &contract.RuntimeError{
		Code: contract.ErrorConflict, Phase: contract.PhaseRun,
		Message: "fixture effect state is started", Retryable: true,
	}
	runID := "run_33333333333333333333333333333333"
	record, err := store.Create(context.Background(), runID, request)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.Start(context.Background(), runID)
	if err != nil {
		t.Fatal(err)
	}
	record, err = store.NeedsReconciliation(
		context.Background(), runID, original,
	)
	if err != nil {
		t.Fatal(err)
	}
	if len(record.Request.PrivateRequest) != 0 {
		t.Fatal("Run Store exposed private Agent execution request")
	}
	outcome := (&runtime.AgentExecutor{Store: store}).Reconcile(
		context.Background(), record,
	)
	if outcome.State != runtime.StateFailed ||
		outcome.Error == nil ||
		outcome.Error.Retryable ||
		outcome.Error.Message ==
			original.Message ||
		!json.Valid(outcome.Result) {
		t.Fatalf("outcome=%#v", outcome)
	}
	var result struct {
		Reconciliation struct {
			Acknowledged bool   `json:"acknowledged"`
			Effect       string `json:"effect_outcome"`
		} `json:"reconciliation"`
	}
	if err := json.Unmarshal(outcome.Result, &result); err != nil {
		t.Fatal(err)
	}
	if !result.Reconciliation.Acknowledged ||
		result.Reconciliation.Effect != "unknown" {
		t.Fatalf("result=%s", outcome.Result)
	}
}

func TestAgentReconcileRecoversSessionBeforeUnknownProjection(t *testing.T) {
	profiles := buildAgentProfiles(t, false)
	root := t.TempDir()
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles, Models: &agentModel{},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_44444444444444444444444444444444"
	turn, runtimeErr := sessions.PrepareAgent(session.RunRequest{
		SessionID: sessionID, RunID: runID,
		ProfileID: "api", Input: "started before process crash",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	events, err := sessions.Events(sessionID, 0)
	if err != nil {
		t.Fatal(err)
	}
	for _, event := range events {
		if event.Type == "agent.needs_reconciliation" {
			t.Fatalf("unexpected Agent reconciliation projection: %#v", events)
		}
	}
	if _, directErr := sessions.Reconcile(
		context.Background(), sessionID,
		session.ReconcileOptions{AcknowledgeUnknown: true},
	); directErr == nil ||
		directErr.Message !=
			"Agent-backed Session must be reconciled through run reconcile" {
		t.Fatalf("direct Session reconcile error=%v", directErr)
	}
	cause := &contract.RuntimeError{
		Code: contract.ErrorConflict, Phase: contract.PhaseRun,
		Message: "tool effect outcome is unknown after process restart",
	}
	store, err := sqlitestore.Open(
		filepath.Join(root, "runtime.db"),
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = store.Close() })
	request := prepareStoredAgentRequest(
		t, store,
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: "api",
			Input: "started before process crash", SessionID: sessionID,
		},
		nil, sessions,
	)
	executor := &runtime.AgentExecutor{Sessions: sessions}
	outcome := executor.Reconcile(context.Background(), runtime.Record{
		ID: runID, State: runtime.StateNeedsReconciliation,
		Request: request,
		Error:   cause,
	})
	if outcome.State != runtime.StateFailed ||
		outcome.Error == nil ||
		!json.Valid(outcome.Result) {
		t.Fatalf("outcome=%#v", outcome)
	}
	result, found, err := sessions.ResultForRun(sessionID, runID)
	if err != nil || !found ||
		result.State != session.TurnFailed ||
		result.TurnID != turn.TurnID {
		t.Fatalf("result=%#v found=%v err=%v", result, found, err)
	}
	execution, err := sessions.Execution(sessionID, turn.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	if execution.State != session.ExecutionSettled ||
		execution.Outcome != session.OutcomeUnknown {
		t.Fatalf("execution=%#v", execution)
	}
	nextTurn, runtimeErr := sessions.PrepareAgent(session.RunRequest{
		SessionID: sessionID,
		RunID:     "run_55555555555555555555555555555555",
		ProfileID: "api",
		Input:     "new Turn after file projection settled",
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	repeated, runtimeErr := sessions.ReconcileAgent(
		context.Background(), sessionID, runID, cause,
	)
	if runtimeErr != nil || !repeated.Resolved ||
		repeated.RunID != runID {
		t.Fatalf("repeated=%#v err=%v", repeated, runtimeErr)
	}
	sessionValue, err := sessions.Get(sessionID)
	if err != nil {
		t.Fatal(err)
	}
	if sessionValue.State != session.SessionActive ||
		sessionValue.ActiveTurnID != nextTurn.TurnID {
		t.Fatalf("new active Turn was overwritten: %#v", sessionValue)
	}
}

func TestAgentRejectsCommandProfileBeforeCreatingRun(t *testing.T) {
	profiles := buildAgentProfiles(t, true)
	registry, err := agent.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		filepath.Join(t.TempDir(), "runtime.db"), sqlitestore.Options{},
	)
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{
		Store: store,
		Executors: map[runtime.Kind]runtime.Executor{
			runtime.KindAgent: &runtime.AgentExecutor{
				Profiles: profiles, Model: &agentModel{}, Tools: registry, Store: store,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runs.Close() })
	if _, runtimeErr := runs.Submit(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "cli", Input: "start",
	}); runtimeErr == nil || runtimeErr.Code != contract.ErrorInvalidRequest {
		t.Fatalf("err=%v", runtimeErr)
	}
	values, err := runs.List(context.Background(), runtime.ListFilter{})
	if err != nil {
		t.Fatal(err)
	}
	if len(values) != 0 {
		t.Fatalf("runs=%#v", values)
	}
}

func buildAgentProfiles(t *testing.T, withCommand bool) *profile.Catalog {
	t.Helper()
	commandValues := map[string]runtimecommand.Profile{}
	if withCommand {
		commandValues["cli"] = runtimecommand.Profile{
			Command: "codex",
		}
	}
	commands, err := runtimecommand.NewCatalog(commandValues)
	if err != nil {
		t.Fatal(err)
	}
	models, err := model.NewCatalog(map[string]model.Profile{
		"api": testAgentModelProfile(),
	})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewCatalog(commands, models)
	if err != nil {
		t.Fatal(err)
	}
	return profiles
}

func testAgentModelProfile() model.Profile {
	return model.Profile{
		Driver:   model.DriverOpenAI,
		Endpoint: "https://example.test/v1/chat/completions",
		Model:    "fixture",
		Headers: map[string]string{
			"Authorization": "${FIXTURE_KEY}",
		},
		Timeout: "1m",
	}
}

func testAgentModelExecutionSnapshot(
	profileID string,
) (model.ExecutionSnapshot, error) {
	if profileID != "api" {
		return model.ExecutionSnapshot{}, fmt.Errorf(
			"unknown profile %q", profileID,
		)
	}
	profile := testAgentModelProfile()
	profileJSON, err := json.Marshal(profile)
	if err != nil {
		return model.ExecutionSnapshot{}, err
	}
	sum := sha256.Sum256(profileJSON)
	return model.ExecutionSnapshot{
		SchemaVersion: model.ExecutionSnapshotSchemaVersion,
		ProfileID:     profileID,
		Profile:       profile,
		ProfileDigest: "sha256:" + hex.EncodeToString(sum[:]),
		DriverIdentity: model.DriverExecutionIdentity{
			Driver:                model.DriverOpenAI,
			Implementation:        "runtime.run-test.agent-model",
			ImplementationVersion: 1,
		},
	}, nil
}

func prepareStoredAgentRequest(
	t *testing.T,
	store runtime.Store,
	request runtime.Request,
	definitions []contract.ToolSpec,
	sessions *session.Service,
) runtime.Request {
	t.Helper()
	registered := make([]agent.RegisteredTool, len(definitions))
	for index := range definitions {
		definition := definitions[index]
		definition.InputSchema = append(
			json.RawMessage(nil), definition.InputSchema...,
		)
		registered[index] = agent.RegisteredTool{
			Definition: definition,
			Handler: func(
				context.Context,
				agent.ToolRequest,
			) (agent.ToolResult, error) {
				return agent.ToolResult{Content: "fixture"}, nil
			},
		}
	}
	registry, err := agent.NewRegistry(registered...)
	if err != nil {
		t.Fatal(err)
	}
	prepared, err := (&runtime.AgentExecutor{
		Profiles: buildAgentProfiles(t, false),
		Model:    &agentModel{},
		Tools:    registry,
		Store:    store,
		Sessions: sessions,
	}).Prepare(context.Background(), request)
	if err != nil {
		t.Fatal(err)
	}
	return prepared
}

func testToolIdempotencyKey(
	runID string,
	call contract.ToolCall,
) string {
	hash := sha256.New()
	hash.Write([]byte(runID))
	hash.Write([]byte{0})
	hash.Write([]byte(call.ID))
	hash.Write([]byte{0})
	hash.Write([]byte(call.Name))
	hash.Write([]byte{0})
	hash.Write(call.Arguments)
	return "sha256:" + hex.EncodeToString(hash.Sum(nil))
}

func setTestModelRequest(
	t *testing.T,
	call *runtime.ModelCall,
	runID string,
	messages []contract.Message,
	tools []contract.ToolSpec,
) {
	t.Helper()
	requestJSON, err := json.Marshal(contract.GenerateRequest{
		ModelProfile: "api",
		Input: contract.ModelRequest{
			Messages: messages,
			Tools:    tools,
			Trace: contract.TraceContext{
				Labels: map[string]string{"run_id": runID},
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(requestJSON)
	call.Request = requestJSON
	call.RequestDigest = "sha256:" + hex.EncodeToString(sum[:])
}

func setTestModelResult(
	t *testing.T,
	call *runtime.ModelCall,
	result contract.ModelResult,
) {
	t.Helper()
	resultJSON, err := json.Marshal(result)
	if err != nil {
		t.Fatal(err)
	}
	sum := sha256.Sum256(resultJSON)
	call.Result = resultJSON
	call.ResultDigest = "sha256:" + hex.EncodeToString(sum[:])
	call.ProviderRequestID = result.Provider.RequestID
}
