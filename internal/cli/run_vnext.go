package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/yy003x/runtime/agent"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
	runtime "github.com/yy003x/runtime/run"
	"github.com/yy003x/runtime/session"
)

func runRunNamespaceVNext(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: run submit|get|list|result|events|watch|cancel|resume|retry|reconcile|gc")
	}
	if args[0] == "watch" {
		output.beginStream()
	}
	var submitRequest runtime.Request
	var resumeInput json.RawMessage
	if args[0] == "submit" {
		var err error
		submitRequest, err = parseDurableSubmit(args[1:], agent.Budget{})
		if err != nil {
			return err
		}
	} else {
		if err := validateRunManagementInvocation(args); err != nil {
			return err
		}
		if args[0] == "resume" {
			var err error
			resumeInput, err = readResumeInput(args[1:])
			if err != nil {
				return err
			}
		}
	}
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	services, err := runtimebootstrap.LoadServices(paths, cwd, fixedNamespaces...)
	if err != nil {
		return err
	}
	defer services.Runs.Close()
	switch args[0] {
	case "submit":
		request := submitRequest
		if request.Kind == runtime.KindAgent {
			request.AgentBudget = services.Config.AgentBudget()
		}
		if request.Kind == runtime.KindSession && request.SessionID == "" {
			request.SessionID, err = session.NewID()
			if err != nil {
				return err
			}
		}
		if request.Kind == runtime.KindSession {
			request.InvocationBase = cwd
			if request.CWD != "" && !filepath.IsAbs(request.CWD) {
				request.CWD = filepath.Join(cwd, request.CWD)
			}
		}
		record, runtimeErr := services.Runs.Submit(context.Background(), request)
		if runtimeErr != nil {
			return runtimeErr
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"run": record})
		}
		return output.line(
			"Submitted run %s (%s, state=%s)",
			record.ID, record.Request.Kind, record.State,
		)
	case "get":
		if err := validateManagementArgs(
			args[1:], []string{"--run-id"}, nil,
		); err != nil {
			return err
		}
		runID, err := requiredOption(args[1:], "--run-id")
		if err != nil {
			return err
		}
		record, err := services.Runs.Get(context.Background(), runID)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"run": record})
		}
		return renderRunRecord(output, record)
	case "list":
		if err := validateManagementArgs(
			args[1:], []string{"--state", "--kind", "--limit"}, nil,
		); err != nil {
			return err
		}
		state, err := optionString(args[1:], "--state")
		if err != nil {
			return err
		}
		kind, err := optionString(args[1:], "--kind")
		if err != nil {
			return err
		}
		limit, err := boundedIntOptionValue(
			args[1:], "--limit", 100, 1000,
		)
		if err != nil {
			return err
		}
		records, err := services.Runs.List(context.Background(), runtime.ListFilter{
			State: runtime.State(state), Kind: runtime.Kind(kind), Limit: limit,
		})
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"runs": records})
		}
		if err := output.line("Runs (%d)", len(records)); err != nil {
			return err
		}
		for _, record := range records {
			if err := output.line(
				"  %s  %s  %s  %s",
				record.ID, record.State, record.Request.Kind,
				record.Request.ProfileID,
			); err != nil {
				return err
			}
		}
		return nil
	case "result":
		if err := validateManagementArgs(
			args[1:], []string{"--run-id"}, nil,
		); err != nil {
			return err
		}
		runID, err := requiredOption(args[1:], "--run-id")
		if err != nil {
			return err
		}
		record, err := services.Runs.Get(context.Background(), runID)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{
				"run_id": runID, "state": record.State,
				"result": record.Result, "error": record.Error,
				"settled_sequence": record.SettledSequence,
			})
		}
		return renderRunResult(output, record)
	case "events":
		if err := validateManagementArgs(
			args[1:], []string{"--run-id", "--after-seq"}, nil,
		); err != nil {
			return err
		}
		runID, err := requiredOption(args[1:], "--run-id")
		if err != nil {
			return err
		}
		after, err := uintOption(args[1:], "--after-seq", 0)
		if err != nil {
			return err
		}
		events, err := services.Runs.Events(
			context.Background(), runID, after, 1000,
		)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"events": events})
		}
		if err := output.line("Run events (%d)", len(events)); err != nil {
			return err
		}
		for _, event := range events {
			if err := output.line(
				"  [%d] %s", event.Sequence, event.Type,
			); err != nil {
				return err
			}
		}
		return nil
	case "watch":
		if err := validateManagementArgs(
			args[1:], []string{"--run-id", "--after-seq"}, nil,
		); err != nil {
			return err
		}
		runID, err := requiredOption(args[1:], "--run-id")
		if err != nil {
			return err
		}
		after, err := uintOption(args[1:], "--after-seq", 0)
		if err != nil {
			return err
		}
		record, err := services.Runs.Watch(
			context.Background(), runID, after,
			func(event contract.Event) error { return output.writeEvent(event) },
		)
		if err != nil {
			return err
		}
		return output.writeFinal(map[string]any{"run": record})
	case "cancel":
		if err := validateManagementArgs(
			args[1:], []string{"--run-id"}, nil,
		); err != nil {
			return err
		}
		runID, err := requiredOption(args[1:], "--run-id")
		if err != nil {
			return err
		}
		record, err := services.Runs.Cancel(context.Background(), runID)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"run": record})
		}
		return output.line("Run %s: cancellation requested", record.ID)
	case "resume":
		if err := validateManagementArgs(
			args[1:],
			[]string{"--run-id", "--input-json", "--input-file"}, nil,
		); err != nil {
			return err
		}
		runID, err := requiredOption(args[1:], "--run-id")
		if err != nil {
			return err
		}
		record, err := services.Runs.Resume(
			context.Background(), runID, resumeInput,
		)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"run": record})
		}
		return output.line("Run %s resumed (state=%s)", record.ID, record.State)
	case "retry":
		if err := validateManagementArgs(
			args[1:], []string{"--run-id"}, nil,
		); err != nil {
			return err
		}
		runID, err := requiredOption(args[1:], "--run-id")
		if err != nil {
			return err
		}
		record, runtimeErr := services.Runs.Retry(
			context.Background(), runID,
		)
		if runtimeErr != nil {
			return runtimeErr
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"run": record})
		}
		return output.line(
			"Retry submitted: %s (retry_of=%s)", record.ID, runID,
		)
	case "reconcile":
		if len(args) != 3 || args[1] != "--run-id" ||
			strings.HasPrefix(args[2], "-") {
			return fmt.Errorf(
				"run reconcile requires exactly --run-id <run-id>",
			)
		}
		runID := args[2]
		record, runtimeErr := services.Runs.ReconcileRun(
			context.Background(), runID,
		)
		if runtimeErr != nil {
			return runtimeErr
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"run": record})
		}
		return output.line(
			"Run %s reconciled (state=%s)", record.ID, record.State,
		)
	case "gc":
		if err := validateManagementArgs(
			args[1:], []string{"--older-than", "--limit"},
			[]string{"--apply"},
		); err != nil {
			return err
		}
		olderThan := services.Config.SettledRetention()
		configured, err := optionString(args[1:], "--older-than")
		if err != nil {
			return err
		}
		if configured != "" {
			olderThan, err = time.ParseDuration(configured)
			if err != nil || olderThan < time.Hour {
				return fmt.Errorf("--older-than must be a duration of at least 1h")
			}
		}
		limit, err := boundedIntOptionValue(
			args[1:], "--limit", 100, 1000,
		)
		if err != nil {
			return err
		}
		result, err := services.Runs.GC(
			context.Background(),
			runtime.GCOptions{
				Before: time.Now().UTC().Add(-olderThan),
				Limit:  limit, Apply: hasFlag(args[1:], "--apply"),
			},
		)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(result)
		}
		return output.line(
			"Run GC: candidates=%d deleted=%d apply=%t",
			len(result.Candidates), len(result.Deleted), result.Applied,
		)
	default:
		return fmt.Errorf("unknown run action %q", args[0])
	}
}

