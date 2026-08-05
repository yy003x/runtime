package runtimebootstrap

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	goruntime "runtime"
	"strings"
	"testing"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/internal/identity"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimeconfig"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/provider"
	runtime "github.com/yy003x/runtime/run"
	sqlitestore "github.com/yy003x/runtime/store/sqlite"
)

func TestExecutionAttemptObserverWritesAPIRecordBestEffort(t *testing.T) {
	logsDir := filepath.Join(t.TempDir(), "logs")
	observer := executionAttemptObserver(logsDir)
	observer(model.Attempt{
		Origin: model.AttemptOrigin{
			Namespace: model.AttemptNamespaceAgent, Source: "agent run_fixture",
		},
		ProfileID: "api-cc",
		Wire: provider.Attempt{
			Started: true,
			Request: provider.Request{
				Method: "POST", URL: "https://example.invalid/v1/messages",
				Headers: map[string]string{"X-Api-Key": "${MODEL_API_KEY}"},
				Body:    json.RawMessage(`{"model":"fixture"}`),
			},
			Response: &provider.Response{Status: 200, Data: []json.RawMessage{}},
		},
	})
	paths, err := filepath.Glob(filepath.Join(logsDir, "*", "api.jsonl"))
	if err != nil || len(paths) != 1 {
		t.Fatalf("paths=%v error=%v", paths, err)
	}
	data, err := os.ReadFile(paths[0])
	if err != nil {
		t.Fatal(err)
	}
	var record struct {
		Namespace string `json:"namespace"`
		Profile   string `json:"profile"`
		Source    string `json:"source"`
		CallID    string `json:"call_id"`
	}
	if err := json.Unmarshal(data, &record); err != nil {
		t.Fatal(err)
	}
	if record.Namespace != "agent" || record.Profile != "api-cc" ||
		record.Source != "agent run_fixture" || identity.Validate(record.CallID, "call") != nil {
		t.Fatalf("record=%#v", record)
	}

	broken := filepath.Join(t.TempDir(), "logs")
	if err := os.WriteFile(broken, []byte("not a directory"), 0o600); err != nil {
		t.Fatal(err)
	}
	// The observer is diagnostic-only: an unwritable target must be swallowed.
	executionAttemptObserver(broken)(model.Attempt{
		Origin:    model.AttemptOrigin{Namespace: model.AttemptNamespaceRequest},
		ProfileID: "api", Wire: provider.Attempt{Started: true},
	})
	content, err := os.ReadFile(broken)
	if err != nil || strings.TrimSpace(string(content)) != "not a directory" {
		t.Fatalf("broken target changed: content=%q error=%v", content, err)
	}
}

func TestBuildAgentToolsCombinesBuiltinAndConfiguredMCPTools(t *testing.T) {
	root := t.TempDir()
	toolsDirectory := filepath.Join(root, "tools")
	if err := os.Mkdir(toolsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfiguredTool(t, toolsDirectory, "web_search", "webSearchPrime", map[string]string{
		"Authorization": "Bearer ${Z_AI_API_KEY}",
	})
	writeConfiguredTool(t, toolsDirectory, "web_fetch", "webReader", map[string]string{
		"Authorization": "Bearer ${Z_AI_API_KEY}",
		"X-Aux-Key":     "${AUX_TOOL_KEY}",
	})
	const secret = "resolved-secret-must-not-appear"
	t.Setenv("Z_AI_API_KEY", secret)
	registry, references, err := buildAgentTools(
		toolsDirectory,
		root,
		runtimeconfig.Default().Agent,
	)
	if err != nil {
		t.Fatal(err)
	}
	definitions := registry.Definitions()
	if len(definitions) != 4 || definitions[0].Name != "list_directory" ||
		definitions[1].Name != "read_file" ||
		definitions[2].Name != "web_fetch" ||
		definitions[3].Name != "web_search" {
		t.Fatalf("definitions=%#v", definitions)
	}
	if len(references) != 2 || references[0] != "AUX_TOOL_KEY" ||
		references[1] != "Z_AI_API_KEY" {
		t.Fatalf("environment references=%#v", references)
	}
	canonical, err := registry.ToolExecutionSnapshot().CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), secret) ||
		!strings.Contains(string(canonical), "${Z_AI_API_KEY}") ||
		!strings.Contains(string(canonical), `"implementation":"runtime.toolbuiltin"`) ||
		!strings.Contains(string(canonical), `"implementation":"runtime.toolmcp"`) {
		t.Fatalf("composite snapshot=%s", canonical)
	}
}

func TestBuildAgentToolsLoadsSourceDefaultCatalog(t *testing.T) {
	_, source, _, ok := goruntime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	repositoryRoot := filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
	registry, references, err := buildAgentTools(
		filepath.Join(repositoryRoot, "resources", "tools"),
		repositoryRoot,
		runtimeconfig.Default().Agent,
	)
	if err != nil {
		t.Fatal(err)
	}
	definitions := registry.Definitions()
	if len(definitions) != 4 || definitions[2].Name != "web_fetch" ||
		definitions[3].Name != "web_search" {
		t.Fatalf("source default definitions=%#v", definitions)
	}
	if len(references) != 1 || references[0] != "Z_AI_API_KEY" {
		t.Fatalf("source default environment references=%#v", references)
	}
}

