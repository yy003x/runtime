package cli

import (
	"bytes"
	"encoding/json"
	"errors"
	"os"
	"strings"
	"testing"

	"github.com/yy003x/runtime/agent"
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
	assertSingleV3StreamError(
		t, stdout.buffer.String(), stderr.String(),
	)
}

func TestDurableSessionSubmitDoesNotCarryAgentBudget(t *testing.T) {
	request, err := parseDurableSubmit(
		[]string{
			"--kind", "session", "--profile", "api", "hello",
		},
		agent.DefaultBudget(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if request.Kind != runtime.KindSession ||
		request.AgentBudget.MaxRounds != 0 ||
		request.AgentBudget.MaxToolCalls != 0 ||
		request.AgentBudget.MaxTotalTokens != 0 ||
		request.AgentBudget.MaxWallTime != 0 {
		t.Fatalf("request=%#v", request)
	}
}

func TestDurableSubmitRejectsDuplicateScalarOptions(t *testing.T) {
	for _, test := range []struct {
		name  string
		value string
	}{
		{"--kind", "agent"},
		{"--profile", "cx"},
		{"--session-id", "session_1"},
		{"--task-id", "task_1"},
		{"--model", "model_1"},
		{"--effort", "high"},
		{"--cwd", "work"},
	} {
		args := []string{
			test.name, test.value, test.name, test.value,
		}
		if test.name != "--profile" {
			args = append(args, "--profile", "cx")
		}
		args = append(args, "hello")
		if _, err := parseDurableSubmit(
			args, agent.DefaultBudget(),
		); err == nil || !strings.Contains(err.Error(), "only be used once") {
			t.Fatalf("option=%s error=%v", test.name, err)
		}
	}
}

func TestDurableSubmitRequiresFinalInputAndSupportsTerminator(t *testing.T) {
	if _, err := parseDurableSubmit(
		[]string{
			"--profile", "cx", "hello", "--kind", "session",
		},
		agent.DefaultBudget(),
	); err == nil || !strings.Contains(err.Error(), "final argument") {
		t.Fatalf("option after input error=%v", err)
	}
	request, err := parseDurableSubmit(
		[]string{"--profile", "cx", "--", "--leading-dash prompt"},
		agent.DefaultBudget(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if request.Input != "--leading-dash prompt" {
		t.Fatalf("request=%#v", request)
	}
	for _, args := range [][]string{
		{"--profile", "cx", "--"},
		{"--profile", "cx", "--", "one", "two"},
	} {
		if _, err := parseDurableSubmit(
			args, agent.DefaultBudget(),
		); err == nil {
			t.Fatalf("accepted terminator args=%#v", args)
		}
	}
}

func TestDurableSubmitLabelsAreRepeatableButKeysAreUnique(t *testing.T) {
	request, err := parseDurableSubmit(
		[]string{
			"--profile", "cx",
			"--label", "team=runtime", "--label", "priority=p1",
			"hello",
		},
		agent.DefaultBudget(),
	)
	if err != nil {
		t.Fatal(err)
	}
	if request.Labels["team"] != "runtime" ||
		request.Labels["priority"] != "p1" {
		t.Fatalf("labels=%#v", request.Labels)
	}
	if _, err := parseDurableSubmit(
		[]string{
			"--profile", "cx",
			"--label", "team=runtime", "--label", "team=platform",
			"hello",
		},
		agent.DefaultBudget(),
	); err == nil || !strings.Contains(err.Error(), "label key") {
		t.Fatalf("duplicate label error=%v", err)
	}
}

func TestRunManagementOptionsRejectUnknownDuplicateAndTrailingArgs(
	t *testing.T,
) {
	for _, args := range [][]string{
		{"--run-id", "run_1", "--unknown"},
		{"--run-id", "run_1", "--run-id", "run_2"},
		{"--run-id", "run_1", "trailing"},
		{"--run-id", "--apply"},
	} {
		if err := validateManagementArgs(
			args, []string{"--run-id"}, []string{"--apply"},
		); err == nil {
			t.Fatalf("accepted args=%#v", args)
		}
	}
	if err := validateManagementArgs(
		[]string{
			"--run-id", "run_1", "--after-seq", "2", "--apply",
		},
		[]string{"--run-id", "--after-seq"},
		[]string{"--apply"},
	); err != nil {
		t.Fatal(err)
	}
}

func TestRunManagementPreflightRejectsBeforeStatefulBootstrap(t *testing.T) {
	paths := prepareVNextHome(t)
	for _, args := range [][]string{
		{"unknown"},
		{
			"submit", "--profile", "cx", "hello",
			"--kind", "session",
		},
		{"list", "--state", "unknown"},
		{"list", "--state", ""},
		{"list", "--kind", "unknown"},
		{"list", "--kind", ""},
		{"list", "--limit", "1001"},
		{"list", "--limit", ""},
		{"gc", "--older-than", ""},
		{"gc", "--limit", "0"},
		{"get", "--run-id", "run_1", "trailing"},
		{"reconcile", "--run-id", ""},
		{
			"resume", "--run-id", "run_1",
			"--input-json", "", "--input-file", "resume.json",
		},
	} {
		output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
		if err := runRunNamespaceVNext(paths, args, output); err == nil {
			t.Fatalf("accepted args=%#v", args)
		}
		if _, err := os.Stat(paths.SessionsDir); !os.IsNotExist(err) {
			t.Fatalf(
				"invalid args=%#v bootstrapped Session state: %v",
				args, err,
			)
		}
		if _, err := os.Stat(paths.RunDBFile); !os.IsNotExist(err) {
			t.Fatalf(
				"invalid args=%#v opened Run database: %v",
				args, err,
			)
		}
	}
}

func TestRunManagementPreflightAcceptsBoundedFilters(t *testing.T) {
	if err := validateRunManagementInvocation([]string{
		"list", "--state", "needs_reconciliation",
		"--kind", "session", "--limit", "1000",
	}); err != nil {
		t.Fatal(err)
	}
	output := newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{})
	if err := runRunNamespaceVNext(
		prepareVNextHome(t),
		[]string{"watch", "--run-id", "--unknown"},
		output,
	); err == nil {
		t.Fatal("accepted invalid watch")
	}
	if !output.streamMode || output.streamStarted {
		t.Fatal("invalid watch did not select machine stream errors")
	}
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
