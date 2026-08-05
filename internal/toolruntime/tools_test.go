package toolruntime

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
)

func TestBuildCombinesToolsAndFreezesChildSnapshots(t *testing.T) {
	const secret = "resolved-secret-must-not-appear"
	t.Setenv("TOOL_RUNTIME_TEST_KEY", secret)
	leftConfiguration := json.RawMessage(
		`{"endpoint":"https://example.invalid/mcp","authorization":"Bearer ${TOOL_RUNTIME_TEST_KEY}"}`,
	)
	registry, err := Build(
		Component{
			Identity: agent.ToolExecutionIdentity{
				Implementation: "fixture.remote", ImplementationVersion: 2,
				Configuration: leftConfiguration,
			},
			Tools: []agent.RegisteredTool{fixtureTool("web_search", "search")},
		},
		Component{
			Identity: agent.ToolExecutionIdentity{
				Implementation: "fixture.local", ImplementationVersion: 3,
				Configuration: json.RawMessage(`{"root":"/workspace"}`),
			},
			Tools: []agent.RegisteredTool{fixtureTool("read_file", "file")},
		},
	)
	if err != nil {
		t.Fatal(err)
	}
	definitions := registry.Definitions()
	if len(definitions) != 2 || definitions[0].Name != "read_file" ||
		definitions[1].Name != "web_search" {
		t.Fatalf("definitions=%#v", definitions)
	}
	result, err := registry.Execute(context.Background(), agent.ToolRequest{
		Name: "web_search", Arguments: json.RawMessage(`{"value":"codex"}`),
	})
	if err != nil || result.Content != "search:codex" {
		t.Fatalf("result=%#v error=%v", result, err)
	}
	snapshot := registry.ToolExecutionSnapshot()
	if snapshot.Implementation != ExecutionImplementation ||
		snapshot.ImplementationVersion != ExecutionImplementationVersion {
		t.Fatalf("identity=%#v", snapshot.ToolExecutionIdentity)
	}
	var configuration executionConfiguration
	if err := json.Unmarshal(snapshot.Configuration, &configuration); err != nil {
		t.Fatal(err)
	}
	if configuration.SchemaVersion != configurationSchemaVersion ||
		len(configuration.Components) != 2 {
		t.Fatalf("configuration=%#v", configuration)
	}
	canonical, err := snapshot.CanonicalJSON()
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(canonical), secret) ||
		!strings.Contains(string(canonical), "${TOOL_RUNTIME_TEST_KEY}") ||
		!strings.Contains(string(canonical), `"name":"web_search"`) ||
		!strings.Contains(string(canonical), `"implementation":"fixture.local"`) {
		t.Fatalf("snapshot does not freeze safe child state: %s", canonical)
	}

	leftConfiguration[0] = '['
	again, err := snapshot.CanonicalJSON()
	if err != nil || string(again) != string(canonical) {
		t.Fatalf("snapshot changed after input mutation: %s error=%v", again, err)
	}
}

func TestBuildRejectsDuplicateToolsAcrossComponents(t *testing.T) {
	_, err := Build(
		Component{
			Identity: agent.ToolExecutionIdentity{
				Implementation: "fixture.a", ImplementationVersion: 1,
			},
			Tools: []agent.RegisteredTool{fixtureTool("same", "a")},
		},
		Component{
			Identity: agent.ToolExecutionIdentity{
				Implementation: "fixture.b", ImplementationVersion: 1,
			},
			Tools: []agent.RegisteredTool{fixtureTool("same", "b")},
		},
	)
	if err == nil || !strings.Contains(err.Error(), "already registered") {
		t.Fatalf("duplicate tool error=%v", err)
	}
}

func fixtureTool(name, prefix string) agent.RegisteredTool {
	return agent.RegisteredTool{
		Definition: contract.ToolSpec{
			Name: name, Description: name,
			InputSchema: json.RawMessage(
				`{"type":"object","properties":{"value":{"type":"string"}},"required":["value"],"additionalProperties":false}`,
			),
		},
		Handler: func(
			_ context.Context, request agent.ToolRequest,
		) (agent.ToolResult, error) {
			var input struct {
				Value string `json:"value"`
			}
			if err := json.Unmarshal(request.Arguments, &input); err != nil {
				return agent.ToolResult{}, err
			}
			return agent.ToolResult{Content: prefix + ":" + input.Value}, nil
		},
	}
}
