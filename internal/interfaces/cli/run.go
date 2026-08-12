package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
)

func runRunNamespaceVNext(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	if len(args) == 0 {
		return cliValidationf("usage: run get|list|result|trace|events|watch|cancel|resume|retry|reconcile|gc")
	}
	if args[0] == "watch" {
		output.beginStream()
	}
	var resumeInput json.RawMessage
	var err error
	if err := validateRunManagementInvocation(args); err != nil {
		return cliValidation(err)
	}
	if args[0] == "resume" {
		resumeInput, err = readResumeInput(args[1:])
		if err != nil {
			return err
		}
	}
	var (
		cwd                 string
		executionServices   *runtimebootstrap.Services
		queryServices       *runtimebootstrap.RunQueryServices
		maintenanceServices *runtimebootstrap.RunMaintenanceServices
		gcOlderThan         time.Duration
	)
	switch args[0] {
	case "get", "list", "result", "events", "watch", "trace":
		queryServices, err = runtimebootstrap.LoadRunQueryServices(paths)
		if err != nil {
			return err
		}
		defer queryServices.Runs.Close()
	case "cancel", "reconcile":
		maintenanceServices, err =
			runtimebootstrap.LoadRunMaintenanceServices(paths)
		if err != nil {
			return err
		}
		defer maintenanceServices.Runs.Close()
	case "gc":
		configured, optionErr := optionString(
			args[1:], "--older-than",
		)
		if optionErr != nil {
			return optionErr
		}
		if configured != "" {
			gcOlderThan, err = time.ParseDuration(configured)
			if err != nil {
				return err
			}
		} else {
			gcOlderThan, err =
				runtimebootstrap.LoadRunSettledRetention(paths)
			if err != nil {
				return err
			}
		}
		queryServices, err = runtimebootstrap.LoadRunQueryServices(paths)
		if err != nil {
			return err
		}
		defer queryServices.Runs.Close()
	case "resume", "retry":
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
		executionServices, err = runtimebootstrap.LoadServices(
			paths, cwd, fixedNamespaces...,
		)
		if err != nil {
			return err
		}
		defer executionServices.Runs.Close()
	default:
		return fmt.Errorf("unknown run action %q", args[0])
	}
	switch args[0] {
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
		record, err := queryServices.Runs.Get(context.Background(), runID)
		if err != nil {
			return canonicalRunManagementError(err)
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"run": record})
		}
		return renderRunRecord(output, record)
	case "list":
		filter, err := parseRunListFilter(args[1:])
		if err != nil {
			return err
		}
		records, err := queryServices.Runs.List(
			context.Background(), filter,
		)
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
		record, err := queryServices.Runs.Get(context.Background(), runID)
		if err != nil {
			return canonicalRunManagementError(err)
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{
				"run_id": runID, "state": record.State,
				"result": record.Result, "error": record.Error,
				"settled_sequence": record.SettledSequence,
			})
		}
		return renderRunResult(output, record)
	case "trace":
		if err := validateManagementArgs(
			args[1:], []string{"--run-id"}, nil,
		); err != nil {
			return err
		}
		runID, err := requiredOption(args[1:], "--run-id")
		if err != nil {
			return err
		}
		runTrace, err := queryServices.Runs.TraceByRun(context.Background(), runID)
		if err != nil {
			return canonicalRunManagementError(err)
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"trace": runTrace})
		}
		return renderRunTrace(output, runTrace)
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
		events, err := queryServices.Runs.Events(
			context.Background(), runID, after, 1000,
		)
		if err != nil {
			return canonicalRunManagementError(err)
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
		record, err := queryServices.Runs.Watch(
			context.Background(), runID, after,
			func(event contract.Event) error { return output.writeEvent(event) },
		)
		if err != nil {
			return canonicalRunManagementError(err)
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
		record, err := maintenanceServices.Runs.Cancel(
			context.Background(), runID,
		)
		if err != nil {
			return canonicalRunManagementError(err)
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
		record, err := executionServices.Runs.Resume(
			context.Background(), runID, resumeInput,
		)
		if err != nil {
			return canonicalRunManagementError(err)
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
		record, runtimeErr := executionServices.Runs.Retry(
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
		record, runtimeErr := maintenanceServices.Runs.ReconcileRun(
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
		limit, err := boundedIntOptionValue(
			args[1:], "--limit", 100, 1000,
		)
		if err != nil {
			return err
		}
		result, err := queryServices.Runs.GC(
			context.Background(),
			runtime.GCOptions{
				Before: time.Now().UTC().Add(-gcOlderThan),
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
	case "get", "result", "trace", "cancel", "retry":
		if err := validateManagementArgs(
			actionArgs, []string{"--run-id"}, nil,
		); err != nil {
			return err
		}
		runID, err := requiredOption(actionArgs, "--run-id")
		if err != nil {
			return err
		}
		return identity.Validate(runID, "run")
	case "list":
		_, err := parseRunListFilter(actionArgs)
		return err
	case "events", "watch":
		if err := validateManagementArgs(
			actionArgs, []string{"--run-id", "--after-seq"}, nil,
		); err != nil {
			return err
		}
		runID, err := requiredOption(actionArgs, "--run-id")
		if err != nil {
			return err
		}
		if err := identity.Validate(runID, "run"); err != nil {
			return err
		}
		_, err = uintOption(actionArgs, "--after-seq", 0)
		return err
	case "resume":
		if err := validateManagementArgs(
			actionArgs,
			[]string{"--run-id", "--input-json", "--input-file"}, nil,
		); err != nil {
			return err
		}
		runID, err := requiredOption(actionArgs, "--run-id")
		if err != nil {
			return err
		}
		return identity.Validate(runID, "run")
	case "reconcile":
		if len(actionArgs) != 2 || actionArgs[0] != "--run-id" ||
			actionArgs[1] == "" ||
			strings.HasPrefix(actionArgs[1], "-") {
			return fmt.Errorf(
				"run reconcile requires exactly --run-id <run-id>",
			)
		}
		return identity.Validate(actionArgs[1], "run")
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

func canonicalRunManagementError(err error) error {
	if errors.Is(err, runtime.ErrNotFound) {
		return &contract.RuntimeError{
			Code: contract.ErrorNotFound, Phase: contract.PhaseRun,
			Message: err.Error(),
		}
	}
	if errors.Is(err, runtime.ErrConflict) {
		return &contract.RuntimeError{
			Code: contract.ErrorConflict, Phase: contract.PhaseRun,
			Message: err.Error(),
		}
	}
	return err
}

func parseRunListFilter(args []string) (runtime.ListFilter, error) {
	if err := validateManagementArgs(
		args, []string{"--state", "--kind", "--limit"}, nil,
	); err != nil {
		return runtime.ListFilter{}, err
	}
	state, err := optionString(args, "--state")
	if err != nil {
		return runtime.ListFilter{}, err
	}
	if state == "" && optionProvided(args, "--state") {
		return runtime.ListFilter{}, fmt.Errorf(
			"state must be queued, running, paused, needs_reconciliation, completed, failed, or cancelled",
		)
	}
	kind, err := optionString(args, "--kind")
	if err != nil {
		return runtime.ListFilter{}, err
	}
	if kind == "" && optionProvided(args, "--kind") {
		return runtime.ListFilter{}, fmt.Errorf(
			"kind must be agent or session",
		)
	}
	limit, err := boundedIntOptionValue(
		args, "--limit", runtime.DefaultListLimit, runtime.MaxListLimit,
	)
	if err != nil {
		return runtime.ListFilter{}, err
	}
	return runtime.NormalizeListFilter(runtime.ListFilter{
		State: runtime.State(state), Kind: runtime.Kind(kind), Limit: limit,
	})
}

func renderRunTrace(output *cliOutput, trace runtime.Trace) error {
	if err := output.line(
		"Run %s  %s  %s  %s",
		trace.Run.ID, trace.Run.State, trace.Run.Request.Kind,
		trace.Run.Request.ProfileID,
	); err != nil {
		return err
	}
	if err := output.line("Events (%d)", len(trace.Events)); err != nil {
		return err
	}
	for _, event := range trace.Events {
		if err := output.line("  #%d  %s", event.Sequence, event.Type); err != nil {
			return err
		}
	}
	if err := output.line("Model calls (%d)", len(trace.ModelCalls)); err != nil {
		return err
	}
	for _, call := range trace.ModelCalls {
		if err := output.line(
			"  #%d  %s  provider=%s",
			call.Sequence, call.State, call.ProviderRequestID,
		); err != nil {
			return err
		}
	}
	if err := output.line("Tool effects (%d)", len(trace.ToolEffects)); err != nil {
		return err
	}
	for _, effect := range trace.ToolEffects {
		if err := output.line(
			"  %s  %s  %s", effect.Name, effect.CallID, effect.State,
		); err != nil {
			return err
		}
	}
	return nil
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
		return nil, cliValidationf("--input-json and --input-file are mutually exclusive")
	}
	if path != "" {
		data, err := strictjson.ReadRegularFileBytes(path, 1<<20)
		if err != nil {
			if strictjson.IsValidation(err) {
				return nil, cliValidation(err)
			}
			return nil, err
		}
		value = string(data)
	}
	if value == "" {
		return nil, cliValidationf("valid --input-json or --input-file is required")
	}
	var validated json.RawMessage
	if err := strictjson.Decode(
		strings.NewReader(value), 1<<20, &validated,
	); err != nil {
		if strictjson.IsValidation(err) {
			return nil, cliValidationf(
				"valid --input-json or --input-file is required: %v", err,
			)
		}
		return nil, err
	}
	return json.RawMessage(value), nil
}
