package run

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"reflect"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/strictjson"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/profile"
	"github.com/yy003x/runtime/session"
)

type agentExecutionSnapshot struct {
	ExecutionContractVersion int `json:"execution_contract_version"`

	ModelExecutionSnapshot model.ExecutionSnapshot     `json:"model_execution_snapshot"`
	ToolExecutionSnapshot  agent.ToolExecutionSnapshot `json:"tool_execution_snapshot"`
	ToolExecutionDigest    string                      `json:"tool_execution_digest"`

	SessionRequestDigest string `json:"session_request_digest,omitempty"`
	SessionConfigDigest  string `json:"session_config_digest,omitempty"`
	ConfigDigest         string `json:"config_digest"`
	RequestDigest        string `json:"request_digest"`
}

type agentExecutionModel interface {
	model.Generator
	model.ExecutionSnapshotter
}

func (executor *AgentExecutor) validateAgentRequestShape(
	request Request,
	requireCurrent bool,
) error {
	if executor == nil || executor.Store == nil {
		return fmt.Errorf("agent executor is not configured")
	}
	if requireCurrent {
		if executor.Profiles == nil || executor.Model == nil ||
			executor.Tools == nil {
			return fmt.Errorf("agent executor is not configured")
		}
		entry, exists := executor.Profiles.Resolve(request.ProfileID)
		if !exists {
			return fmt.Errorf("unknown profile %q", request.ProfileID)
		}
		if entry.Kind != profile.KindModel {
			return fmt.Errorf(
				"agent requires an API model profile; %q is a command profile",
				request.ProfileID,
			)
		}
		if _, ok := executor.Model.(agentExecutionModel); !ok {
			return fmt.Errorf(
				"agent model must provide generation and execution snapshots on the same object",
			)
		}
		if _, ok := executor.Tools.(agent.ToolExecutionSnapshotter); !ok {
			return fmt.Errorf(
				"agent tools must provide an execution snapshot",
			)
		}
	}
	if request.Model != "" || request.Effort != "" ||
		request.ModelOptions.MaxOutputTokens != nil ||
		request.ModelOptions.Temperature != nil ||
		request.BasePromptDigest != "" {
		return fmt.Errorf(
			"model, effort, model_options, and base_prompt_digest are invalid for agent runs",
		)
	}
	if request.CWD != "" {
		return fmt.Errorf("cwd is invalid for agent runs")
	}
	if request.SessionID != "" && executor.Sessions == nil {
		return fmt.Errorf("agent Session service is unavailable")
	}
	if err := request.AgentBudget.Validate(); err != nil {
		return fmt.Errorf("agent_budget: %w", err)
	}
	return nil
}

// Prepare freezes the complete non-secret Agent execution contract before the
// Run is created. An existing private request is accepted only for Retry and
// remains byte-for-byte unchanged.
func (executor *AgentExecutor) Prepare(
	ctx context.Context,
	request Request,
) (Request, error) {
	if request.Kind != KindAgent {
		return Request{}, fmt.Errorf("agent executor requires run kind %q", KindAgent)
	}
	request.AgentBudget = request.AgentBudget.Effective()
	if err := request.AgentBudget.Validate(); err != nil {
		return Request{}, fmt.Errorf("agent_budget: %w", err)
	}
	if len(request.PrivateRequest) != 0 {
		if request.RetryOf == "" {
			return Request{}, fmt.Errorf(
				"private Agent execution request is retry-owned",
			)
		}
		snapshot, _, err := decodeAgentExecutionSnapshot(request)
		if err != nil {
			return Request{}, &contract.RuntimeError{
				Code: contract.ErrorConflict, Phase: contract.PhaseRun,
				Message: "validate retry Agent execution snapshot: " + err.Error(),
			}
		}
		if runtimeErr := executor.currentAgentExecutionGate(
			ctx, request, snapshot,
		); runtimeErr != nil {
			return Request{}, runtimeErr
		}
		return request, nil
	}
	if request.RetryOf != "" {
		return Request{}, fmt.Errorf(
			"Agent retry requires its original private execution request",
		)
	}
	if request.RequestDigest != "" || request.ConfigDigest != "" ||
		request.BasePromptDigest != "" {
		return Request{}, fmt.Errorf(
			"Agent execution digests are Runtime-owned",
		)
	}
	if err := executor.validateAgentRequestShape(request, true); err != nil {
		return Request{}, err
	}

	snapshot, privateRequest, err := executor.freezeAgentExecutionSnapshot(
		request,
	)
	if err != nil {
		return Request{}, err
	}
	request.PrivateRequest = privateRequest
	request.ConfigDigest = snapshot.ConfigDigest
	request.RequestDigest = snapshot.RequestDigest
	if runtimeErr := executor.currentAgentExecutionGate(
		ctx, request, snapshot,
	); runtimeErr != nil {
		return Request{}, runtimeErr
	}
	return request, nil
}

