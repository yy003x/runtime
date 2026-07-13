package agentrun

import (
	"context"
	"encoding/json"
	"os"
	"path/filepath"
	"testing"
	"time"
)

func TestManagedRunRequiresAndAcceptsResultContract(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "provider.sh")
	writeExecutable(t, script, `#!/bin/sh
printf '{"schema_version":1,"run_id":"%s","outcome":"succeeded","summary":"managed ok","artifacts":[],"errors":[],"validation":{"commands":[],"passed":true}}\n' "$AGENTRUN_RUN_ID" > "$AGENTRUN_RESULT_FILE"
printf 'stdout is only a log\n'
`)
	writeProfile(t, root, "managed", script, "required")
	service := New(root)
	result, err := service.Run(context.Background(), RunOptions{Profile: "managed", Prompt: "do it", ExecutionMode: ModeManaged})
	if err != nil {
		t.Fatalf("Run: %v", err)
	}
	if result.State != StateDone {
		t.Fatalf("state=%q", result.State)
	}
	contract, err := service.store.ReadResult(mustPaths(t, service, result))
	if err != nil || contract.Summary != "managed ok" {
		t.Fatalf("result=%#v err=%v", contract, err)
	}
}

func TestCaptureSynthesizesResultAndRunIDIsIdempotent(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "capture.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf 'capture ok\\n'\n")
	writeProfile(t, root, "capture", script, "required")
	service := New(root)
	options := RunOptions{RunID: "task-20260713-120000-fixed", Profile: "capture", Prompt: "hello", ExecutionMode: ModeCapture}
	first, err := service.Run(context.Background(), options)
	if err != nil || first.State != StateDone {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	second, err := service.Run(context.Background(), options)
	if err != nil || !second.Idempotent {
		t.Fatalf("second=%#v err=%v", second, err)
	}
	contract, _ := service.store.ReadResult(mustPaths(t, service, first))
	if contract.Summary != "capture ok" {
		t.Fatalf("summary=%q", contract.Summary)
	}
}

func TestForceRunDoesNotReuseStaleResult(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "force.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf 'first result\\n'\n")
	writeProfile(t, root, "force", script, "required")
	service := New(root)
	runID := "task-20260713-120000-force"
	first, err := service.Run(context.Background(), RunOptions{RunID: runID, Profile: "force", Prompt: "first", ExecutionMode: ModeCapture})
	if err != nil || first.State != StateDone {
		t.Fatalf("first=%#v err=%v", first, err)
	}
	writeExecutable(t, script, "#!/bin/sh\nprintf 'no new result\\n'\n")
	forced, err := service.Run(context.Background(), RunOptions{RunID: runID, Profile: "force", Prompt: "second", ExecutionMode: ModeManaged, Force: true})
	if err == nil || forced.FailureReason != "result_missing" {
		t.Fatalf("forced=%#v err=%v", forced, err)
	}
}

func TestManagedRunFailsWithoutRequiredResult(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "missing.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf 'no result\\n'\n")
	writeProfile(t, root, "missing", script, "required")
	service := New(root)
	result, err := service.Run(context.Background(), RunOptions{Profile: "missing", Prompt: "hello"})
	if err == nil {
		t.Fatal("Run returned nil error")
	}
	if result.State != StateFailed || result.FailureReason != "result_missing" {
		t.Fatalf("result=%#v", result)
	}
}

func TestAPIMockSynthesizesResult(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test/v1","model":"mock-model","api_key_env":"UNSET_TEST_KEY","mock":true}}`
	if err := os.WriteFile(filepath.Join(root, "configs", "mock.json"), []byte(profile), 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	result, err := service.Run(context.Background(), RunOptions{Profile: "mock", Prompt: "hello"})
	if err != nil || result.State != StateDone {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestInvalidExternalResultIsSchemaInvalid(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "invalid.sh")
	writeExecutable(t, script, "#!/bin/sh\nprintf '{\"schema_version\":1}' > \"$AGENTRUN_RESULT_FILE\"\n")
	writeProfile(t, root, "invalid", script, "required")
	service := New(root)
	result, err := service.Run(context.Background(), RunOptions{Profile: "invalid", Prompt: "hello"})
	if err == nil || result.FailureReason != "schema_invalid" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
}

func TestRunTimeoutIsClassified(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "slow.sh")
	writeExecutable(t, script, "#!/bin/sh\nsleep 5\n")
	writeProfile(t, root, "slow", script, "required")
	service := New(root)
	started := time.Now()
	result, err := service.Run(context.Background(), RunOptions{Profile: "slow", Prompt: "hello", DeadlineSeconds: 1})
	if err == nil || result.FailureReason != "timeout" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	if time.Since(started) > 4*time.Second {
		t.Fatalf("timeout took too long: %s", time.Since(started))
	}
}

func writeProfile(t *testing.T, root, id, script, contract string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	profile := map[string]any{
		"type": "cli",
		"cli": map[string]any{
			"driver": "generic", "executor": "command",
			"command": map[string]any{"binary": script, "args": []string{}, "model": ""},
			"runtime": map[string]any{"prompt_delivery": "stdin", "result_contract": contract},
		},
	}
	data, _ := json.Marshal(profile)
	if err := os.WriteFile(filepath.Join(root, "configs", id+".json"), data, 0o644); err != nil {
		t.Fatal(err)
	}
}

func writeExecutable(t *testing.T, path, body string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}
}

func mustPaths(t *testing.T, service *Service, summary RunSummary) Paths {
	t.Helper()
	paths, err := RunPaths(service.RunsDir, summary.RunType, summary.RunID)
	if err != nil {
		t.Fatal(err)
	}
	return paths
}
