package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/session"
)

func checkpointMatchesLoopState(
	checkpoint Checkpoint,
	exists bool,
	state agent.LoopState,
) bool {
	if !exists ||
		checkpoint.RunID != state.RunID ||
		checkpoint.Sequence != state.NextEventSequence {
		return false
	}
	stateJSON, err := json.Marshal(state)
	return err == nil && bytes.Equal(checkpoint.State, stateJSON)
}

func encodeAgentExecutionResult(
	state agent.LoopState,
	outcome agent.Outcome,
	sessionResult *session.RunResult,
) (json.RawMessage, error) {
	return json.Marshal(map[string]any{
		"outcome":        outcome,
		"session_result": sessionResult,
		"state": map[string]any{
			"round":           state.Round,
			"tool_call_count": state.ToolCallCount,
			"total_tokens":    state.TotalTokens,
		},
	})
}

func (executor *AgentExecutor) saveState(
	ctx context.Context,
	state agent.LoopState,
) error {
	checkpointID, err := identity.New("checkpoint")
	if err != nil {
		return err
	}
	stateJSON, err := json.Marshal(state)
	if err != nil {
		return err
	}
	checkpoint := Checkpoint{
		ID: checkpointID, RunID: state.RunID,
		Sequence: state.NextEventSequence, State: stateJSON,
	}
	if err := executor.Store.SaveCheckpoint(ctx, checkpoint); err != nil {
		latest, exists, lookupErr := executor.Store.LatestCheckpoint(
			context.WithoutCancel(ctx), state.RunID,
		)
		if lookupErr == nil &&
			checkpointMatchesLoopState(latest, exists, state) {
			return nil
		}
		if lookupErr != nil {
			return fmt.Errorf(
				"%w; verify durable checkpoint: %v", err, lookupErr,
			)
		}
		return err
	}
	return nil
}