func validateRunManagementInvocation(args []string) error {
	action := args[0]
	actionArgs := args[1:]
	switch action {
	case "get", "result", "cancel", "retry":
		if err := validateManagementArgs(
			actionArgs, []string{"--run-id"}, nil,
		); err != nil {
			return err
		}
		_, err := requiredOption(actionArgs, "--run-id")
		return err
	case "list":
		if err := validateManagementArgs(
			actionArgs, []string{"--state", "--kind", "--limit"}, nil,
		); err != nil {
			return err
		}
		state, err := optionString(actionArgs, "--state")
		if err != nil {
			return err
		}
		if state == "" && optionProvided(actionArgs, "--state") {
			return fmt.Errorf(
				"--state must be queued, running, paused, needs_reconciliation, completed, failed, or cancelled",
			)
		}
		if err := validateRunStateFilter(runtime.State(state)); err != nil {
			return err
		}
		kind, err := optionString(actionArgs, "--kind")
		if err != nil {
			return err
		}
		if kind == "" && optionProvided(actionArgs, "--kind") {
			return fmt.Errorf("--kind must be agent or session")
		}
		if err := validateRunKindFilter(runtime.Kind(kind)); err != nil {
			return err
		}
		_, err = boundedIntOptionValue(
			actionArgs, "--limit", 100, 1000,
		)
		return err
	case "events", "watch":
		if err := validateManagementArgs(
			actionArgs, []string{"--run-id", "--after-seq"}, nil,
		); err != nil {
			return err
		}
		if _, err := requiredOption(actionArgs, "--run-id"); err != nil {
			return err
		}
		_, err := uintOption(actionArgs, "--after-seq", 0)
		return err
	case "resume":
		if err := validateManagementArgs(
			actionArgs,
			[]string{"--run-id", "--input-json", "--input-file"}, nil,
		); err != nil {
			return err
		}
		if _, err := requiredOption(actionArgs, "--run-id"); err != nil {
			return err
		}
		return nil
	case "reconcile":
		if len(actionArgs) != 2 || actionArgs[0] != "--run-id" ||
			actionArgs[1] == "" ||
			strings.HasPrefix(actionArgs[1], "-") {
			return fmt.Errorf(
				"run reconcile requires exactly --run-id <run-id>",
			)
		}
		return nil
	case "gc":
		if err := validateManagementArgs(
			actionArgs, []string{"--older-than", "--limit"},
			[]string{"--apply"},
		); err != nil {
			return err
		}
		configured, err := optionString(actionArgs, "--older-than")
		if err != nil {
			return err
		}
		if configured == "" &&
			optionProvided(actionArgs, "--older-than") {
			return fmt.Errorf(
				"--older-than must be a duration of at least 1h",
			)
		}
		if configured != "" {
			olderThan, parseErr := time.ParseDuration(configured)
			if parseErr != nil || olderThan < time.Hour {
				return fmt.Errorf(
					"--older-than must be a duration of at least 1h",
				)
			}
		}
		_, err = boundedIntOptionValue(
			actionArgs, "--limit", 100, 1000,
		)
		return err
	default:
		return fmt.Errorf("unknown run action %q", action)
	}
}

