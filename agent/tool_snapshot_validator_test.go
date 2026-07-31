package agent

import (
	"context"
	"encoding/json"
	"reflect"
	"strings"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/yy003x/runtime/contract"
)

func TestToolSnapshotValidatorUsesFrozenSchemasWithoutExecutingHandlers(
	t *testing.T,
) {
	var handlerCalls atomic.Int64
	registry, err := NewRegistryWithToolExecution(
		ToolExecutionIdentity{
			Implementation:        "test.snapshot-validator",
			ImplementationVersion: 1,
		},
		RegisteredTool{
			Definition: contract.ToolSpec{
				Name:        "lookup",
				Description: "look up one value",
				InputSchema: json.RawMessage(`{
					"type":"object",
					"properties":{"key":{"type":"string","minLength":1}},
					"required":["key"],
					"additionalProperties":false
				}`),
			},
			Handler: func(
				context.Context,
				ToolRequest,
			) (ToolResult, error) {
				handlerCalls.Add(1)
				return ToolResult{Content: "unexpected"}, nil
			},
		},
	)
	if err != nil {
		t.Fatal(err)
	}

	validator, err := NewToolSnapshotValidator(
		registry.ToolExecutionSnapshot(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if err := validator.Validate(ToolRequest{
		Name: "lookup", Arguments: json.RawMessage(`{"key":"value"}`),
	}); err != nil {
		t.Fatalf("valid arguments were rejected: %v", err)
	}
	if err := validator.Validate(ToolRequest{
		Name: "lookup", Arguments: json.RawMessage(`{"key":""}`),
	}); err == nil || !strings.Contains(err.Error(), "input_schema") {
		t.Fatalf("schema-invalid arguments returned %v", err)
	}
	if calls := handlerCalls.Load(); calls != 0 {
		t.Fatalf("validation executed handler %d times", calls)
	}
	if _, exists := reflect.TypeOf(validator).MethodByName("Execute"); exists {
		t.Fatal("ToolSnapshotValidator unexpectedly exposes Execute")
	}
}

func TestToolSnapshotValidatorRejectsUnknownAndNonStrictArguments(
	t *testing.T,
) {
	validator := newTestToolSnapshotValidator(t)

	if err := validator.Validate(ToolRequest{
		Name: "missing", Arguments: json.RawMessage(`{}`),
	}); err == nil || !strings.Contains(err.Error(), `unknown tool "missing"`) {
		t.Fatalf("unknown tool returned %v", err)
	}
	for name, arguments := range map[string]json.RawMessage{
		"duplicate_key": json.RawMessage(`{"value":"a","value":"b"}`),
		"trailing_data": json.RawMessage(`{"value":"a"} {}`),
		"wrong_type":    json.RawMessage(`{"value":1}`),
	} {
		t.Run(name, func(t *testing.T) {
			if err := validator.Validate(ToolRequest{
				Name: "echo", Arguments: arguments,
			}); err == nil {
				t.Fatal("invalid arguments were accepted")
			}
		})
	}
}

func TestToolSnapshotValidatorOwnsDeepFrozenDefinitions(t *testing.T) {
	registry, err := NewRegistryWithToolExecution(
		ToolExecutionIdentity{
			Implementation:        "test.snapshot-validator",
			ImplementationVersion: 1,
			Configuration:         json.RawMessage(`{"root":"/workspace"}`),
		},
		testSnapshotValidatorTool(),
	)
	if err != nil {
		t.Fatal(err)
	}
	snapshot := registry.ToolExecutionSnapshot()
	validator, err := NewToolSnapshotValidator(snapshot)
	if err != nil {
		t.Fatal(err)
	}

	snapshot.Definitions[0].Name = "mutated-input"
	snapshot.Definitions[0].InputSchema[0] = '['
	snapshot.Configuration[0] = '['

	first := validator.Definitions()
	first[0].Name = "mutated-output"
	first[0].Description = "mutated-output"
	first[0].InputSchema[0] = '['

	second := validator.Definitions()
	if len(second) != 1 ||
		second[0].Name != "echo" ||
		second[0].Description != "echo one value" ||
		string(second[0].InputSchema) !=
			`{"additionalProperties":false,"properties":{"value":{"type":"string"}},"required":["value"],"type":"object"}` {
		t.Fatalf("validator definitions share mutable storage: %#v", second)
	}
	if err := validator.Validate(ToolRequest{
		Name: "echo", Arguments: json.RawMessage(`{"value":"ok"}`),
	}); err != nil {
		t.Fatalf("caller mutation changed validation: %v", err)
	}
}

func TestNewToolSnapshotValidatorRejectsTamperedSnapshot(t *testing.T) {
	base := func() ToolExecutionSnapshot {
		registry, err := NewRegistryWithToolExecution(
			ToolExecutionIdentity{
				Implementation:        "test.snapshot-validator",
				ImplementationVersion: 1,
				Configuration:         json.RawMessage(`{"mode":"test"}`),
			},
			testSnapshotValidatorTool(),
		)
		if err != nil {
			t.Fatal(err)
		}
		return registry.ToolExecutionSnapshot()
	}

	for name, mutate := range map[string]func(*ToolExecutionSnapshot){
		"schema_version": func(snapshot *ToolExecutionSnapshot) {
			snapshot.SchemaVersion++
		},
		"missing_implementation": func(snapshot *ToolExecutionSnapshot) {
			snapshot.Implementation = ""
		},
		"invalid_implementation_version": func(
			snapshot *ToolExecutionSnapshot,
		) {
			snapshot.ImplementationVersion = 0
		},
		"invalid_configuration": func(snapshot *ToolExecutionSnapshot) {
			snapshot.Configuration = json.RawMessage(`[]`)
		},
		"duplicate_definition": func(snapshot *ToolExecutionSnapshot) {
			snapshot.Definitions = append(
				snapshot.Definitions,
				snapshot.Definitions[0],
			)
		},
		"invalid_definition": func(snapshot *ToolExecutionSnapshot) {
			snapshot.Definitions[0].Name = ""
		},
		"non_object_schema": func(snapshot *ToolExecutionSnapshot) {
			snapshot.Definitions[0].InputSchema = json.RawMessage(`[]`)
		},
		"uncompilable_schema": func(snapshot *ToolExecutionSnapshot) {
			snapshot.Definitions[0].InputSchema =
				json.RawMessage(`{"type":7}`)
		},
	} {
		t.Run(name, func(t *testing.T) {
			snapshot := base()
			mutate(&snapshot)
			if _, err := NewToolSnapshotValidator(snapshot); err == nil {
				t.Fatal("tampered snapshot was accepted")
			}
		})
	}
}

func TestToolSnapshotValidatorIsConcurrentAndReturnsIndependentClones(
	t *testing.T,
) {
	validator := newTestToolSnapshotValidator(t)
	const goroutines = 32
	const iterations = 50

	var wait sync.WaitGroup
	wait.Add(goroutines)
	for worker := 0; worker < goroutines; worker++ {
		go func() {
			defer wait.Done()
			for iteration := 0; iteration < iterations; iteration++ {
				definitions := validator.Definitions()
				definitions[0].Name = "mutated"
				definitions[0].InputSchema[0] = '['
				if err := validator.Validate(ToolRequest{
					Name: "echo",
					Arguments: json.RawMessage(
						`{"value":"concurrent"}`,
					),
				}); err != nil {
					t.Errorf("concurrent Validate: %v", err)
					return
				}
			}
		}()
	}
	wait.Wait()

	definitions := validator.Definitions()
	if len(definitions) != 1 || definitions[0].Name != "echo" ||
		definitions[0].InputSchema[0] != '{' {
		t.Fatalf("concurrent mutation reached frozen state: %#v", definitions)
	}
}

func newTestToolSnapshotValidator(t *testing.T) *ToolSnapshotValidator {
	t.Helper()
	registry, err := NewRegistryWithToolExecution(
		ToolExecutionIdentity{
			Implementation:        "test.snapshot-validator",
			ImplementationVersion: 1,
		},
		testSnapshotValidatorTool(),
	)
	if err != nil {
		t.Fatal(err)
	}
	validator, err := NewToolSnapshotValidator(
		registry.ToolExecutionSnapshot(),
	)
	if err != nil {
		t.Fatal(err)
	}
	return validator
}

func testSnapshotValidatorTool() RegisteredTool {
	return RegisteredTool{
		Definition: contract.ToolSpec{
			Name:        "echo",
			Description: "echo one value",
			InputSchema: json.RawMessage(`{
				"type":"object",
				"properties":{"value":{"type":"string"}},
				"required":["value"],
				"additionalProperties":false
			}`),
		},
		Handler: func(context.Context, ToolRequest) (ToolResult, error) {
			return ToolResult{Content: "not used"}, nil
		},
	}
}
