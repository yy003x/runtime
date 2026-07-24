package provider

import (
	"context"
	"path/filepath"
	"testing"

	"github.com/yy003x/runtime/internal/llm"
	nativeengine "github.com/yy003x/runtime/internal/provider/native"
)

func TestBuildNativeToolRuntimeRequiresExplicitAuthorization(t *testing.T) {
	tools, executor := buildNativeToolRuntime(Request{Allowed: []string{"echo"}})
	if len(tools) != 1 || tools[0].Name != "echo" || executor == nil {
		t.Fatalf("tools=%#v executor=%T", tools, executor)
	}
	output, err := executor.Execute(context.Background(), nativeengine.ToolCall{ID: "one", Name: "echo", Arguments: map[string]any{"value": "ok"}})
	if err != nil || output.(map[string]any)["value"] != "ok" {
		t.Fatalf("output=%#v err=%v", output, err)
	}
	blocked, blockedExecutor := buildNativeToolRuntime(Request{Allowed: []string{"echo"}, Forbidden: []string{"echo"}})
	if len(blocked) != 0 || blockedExecutor != nil {
		t.Fatalf("blocked tools=%#v executor=%T", blocked, blockedExecutor)
	}
}

func TestNativeProviderExecutesMockAndPersistsSnapshot(t *testing.T) {
	provider := nativeProvider{}
	cfg := Config{ID: "native-mock", Type: TypeNative, Native: &NativeConfig{
		MaxRounds: 2, Mock: &NativeMockConfig{Responses: []string{"native ok"}, DoneAfter: 1},
	}}
	if cfg.Native.Persona != "" {
		t.Fatalf("unexpected implicit persona=%q", cfg.Native.Persona)
	}
	snapshotFile := filepath.Join(t.TempDir(), "snapshot.json")
	prepared, err := provider.Prepare(context.Background(), cfg, Request{
		Prompt: "hello", RunID: "run-native", SnapshotFile: snapshotFile, PersonaDir: t.TempDir(),
	})
	if err != nil {
		t.Fatal(err)
	}
	result, err := provider.Execute(context.Background(), prepared, nopSink{})
	if err != nil || result.FinalText != "native ok" || result.State != "completed" {
		t.Fatalf("result=%#v err=%v", result, err)
	}
	snapshot, err := ReadNativeSnapshot(snapshotFile)
	if err != nil || snapshot["native_state"] != nativeengine.StateCompleted {
		t.Fatalf("snapshot=%#v err=%v", snapshot, err)
	}
	controlled, err := ControlNative(snapshotFile, "stop", "finished")
	if err != nil || controlled["native_state"] != nativeengine.StateCompleted {
		t.Fatalf("controlled=%#v err=%v", controlled, err)
	}
}

func TestBuildNativeClientValidatesReferencedAPIProfile(t *testing.T) {
	prepared := PreparedRequest{Config: Config{ID: "native", Type: TypeNative, Native: &NativeConfig{}}, Native: &NativeRequest{
		EffectiveOptions: map[string]any{"model_profile": "missing"},
	}, Request: Request{Profiles: map[string]Config{}}}
	if _, err := buildNativeClient(prepared); err == nil {
		t.Fatal("missing model profile was accepted")
	}
	prepared.Request.Profiles["api"] = Config{ID: "api", Type: TypeAPI, API: &APIConfig{Protocol: "openai", APIKey: "${NATIVE_PROVIDER_TEST_KEY}"}}
	prepared.Native.EffectiveOptions["model_profile"] = "api"
	if _, err := buildNativeClient(prepared); err == nil {
		t.Fatal("missing credential was accepted")
	}
}

func TestNativeClientAdapterPreservesDoneAndToolCalls(t *testing.T) {
	client := &recordingLLMClient{response: llm.Response{
		OutputText: "call", ToolCalls: []llm.ToolCall{{ID: "one", Name: "echo", Arguments: map[string]any{"value": "ok"}}},
		FinishReason: "tool_calls", Done: false, InputTokens: 4, OutputTokens: 2,
	}}
	adapter := nativeClientAdapter{client: client, model: "model"}
	response, err := adapter.Generate(context.Background(), nativeengine.Request{
		Messages: []nativeengine.Message{{Role: "system", Content: "system"}, {Role: "user", Content: "hello"}},
		Tools:    []nativeengine.Tool{{Name: "echo", Parameters: map[string]any{"type": "object"}}},
	})
	if err != nil {
		t.Fatal(err)
	}
	if response.Done || len(response.ToolCalls) != 1 || response.FinishReason != "tool_calls" || response.InputTokens != 4 {
		t.Fatalf("response=%#v", response)
	}
	if client.request.System != "system" || len(client.request.Tools) != 1 {
		t.Fatalf("request=%#v", client.request)
	}
}

func TestNativeProviderHelpersPreservePatchAndNopOutput(t *testing.T) {
	if (nativeProvider{}).Kind() != TypeNative {
		t.Fatal("native provider kind mismatch")
	}
	patch := convertNativePatch(NativePatch{
		Operation: "replace", SystemInstructions: []NativeMessage{{Role: "system", Content: "system", Pinned: true}},
		Messages: []NativeMessage{{Role: "user", Content: "hello"}},
	})
	if patch.Operation != nativeengine.PatchReplace || len(patch.SystemInstructions) != 1 || len(patch.Messages) != 1 {
		t.Fatalf("patch=%#v", patch)
	}
	sink := nopSink{}
	if err := sink.Stdout([]byte("ignored")); err != nil {
		t.Fatal(err)
	}
	if err := sink.Stderr([]byte("ignored")); err != nil {
		t.Fatal(err)
	}
	if value, ok := integerValue(float64(3)); !ok || value != 3 {
		t.Fatalf("integer=%v ok=%v", value, ok)
	}
}

type recordingLLMClient struct {
	request  llm.Request
	response llm.Response
}

func (client *recordingLLMClient) Generate(_ context.Context, request llm.Request) (llm.Response, error) {
	client.request = request
	return client.response, nil
}
