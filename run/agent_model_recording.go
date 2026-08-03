package run

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"reflect"
	"sync"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/identity"
	"github.com/yy003x/runtime/model"
)

type recordingModel struct {
	runID        string
	model        model.Generator
	store        Store
	beforeEffect agent.PreEffectGate

	mu       sync.Mutex
	sequence int
}

func (recorder *recordingModel) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return recorder.GenerateStream(ctx, request, nil)
}

func (recorder *recordingModel) GenerateStream(
	ctx context.Context,
	request contract.GenerateRequest,
	sink contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	if recorder.beforeEffect != nil {
		if runtimeErr := recorder.beforeEffect(ctx); runtimeErr != nil {
			return contract.ModelResult{}, runtimeErr
		}
	}
	recorder.mu.Lock()
	recorder.sequence++
	sequence := recorder.sequence
	recorder.mu.Unlock()
	callID, err := identity.New("model_call")
	if err != nil {
		return contract.ModelResult{}, runError(contract.ErrorInternal, err.Error())
	}
	requestJSON, err := json.Marshal(request)
	if err != nil {
		return contract.ModelResult{}, runError(contract.ErrorInternal, err.Error())
	}
	sum := sha256.Sum256(requestJSON)
	call := ModelCall{
		ID: callID, RunID: recorder.runID, Sequence: sequence,
		RequestDigest: "sha256:" + hex.EncodeToString(sum[:]),
		Request:       requestJSON,
	}
	if err := recorder.store.StartModelCall(ctx, call); err != nil {
		durableContext := context.WithoutCancel(ctx)
		committed, lookupErr := recorder.modelCallMatches(
			durableContext, call, "running",
		)
		if !committed {
			retryErr := recorder.store.StartModelCall(
				durableContext, call,
			)
			if retryErr == nil {
				committed = true
			} else {
				committed, lookupErr = recorder.modelCallMatches(
					durableContext, call, "running",
				)
			}
			if !committed {
				message := "record model call start outcome is unknown: " +
					err.Error()
				if retryErr != nil {
					message += "; retry: " + retryErr.Error()
				}
				if lookupErr != nil {
					message += "; verify: " + lookupErr.Error()
				}
				return contract.ModelResult{}, runError(
					contract.ErrorConflict, message,
				)
			}
		}
	}
	result, runtimeErr := recorder.model.GenerateStream(ctx, request, sink)
	call.State = "completed"
	if runtimeErr != nil {
		call.State = "failed"
		if runtimeErr.Code == contract.ErrorCancelled ||
			runtimeErr.Code == contract.ErrorTimeout {
			call.State = "cancelled"
		}
		call.Error = runtimeErr
	} else {
		call.ProviderRequestID = result.Provider.RequestID
		resultJSON, err := json.Marshal(result)
		if err != nil {
			return contract.ModelResult{}, runError(
				contract.ErrorConflict,
				"model call is running but its durable result cannot be encoded: "+
					err.Error(),
			)
		}
		sum := sha256.Sum256(resultJSON)
		call.Result = resultJSON
		call.ResultDigest = "sha256:" + hex.EncodeToString(sum[:])
	}
	if err := recorder.store.FinishModelCall(
		context.WithoutCancel(ctx), call,
	); err != nil {
		durableContext := context.WithoutCancel(ctx)
		committed, lookupErr := recorder.modelCallMatches(
			durableContext, call, call.State,
		)
		if !committed {
			retryErr := recorder.store.FinishModelCall(
				durableContext, call,
			)
			if retryErr == nil {
				committed = true
			} else {
				committed, lookupErr = recorder.modelCallMatches(
					durableContext, call, call.State,
				)
			}
			if !committed {
				message := "record model call terminal outcome is unknown: " +
					err.Error()
				if retryErr != nil {
					message += "; retry: " + retryErr.Error()
				}
				if lookupErr != nil {
					message += "; verify: " + lookupErr.Error()
				}
				return contract.ModelResult{}, runError(
					contract.ErrorConflict, message,
				)
			}
		}
	}
	return result, runtimeErr
}

func (recorder *recordingModel) modelCallMatches(
	ctx context.Context,
	expected ModelCall,
	expectedState string,
) (bool, error) {
	current, exists, err := recorder.store.LatestModelCall(
		ctx, recorder.runID,
	)
	if err != nil || !exists {
		return false, err
	}
	if current.ID != expected.ID ||
		current.RunID != expected.RunID ||
		current.Sequence != expected.Sequence ||
		current.RequestDigest != expected.RequestDigest ||
		!bytes.Equal(current.Request, expected.Request) ||
		current.ProviderRequestID != expected.ProviderRequestID ||
		current.State != expectedState {
		return false, nil
	}
	switch expectedState {
	case "running":
		return len(current.Result) == 0 &&
			current.ResultDigest == "" &&
			current.Error == nil, nil
	case "completed":
		return bytes.Equal(current.Result, expected.Result) &&
			current.ResultDigest == expected.ResultDigest &&
			current.Error == nil, nil
	case "failed", "cancelled":
		return len(current.Result) == 0 &&
			current.ResultDigest == "" &&
			reflect.DeepEqual(current.Error, expected.Error), nil
	default:
		return false, nil
	}
}
