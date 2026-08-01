package run

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/runtime/agent"
	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/profile"
	"github.com/yy003x/runtime/session"
)

func TestDecodeAgentResumeInputUsesStrictObjectContract(t *testing.T) {
	valid, err := decodeAgentResumeInput(json.RawMessage(
		`{"pause_id":"pause_1","input":{"approved":true,"note":null}}`,
	))
	if err != nil || valid.PauseID != "pause_1" {
		t.Fatalf("valid=%#v error=%v", valid, err)
	}
	for name, value := range map[string]string{
		"string": `{"pause_id":"pause_1","input":"approved"}`,
		"array":  `{"pause_id":"pause_1","input":[1,2]}`,
		"number": `{"pause_id":"pause_1","input":7}`,
		"null":   `{"pause_id":"pause_1","input":null}`,
	} {
		t.Run("valid_"+name, func(t *testing.T) {
			if _, err := decodeAgentResumeInput(
				json.RawMessage(value),
			); err != nil {
				t.Fatalf("resume input %s was rejected: %v", value, err)
			}
		})
	}
	for name, value := range map[string]string{
		"unknown": `{
			"pause_id":"pause_1",
			"input":{"approved":true},
			"future":true
		}`,
		"duplicate": `{
			"pause_id":"pause_1",
			"pause_id":"pause_2",
			"input":{"approved":true}
		}`,
		"nested_duplicate": `{
			"pause_id":"pause_1",
			"input":{"approved":true,"approved":false}
		}`,
		"trailing": `{
			"pause_id":"pause_1",
			"input":{"approved":true}
		} {}`,
		"null": `null`,
		"null_pause_id": `{
			"pause_id":null,
			"input":{"approved":true}
		}`,
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := decodeAgentResumeInput(
				json.RawMessage(value),
			); err == nil {
				t.Fatalf("resume input %s was accepted", value)
			}
		})
	}
}

func TestDecodeAgentResumeInputMatchesPublicOneMiBEnvelopeLimit(t *testing.T) {
	const prefix = `{"pause_id":"pause_1","input":"`
	const suffix = `"}`
	value := json.RawMessage(
		prefix +
			strings.Repeat("x", MaxResumeInputBytes-len(prefix)-len(suffix)) +
			suffix,
	)
	if len(value) != MaxResumeInputBytes {
		t.Fatalf("fixture bytes=%d", len(value))
	}
	if _, err := decodeAgentResumeInput(value); err != nil {
		t.Fatalf("one MiB resume envelope was rejected: %v", err)
	}
	value = append(value, ' ')
	if _, err := decodeAgentResumeInput(value); err == nil {
		t.Fatal("oversized resume envelope was accepted")
	}
}

func TestRecordJSONRedactsLatestResumeInput(t *testing.T) {
	record := Record{
		ID:    "run_11111111111111111111111111111111",
		State: StatePaused,
		Request: Request{
			Kind: KindAgent, ProfileID: "api", Input: "start",
			Resume: json.RawMessage(
				`{"pause_id":"pause_1","input":{"secret":"private"}}`,
			),
		},
	}
	encoded, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(encoded), "private") ||
		strings.Contains(string(encoded), "pause_1") ||
		strings.Contains(string(encoded), `"resume"`) {
		t.Fatalf("public Run JSON exposed latest resume input: %s", encoded)
	}
	if len(record.Request.Resume) == 0 {
		t.Fatal("marshalling mutated the durable in-memory resume")
	}
}

func TestValidateAgentSessionPrefixProjectionRequiresSafeBoundary(
	t *testing.T,
) {
	call := contract.ToolCall{
		ID: "call_prefix_boundary", Name: "echo",
		Arguments: json.RawMessage(`{}`),
	}
	state := agent.LoopState{
		SchemaVersion:    agent.LoopStateSchemaVersion,
		BaseMessageCount: 1,
		Messages: []contract.Message{
			{Role: contract.RoleUser, Content: "start"},
			{
				Role:      contract.RoleAssistant,
				ToolCalls: []contract.ToolCall{call},
			},
			{
				Role: contract.RoleTool, ToolCallID: call.ID,
				Content: "done",
			},
		},
	}
	for name, messages := range map[string][]contract.Message{
		"earlier_safe_boundary": state.Messages[:1],
		"closed_tool_round":     state.Messages,
	} {
		t.Run(name, func(t *testing.T) {
			if runtimeErr := validateAgentSessionPrefixProjection(
				session.AgentTurn{
					BaseMessageCount: 1,
					Messages:         cloneMessagesForTest(messages),
				},
				state,
			); runtimeErr != nil {
				t.Fatal(runtimeErr)
			}
		})
	}
	if runtimeErr := validateAgentSessionPrefixProjection(
		session.AgentTurn{
			BaseMessageCount: 1,
			Messages:         cloneMessagesForTest(state.Messages[:2]),
		},
		state,
	); runtimeErr == nil {
		t.Fatal("partial provider tool-call round was accepted")
	}
}

func cloneMessagesForTest(
	values []contract.Message,
) []contract.Message {
	result := make([]contract.Message, len(values))
	for index := range values {
		result[index] = values[index]
		result[index].ToolCalls = append(
			[]contract.ToolCall(nil), values[index].ToolCalls...,
		)
		for callIndex := range result[index].ToolCalls {
			result[index].ToolCalls[callIndex].Arguments = append(
				json.RawMessage(nil),
				result[index].ToolCalls[callIndex].Arguments...,
			)
		}
	}
	return result
}