func (executor *AgentExecutor) freezeAgentExecutionSnapshot(
	request Request,
) (agentExecutionSnapshot, json.RawMessage, error) {
	entry, exists := executor.Profiles.Resolve(request.ProfileID)
	if !exists {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"unknown profile %q", request.ProfileID,
		)
	}
	if entry.Kind != profile.KindModel || entry.Model == nil {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"agent requires an API model profile; %q is a command profile",
			request.ProfileID,
		)
	}
	executionModel, ok := executor.Model.(agentExecutionModel)
	if !ok {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"agent model must provide generation and execution snapshots on the same object",
		)
	}
	modelSnapshot, err := executionModel.ExecutionSnapshot(request.ProfileID)
	if err != nil {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"freeze Agent model execution: %w", err,
		)
	}
	if !reflect.DeepEqual(*entry.Model, modelSnapshot.Profile) {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"Agent Profile catalog and model execution service disagree",
		)
	}
	toolSnapshotter, ok := executor.Tools.(agent.ToolExecutionSnapshotter)
	if !ok {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"agent tools must provide an execution snapshot",
		)
	}
	toolSnapshot := toolSnapshotter.ToolExecutionSnapshot()
	if _, err := agent.NewToolSnapshotValidator(toolSnapshot); err != nil {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"freeze Agent tool execution: %w", err,
		)
	}
	toolDigest, err := toolSnapshot.Digest()
	if err != nil {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"digest Agent tool execution: %w", err,
		)
	}

	sessionRequestDigest, sessionConfigDigest, err :=
		executor.currentAgentSessionDigests(request)
	if err != nil {
		return agentExecutionSnapshot{}, nil, err
	}
	snapshot := agentExecutionSnapshot{
		ExecutionContractVersion: agent.ExecutionContractVersion,
		ModelExecutionSnapshot:   modelSnapshot,
		ToolExecutionSnapshot:    toolSnapshot,
		ToolExecutionDigest:      toolDigest,
		SessionRequestDigest:     sessionRequestDigest,
		SessionConfigDigest:      sessionConfigDigest,
	}
	snapshot.ConfigDigest = computeAgentConfigDigest(snapshot)
	snapshot.RequestDigest = computeAgentRequestDigest(request, snapshot)
	canonical, err := canonicalAgentExecutionSnapshot(request, snapshot)
	if err != nil {
		return agentExecutionSnapshot{}, nil, err
	}
	if len(canonical) > MaxPrivateRequestBytes {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"private Agent execution request exceeds %d bytes",
			MaxPrivateRequestBytes,
		)
	}
	var frozen agentExecutionSnapshot
	if err := json.Unmarshal(canonical, &frozen); err != nil {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"decode canonical Agent execution snapshot: %w", err,
		)
	}
	return frozen, json.RawMessage(canonical), nil
}

