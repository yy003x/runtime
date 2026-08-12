package toolbuiltin

import (
	"context"
	"encoding/json"
	"errors"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
)

type readOnlyErrorLoopModel struct {
	results            []contract.ModelResult
	sawErrorToolResult bool
}

func (model *readOnlyErrorLoopModel) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return model.GenerateStream(ctx, request, nil)
}

func (model *readOnlyErrorLoopModel) GenerateStream(
	_ context.Context,
	request contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	if len(model.results) == 1 && len(request.Input.Messages) > 0 {
		message := request.Input.Messages[len(request.Input.Messages)-1]
		model.sawErrorToolResult =
			message.Role == contract.RoleTool &&
				message.ToolCallID == "call_missing" &&
				message.IsError &&
				message.Content != ""
	}
	if len(model.results) == 0 {
		return contract.ModelResult{}, &contract.RuntimeError{
			Code:    contract.ErrorInternal,
			Phase:   contract.PhaseProvider,
			Message: "no scripted model result",
		}
	}
	result := model.results[0]
	model.results = model.results[1:]
	if sink != nil {
		if err := sink(contract.Event{
			Type: contract.EventModelStarted,
		}); err != nil {
			return contract.ModelResult{}, &contract.RuntimeError{
				Code:    contract.ErrorCancelled,
				Phase:   contract.PhaseConsumer,
				Message: "model event consumer stopped",
			}
		}
		if err := sink(contract.Event{
			Type:  contract.EventModelCompleted,
			Model: &contract.ModelEvent{Result: &result},
		}); err != nil {
			return contract.ModelResult{}, &contract.RuntimeError{
				Code:    contract.ErrorCancelled,
				Phase:   contract.PhaseConsumer,
				Message: "model event consumer stopped",
			}
		}
	}
	return result, nil
}

type readOnlyErrorEffects struct {
	completed []agent.ToolResult
	failed    int
}

func (*readOnlyErrorEffects) Lookup(
	context.Context,
	string,
	string,
) (agent.EffectRecord, bool, error) {
	return agent.EffectRecord{}, false, nil
}

func (*readOnlyErrorEffects) Prepared(
	_ context.Context,
	request *agent.ToolRequest,
	state *agent.LoopState,
) (string, error) {
	request.CheckpointID = "checkpoint_read_only_error"
	state.PendingEffectCheckpointID = request.CheckpointID
	state.PendingCheckpointID = request.CheckpointID
	return request.CheckpointID, nil
}

func (*readOnlyErrorEffects) Started(
	context.Context,
	agent.ToolRequest,
) error {
	return nil
}

func (effects *readOnlyErrorEffects) Completed(
	_ context.Context,
	_ agent.ToolRequest,
	result agent.ToolResult,
) error {
	effects.completed = append(effects.completed, result)
	return nil
}

func (effects *readOnlyErrorEffects) Failed(
	context.Context,
	agent.ToolRequest,
	*contract.RuntimeError,
) error {
	effects.failed++
	return nil
}