func TestBuildAgentToolsCatalogSelectionIsFailClosed(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing-tools")
	registry, references, err := buildAgentTools(
		missing, root, runtimeconfig.Agent{Tools: []string{"read_file"}},
	)
	if err != nil || len(registry.Definitions()) != 1 || len(references) != 0 {
		t.Fatalf(
			"built-in-only registry=%#v references=%#v error=%v",
			registry, references, err,
		)
	}
	if _, _, err := buildAgentTools(
		missing, root, runtimeconfig.Agent{Tools: []string{"web_search"}},
	); err == nil || !strings.Contains(err.Error(), "load Tool Catalog") {
		t.Fatalf("missing configured catalog error=%v", err)
	}

	toolsDirectory := filepath.Join(root, "tools")
	if err := os.Mkdir(toolsDirectory, 0o700); err != nil {
		t.Fatal(err)
	}
	writeConfiguredTool(t, toolsDirectory, "read_file", "remoteRead", map[string]string{})
	if _, _, err := buildAgentTools(
		toolsDirectory, root,
		runtimeconfig.Agent{Tools: []string{"read_file"}},
	); err == nil || !strings.Contains(err.Error(), "conflicts with a built-in tool") {
		t.Fatalf("built-in manifest collision error=%v", err)
	}
}

func writeConfiguredTool(
	t *testing.T,
	directory string,
	name string,
	remoteTool string,
	headers map[string]string,
) {
	t.Helper()
	document := map[string]any{
		"schema_version": 1,
		"name":           name,
		"effect":         "read_only",
		"description":    "fixture " + name,
		"input_schema": map[string]any{
			"type": "object",
			"properties": map[string]any{
				"query": map[string]any{"type": "string"},
			},
			"required":             []string{"query"},
			"additionalProperties": false,
		},
		"executor": map[string]any{
			"type": "mcp", "endpoint": "https://example.invalid/mcp",
			"remote_tool": remoteTool, "headers": headers,
			"timeout": "30s", "max_response_bytes": 1 << 20,
		},
	}
	data, err := json.Marshal(document)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(directory, name+".json"), data, 0o600,
	); err != nil {
		t.Fatal(err)
	}
}

