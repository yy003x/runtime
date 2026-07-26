package provider

import (
	"context"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestEstimateStaticContextIsDeterministicAndOrdered(t *testing.T) {
	cfg := Config{
		ID: "native", Type: TypeNative,
		Native: &NativeConfig{SystemPrompt: "固定系统提示"},
	}
	request := ContextEstimateRequest{
		Prompt: "执行任务", ToolDir: t.TempDir(), Allowed: []string{"*"},
	}
	first, err := EstimateStaticContext(context.Background(), cfg, request)
	if err != nil {
		t.Fatal(err)
	}
	second, err := EstimateStaticContext(context.Background(), cfg, request)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("estimate is not deterministic:\nfirst=%#v\nsecond=%#v", first, second)
	}
	if first.Snapshot.Digest == "" || len(first.Counted) < 2 || len(first.Unknown) != 1 {
		t.Fatalf("estimate=%#v", first)
	}
	for index := 1; index < len(first.Counted); index++ {
		left, right := first.Counted[index-1], first.Counted[index]
		if left.Category > right.Category ||
			left.Category == right.Category && left.ID > right.ID {
			t.Fatalf("components are not ordered: %#v", first.Counted)
		}
	}
	if first.Unknown[0].Category != "model_tokenizer_special_tokens" {
		t.Fatalf("unknown=%#v", first.Unknown)
	}
}

func TestEstimateStaticContextMarksCLIProviderContextUnknown(t *testing.T) {
	estimate, err := EstimateStaticContext(context.Background(), Config{
		ID: "cli", Type: TypeCLI,
	}, ContextEstimateRequest{})
	if err != nil {
		t.Fatal(err)
	}
	if len(estimate.Unknown) != 2 ||
		estimate.Unknown[0].Category != "model_tokenizer_special_tokens" ||
		estimate.Unknown[1].Category != "provider_managed_context" {
		t.Fatalf("unknown=%#v", estimate.Unknown)
	}
}

func TestProviderPrepareRejectsChangedStaticContext(t *testing.T) {
	memoryFile := filepath.Join(t.TempDir(), "memory.json")
	cfg := Config{
		ID: "api", Type: TypeAPI,
		API: &APIConfig{
			Protocol: "openai", BaseURL: "https://example.test/v1",
			Model: "test", APIKey: "${TEST_KEY}",
			Runtime: &APIRuntimeConfig{
				Enabled: true, Memory: &APIMemoryConfig{Enabled: true},
			},
		},
	}
	estimateRequest := ContextEstimateRequest{
		Prompt: "execute", MemoryFile: memoryFile,
	}
	estimate, err := EstimateStaticContext(context.Background(), cfg, estimateRequest)
	if err != nil {
		t.Fatal(err)
	}
	request := Request{
		Prompt: "execute", MemoryFile: memoryFile,
		StaticContext: &estimate.Snapshot,
	}
	if _, err := (apiProvider{}).Prepare(context.Background(), cfg, request); err != nil {
		t.Fatalf("unchanged snapshot was rejected: %v", err)
	}
	memory := `[{"id":"changed","type":"fact","content":"execute changed fact","source":"test"}]`
	if err := os.WriteFile(memoryFile, []byte(memory), 0o600); err != nil {
		t.Fatal(err)
	}
	if _, err := (apiProvider{}).Prepare(context.Background(), cfg, request); err == nil ||
		!strings.Contains(err.Error(), "context_inputs_changed") {
		t.Fatalf("changed snapshot err=%v", err)
	}
}
