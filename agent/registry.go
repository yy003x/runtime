package agent

import (
	"bytes"
	"context"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"sort"
	"sync"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/strictjson"
)

const maxToolJSONBytes = 256 << 10

type ToolHandler func(context.Context, ToolRequest) (ToolResult, error)

type RegisteredTool struct {
	Definition contract.ToolSpec
	Handler    ToolHandler
}

type compiledTool struct {
	registered RegisteredTool
	schema     *jsonschema.Schema
}

type Registry struct {
	mu       sync.RWMutex
	tools    map[string]compiledTool
	identity ToolExecutionIdentity
}

func NewRegistry(values ...RegisteredTool) (*Registry, error) {
	return NewRegistryWithToolExecution(ToolExecutionIdentity{
		Implementation:        "agent.registry",
		ImplementationVersion: 1,
	}, values...)
}

func NewRegistryWithToolExecution(
	identity ToolExecutionIdentity,
	values ...RegisteredTool,
) (*Registry, error) {
	canonicalIdentity, err := canonicalToolExecutionIdentity(identity)
	if err != nil {
		return nil, err
	}
	registry := &Registry{
		tools:    make(map[string]compiledTool, len(values)),
		identity: canonicalIdentity,
	}
	for _, value := range values {
		if err := registry.Register(value); err != nil {
			return nil, err
		}
	}
	return registry, nil
}

func (registry *Registry) Register(value RegisteredTool) error {
	if registry == nil {
		return fmt.Errorf("tool registry is nil")
	}
	if err := value.Definition.Validate(); err != nil {
		return err
	}
	if value.Handler == nil {
		return fmt.Errorf("tool %q handler is required", value.Definition.Name)
	}
	canonicalSchema, err := canonicalJSONObject(value.Definition.InputSchema)
	if err != nil {
		return fmt.Errorf("tool %q input_schema: %w", value.Definition.Name, err)
	}
	value.Definition.InputSchema = canonicalSchema
	schema, err := compileInputSchema(value.Definition)
	if err != nil {
		return err
	}
	registry.mu.Lock()
	defer registry.mu.Unlock()
	if _, exists := registry.tools[value.Definition.Name]; exists {
		return fmt.Errorf("tool %q is already registered", value.Definition.Name)
	}
	registry.tools[value.Definition.Name] = compiledTool{
		registered: value,
		schema:     schema,
	}
	return nil
}