func TestRunRecoveryLoaderReconcilesAfterCompositionOnly(t *testing.T) {
	ctx := context.Background()
	home := t.TempDir()
	paths, err := layout.FromHome(home)
	if err != nil {
		t.Fatal(err)
	}
	for _, directory := range []string{
		paths.ConfigDir, paths.StateDir, paths.SessionsDir,
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			t.Fatal(err)
		}
	}
	if err := os.WriteFile(
		filepath.Join(paths.ConfigDir, "api.json"),
		[]byte(`{
			"type":"api",
			"driver":"openai",
			"endpoint":"https://example.invalid/v1/chat/completions",
			"model":"fixture",
			"headers":{"Authorization":"${FIXTURE_API_KEY}"},
			"timeout":"1m"
		}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.RuntimeConfigFile,
		[]byte(`{"agent":{"tools":["read_file","list_directory"]}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	setup, err := LoadServices(paths, home)
	if err != nil {
		t.Fatal(err)
	}
	submitted, runtimeErr := setup.Runs.Submit(ctx, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api",
		Input:       "cancel across process crash",
		AgentBudget: agent.DefaultBudget(),
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	runID := submitted.ID
	if err := setup.Runs.Close(); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		paths.RunDBFile,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(ctx, runID); err != nil {
		t.Fatal(err)
	}
	reserved, err := store.RequestCancel(ctx, runID)
	if err != nil || reserved.State != runtime.StateRunning ||
		!reserved.CancelRequested {
		t.Fatalf("reserved=%#v err=%v", reserved, err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	query, err := LoadRunQueryServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err := query.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != runtime.StateRunning ||
		!unchanged.CancelRequested ||
		unchanged.SettledSequence != 0 {
		t.Fatalf("query loader mutated durable state: %#v", unchanged)
	}
	if err := query.Runs.Close(); err != nil {
		t.Fatal(err)
	}

	maintenance, err := LoadRunMaintenanceServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err = maintenance.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != runtime.StateRunning ||
		!unchanged.CancelRequested ||
		unchanged.SettledSequence != 0 {
		t.Fatalf("maintenance loader mutated durable state: %#v", unchanged)
	}
	if err := maintenance.Runs.Close(); err != nil {
		t.Fatal(err)
	}

	ordinary, err := LoadServices(paths, home)
	if err != nil {
		t.Fatal(err)
	}
	unchanged, err = ordinary.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if unchanged.State != runtime.StateRunning ||
		!unchanged.CancelRequested ||
		unchanged.SettledSequence != 0 {
		t.Fatalf("ordinary loader mutated durable state: %#v", unchanged)
	}
	if err := ordinary.Runs.Close(); err != nil {
		t.Fatal(err)
	}

	recovered, err := LoadServicesWithRunRecovery(paths, home)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = recovered.Runs.Close() })
	cancelled, err := recovered.Runs.Get(ctx, runID)
	if err != nil {
		t.Fatal(err)
	}
	if cancelled.State != runtime.StateCancelled ||
		!cancelled.CancelRequested ||
		cancelled.SettledSequence == 0 {
		t.Fatalf("recovery loader did not converge reservation: %#v", cancelled)
	}
}

func TestRunMaintenanceLoaderClosesStoreAfterSessionFailure(t *testing.T) {
	paths, err := layout.FromHome(t.TempDir())
	if err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.SessionsDir, []byte("not a directory"), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRunMaintenanceServices(paths); err == nil {
		t.Fatal("maintenance loader accepted a non-directory Session root")
	}
	for _, suffix := range []string{"-wal", "-shm"} {
		if _, err := os.Stat(paths.RunDBFile + suffix); !os.IsNotExist(err) {
			t.Fatalf("Run Store sidecar remained after loader error: %s", suffix)
		}
	}
	store, err := sqlitestore.Open(
		paths.RunDBFile,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatalf("reopen Run Store after loader error: %v", err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
}

func TestRunQueryAndMaintenanceLoadersIgnoreExecutionInputsAndClose(
	t *testing.T,
) {
	ctx := context.Background()
	home := t.TempDir()
	paths, err := layout.FromHome(home)
	if err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(paths.ConfigDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(paths.ConfigDir, "api.json"),
		[]byte(`{
			"type":"api",
			"driver":"openai",
			"endpoint":"https://example.invalid/v1/chat/completions",
			"model":"fixture",
			"headers":{"Authorization":"${FIXTURE_API_KEY}"},
			"timeout":"1m"
		}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.RuntimeConfigFile,
		[]byte(`{"agent":{"tools":["read_file","list_directory"]}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	setup, err := LoadServices(paths, home)
	if err != nil {
		t.Fatal(err)
	}
	submitted, runtimeErr := setup.Runs.Submit(ctx, runtime.Request{
		Kind: runtime.KindAgent, ProfileID: "api", Input: "cancel",
		AgentBudget: agent.DefaultBudget(),
	})
	if runtimeErr != nil {
		t.Fatal(runtimeErr)
	}
	cancelID := submitted.ID
	if err := setup.Runs.Close(); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(paths.ConfigDir, "api.json"),
		[]byte(`{"type":"api","driver":`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		paths.RuntimeConfigFile, []byte(`{"agent":`), 0o600,
	); err != nil {
		t.Fatal(err)
	}
	store, err := sqlitestore.Open(
		paths.RunDBFile,
		sqlitestore.Options{SkipReconcile: true},
	)
	if err != nil {
		t.Fatal(err)
	}
	reconcileID := "run_99999999999999999999999999999999"
	if _, err := store.Create(ctx, reconcileID, runtime.Request{
		Kind: runtime.KindSession, ProfileID: "missing", Input: "done",
		SessionID: "session_99999999999999999999999999999999",
	}); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Start(ctx, reconcileID); err != nil {
		t.Fatal(err)
	}
	if _, err := store.Settle(
		ctx, reconcileID, runtime.StateCompleted, []byte(`{"ok":true}`), nil,
	); err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}

	query, err := LoadRunQueryServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	if value, err := query.Runs.Get(ctx, cancelID); err != nil ||
		value.ID != cancelID {
		_ = query.Runs.Close()
		t.Fatalf("value=%#v err=%v", value, err)
	}
	if err := query.Runs.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := query.Runs.Get(ctx, cancelID); err == nil {
		t.Fatal("Run query Store remained open after Close")
	}

	maintenance, err := LoadRunMaintenanceServices(paths)
	if err != nil {
		t.Fatal(err)
	}
	cancelled, err := maintenance.Runs.Cancel(ctx, cancelID)
	if err != nil || cancelled.State != runtime.StateCancelled {
		_ = maintenance.Runs.Close()
		t.Fatalf("cancelled=%#v err=%v", cancelled, err)
	}
	reconciled, runtimeErr := maintenance.Runs.ReconcileRun(
		ctx, reconcileID,
	)
	if runtimeErr != nil ||
		reconciled.State != runtime.StateCompleted {
		_ = maintenance.Runs.Close()
		t.Fatalf("reconciled=%#v err=%v", reconciled, runtimeErr)
	}
	if err := maintenance.Runs.Close(); err != nil {
		t.Fatal(err)
	}
	if _, err := maintenance.Runs.Get(ctx, cancelID); err == nil {
		t.Fatal("Run maintenance Store remained open after Close")
	}
}