func validateRunStateFilter(state runtime.State) error {
	switch state {
	case "", runtime.StateQueued, runtime.StateRunning, runtime.StatePaused,
		runtime.StateNeedsReconciliation, runtime.StateCompleted,
		runtime.StateFailed, runtime.StateCancelled:
		return nil
	default:
		return fmt.Errorf(
			"--state must be queued, running, paused, needs_reconciliation, completed, failed, or cancelled",
		)
	}
}

func validateRunKindFilter(kind runtime.Kind) error {
	switch kind {
	case "", runtime.KindAgent, runtime.KindSession:
		return nil
	default:
		return fmt.Errorf("--kind must be agent or session")
	}
}

func renderRunRecord(output *cliOutput, record runtime.Record) error {
	if err := output.line("Run: %s", record.ID); err != nil {
		return err
	}
	if err := output.line(
		"State: %s, kind: %s, profile: %s",
		record.State, record.Request.Kind, record.Request.ProfileID,
	); err != nil {
		return err
	}
	if record.Request.SessionID != "" {
		if err := output.line("Session: %s", record.Request.SessionID); err != nil {
			return err
		}
	}
	if record.Error != nil {
		return output.line(
			"Failure: %s: %s", record.Error.Code, record.Error.Message,
		)
	}
	return nil
}

func renderRunResult(output *cliOutput, record runtime.Record) error {
	printedMessage := false
	var sessionResult session.RunResult
	if len(record.Result) > 0 &&
		json.Unmarshal(record.Result, &sessionResult) == nil &&
		sessionResult.SessionID != "" {
		if sessionResult.Message != nil &&
			strings.TrimSpace(sessionResult.Message.Content) != "" {
			if err := output.text(sessionResult.Message.Content); err != nil {
				return err
			}
			printedMessage = true
		}
	}
	if !printedMessage && len(record.Result) > 0 {
		var agentResult struct {
			Outcome struct {
				Message *contract.Message `json:"message"`
			} `json:"outcome"`
		}
		if json.Unmarshal(record.Result, &agentResult) == nil &&
			agentResult.Outcome.Message != nil &&
			strings.TrimSpace(agentResult.Outcome.Message.Content) != "" {
			if err := output.text(agentResult.Outcome.Message.Content); err != nil {
				return err
			}
		}
	}
	if record.Error != nil {
		if err := output.line(
			"Error: %s: %s", record.Error.Code, record.Error.Message,
		); err != nil {
			return err
		}
	}
	return output.line(
		"Run %s: %s (settled_sequence=%d)",
		record.ID, record.State, record.SettledSequence,
	)
}

