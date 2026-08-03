package run

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/yy003x/runtime/agent"
	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/profile"
)

type snapshotContractDriver struct {
	mu       sync.Mutex
	identity model.DriverExecutionIdentity
}

func (driver *snapshotContractDriver) ExecutionIdentity() model.DriverExecutionIdentity {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	return driver.identity
}

func (*snapshotContractDriver) Validate(value model.Profile) error {
	return value.Validate()
}

func (*snapshotContractDriver) Stream(
	context.Context,
	model.ResolvedModel,
	contract.ModelRequest,
	contract.EventSink,
) (contract.ModelResult, *contract.RuntimeError) {
	return contract.ModelResult{}, &contract.RuntimeError{
		Code: contract.ErrorInternal, Phase: contract.PhaseProvider,
		Message: "snapshot contract fixture must not execute the Provider",
	}
}

func (driver *snapshotContractDriver) setVersion(version int) {
	driver.mu.Lock()
	defer driver.mu.Unlock()
	driver.identity.ImplementationVersion = version
}

type snapshotContractStore struct {
	Store

	mu      sync.Mutex
	records map[string]Record
	private map[string]json.RawMessage
	creates int
}

func newSnapshotContractStore() *snapshotContractStore {
	return &snapshotContractStore{
		records: make(map[string]Record),
		private: make(map[string]json.RawMessage),
	}
}

func (store *snapshotContractStore) Create(
	_ context.Context,
	runID string,
	request Request,
) (Record, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	if _, exists := store.records[runID]; exists {
		return Record{}, errors.New("duplicate Run")
	}
	private := append(json.RawMessage(nil), request.PrivateRequest...)
	publicRequest := cloneRequest(request)
	publicRequest.PrivateRequest = nil
	now := time.Date(2026, time.July, 30, 12, 0, 0, 0, time.UTC)
	record := Record{
		SchemaVersion: 4,
		ID:            runID,
		State:         StateQueued,
		Request:       publicRequest,
		RetryOf:       request.RetryOf,
		CreatedAt:     now,
		UpdatedAt:     now,
	}
	store.records[runID] = record
	store.private[runID] = private
	store.creates++
	return cloneSnapshotContractRecord(record), nil
}

func (store *snapshotContractStore) Get(
	_ context.Context,
	runID string,
) (Record, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	record, exists := store.records[runID]
	if !exists {
		return Record{}, ErrNotFound
	}
	return cloneSnapshotContractRecord(record), nil
}

func (store *snapshotContractStore) PrivateRequest(
	_ context.Context,
	runID string,
) (json.RawMessage, error) {
	store.mu.Lock()
	defer store.mu.Unlock()
	value, exists := store.private[runID]
	if !exists {
		return nil, ErrNotFound
	}
	return append(json.RawMessage(nil), value...), nil
}

func (store *snapshotContractStore) markTerminal(
	t *testing.T,
	runID string,
) {
	t.Helper()
	store.mu.Lock()
	defer store.mu.Unlock()
	record, exists := store.records[runID]
	if !exists {
		t.Fatalf("Run %s does not exist", runID)
	}
	record.State = StateCompleted
	record.SettledSequence = 1
	store.records[runID] = record
}

func (store *snapshotContractStore) createCount() int {
	store.mu.Lock()
	defer store.mu.Unlock()
	return store.creates
}

func cloneSnapshotContractRecord(value Record) Record {
	value.Request = cloneRequest(value.Request)
	value.Result = append(json.RawMessage(nil), value.Result...)
	value.Pause = append(json.RawMessage(nil), value.Pause...)
	if value.Error != nil {
		current := *value.Error
		value.Error = &current
	}
	return value
}

type snapshotContractFixture struct {
	executor    *AgentExecutor
	service     *Service
	store       *snapshotContractStore
	driver      *snapshotContractDriver
	secret      *string
	secretReads *int
}