func (registry *Registry) Definitions() []contract.ToolSpec {
	if registry == nil {
		return nil
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	names := make([]string, 0, len(registry.tools))
	for name := range registry.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	values := make([]contract.ToolSpec, 0, len(names))
	for _, name := range names {
		value := registry.tools[name].registered.Definition
		value.InputSchema = append([]byte(nil), value.InputSchema...)
		values = append(values, value)
	}
	return values
}

func (registry *Registry) ToolExecutionSnapshot() ToolExecutionSnapshot {
	if registry == nil {
		return ToolExecutionSnapshot{}
	}
	registry.mu.RLock()
	defer registry.mu.RUnlock()
	names := make([]string, 0, len(registry.tools))
	for name := range registry.tools {
		names = append(names, name)
	}
	sort.Strings(names)
	definitions := make([]contract.ToolSpec, 0, len(names))
	for _, name := range names {
		definition := registry.tools[name].registered.Definition
		definition.InputSchema = append([]byte(nil), definition.InputSchema...)
		definitions = append(definitions, definition)
	}
	return ToolExecutionSnapshot{
		SchemaVersion: ToolExecutionSnapshotSchemaVersion,
		ToolExecutionIdentity: ToolExecutionIdentity{
			Implementation:        registry.identity.Implementation,
			ImplementationVersion: registry.identity.ImplementationVersion,
			Configuration: append(
				json.RawMessage(nil), registry.identity.Configuration...,
			),
		},
		Definitions: definitions,
	}
}

func (registry *Registry) Validate(request ToolRequest) error {
	if registry == nil {
		return fmt.Errorf("tool registry is unavailable")
	}
	registry.mu.RLock()
	value, exists := registry.tools[request.Name]
	registry.mu.RUnlock()
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
	if err := value.schema.Validate(arguments); err != nil {
		return fmt.Errorf("tool %q arguments do not match input_schema: %w", request.Name, err)
	}
	return nil
}

func (registry *Registry) Execute(
	ctx context.Context,
	request ToolRequest,
) (ToolResult, error) {
	if err := registry.Validate(request); err != nil {
		return ToolResult{}, err
	}
	if registry == nil {
		return ToolResult{}, fmt.Errorf("tool registry is unavailable")
	}
	registry.mu.RLock()
	value, exists := registry.tools[request.Name]
	registry.mu.RUnlock()
	if !exists {
		return ToolResult{}, fmt.Errorf("unknown tool %q", request.Name)
	}
	return value.registered.Handler(ctx, request)
}

func canonicalToolExecutionSnapshot(
	snapshot ToolExecutionSnapshot,
) ([]byte, error) {
	if snapshot.SchemaVersion != ToolExecutionSnapshotSchemaVersion {
		return nil, fmt.Errorf(
			"unsupported tool execution snapshot schema_version %d",
			snapshot.SchemaVersion,
		)
	}
	identity, err := canonicalToolExecutionIdentity(
		snapshot.ToolExecutionIdentity,
	)
	if err != nil {
		return nil, err
	}
	definitions := make([]contract.ToolSpec, len(snapshot.Definitions))
	copy(definitions, snapshot.Definitions)
	for index := range definitions {
		if err := definitions[index].Validate(); err != nil {
			return nil, fmt.Errorf("definitions[%d]: %w", index, err)
		}
		canonicalSchema, err := canonicalJSONObject(
			definitions[index].InputSchema,
		)
		if err != nil {
			return nil, fmt.Errorf(
				"definitions[%d].input_schema: %w", index, err,
			)
		}
		definitions[index].InputSchema = canonicalSchema
	}
	sort.Slice(definitions, func(left, right int) bool {
		return definitions[left].Name < definitions[right].Name
	})
	for index := 1; index < len(definitions); index++ {
		if definitions[index-1].Name == definitions[index].Name {
			return nil, fmt.Errorf(
				"duplicate tool definition %q", definitions[index].Name,
			)
		}
	}
	return json.Marshal(ToolExecutionSnapshot{
		SchemaVersion:         ToolExecutionSnapshotSchemaVersion,
		ToolExecutionIdentity: identity,
		Definitions:           definitions,
	})
}

func canonicalToolExecutionIdentity(
	identity ToolExecutionIdentity,
) (ToolExecutionIdentity, error) {
	if identity.Implementation == "" {
		return ToolExecutionIdentity{}, fmt.Errorf(
			"tool execution implementation is required",
		)
	}
	if identity.ImplementationVersion < 1 {
		return ToolExecutionIdentity{}, fmt.Errorf(
			"tool execution implementation_version must be positive",
		)
	}
	canonical := ToolExecutionIdentity{
		Implementation:        identity.Implementation,
		ImplementationVersion: identity.ImplementationVersion,
	}
	if len(identity.Configuration) > 0 {
		configuration, err := canonicalJSONObject(identity.Configuration)
		if err != nil {
			return ToolExecutionIdentity{}, fmt.Errorf(
				"tool execution configuration: %w", err,
			)
		}
		canonical.Configuration = configuration
	}
	return canonical, nil
}

func canonicalJSONObject(value json.RawMessage) (json.RawMessage, error) {
	var raw json.RawMessage
	if err := strictjson.Decode(
		bytes.NewReader(value), maxToolJSONBytes, &raw,
	); err != nil {
		return nil, err
	}
	var document any
	decoder := json.NewDecoder(bytes.NewReader(raw))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, err
	}
	if _, ok := document.(map[string]any); !ok {
		return nil, fmt.Errorf("must be a JSON object")
	}
	canonical, err := json.Marshal(document)
	if err != nil {
		return nil, err
	}
	return json.RawMessage(canonical), nil
}

func compileInputSchema(definition contract.ToolSpec) (*jsonschema.Schema, error) {
	var raw json.RawMessage
	if err := strictjson.Decode(
		bytes.NewReader(definition.InputSchema), maxToolJSONBytes, &raw,
	); err != nil {
		return nil, fmt.Errorf(
			"tool %q input_schema: %w", definition.Name, err,
		)
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return nil, fmt.Errorf(
			"tool %q input_schema: %w", definition.Name, err,
		)
	}
	sum := sha256.Sum256(append(
		append([]byte(definition.Name), 0),
		definition.InputSchema...,
	))
	resource := fmt.Sprintf("urn:sn-runtime:tool-schema:%x", sum[:])
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resource, document); err != nil {
		return nil, fmt.Errorf(
			"register tool %q input_schema: %w", definition.Name, err,
		)
	}
	schema, err := compiler.Compile(resource)
	if err != nil {
		return nil, fmt.Errorf(
			"compile tool %q input_schema: %w", definition.Name, err,
		)
	}
	return schema, nil
}