func decodeAgentExecutionSnapshot(
	request Request,
) (agentExecutionSnapshot, *agent.ToolSnapshotValidator, error) {
	if len(request.PrivateRequest) == 0 {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"private Agent execution request is required",
		)
	}
	if len(request.PrivateRequest) > MaxPrivateRequestBytes {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"private Agent execution request exceeds %d bytes",
			MaxPrivateRequestBytes,
		)
	}
	if !validSHA256Digest(request.RequestDigest) ||
		!validSHA256Digest(request.ConfigDigest) {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"durable Agent request requires public request/config digests",
		)
	}
	var snapshot agentExecutionSnapshot
	if err := strictjson.DecodeObjectWithNullPolicy(
		bytes.NewReader(request.PrivateRequest),
		MaxPrivateRequestBytes,
		&snapshot,
		agentExecutionSnapshotAllowsNull,
	); err != nil {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"decode private Agent execution request: %w", err,
		)
	}
	canonical, err := canonicalAgentExecutionSnapshot(request, snapshot)
	if err != nil {
		return agentExecutionSnapshot{}, nil, err
	}
	if !bytes.Equal(canonical, request.PrivateRequest) {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"private Agent execution request is not canonical",
		)
	}
	validator, err := agent.NewToolSnapshotValidator(
		snapshot.ToolExecutionSnapshot,
	)
	if err != nil {
		return agentExecutionSnapshot{}, nil, fmt.Errorf(
			"validate private Agent tool execution snapshot: %w", err,
		)
	}
	return snapshot, validator, nil
}

func agentExecutionSnapshotAllowsNull(path []string) bool {
	if len(path) < 2 || path[0] != "tool_execution_snapshot" {
		return false
	}
	if path[1] == "configuration" {
		return len(path) > 2
	}
	if path[1] != "definitions" {
		return false
	}
	for index := 2; index < len(path); index++ {
		if path[index] == "input_schema" {
			return index < len(path)-1
		}
	}
	return false
}

func canonicalAgentExecutionSnapshot(
	request Request,
	snapshot agentExecutionSnapshot,
) ([]byte, error) {
	if snapshot.ExecutionContractVersion != agent.ExecutionContractVersion {
		return nil, fmt.Errorf(
			"unsupported Agent execution_contract_version %d",
			snapshot.ExecutionContractVersion,
		)
	}
	modelJSON, err := snapshot.ModelExecutionSnapshot.CanonicalJSON()
	if err != nil {
		return nil, fmt.Errorf("Agent model execution snapshot: %w", err)
	}
	var modelSnapshot model.ExecutionSnapshot
	if err := json.Unmarshal(modelJSON, &modelSnapshot); err != nil {
		return nil, fmt.Errorf(
			"decode canonical Agent model execution snapshot: %w", err,
		)
	}
	if modelSnapshot.ProfileID != request.ProfileID {
		return nil, fmt.Errorf(
			"Agent model execution snapshot does not match profile_id",
		)
	}
	toolJSON, err := snapshot.ToolExecutionSnapshot.CanonicalJSON()
	if err != nil {
		return nil, fmt.Errorf("Agent tool execution snapshot: %w", err)
	}
	var toolSnapshot agent.ToolExecutionSnapshot
	if err := json.Unmarshal(toolJSON, &toolSnapshot); err != nil {
		return nil, fmt.Errorf(
			"decode canonical Agent tool execution snapshot: %w", err,
		)
	}
	toolDigest, err := toolSnapshot.Digest()
	if err != nil {
		return nil, fmt.Errorf("Agent tool execution digest: %w", err)
	}
	if snapshot.ToolExecutionDigest != toolDigest {
		return nil, fmt.Errorf(
			"Agent tool_execution_digest does not match its snapshot",
		)
	}
	if request.SessionID == "" {
		if snapshot.SessionRequestDigest != "" ||
			snapshot.SessionConfigDigest != "" {
			return nil, fmt.Errorf(
				"unbound Agent execution has Session digests",
			)
		}
	} else if !validSHA256Digest(snapshot.SessionRequestDigest) ||
		!validSHA256Digest(snapshot.SessionConfigDigest) {
		return nil, fmt.Errorf(
			"Session-bound Agent execution requires valid Session digests",
		)
	}
	canonicalSnapshot := agentExecutionSnapshot{
		ExecutionContractVersion: agent.ExecutionContractVersion,
		ModelExecutionSnapshot:   modelSnapshot,
		ToolExecutionSnapshot:    toolSnapshot,
		ToolExecutionDigest:      toolDigest,
		SessionRequestDigest:     snapshot.SessionRequestDigest,
		SessionConfigDigest:      snapshot.SessionConfigDigest,
	}
	canonicalSnapshot.ConfigDigest =
		computeAgentConfigDigest(canonicalSnapshot)
	if snapshot.ConfigDigest != canonicalSnapshot.ConfigDigest ||
		request.ConfigDigest != "" &&
			request.ConfigDigest != canonicalSnapshot.ConfigDigest {
		return nil, fmt.Errorf(
			"Agent config_digest does not match its execution snapshot",
		)
	}
	canonicalSnapshot.RequestDigest =
		computeAgentRequestDigest(request, canonicalSnapshot)
	if snapshot.RequestDigest != canonicalSnapshot.RequestDigest ||
		request.RequestDigest != "" &&
			request.RequestDigest != canonicalSnapshot.RequestDigest {
		return nil, fmt.Errorf(
			"Agent request_digest does not match its execution snapshot",
		)
	}
	return json.Marshal(canonicalSnapshot)
}

