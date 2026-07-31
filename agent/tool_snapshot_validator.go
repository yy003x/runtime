package agent

import (
	"bytes"
	"encoding/json"
	"fmt"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/strictjson"
)

// ExecutionContractVersion identifies Agent orchestration behavior that is
// not represented by the independently versioned LoopState or tool snapshot
// schemas.
const ExecutionContractVersion = 1

// ToolSnapshotValidator is a frozen, validation-only view of one tool
// execution snapshot. It deliberately has no Execute method: handlers belong
// to the current executor and cannot be reconstructed from durable metadata.
//
// All mutable input is canonicalized and copied during construction. The
// resulting definitions and compiled schemas are immutable, so Definitions
// and Validate are safe for concurrent use.
type ToolSnapshotValidator struct {
	definitions []contract.ToolSpec
	schemas     map[string]*jsonschema.Schema
}

// NewToolSnapshotValidator constructs a validator only after the complete
// snapshot has passed its canonical self-validation and every input schema has
// compiled successfully.
func NewToolSnapshotValidator(
	snapshot ToolExecutionSnapshot,
) (*ToolSnapshotValidator, error) {
	canonical, err := canonicalToolExecutionSnapshot(snapshot)
	if err != nil {
		return nil, fmt.Errorf("tool execution snapshot: %w", err)
	}

	// canonical is produced by json.Marshal after strict snapshot validation.
	// Decoding it, instead of retaining caller-owned fields, establishes the
	// validator's immutable storage boundary.
	var frozen ToolExecutionSnapshot
	if err := json.Unmarshal(canonical, &frozen); err != nil {
		return nil, fmt.Errorf(
			"decode canonical tool execution snapshot: %w", err,
		)
	}

	definitions := cloneToolDefinitions(frozen.Definitions)
	schemas := make(map[string]*jsonschema.Schema, len(definitions))
	for index := range definitions {
		schema, err := compileInputSchema(definitions[index])
		if err != nil {
			return nil, fmt.Errorf(
				"tool execution snapshot definitions[%d]: %w", index, err,
			)
		}
		schemas[definitions[index].Name] = schema
	}

	return &ToolSnapshotValidator{
		definitions: definitions,
		schemas:     schemas,
	}, nil
}

// Definitions returns a deep copy of the frozen tool definitions.
func (validator *ToolSnapshotValidator) Definitions() []contract.ToolSpec {
	if validator == nil {
		return nil
	}
	return cloneToolDefinitions(validator.definitions)
}

// Validate strictly decodes one tool request and validates its arguments
// against the schema frozen in the snapshot.
func (validator *ToolSnapshotValidator) Validate(request ToolRequest) error {
	if validator == nil {
		return fmt.Errorf("tool snapshot validator is unavailable")
	}
	schema, exists := validator.schemas[request.Name]
	if !exists {
		return fmt.Errorf("unknown tool %q", request.Name)
	}

	var raw json.RawMessage
	if err := strictjson.Decode(
		bytes.NewReader(request.Arguments), maxToolJSONBytes, &raw,
	); err != nil {
		return fmt.Errorf("tool %q arguments: %w", request.Name, err)
	}
	arguments, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("tool %q arguments: %w", request.Name, err)
	}
	if err := schema.Validate(arguments); err != nil {
		return fmt.Errorf(
			"tool %q arguments do not match input_schema: %w",
			request.Name,
			err,
		)
	}
	return nil
}

func cloneToolDefinitions(
	definitions []contract.ToolSpec,
) []contract.ToolSpec {
	if definitions == nil {
		return nil
	}
	cloned := make([]contract.ToolSpec, len(definitions))
	copy(cloned, definitions)
	for index := range cloned {
		cloned[index].InputSchema = append(
			json.RawMessage(nil),
			definitions[index].InputSchema...,
		)
	}
	return cloned
}
