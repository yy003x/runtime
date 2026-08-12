package run

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/contract"
)

type durableEffects struct {
	store ToolEffectStore
}

func (effects *durableEffects) Lookup(
	ctx context.Context,
	runID, callID string,
) (agent.EffectRecord, bool, error) {
	effect, exists, err := effects.store.ToolEffect(ctx, runID, callID)
	if err != nil || !exists {
		return agent.EffectRecord{}, exists, err
	}
	var request agent.ToolRequest
	if err := strictjson.Decode(
		bytes.NewReader(effect.Request), 4<<20, &request,
	); err != nil {
		return agent.EffectRecord{}, false, fmt.Errorf(
			"decode durable tool request: %w", err,
		)
	}
	if request.RunID != effect.RunID ||
		request.CallID != effect.CallID ||
		request.IdempotencyKey != effect.IdempotencyKey ||
		request.Name != effect.Name {
		return agent.EffectRecord{}, false, fmt.Errorf(
			"durable tool request identity does not match indexed effect",
		)
	}
	record := agent.EffectRecord{
		State: effect.State, Request: request, Error: effect.Error,
	}
	if len(effect.Result) > 0 {
		var result agent.ToolResult
		if err := strictjson.Decode(
			bytes.NewReader(effect.Result), 4<<20, &result,
		); err != nil {
			return agent.EffectRecord{}, false, fmt.Errorf(
				"decode durable tool result: %w", err,
			)
		}
		record.Result = &result
	}
	return record, true, nil
}

func (effects *durableEffects) Prepared(
	ctx context.Context,
	request *agent.ToolRequest,
	state *agent.LoopState,
) (string, error) {
	checkpointID, err := identity.New("checkpoint")
	if err != nil {
		return "", err
	}
	request.CheckpointID = checkpointID
	state.PendingEffectCheckpointID = checkpointID
	state.PendingCheckpointID = checkpointID
	stateJSON, err := json.Marshal(*state)
	if err != nil {
		return "", err
	}
	checkpoint := Checkpoint{
		ID: checkpointID, RunID: request.RunID,
		Sequence: state.NextEventSequence, State: stateJSON,
	}
	requestJSON, err := json.Marshal(*request)
	if err != nil {
		return "", err
	}
	if err := effects.store.PrepareToolEffect(ctx, ToolEffect{
		RunID: request.RunID, CallID: request.CallID,
		IdempotencyKey: request.IdempotencyKey, Name: request.Name,
		State: "prepared", Request: requestJSON,
	}, checkpoint); err != nil {
		return "", err
	}
	return checkpointID, nil
}

func (effects *durableEffects) Started(
	ctx context.Context,
	request agent.ToolRequest,
) error {
	return effects.store.StartToolEffect(ctx, request.RunID, request.CallID)
}

func (effects *durableEffects) Completed(
	ctx context.Context,
	request agent.ToolRequest,
	result agent.ToolResult,
) error {
	resultJSON, err := json.Marshal(result)
	if err != nil {
		return err
	}
	return effects.store.CompleteToolEffect(ctx, ToolEffect{
		RunID: request.RunID, CallID: request.CallID,
		IdempotencyKey: request.IdempotencyKey, Name: request.Name,
		State: "completed", Result: resultJSON,
	})
}

func (effects *durableEffects) Failed(
	ctx context.Context,
	request agent.ToolRequest,
	runtimeErr *contract.RuntimeError,
) error {
	return effects.store.FailToolEffect(ctx, ToolEffect{
		RunID: request.RunID, CallID: request.CallID,
		IdempotencyKey: request.IdempotencyKey, Name: request.Name,
		State: "failed", Error: runtimeErr,
	})
}