func computeAgentConfigDigest(snapshot agentExecutionSnapshot) string {
	value := struct {
		ExecutionContractVersion int                     `json:"execution_contract_version"`
		ModelExecutionSnapshot   model.ExecutionSnapshot `json:"model_execution_snapshot"`
		ToolExecutionDigest      string                  `json:"tool_execution_digest"`
		SessionConfigDigest      string                  `json:"session_config_digest,omitempty"`
	}{
		ExecutionContractVersion: snapshot.ExecutionContractVersion,
		ModelExecutionSnapshot:   snapshot.ModelExecutionSnapshot,
		ToolExecutionDigest:      snapshot.ToolExecutionDigest,
		SessionConfigDigest:      snapshot.SessionConfigDigest,
	}
	data, _ := json.Marshal(value)
	return digestAgentExecution(data)
}

func computeAgentRequestDigest(
	request Request,
	snapshot agentExecutionSnapshot,
) string {
	value := struct {
		Kind                 Kind                     `json:"kind"`
		ProfileID            string                   `json:"profile_id"`
		Input                string                   `json:"input"`
		SessionID            string                   `json:"session_id,omitempty"`
		SessionRetention     string                   `json:"session_retention,omitempty"`
		TaskID               string                   `json:"task_id,omitempty"`
		CWD                  string                   `json:"cwd,omitempty"`
		ModelOptions         contract.GenerateOptions `json:"model_options,omitempty"`
		AgentBudget          agent.Budget             `json:"agent_budget"`
		Labels               map[string]string        `json:"labels,omitempty"`
		ConfigDigest         string                   `json:"config_digest"`
		SessionRequestDigest string                   `json:"session_request_digest,omitempty"`
	}{
		Kind: KindAgent, ProfileID: request.ProfileID, Input: request.Input,
		SessionID:        request.SessionID,
		SessionRetention: request.SessionRetention,
		TaskID:           request.TaskID,
		CWD:              request.CWD,
		ModelOptions:     request.ModelOptions,
		AgentBudget:      request.AgentBudget,
		Labels:           request.Labels, ConfigDigest: snapshot.ConfigDigest,
		SessionRequestDigest: snapshot.SessionRequestDigest,
	}
	data, _ := json.Marshal(value)
	return digestAgentExecution(data)
}

func digestAgentExecution(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}

