package cli

import (
	"bytes"
	"encoding/json"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/layout"
	runtime "github.com/yy003x/runtime/run"
)

func TestRenderAgentRunPrintsAssistantTextAndIdentity(t *testing.T) {
	result, err := json.Marshal(map[string]any{
		"outcome": map[string]any{
			"state": "completed",
			"message": map[string]any{
				"role": "assistant", "content": "agent answer",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	output := newCLIOutput(false, &stdout, &bytes.Buffer{})
	if err := renderAgentRun(output, runtime.Record{
		ID: "run_1", State: runtime.StateCompleted, Result: result,
		CreatedAt: time.Now(), UpdatedAt: time.Now(),
	}); err != nil {
		t.Fatal(err)
	}
	if value := stdout.String(); !strings.Contains(value, "agent answer\n") ||
		!strings.Contains(value, "Run run_1: completed\n") {
		t.Fatalf("output=%q", value)
	}
}

func TestAgentStreamOptionValueDoesNotSelectStreamMode(t *testing.T) {
	paths := prepareVNextHome(t)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
	err := runAgentNamespace(
		paths,
		[]string{"run", "--profile", "--stream", "input"},
		output,
	)
	if err == nil || !strings.Contains(err.Error(), "--profile requires value") {
		t.Fatalf("error=%v", err)
	}
	if output.streamMode || output.streamStarted {
		t.Fatalf(
			"streamMode=%t streamStarted=%t error=%v",
			output.streamMode, output.streamStarted, err,
		)
	}
}

func TestAgentRejectsCommandProfileBeforeStatefulBootstrap(t *testing.T) {
	paths := prepareVNextHome(t)
	writeVNextCommand(t, paths.ConfigDir, "cx")
	output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
	err := runAgentNamespace(
		paths,
		[]string{"run", "--profile", "cx", "input"},
		output,
	)
	if err == nil ||
		!strings.Contains(err.Error(), "requires an API model profile") {
		t.Fatalf("error=%v", err)
	}
	resolved, err := layout.FromHome(paths.Home)
	if err != nil {
		t.Fatal(err)
	}
	if _, statErr := os.Stat(resolved.RunDBFile); !os.IsNotExist(statErr) {
		t.Fatalf("invalid Agent request created Run database: %v", statErr)
	}
}

func TestParseAgentRunEnforcesStrictOptionAndInputGrammar(t *testing.T) {
	options, err := parseAgentRun([]string{
		"--profile", "api-agent",
		"--stream",
		"--label", "team=runtime",
		"--label", "task=review",
		"--", "--leading-input",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.profileID != "api-agent" || !options.stream ||
		options.input != "--leading-input" ||
		options.labels["team"] != "runtime" ||
		options.labels["task"] != "review" {
		t.Fatalf("options=%#v", options)
	}

	for _, args := range [][]string{
		{"--profile", "one", "--profile", "two", "input"},
		{"--profile", "api", "--stream", "--stream", "input"},
		{"--profile", "api", "--label", "task=one", "--label", "task=two", "input"},
		{"--profile", "--stream", "input"},
		{"--profile", "api", "--label", "--stream", "input"},
		{"--profile", "api", "input", "--stream"},
		{"--profile", "api", "input", "extra"},
		{"--profile", "api", "input", "--", "extra"},
		{"--profile", "api", "--", "one", "two"},
	} {
		if _, err := parseAgentRun(args); err == nil {
			t.Fatalf("args=%q returned nil error", args)
		}
	}
}

func TestParseAgentRunUsesRuntimeConfigBudgetLimits(t *testing.T) {
	options, err := parseAgentRun([]string{
		"--profile", "api-agent",
		"--max-rounds", "128",
		"--max-tool-calls", "1024",
		"--max-total-tokens", "9223372036854775807",
		"--max-wall-time", "24h",
		"input",
	})
	if err != nil {
		t.Fatal(err)
	}
	if options.maxRounds != 128 || options.maxToolCalls != 1024 ||
		options.maxTotalTokens != int64(^uint64(0)>>1) ||
		options.maxWallTime != 24*time.Hour {
		t.Fatalf("options=%#v", options)
	}

	for _, args := range [][]string{
		{"--profile", "api", "--max-rounds", "129", "input"},
		{"--profile", "api", "--max-tool-calls", "1025", "input"},
		{"--profile", "api", "--max-wall-time", "24h1ns", "input"},
		{"--profile", "api", "--max-wall-time", "500ms", "input"},
		{"--profile", "api", "--max-rounds", "--stream", "input"},
		{"--profile", "api", "--max-rounds", "1", "--max-rounds", "2", "input"},
	} {
		if _, err := parseAgentRun(args); err == nil {
			t.Fatalf("args=%q returned nil error", args)
		}
	}
}

func TestAgentStreamSuccessEndsWithUniqueFinal(t *testing.T) {
	fixture := executeAgentStreamFixture(t, 200, `{
	  "id":"agent-success",
	  "model":"fixture",
	  "choices":[{
	    "message":{"role":"assistant","content":"agent done"},
	    "finish_reason":"stop"
	  }]
	}`)
	if fixture.Err != nil {
		t.Fatal(fixture.Err)
	}
	inspection := inspectRunStream(t, fixture.Stdout.String())
	if inspection.EventCount == 0 || inspection.FinalCount != 1 ||
		inspection.FinalIndex != inspection.LineCount-1 ||
		inspection.Run.State != runtime.StateCompleted {
		t.Fatalf(
			"inspection=%#v stdout=%q",
			inspection, fixture.Stdout.String(),
		)
	}
	if fixture.Stderr.Len() != 0 {
		t.Fatalf("stderr=%q", fixture.Stderr.String())
	}
}

func TestAgentStreamFailureAfterEventsHasNoFinal(t *testing.T) {
	fixture := executeAgentStreamFixture(t, 200, `{
	  "id":"agent-failure",
	  "model":"fixture",
	  "choices":[{
	    "message":{
	      "role":"assistant",
	      "tool_calls":[{
	        "id":"call-unknown",
	        "type":"function",
	        "function":{"name":"unknown_tool","arguments":"{}"}
	      }]
	    },
	    "finish_reason":"tool_calls"
	  }]
	}`)
	if fixture.Err == nil {
		t.Fatal("expected unknown tool failure")
	}
	if !fixture.Output.streamMode || !fixture.Output.streamStarted {
		t.Fatalf(
			"streamMode=%t streamStarted=%t error=%v",
			fixture.Output.streamMode, fixture.Output.streamStarted, fixture.Err,
		)
	}
	if exitCode := fixture.Output.fail(fixture.Err); exitCode != 1 {
		t.Fatalf("exit=%d", exitCode)
	}
	inspection := inspectRunStream(t, fixture.Stdout.String())
	if inspection.EventCount == 0 || inspection.FinalCount != 0 {
		t.Fatalf(
			"inspection=%#v stdout=%q",
			inspection, fixture.Stdout.String(),
		)
	}
	assertSingleV3StreamError(
		t, fixture.Stdout.String(), fixture.Stderr.String(),
	)
}
