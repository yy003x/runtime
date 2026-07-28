package cli

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
	"time"

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
	if err == nil {
		t.Fatal("expected unknown profile failure")
	}
	if output.streamMode || output.streamStarted {
		t.Fatalf(
			"streamMode=%t streamStarted=%t error=%v",
			output.streamMode, output.streamStarted, err,
		)
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
	assertSingleV2StreamError(
		t, fixture.Stdout.String(), fixture.Stderr.String(),
	)
}
