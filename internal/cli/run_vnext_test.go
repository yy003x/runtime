package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"strings"
	"testing"

	"github.com/yy003x/runtime/contract"
	runtime "github.com/yy003x/runtime/run"
)

func TestRenderRunResultPrintsStoredAgentAnswer(t *testing.T) {
	result, err := json.Marshal(map[string]any{
		"outcome": map[string]any{
			"message": contract.Message{
				Role: contract.RoleAssistant, Content: "stored answer",
			},
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var stdout bytes.Buffer
	output := newCLIOutput(false, &stdout, &bytes.Buffer{})
	if err := renderRunResult(output, runtime.Record{
		ID: "run_2", State: runtime.StateCompleted,
		Result: result, SettledSequence: 9,
	}); err != nil {
		t.Fatal(err)
	}
	if value := stdout.String(); !strings.Contains(value, "stored answer\n") ||
		!strings.Contains(value, "settled_sequence=9") {
		t.Fatalf("output=%q", value)
	}
}

func TestRunWatchSuccessEndsWithUniqueFinal(t *testing.T) {
	fixture := executeAgentStreamFixture(t, 200, `{
	  "id":"watch-success",
	  "model":"fixture",
	  "choices":[{
	    "message":{"role":"assistant","content":"watch me"},
	    "finish_reason":"stop"
	  }]
	}`)
	if fixture.Err != nil {
		t.Fatal(fixture.Err)
	}
	agentInspection := inspectRunStream(t, fixture.Stdout.String())
	if agentInspection.FinalCount != 1 || agentInspection.Run.ID == "" {
		t.Fatalf("agent stream=%#v", agentInspection)
	}

	var stdout bytes.Buffer
	var stderr bytes.Buffer
	output := newCLIOutput(false, &stdout, &stderr)
	if err := runRunNamespaceVNext(
		fixture.Paths,
		[]string{
			"watch", "--run-id", agentInspection.Run.ID,
		},
		output,
	); err != nil {
		t.Fatal(err)
	}
	inspection := inspectRunStream(t, stdout.String())
	if inspection.EventCount == 0 || inspection.FinalCount != 1 ||
		inspection.FinalIndex != inspection.LineCount-1 ||
		inspection.Run.ID != agentInspection.Run.ID {
		t.Fatalf("inspection=%#v stdout=%q", inspection, stdout.String())
	}
	if stderr.Len() != 0 {
		t.Fatalf("stderr=%q", stderr.String())
	}
}

func TestRunWatchFailureAfterEventHasNoFinal(t *testing.T) {
	fixture := executeAgentStreamFixture(t, 200, `{
	  "id":"watch-failure",
	  "model":"fixture",
	  "choices":[{
	    "message":{"role":"assistant","content":"watch failure fixture"},
	    "finish_reason":"stop"
	  }]
	}`)
	if fixture.Err != nil {
		t.Fatal(fixture.Err)
	}
	agentInspection := inspectRunStream(t, fixture.Stdout.String())
	if agentInspection.FinalCount != 1 || agentInspection.Run.ID == "" {
		t.Fatalf("agent stream=%#v", agentInspection)
	}

	stdout := &failAfterWrites{remaining: 1}
	var stderr bytes.Buffer
	output := newCLIOutput(false, stdout, &stderr)
	err := runRunNamespaceVNext(
		fixture.Paths,
		[]string{
			"watch", "--run-id", agentInspection.Run.ID,
		},
		output,
	)
	if err == nil {
		t.Fatal("expected event consumer failure")
	}
	if !output.streamMode || !output.streamStarted {
		t.Fatalf(
			"streamMode=%t streamStarted=%t error=%v",
			output.streamMode, output.streamStarted, err,
		)
	}
	if exitCode := output.fail(err); exitCode != 1 {
		t.Fatalf("exit=%d", exitCode)
	}
	inspection := inspectRunStream(t, stdout.buffer.String())
	if inspection.EventCount != 1 || inspection.FinalCount != 0 {
		t.Fatalf(
			"inspection=%#v stdout=%q",
			inspection, stdout.buffer.String(),
		)
	}
	assertSingleV2StreamError(
		t, stdout.buffer.String(), stderr.String(),
	)
}

type failAfterWrites struct {
	buffer    bytes.Buffer
	remaining int
}

func (writer *failAfterWrites) Write(value []byte) (int, error) {
	if writer.remaining == 0 {
		return 0, errors.New("fixture output failure")
	}
	writer.remaining--
	return writer.buffer.Write(value)
}