func parseDurableSubmit(
	args []string,
	defaultBudget agent.Budget,
) (runtime.Request, error) {
	request := runtime.Request{
		Kind: runtime.KindAgent, Labels: make(map[string]string),
		AgentBudget: defaultBudget,
	}
	seen := make(map[string]bool)
	inputSet := false
	for index := 0; index < len(args); index++ {
		current := args[index]
		if current == "--" {
			if index+2 != len(args) {
				return request, fmt.Errorf(
					"run submit -- must be followed by exactly one input argument",
				)
			}
			request.Input = args[index+1]
			inputSet = true
			break
		}
		if !strings.HasPrefix(current, "-") {
			if index != len(args)-1 {
				return request, fmt.Errorf(
					"run submit input must be the final argument",
				)
			}
			request.Input = current
			inputSet = true
			break
		}
		switch current {
		case "--kind":
			if seen[current] {
				return request, fmt.Errorf(
					"run submit option %s may only be used once", current,
				)
			}
			seen[current] = true
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return request, fmt.Errorf("--kind requires value")
			}
			request.Kind = runtime.Kind(args[index])
		case "--profile":
			if seen[current] {
				return request, fmt.Errorf(
					"run submit option %s may only be used once", current,
				)
			}
			seen[current] = true
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return request, fmt.Errorf("--profile requires value")
			}
			request.ProfileID = args[index]
		case "--session-id":
			if seen[current] {
				return request, fmt.Errorf(
					"run submit option %s may only be used once", current,
				)
			}
			seen[current] = true
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return request, fmt.Errorf("--session-id requires value")
			}
			request.SessionID = args[index]
		case "--task-id":
			if seen[current] {
				return request, fmt.Errorf(
					"run submit option %s may only be used once", current,
				)
			}
			seen[current] = true
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return request, fmt.Errorf("--task-id requires value")
			}
			request.TaskID = args[index]
		case "--model":
			if seen[current] {
				return request, fmt.Errorf(
					"run submit option %s may only be used once", current,
				)
			}
			seen[current] = true
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return request, fmt.Errorf("--model requires value")
			}
			request.Model = args[index]
		case "--effort":
			if seen[current] {
				return request, fmt.Errorf(
					"run submit option %s may only be used once", current,
				)
			}
			seen[current] = true
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return request, fmt.Errorf("--effort requires value")
			}
			request.Effort = args[index]
		case "--cwd":
			if seen[current] {
				return request, fmt.Errorf(
					"run submit option %s may only be used once", current,
				)
			}
			seen[current] = true
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return request, fmt.Errorf("--cwd requires value")
			}
			request.CWD = args[index]
		case "--label":
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return request, fmt.Errorf("--label requires key=value")
			}
			key, value, exists := strings.Cut(args[index], "=")
			if !exists || key == "" {
				return request, fmt.Errorf("--label requires key=value")
			}
			if _, exists := request.Labels[key]; exists {
				return request, fmt.Errorf(
					"--label key %q may only be used once", key,
				)
			}
			request.Labels[key] = value
		default:
			return request, fmt.Errorf(
				"unknown run submit option %s", current,
			)
		}
	}
	if request.ProfileID == "" || !inputSet ||
		strings.TrimSpace(request.Input) == "" {
		return request, fmt.Errorf("run submit requires --profile and one quoted input")
	}
	if request.Kind != runtime.KindAgent && request.Kind != runtime.KindSession {
		return request, fmt.Errorf("--kind must be agent or session")
	}
	if request.Kind == runtime.KindSession {
		request.AgentBudget = agent.Budget{}
	}
	return request, nil
}

func readResumeInput(args []string) (json.RawMessage, error) {
	value, err := optionString(args, "--input-json")
	if err != nil {
		return nil, err
	}
	path, err := optionString(args, "--input-file")
	if err != nil {
		return nil, err
	}
	if optionProvided(args, "--input-json") &&
		optionProvided(args, "--input-file") {
		return nil, fmt.Errorf("--input-json and --input-file are mutually exclusive")
	}
	if path != "" {
		info, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
			info.Size() > 1<<20 {
			return nil, fmt.Errorf("resume input file must be a regular file no larger than 1048576 bytes")
		}
		data, err := os.ReadFile(path)
		if err != nil {
			return nil, err
		}
		value = string(data)
	}
	if value == "" || !json.Valid([]byte(value)) {
		return nil, fmt.Errorf("valid --input-json or --input-file is required")
	}
	return json.RawMessage(value), nil
}