func (executor *AgentExecutor) currentAgentExecutionGate(
	_ context.Context,
	request Request,
	frozen agentExecutionSnapshot,
) *contract.RuntimeError {
	changed := func(component string) *contract.RuntimeError {
		return &contract.RuntimeError{
			Code: contract.ErrorConflict, Phase: contract.PhaseProfile,
			Message: "Agent execution snapshot changed: " + component,
		}
	}
	if frozen.ExecutionContractVersion != agent.ExecutionContractVersion {
		return changed("agent contract")
	}
	if executor == nil || executor.Profiles == nil {
		return changed("Profile catalog unavailable")
	}
	entry, exists := executor.Profiles.Resolve(request.ProfileID)
	if !exists || entry.Kind != profile.KindModel || entry.Model == nil ||
		!reflect.DeepEqual(*entry.Model, frozen.ModelExecutionSnapshot.Profile) {
		return changed("model Profile")
	}
	executionModel, ok := executor.Model.(agentExecutionModel)
	if !ok {
		return changed("model execution service unavailable")
	}
	currentModel, err := executionModel.ExecutionSnapshot(request.ProfileID)
	if err != nil || !equalModelExecutionSnapshots(
		currentModel, frozen.ModelExecutionSnapshot,
	) {
		return changed("model execution")
	}
	toolSnapshotter, ok := executor.Tools.(agent.ToolExecutionSnapshotter)
	if !ok {
		return changed("tool execution service unavailable")
	}
	currentTools := toolSnapshotter.ToolExecutionSnapshot()
	if !equalToolExecutionSnapshots(
		currentTools, frozen.ToolExecutionSnapshot,
	) {
		return changed("tool execution")
	}
	sessionRequestDigest, sessionConfigDigest, err :=
		executor.currentAgentSessionDigests(request)
	if err != nil ||
		sessionRequestDigest != frozen.SessionRequestDigest ||
		sessionConfigDigest != frozen.SessionConfigDigest {
		return changed("Session execution")
	}
	return nil
}

func equalModelExecutionSnapshots(
	left, right model.ExecutionSnapshot,
) bool {
	leftJSON, leftErr := left.CanonicalJSON()
	rightJSON, rightErr := right.CanonicalJSON()
	return leftErr == nil && rightErr == nil &&
		bytes.Equal(leftJSON, rightJSON)
}

func equalToolExecutionSnapshots(
	left, right agent.ToolExecutionSnapshot,
) bool {
	leftJSON, leftErr := left.CanonicalJSON()
	rightJSON, rightErr := right.CanonicalJSON()
	return leftErr == nil && rightErr == nil &&
		bytes.Equal(leftJSON, rightJSON)
}

func (executor *AgentExecutor) currentAgentSessionDigests(
	request Request,
) (string, string, error) {
	if request.SessionID == "" {
		return "", "", nil
	}
	if executor == nil || executor.Sessions == nil {
		return "", "", fmt.Errorf("agent Session service is unavailable")
	}
	prepared, runtimeErr := executor.Sessions.PrepareRunRequest(
		agentSessionRunRequest(request, ""),
	)
	if runtimeErr != nil {
		return "", "", runtimeErr
	}
	return prepared.SnapshotDigest(), prepared.ConfigDigest(), nil
}

func agentSessionRunRequest(request Request, runID string) session.RunRequest {
	return session.RunRequest{
		SessionID:    request.SessionID,
		RunID:        runID,
		TaskID:       request.TaskID,
		ProfileID:    request.ProfileID,
		Input:        request.Input,
		Retention:    session.Retention(request.SessionRetention),
		ModelOptions: request.ModelOptions,
	}
}

type snapshotBoundTools struct {
	validator *agent.ToolSnapshotValidator
	executor  agent.ToolExecutor
}

type unavailableAgentModel struct{}

func (unavailableAgentModel) Generate(
	ctx context.Context,
	request contract.GenerateRequest,
) (contract.ModelResult, *contract.RuntimeError) {
	return unavailableAgentModel{}.GenerateStream(ctx, request, nil)
}

func (unavailableAgentModel) GenerateStream(
	context.Context,
	contract.GenerateRequest,
	contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	return contract.ModelResult{}, &contract.RuntimeError{
		Code: contract.ErrorConflict, Phase: contract.PhaseProfile,
		Message: "current Agent model execution is unavailable",
	}
}

