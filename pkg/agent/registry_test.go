package agent

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
)

func TestRegistryToolExecutionSnapshotIsCanonicalAndChangesOnRegister(
	t *testing.T,
) {
	registry, err := NewRegistryWithToolExecution(ToolExecutionIdentity{
		Implementation:        "test.tools",
		ImplementationVersion: 2,
		Configuration:         json.RawMessage(`{"z":2,"a":1}`),
	}, registeredSnapshotTool(
		"zeta",
		`{"required":["value"],"properties":{"value":{"type":"string"}},"type":"object"}`,
	))
	if err != nil {
		t.Fatal(err)
	}
	before := registry.ToolExecutionSnapshot()
	beforeDigest, err := before.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if err := registry.Register(registeredSnapshotTool(
		"alpha",
		`{"type":"object","additionalProperties":false}`,
	)); err != nil {
		t.Fatal(err)
	}
	after := registry.ToolExecutionSnapshot()
	afterDigest, err := after.Digest()
	if err != nil {
		t.Fatal(err)
	}
	if beforeDigest == afterDigest {
		t.Fatalf("digest did not change after Register: %s", beforeDigest)
	}
	if len(after.Definitions) != 2 ||
		after.Definitions[0].Name != "alpha" ||
		after.Definitions[1].Name != "zeta" {
		t.Fatalf("definitions are not complete and sorted: %#v", after.Definitions)
	}
	if after.SchemaVersion != ToolExecutionSnapshotSchemaVersion ||
		after.Implementation != "test.tools" ||
		after.ImplementationVersion != 2 {
		t.Fatalf("unexpected identity: %#v", after)
	}
	canonical, err := after.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	const wantConfiguration = `"configuration":{"a":1,"z":2}`
	if !json.Valid(canonical) ||
		!containsJSONFragment(canonical, wantConfiguration) {
		t.Fatalf("canonical snapshot=%s", canonical)
	}
}

func TestRegistryToolExecutionSnapshotDoesNotShareMutableStorage(t *testing.T) {
	registry, err := NewRegistryWithToolExecution(ToolExecutionIdentity{
		Implementation:        "test.tools",
		ImplementationVersion: 1,
		Configuration:         json.RawMessage(`{"root":"/workspace"}`),
	}, registeredSnapshotTool("read", `{"type":"object"}`))
	if err != nil {
		t.Fatal(err)
	}
	first := registry.ToolExecutionSnapshot()
	first.Configuration[0] = '['
	first.Definitions[0].InputSchema[0] = '['
	first.Definitions[0].Name = "mutated"

	second := registry.ToolExecutionSnapshot()
	if string(second.Configuration) != `{"root":"/workspace"}` {
		t.Fatalf("configuration mutated through snapshot: %s", second.Configuration)
	}
	if second.Definitions[0].Name != "read" ||
		string(second.Definitions[0].InputSchema) != `{"type":"object"}` {
		t.Fatalf("definition mutated through snapshot: %#v", second.Definitions[0])
	}
}

func TestRegistryToolExecutionDigestIgnoresRegistrationAndObjectKeyOrder(
	t *testing.T,
) {
	left, err := NewRegistryWithToolExecution(ToolExecutionIdentity{
		Implementation:        "test.tools",
		ImplementationVersion: 1,
		Configuration:         json.RawMessage(`{"b":2,"a":1}`),
	},
		registeredSnapshotTool(
			"beta",
			`{"properties":{"b":{"type":"string"},"a":{"type":"number"}},"type":"object"}`,
		),
		registeredSnapshotTool("alpha", `{"type":"object"}`),
	)
	if err != nil {
		t.Fatal(err)
	}
	right, err := NewRegistryWithToolExecution(ToolExecutionIdentity{
		Implementation:        "test.tools",
		ImplementationVersion: 1,
		Configuration:         json.RawMessage(`{"a":1,"b":2}`),
	},
		registeredSnapshotTool("alpha", `{"type":"object"}`),
		registeredSnapshotTool(
			"beta",
			`{"type":"object","properties":{"a":{"type":"number"},"b":{"type":"string"}}}`,
		),
	)
	if err != nil {
		t.Fatal(err)
	}
	leftDigest, err := left.ToolExecutionSnapshot().Digest()
	if err != nil {
		t.Fatal(err)
	}
	rightDigest, err := right.ToolExecutionSnapshot().Digest()
	if err != nil {
		t.Fatal(err)
	}
	if leftDigest != rightDigest {
		t.Fatalf("digests differ: left=%s right=%s", leftDigest, rightDigest)
	}
}

func registeredSnapshotTool(name string, schema string) RegisteredTool {
	return RegisteredTool{
		Definition: contract.ToolSpec{
			Name: name, InputSchema: json.RawMessage(schema),
		},
		Handler: func(context.Context, ToolRequest) (ToolResult, error) {
			return ToolResult{}, nil
		},
	}
}

func TestRegistryDefaultsAndValidatesEffectRisk(t *testing.T) {
	handler := func(context.Context, ToolRequest) (ToolResult, error) {
		return ToolResult{}, nil
	}
	for name, test := range map[string]struct {
		tool RegisteredTool
		want string
	}{
		"defaults read_only low": {RegisteredTool{
			Definition: contract.ToolSpec{
				Name: "t1", InputSchema: json.RawMessage(`{"type":"object"}`),
			}, Handler: handler,
		}, ""},
		"write_external high": {RegisteredTool{
			Definition: contract.ToolSpec{
				Name: "t2", InputSchema: json.RawMessage(`{"type":"object"}`),
			}, Handler: handler,
			Effect: contract.EffectWriteExternal, Risk: contract.RiskHigh,
		}, ""},
		"invalid effect": {RegisteredTool{
			Definition: contract.ToolSpec{
				Name: "t3", InputSchema: json.RawMessage(`{"type":"object"}`),
			}, Handler: handler, Effect: contract.Effect("nuke"),
		}, "effect must be"},
		"invalid risk": {RegisteredTool{
			Definition: contract.ToolSpec{
				Name: "t4", InputSchema: json.RawMessage(`{"type":"object"}`),
			}, Handler: handler, Risk: contract.Risk("medium"),
		}, "risk must be"},
	} {
		t.Run(name, func(t *testing.T) {
			registry, err := NewRegistry(test.tool)
			if test.want == "" {
				if err != nil {
					t.Fatalf("unexpected error: %v", err)
				}
				if registry == nil || len(registry.Definitions()) != 1 {
					t.Fatalf("registry=%#v", registry)
				}
				return
			}
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error=%v want %q", err, test.want)
			}
		})
	}
}

func containsJSONFragment(value []byte, fragment string) bool {
	for index := 0; index+len(fragment) <= len(value); index++ {
		if string(value[index:index+len(fragment)]) == fragment {
			return true
		}
	}
	return false
}