func newSnapshotContractFixture(t *testing.T) snapshotContractFixture {
	t.Helper()
	modelProfile := model.Profile{
		Driver:   model.DriverOpenAI,
		Endpoint: "https://example.test/v1/chat/completions",
		Model:    "snapshot-fixture",
		Headers: map[string]string{
			"Authorization": "${SN_SNAPSHOT_TEST_TOKEN}",
			"X-Fixture":     "snapshot",
		},
		Timeout: "1m",
	}
	modelCatalog, err := model.NewCatalog(map[string]model.Profile{
		"api": modelProfile,
	})
	if err != nil {
		t.Fatal(err)
	}
	commandCatalog, err := runtimecommand.NewCatalog(
		map[string]runtimecommand.Profile{},
	)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewCatalog(commandCatalog, modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	driver := &snapshotContractDriver{
		identity: model.DriverExecutionIdentity{
			Driver:                model.DriverOpenAI,
			Implementation:        "runtime.run.snapshot-contract-test",
			ImplementationVersion: 1,
		},
	}
	secret := "secret-value-that-must-not-be-persisted"
	secretReads := 0
	models, err := model.NewService(
		modelCatalog,
		map[model.DriverName]model.Driver{
			model.DriverOpenAI: driver,
		},
		model.ServiceOptions{Getenv: func(name string) (string, bool) {
			secretReads++
			if name != "SN_SNAPSHOT_TEST_TOKEN" {
				return "", false
			}
			return secret, true
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := agent.NewRegistry(agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name:        "echo",
			Description: "snapshot fixture",
			InputSchema: json.RawMessage(
				`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`,
			),
		},
		Handler: func(
			context.Context,
			agent.ToolRequest,
		) (agent.ToolResult, error) {
			return agent.ToolResult{Content: "unused"}, nil
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	store := newSnapshotContractStore()
	executor := &AgentExecutor{
		Profiles: profiles,
		Model:    models,
		Tools:    tools,
		Store:    store,
	}
	service, err := NewService(ServiceOptions{
		Store: store,
		Executors: map[Kind]Executor{
			KindAgent: executor,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return snapshotContractFixture{
		executor: executor, service: service, store: store, driver: driver,
		secret: &secret, secretReads: &secretReads,
	}
}

func snapshotContractRequest() Request {
	return Request{
		Kind: KindAgent, ProfileID: "api", Input: "snapshot input",
		Labels: map[string]string{
			"source": "contract-test",
		},
	}
}

func TestAgentExecutionSnapshotStrictlyBindsPrivateAndPublicFacts(
	t *testing.T,
) {
	fixture := newSnapshotContractFixture(t)
	prepared, err := fixture.executor.Prepare(
		context.Background(), snapshotContractRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := decodeAgentExecutionSnapshot(prepared)
	if err != nil {
		t.Fatal(err)
	}
	if prepared.AgentBudget != agent.DefaultBudget() ||
		prepared.ConfigDigest != snapshot.ConfigDigest ||
		prepared.RequestDigest != snapshot.RequestDigest {
		t.Fatalf(
			"prepared=%#v snapshot=%#v",
			prepared, snapshot,
		)
	}

	oversized := prepared
	oversized.PrivateRequest = bytes.Repeat(
		[]byte{'x'}, MaxPrivateRequestBytes+1,
	)
	if _, _, err := decodeAgentExecutionSnapshot(oversized); err == nil {
		t.Fatal("oversized private snapshot was accepted")
	}

	nonCanonical := prepared
	nonCanonical.PrivateRequest = append(
		append(json.RawMessage(nil), prepared.PrivateRequest...), '\n',
	)
	if _, _, err := decodeAgentExecutionSnapshot(nonCanonical); err == nil {
		t.Fatal("non-canonical private snapshot was accepted")
	}

	unknown := prepared
	unknown.PrivateRequest = append(
		append(
			json.RawMessage(nil),
			prepared.PrivateRequest[:len(prepared.PrivateRequest)-1]...,
		),
		[]byte(`,"unknown":true}`)...,
	)
	if _, _, err := decodeAgentExecutionSnapshot(unknown); err == nil {
		t.Fatal("private snapshot with an unknown field was accepted")
	}

	testCases := []struct {
		name   string
		mutate func(*Request, *agentExecutionSnapshot)
	}{
		{
			name: "execution_contract_version",
			mutate: func(_ *Request, value *agentExecutionSnapshot) {
				value.ExecutionContractVersion++
			},
		},
		{
			name: "tool_definition",
			mutate: func(_ *Request, value *agentExecutionSnapshot) {
				value.ToolExecutionSnapshot.Definitions[0].Description =
					"tampered"
			},
		},
		{
			name: "tool_execution_digest",
			mutate: func(_ *Request, value *agentExecutionSnapshot) {
				value.ToolExecutionDigest =
					"sha256:" + string(bytes.Repeat([]byte{'0'}, 64))
			},
		},
		{
			name: "config_digest",
			mutate: func(_ *Request, value *agentExecutionSnapshot) {
				value.ConfigDigest =
					"sha256:" + string(bytes.Repeat([]byte{'1'}, 64))
			},
		},
		{
			name: "request_digest",
			mutate: func(_ *Request, value *agentExecutionSnapshot) {
				value.RequestDigest =
					"sha256:" + string(bytes.Repeat([]byte{'2'}, 64))
			},
		},
		{
			name: "missing_public_config_digest",
			mutate: func(request *Request, _ *agentExecutionSnapshot) {
				request.ConfigDigest = ""
			},
		},
		{
			name: "missing_public_request_digest",
			mutate: func(request *Request, _ *agentExecutionSnapshot) {
				request.RequestDigest = ""
			},
		},
		{
			name: "public_effective_budget",
			mutate: func(request *Request, _ *agentExecutionSnapshot) {
				request.AgentBudget.MaxRounds++
			},
		},
		{
			name: "public_cwd",
			mutate: func(request *Request, _ *agentExecutionSnapshot) {
				request.CWD = "/tmp/tampered-cwd"
			},
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			request := cloneRequest(prepared)
			current := snapshot
			testCase.mutate(&request, &current)
			privateRequest, marshalErr := json.Marshal(current)
			if marshalErr != nil {
				t.Fatal(marshalErr)
			}
			request.PrivateRequest = privateRequest
			if _, _, decodeErr := decodeAgentExecutionSnapshot(
				request,
			); decodeErr == nil {
				t.Fatal("tampered Agent execution snapshot was accepted")
			}
		})
	}
}

func TestAgentExecutionRequestDigestBindsAllImmutableInputs(
	t *testing.T,
) {
	fixture := newSnapshotContractFixture(t)
	prepared, err := fixture.executor.Prepare(
		context.Background(), snapshotContractRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot, _, err := decodeAgentExecutionSnapshot(prepared)
	if err != nil {
		t.Fatal(err)
	}
	base := computeAgentRequestDigest(prepared, snapshot)

	changedOptions := cloneRequest(prepared)
	maxOutputTokens := int64(128)
	changedOptions.ModelOptions.MaxOutputTokens = &maxOutputTokens
	if computeAgentRequestDigest(changedOptions, snapshot) == base {
		t.Fatal("Agent request digest does not bind model_options")
	}

	withCWD := snapshotContractRequest()
	withCWD.CWD = "/tmp/unsupported-agent-cwd"
	if _, err := fixture.executor.Prepare(
		context.Background(), withCWD,
	); err == nil || !strings.Contains(err.Error(), "cwd is invalid") {
		t.Fatalf("Agent cwd was accepted: %v", err)
	}

	changedBudget := cloneRequest(prepared)
	changedBudget.AgentBudget.MaxRounds++
	if _, _, err := decodeAgentExecutionSnapshot(
		changedBudget,
	); err == nil {
		t.Fatal("public Agent budget tamper was accepted")
	}

	zeroBudget := snapshotContractRequest()
	zeroBudget.AgentBudget = agent.Budget{}
	effective := zeroBudget
	effective.AgentBudget = zeroBudget.AgentBudget.Effective()
	if computeAgentRequestDigest(zeroBudget, snapshot) ==
		computeAgentRequestDigest(effective, snapshot) {
		t.Fatal("Agent request digest does not bind the effective budget")
	}
}

func TestAgentExecutionSnapshotExcludesAndIgnoresSecretValues(
	t *testing.T,
) {
	fixture := newSnapshotContractFixture(t)
	prepared, err := fixture.executor.Prepare(
		context.Background(), snapshotContractRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if bytes.Contains(
		prepared.PrivateRequest, []byte(*fixture.secret),
	) {
		t.Fatal("resolved secret value leaked into private snapshot")
	}
	if *fixture.secretReads != 0 {
		t.Fatalf("snapshot preparation resolved secret %d times", *fixture.secretReads)
	}
	snapshot, _, err := decodeAgentExecutionSnapshot(prepared)
	if err != nil {
		t.Fatal(err)
	}
	*fixture.secret = "rotated-secret-value"
	if runtimeErr := fixture.executor.currentAgentExecutionGate(
		context.Background(), prepared, snapshot,
	); runtimeErr != nil {
		t.Fatalf("secret rotation caused execution drift: %v", runtimeErr)
	}
	if *fixture.secretReads != 0 {
		t.Fatalf("drift gate resolved secret %d times", *fixture.secretReads)
	}
}

func TestAgentPrivateSnapshotIsExcludedFromPublicRunJSON(t *testing.T) {
	fixture := newSnapshotContractFixture(t)
	prepared, err := fixture.executor.Prepare(
		context.Background(), snapshotContractRequest(),
	)
	if err != nil {
		t.Fatal(err)
	}
	record := Record{
		SchemaVersion: 4,
		ID:            "run_11111111111111111111111111111111",
		State:         StateQueued,
		Request:       prepared,
	}
	data, err := json.Marshal(record)
	if err != nil {
		t.Fatal(err)
	}
	for _, forbidden := range [][]byte{
		prepared.PrivateRequest,
		[]byte(`"model_execution_snapshot"`),
		[]byte(`"tool_execution_snapshot"`),
		[]byte(`"private_request"`),
		[]byte(*fixture.secret),
	} {
		if len(forbidden) > 0 && bytes.Contains(data, forbidden) {
			t.Fatalf("public Run JSON leaked private data: %s", data)
		}
	}
	if !bytes.Contains(data, []byte(`"request_digest"`)) ||
		!bytes.Contains(data, []byte(`"config_digest"`)) {
		t.Fatalf("public Run JSON omitted public digests: %s", data)
	}
}

func TestAgentRetryPreservesPrivateBytesAndRejectsCurrentDriftBeforeCreate(
	t *testing.T,
) {
	fixture := newSnapshotContractFixture(t)
	original, runtimeErr := fixture.service.Submit(
		context.Background(), snapshotContractRequest(),
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	fixture.store.markTerminal(t, original.ID)
	originalPrivate, err := fixture.store.PrivateRequest(
		context.Background(), original.ID,
	)
	if err != nil {
		t.Fatal(err)
	}

	*fixture.secret = "rotated-before-retry"
	retry, runtimeErr := fixture.service.Retry(
		context.Background(), original.ID,
	)
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	retryPrivate, err := fixture.store.PrivateRequest(
		context.Background(), retry.ID,
	)
	if err != nil {
		t.Fatal(err)
	}
	if retry.RetryOf != original.ID ||
		!bytes.Equal(retryPrivate, originalPrivate) {
		t.Fatalf(
			"retry=%#v\noriginal private=%s\nretry private=%s",
			retry, originalPrivate, retryPrivate,
		)
	}

	before := fixture.store.createCount()
	fixture.driver.setVersion(2)
	_, runtimeErr = fixture.service.Retry(
		context.Background(), original.ID,
	)
	if runtimeErr == nil ||
		runtimeErr.Code != contract.ErrorConflict ||
		runtimeErr.Phase != contract.PhaseProfile {
		t.Fatalf("drift retry error=%#v", runtimeErr)
	}
	if fixture.store.createCount() != before {
		t.Fatal("drifted Retry created a new Run")
	}
}