func (tools *snapshotBoundTools) Definitions() []contract.ToolSpec {
	if tools == nil || tools.validator == nil {
		return nil
	}
	return tools.validator.Definitions()
}

func (tools *snapshotBoundTools) Validate(request agent.ToolRequest) error {
	if tools == nil || tools.validator == nil {
		return fmt.Errorf("frozen Agent tool validator is unavailable")
	}
	return tools.validator.Validate(request)
}

func (tools *snapshotBoundTools) Execute(
	ctx context.Context,
	request agent.ToolRequest,
) (agent.ToolResult, error) {
	if tools == nil || tools.executor == nil {
		return agent.ToolResult{}, fmt.Errorf(
			"current Agent tool executor is unavailable",
		)
	}
	return tools.executor.Execute(ctx, request)
}

func (executor *AgentExecutor) loadAgentExecutionSnapshot(
	ctx context.Context,
	record *Record,
) (agentExecutionSnapshot, *agent.ToolSnapshotValidator, *contract.RuntimeError) {
	if record == nil {
		return agentExecutionSnapshot{}, nil, agentRecoveryError(
			"Agent Run record is required",
		)
	}
	if len(record.Request.PrivateRequest) == 0 {
		if executor == nil || executor.Store == nil {
			return agentExecutionSnapshot{}, nil, agentRecoveryError(
				"Agent private execution request is unavailable",
			)
		}
		privateRequest, err := executor.Store.PrivateRequest(ctx, record.ID)
		if err != nil {
			return agentExecutionSnapshot{}, nil, agentRecoveryError(
				"load Agent private execution request: " + err.Error(),
			)
		}
		record.Request.PrivateRequest = privateRequest
	}
	snapshot, validator, err := decodeAgentExecutionSnapshot(record.Request)
	if err != nil {
		return agentExecutionSnapshot{}, nil, agentRecoveryError(
			"validate Agent private execution request: " + err.Error(),
		)
	}
	return snapshot, validator, nil
}

func validateAgentSessionSnapshot(
	record Record,
	turn session.AgentTurn,
	snapshot agentExecutionSnapshot,
) *contract.RuntimeError {
	if record.Request.SessionID == "" ||
		turn.RequestDigest != snapshot.SessionRequestDigest ||
		turn.ConfigDigest != snapshot.SessionConfigDigest {
		return agentRecoveryError(
			"Agent Session digests do not match the frozen execution snapshot",
		)
	}
	return nil
}

func executionSnapshotChangedOutcome(
	runtimeErr *contract.RuntimeError,
) ExecutionOutcome {
	if runtimeErr == nil {
		runtimeErr = &contract.RuntimeError{
			Code: contract.ErrorConflict, Phase: contract.PhaseProfile,
			Message: "Agent execution snapshot changed",
		}
	}
	result, _ := json.Marshal(map[string]any{
		"outcome": agent.Outcome{
			State:      agent.StateFailed,
			StopReason: "execution_snapshot_changed",
			Error:      runtimeErr,
		},
	})
	return ExecutionOutcome{
		State: StateFailed, Result: result, Error: runtimeErr,
	}
}

func isExecutionSnapshotChangedError(
	runtimeErr *contract.RuntimeError,
) bool {
	return runtimeErr != nil &&
		runtimeErr.Code == contract.ErrorConflict &&
		runtimeErr.Phase == contract.PhaseProfile
}

func preservePausedAgentSnapshot(
	state agent.LoopState,
) ExecutionOutcome {
	if state.Pause == nil {
		return pendingAgentReconciliation(
			"Agent execution snapshot changed but no durable pause can be restored",
		)
	}
	pause, err := json.Marshal(state.Pause)
	if err != nil {
		return pendingAgentReconciliation(
			"encode durable Agent pause after execution snapshot change: " +
				err.Error(),
		)
	}
	return ExecutionOutcome{State: StatePaused, Pause: pause}
}