func TestKernelContinuesAfterReadOnlyToolError(t *testing.T) {
	root := t.TempDir()
	registry, err := Build(Options{
		Names: []string{"read_file"},
		Roots: []string{root},
		CWD:   root,
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &readOnlyErrorLoopModel{results: []contract.ModelResult{
		{
			Message: contract.Message{
				Role: contract.RoleAssistant,
				ToolCalls: []contract.ToolCall{{
					ID:        "call_missing",
					Name:      "read_file",
					Arguments: json.RawMessage(`{"path":"missing.txt"}`),
				}},
			},
			FinishReason: contract.FinishToolCall,
		},
		{
			Message: contract.Message{
				Role:    contract.RoleAssistant,
				Content: "recovered",
			},
			FinishReason: contract.FinishStop,
		},
	}}
	effects := &readOnlyErrorEffects{}
	var events []contract.Event
	state, outcome, runtimeErr := (&agent.Kernel{
		Model: model, Tools: registry, Effects: effects,
	}).Run(
		context.Background(),
		agent.LoopState{
			SchemaVersion: agent.LoopStateSchemaVersion,
			RunID:         "run_11111111111111111111111111111111",
			ModelProfile:  "api",
			Messages: []contract.Message{{
				Role: contract.RoleUser, Content: "read missing.txt",
			}},
			BaseMessageCount: 1,
		},
		func(event contract.Event) error {
			events = append(events, event)
			return nil
		},
	)
	if runtimeErr != nil ||
		outcome.State != agent.StateCompleted ||
		outcome.Message == nil ||
		outcome.Message.Content != "recovered" {
		t.Fatalf(
			"state=%#v outcome=%#v error=%v",
			state,
			outcome,
			runtimeErr,
		)
	}
	if !model.sawErrorToolResult {
		t.Fatal("next model round did not receive the error tool result")
	}
	if len(effects.completed) != 1 ||
		!effects.completed[0].IsError ||
		effects.completed[0].Content == "" ||
		effects.failed != 0 {
		t.Fatalf("effects=%#v", effects)
	}
	if len(state.Messages) != 4 ||
		state.Messages[2].Role != contract.RoleTool ||
		state.Messages[2].ToolCallID != "call_missing" ||
		!state.Messages[2].IsError ||
		state.Messages[2].Content != effects.completed[0].Content {
		t.Fatalf("messages=%#v", state.Messages)
	}
	var completed bool
	for _, event := range events {
		if event.Type == contract.EventToolFailed ||
			event.Agent != nil &&
				event.Agent.State == string(agent.StateNeedsReconciliation) {
			t.Fatalf("unexpected reconciliation event: %#v", event)
		}
		if event.Type == contract.EventToolCompleted &&
			event.Tool != nil &&
			event.Tool.IsError &&
			event.Tool.Content == effects.completed[0].Content {
			completed = true
		}
	}
	if !completed {
		t.Fatalf("durable error tool event missing: %#v", events)
	}
}

func TestKernelTreatsWriteDirectorySyncFailureAsUnknown(t *testing.T) {
	root := t.TempDir()
	resolver := newSafeTestResolver(t, root)
	injected := errors.New("injected directory fsync failure")
	resolver.testHooks = &resolverTestHooks{
		syncWriteDirectory: func(int) error {
			return injected
		},
	}
	registry, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name:        "write_file",
			Description: "write test",
			InputSchema: json.RawMessage(
				`{"type":"object","properties":{"path":{"type":"string"},"content":{"type":"string"}},"required":["path","content"],"additionalProperties":false}`,
			),
		},
		Handler: resolver.writeFile,
	})
	if err != nil {
		t.Fatal(err)
	}
	model := &readOnlyErrorLoopModel{results: []contract.ModelResult{{
		Message: contract.Message{
			Role: contract.RoleAssistant,
			ToolCalls: []contract.ToolCall{{
				ID:   "call_write_sync",
				Name: "write_file",
				Arguments: json.RawMessage(
					`{"path":"value.txt","content":"published"}`,
				),
			}},
		},
		FinishReason: contract.FinishToolCall,
	}}}
	effects := &readOnlyErrorEffects{}
	state, outcome, runtimeErr := (&agent.Kernel{
		Model: model, Tools: registry, Effects: effects,
	}).Run(
		context.Background(),
		agent.LoopState{
			SchemaVersion: agent.LoopStateSchemaVersion,
			RunID:         "run_22222222222222222222222222222222",
			ModelProfile:  "api",
			Messages: []contract.Message{{
				Role: contract.RoleUser, Content: "write value.txt",
			}},
			BaseMessageCount: 1,
		},
		nil,
	)
	if runtimeErr == nil ||
		!strings.Contains(runtimeErr.Message, injected.Error()) ||
		outcome.State != agent.StateNeedsReconciliation ||
		outcome.StopReason != "tool_effect_unknown" ||
		len(effects.completed) != 0 ||
		effects.failed != 0 {
		t.Fatalf(
			"state=%#v outcome=%#v error=%v effects=%#v",
			state,
			outcome,
			runtimeErr,
			effects,
		)
	}
	data, err := os.ReadFile(filepath.Join(root, "value.txt"))
	if err != nil || string(data) != "published" {
		t.Fatalf("published data=%q error=%v", data, err)
	}
}
