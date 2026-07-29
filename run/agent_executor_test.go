package run_test

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"

	"github.com/yy003x/runtime/agent"
	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/profile"
	runtime "github.com/yy003x/runtime/run"
	"github.com/yy003x/runtime/session"
	sqlitestore "github.com/yy003x/runtime/store/sqlite"
)

type agentModel struct {
	mu      sync.Mutex
	results []contract.ModelResult
}

func (value *agentModel) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return value.GenerateStream(ctx, request, nil)
}

func (value *agentModel) GenerateStream(
	_ context.Context,
	_ contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	value.mu.Lock()
	defer value.mu.Unlock()
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
	return result, nil
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
				Profiles: profiles, Model: generator, Tools: registry, Store: store,
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runs.Close() })
	paused, runtimeErr := runs.RunNow(context.Background(), runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "start",
	}, nil)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	if paused.State != runtime.StatePaused || len(paused.Pause) == 0 {
		t.Fatalf("paused=%#v", paused)
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
		"api": {
			Driver:   model.DriverOpenAICompatible,
			Endpoint: "https://example.test/v1/chat/completions",
			Model:    "fixture",
			Auth: model.Auth{
				Header: "Authorization", Scheme: "Bearer", FromEnv: "FIXTURE_KEY",
			},
			Timeout: "1m",
		},
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