func TestValidateExistingAgentSessionResultRequiresExactTerminalEvidence(
	t *testing.T,
) {
	commands, err := runtimecommand.NewCatalog(
		map[string]runtimecommand.Profile{},
	)
	if err != nil {
		t.Fatal(err)
	}
	models, err := model.NewCatalog(map[string]model.Profile{
		"api": {
			Driver:   model.DriverOpenAICompatible,
			Endpoint: "https://example.test/v1/chat/completions",
			Model:    "fixture",
			Auth: model.Auth{
				Header: "Authorization", Scheme: "Bearer",
				FromEnv: "FIXTURE_KEY",
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
	root := t.TempDir()
	sessionStore, err := session.NewStore(
		filepath.Join(root, "sessions"), filepath.Join(root, "state"),
	)
	if err != nil {
		t.Fatal(err)
	}
	sessions, err := session.NewService(session.ServiceOptions{
		Store: sessionStore, Profiles: profiles,
		Models: &terminalEvidenceModel{},
	})
	if err != nil {
		t.Fatal(err)
	}
	sessionID, err := session.NewID()
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_99999999999999999999999999999999"
	request := session.RunRequest{
		SessionID: sessionID, RunID: runID,
		ProfileID: "api", Input: "start",
	}
	turn, runtimeErr := sessions.PrepareAgent(request)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	finalMessage := contract.Message{
		Role: contract.RoleAssistant, Content: "durable final",
	}
	messages := append(
		append([]contract.Message(nil), turn.Messages...),
		finalMessage,
	)
	if _, runtimeErr := sessions.SettleAgent(
		turn, messages,
		agent.Outcome{
			State: agent.StateCompleted, StopReason: "stop",
			Message: &finalMessage,
		},
	); runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	recovered, runtimeErr := sessions.RecoverAgent(request)
	if runtimeErr != nil || recovered.ExistingResult == nil {
		t.Fatalf("turn=%#v error=%v", recovered, runtimeErr)
	}
	record := Record{
		ID: runID,
		Request: Request{
			Kind: KindAgent, ProfileID: "api",
			Input: "start", SessionID: sessionID,
		},
	}
	state := agent.LoopState{
		SchemaVersion: agent.LoopStateSchemaVersion,
		RunID:         runID, ModelProfile: "api", Messages: messages,
		BaseMessageCount: turn.BaseMessageCount,
		TerminalOutcome: &agent.Outcome{
			State: agent.StateCompleted, StopReason: "stop",
			Message: &finalMessage,
		},
	}
	executor := &AgentExecutor{Sessions: sessions}
	if runtimeErr := executor.validateExistingAgentSessionResult(
		record, recovered, state,
	); runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	for name, mutate := range map[string]func(
		*session.AgentTurn,
		*agent.LoopState,
	){
		"forged_session_state": func(
			turn *session.AgentTurn,
			_ *agent.LoopState,
		) {
			turn.ExistingResult.State = session.TurnFailed
		},
		"forged_session_message": func(
			turn *session.AgentTurn,
			_ *agent.LoopState,
		) {
			message := finalMessage
			message.Content = "forged"
			turn.ExistingResult.Message = &message
		},
		"forged_complete_history": func(
			turn *session.AgentTurn,
			_ *agent.LoopState,
		) {
			turn.Messages = append(
				append([]contract.Message(nil), turn.Messages...),
				contract.Message{
					Role:       contract.RoleTool,
					ToolCallID: "call_forged",
					Content:    "forged",
				},
			)
		},
		"forged_checkpoint_outcome": func(
			_ *session.AgentTurn,
			state *agent.LoopState,
		) {
			state.TerminalOutcome = &agent.Outcome{
				State: agent.StateFailed,
				Error: &contract.RuntimeError{
					Code:    contract.ErrorInternal,
					Phase:   contract.PhaseRun,
					Message: "forged",
				},
			}
		},
	} {
		t.Run(name, func(t *testing.T) {
			currentTurn := recovered
			currentResult := *recovered.ExistingResult
			if recovered.ExistingResult.Message != nil {
				message := *recovered.ExistingResult.Message
				currentResult.Message = &message
			}
			currentTurn.ExistingResult = &currentResult
			currentState := state
			currentOutcome := *state.TerminalOutcome
			currentState.TerminalOutcome = &currentOutcome
			mutate(&currentTurn, &currentState)
			if runtimeErr := executor.validateExistingAgentSessionResult(
				record, currentTurn, currentState,
			); runtimeErr == nil {
				t.Fatal("forged terminal evidence was accepted")
			}
		})
	}

	execution, err := sessions.Execution(sessionID, turn.ExecutionID)
	if err != nil {
		t.Fatal(err)
	}
	execution.Outcome = session.OutcomeFailed
	executionJSON, err := json.Marshal(execution)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(
			root, "sessions", sessionID, "executions",
			turn.ExecutionID+".json",
		),
		append(executionJSON, '\n'), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if runtimeErr := executor.validateExistingAgentSessionResult(
		record, recovered, state,
	); runtimeErr == nil {
		t.Fatal("forged execution outcome was accepted")
	}
}

type terminalEvidenceModel struct{}

func (*terminalEvidenceModel) Generate(
	_ context.Context,
	_ contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return contract.ModelResult{}, nil
}

func (*terminalEvidenceModel) GenerateStream(
	_ context.Context,
	_ contract.GenerateRequest,
	_ contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	return contract.ModelResult{}, nil
}
