package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"math"
	"os"
	"os/signal"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
	runtimemodel "github.com/yy003x/runtime/model"
	runtimeprofile "github.com/yy003x/runtime/profile"
	runtime "github.com/yy003x/runtime/run"
	"github.com/yy003x/runtime/session"
)

func runSessionNamespaceVNext(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: session run|submit|list|show|messages|events|logs|executions|execution|reconcile|tool-result|configure|export|delete|gc")
	}
	switch args[0] {
	case "run", "submit":
		return runSessionExecution(paths, args[0], args[1:], output)
	}
	if err := validateSessionManagementInvocation(args); err != nil {
		return err
	}
	services, err := runtimebootstrap.LoadSessionServices(paths, fixedNamespaces...)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
		if err := validateManagementArgs(
			args[1:], []string{"--state"}, nil,
		); err != nil {
			return err
		}
		state, err := optionString(args[1:], "--state")
		if err != nil {
			return err
		}
		values, err := services.Sessions.List(session.ListFilter{
			State: session.SessionState(state),
		})
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"sessions": values})
		}
		if err := output.line("Sessions (%d)", len(values)); err != nil {
			return err
		}
		for _, value := range values {
			if err := output.line(
				"  %s  %s  messages=%d",
				value.ID, value.State, value.MessageCount,
			); err != nil {
				return err
			}
		}
		return nil
	case "show":
		if err := validateManagementArgs(
			args[1:], []string{"--session-id"}, nil,
		); err != nil {
			return err
		}
		sessionID, err := requiredOption(args[1:], "--session-id")
		if err != nil {
			return err
		}
		value, err := services.Sessions.Get(sessionID)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"session": value})
		}
		return renderSessionSummary(output, value)
	case "messages":
		if err := validateManagementArgs(
			args[1:], []string{"--session-id", "--after-seq"}, nil,
		); err != nil {
			return err
		}
		sessionID, err := requiredOption(args[1:], "--session-id")
		if err != nil {
			return err
		}
		after, err := uintOption(args[1:], "--after-seq", 0)
		if err != nil {
			return err
		}
		values, err := services.Sessions.Messages(sessionID, after)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"messages": values})
		}
		if err := output.line("Messages (%d)", len(values)); err != nil {
			return err
		}
		for _, value := range values {
			content := strings.TrimSpace(value.Message.Content)
			if content == "" && len(value.Message.ToolCalls) > 0 {
				content = fmt.Sprintf("%d tool call(s)", len(value.Message.ToolCalls))
			}
			if err := output.line(
				"  [%d] %s: %s", value.Sequence, value.Message.Role, content,
			); err != nil {
				return err
			}
		}
		return nil
	case "events", "logs":
		valueOptions := []string{"--session-id", "--after-seq"}
		if args[0] == "logs" {
			valueOptions = append(valueOptions, "--tail")
		}
		if err := validateManagementArgs(
			args[1:], valueOptions, nil,
		); err != nil {
			return err
		}
		sessionID, err := requiredOption(args[1:], "--session-id")
		if err != nil {
			return err
		}
		after, err := uintOption(args[1:], "--after-seq", 0)
		if err != nil {
			return err
		}
		values, err := services.Sessions.Events(sessionID, after)
		if err != nil {
			return err
		}
		if args[0] == "logs" {
			tail, err := intOptionValue(args[1:], "--tail", 120)
			if err != nil {
				return err
			}
			if len(values) > tail {
				values = values[len(values)-tail:]
			}
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"events": values})
		}
		label := "Events"
		if args[0] == "logs" {
			label = "Logs"
		}
		if err := output.line("%s (%d)", label, len(values)); err != nil {
			return err
		}
		for _, value := range values {
			if err := output.line(
				"  [%d] %s %s", value.Sequence, value.Type, value.State,
			); err != nil {
				return err
			}
		}
		return nil
	case "executions":
		if err := validateManagementArgs(
			args[1:], []string{"--session-id"}, nil,
		); err != nil {
			return err
		}
		sessionID, err := requiredOption(args[1:], "--session-id")
		if err != nil {
			return err
		}
		values, err := services.Sessions.Executions(sessionID)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"executions": values})
		}
		if err := output.line("Executions (%d)", len(values)); err != nil {
			return err
		}
		for _, value := range values {
			if err := output.line(
				"  %s  %s  %s", value.ID, value.State, value.Outcome,
			); err != nil {
				return err
			}
		}
		return nil
	case "execution":
		if err := validateManagementArgs(
			args[1:],
			[]string{"--session-id", "--execution-id"}, nil,
		); err != nil {
			return err
		}
		sessionID, err := requiredOption(args[1:], "--session-id")
		if err != nil {
			return err
		}
		executionID, err := requiredOption(args[1:], "--execution-id")
		if err != nil {
			return err
		}
		value, err := services.Sessions.Execution(sessionID, executionID)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"execution": value})
		}
		return output.line(
			"Execution %s: state=%s outcome=%s",
			value.ID, value.State, value.Outcome,
		)
	case "reconcile":
		sessionID, reconcileOptions, err := parseSessionReconcileOptions(args[1:])
		if err != nil {
			return err
		}
		value, runtimeErr := services.Sessions.Reconcile(
			context.Background(), sessionID, reconcileOptions,
		)
		if runtimeErr != nil {
			return runtimeErr
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{
				"reconciliation": value,
			})
		}
		return output.line(
			"Session %s reconciled: turn=%s state=%s",
			value.SessionID, value.TurnID, value.State,
		)
	case "tool-result":
		return submitSessionToolResult(services.Sessions, args[1:], output)
	case "configure":
		if err := validateManagementArgs(
			args[1:], []string{"--session-id", "--retention"}, nil,
		); err != nil {
			return err
		}
		sessionID, err := requiredOption(args[1:], "--session-id")
		if err != nil {
			return err
		}
		retention, err := requiredOption(args[1:], "--retention")
		if err != nil {
			return err
		}
		value, err := services.Sessions.ConfigureRetention(
			sessionID, session.Retention(retention),
		)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"session": value})
		}
		return output.line(
			"Session %s retention=%s", value.ID, value.Retention,
		)
	case "export":
		return exportSession(services.Sessions, args[1:], output)
	case "delete":
		if err := validateManagementArgs(
			args[1:], []string{"--session-id"}, nil,
		); err != nil {
			return err
		}
		sessionID, err := requiredOption(args[1:], "--session-id")
		if err != nil {
			return err
		}
		target, err := services.Sessions.Delete(sessionID)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{
				"session_id": sessionID, "moved_to": target, "recoverable": true,
			})
		}
		return output.line(
			"Session %s moved to %s (recoverable)", sessionID, target,
		)
	case "gc":
		if err := validateManagementArgs(
			args[1:],
			[]string{"--older-than-hours", "--limit"},
			[]string{"--apply"},
		); err != nil {
			return err
		}
		olderThan, err := durationHoursOption(
			args[1:], "--older-than-hours", 24*time.Hour,
		)
		if err != nil {
			return err
		}
		limit, err := boundedIntOptionValue(
			args[1:], "--limit", 100, 1000,
		)
		if err != nil {
			return err
		}
		value, err := services.Sessions.GC(session.GCOptions{
			OlderThan: olderThan,
			Limit:     limit, Apply: hasFlag(args[1:], "--apply"),
		})
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(value)
		}
		return output.line(
			"Session GC: candidates=%d moved=%d apply=%t",
			len(value.Candidates), len(value.Moved), !value.DryRun,
		)
	default:
		return fmt.Errorf("unknown session action %q", args[0])
	}
}

func validateSessionManagementInvocation(args []string) error {
	action := args[0]
	actionArgs := args[1:]
	switch action {
	case "list":
		if err := validateManagementArgs(
			actionArgs, []string{"--state"}, nil,
		); err != nil {
			return err
		}
		state, err := optionString(actionArgs, "--state")
		if err != nil {
			return err
		}
		if state == "" && optionProvided(actionArgs, "--state") {
			return fmt.Errorf(
				"--state must be idle, active, blocked, or archived",
			)
		}
		return validateSessionStateFilter(session.SessionState(state))
	case "show", "executions", "delete":
		if err := validateManagementArgs(
			actionArgs, []string{"--session-id"}, nil,
		); err != nil {
			return err
		}
		_, err := requiredOption(actionArgs, "--session-id")
		return err
	case "messages", "events":
		if err := validateManagementArgs(
			actionArgs, []string{"--session-id", "--after-seq"}, nil,
		); err != nil {
			return err
		}
		if _, err := requiredOption(actionArgs, "--session-id"); err != nil {
			return err
		}
		_, err := uintOption(actionArgs, "--after-seq", 0)
		return err
	case "logs":
		if err := validateManagementArgs(
			actionArgs,
			[]string{"--session-id", "--after-seq", "--tail"}, nil,
		); err != nil {
			return err
		}
		if _, err := requiredOption(actionArgs, "--session-id"); err != nil {
			return err
		}
		if _, err := uintOption(actionArgs, "--after-seq", 0); err != nil {
			return err
		}
		_, err := intOptionValue(actionArgs, "--tail", 120)
		return err
	case "execution":
		if err := validateManagementArgs(
			actionArgs,
			[]string{"--session-id", "--execution-id"}, nil,
		); err != nil {
			return err
		}
		if _, err := requiredOption(actionArgs, "--session-id"); err != nil {
			return err
		}
		_, err := requiredOption(actionArgs, "--execution-id")
		return err
	case "reconcile":
		_, _, err := parseSessionReconcileOptions(actionArgs)
		return err
	case "tool-result":
		if err := validateManagementArgs(
			actionArgs,
			[]string{
				"--session-id", "--turn-id", "--tool-call-id",
				"--idempotency-key", "--content", "--content-file",
			},
			[]string{"--error"},
		); err != nil {
			return err
		}
		for _, name := range []string{
			"--session-id", "--turn-id", "--tool-call-id",
			"--idempotency-key",
		} {
			if _, err := requiredOption(actionArgs, name); err != nil {
				return err
			}
		}
		if optionProvided(actionArgs, "--content") &&
			optionProvided(actionArgs, "--content-file") {
			return fmt.Errorf(
				"--content and --content-file are mutually exclusive",
			)
		}
		return nil
	case "configure":
		if err := validateManagementArgs(
			actionArgs, []string{"--session-id", "--retention"}, nil,
		); err != nil {
			return err
		}
		if _, err := requiredOption(actionArgs, "--session-id"); err != nil {
			return err
		}
		retention, err := requiredOption(actionArgs, "--retention")
		if err != nil {
			return err
		}
		return validateSessionRetention(session.Retention(retention))
	case "export":
		if err := validateManagementArgs(
			actionArgs, []string{"--session-id", "--output"}, nil,
		); err != nil {
			return err
		}
		if _, err := requiredOption(actionArgs, "--session-id"); err != nil {
			return err
		}
		_, err := requiredOption(actionArgs, "--output")
		return err
	case "gc":
		if err := validateManagementArgs(
			actionArgs,
			[]string{"--older-than-hours", "--limit"},
			[]string{"--apply"},
		); err != nil {
			return err
		}
		if _, err := durationHoursOption(
			actionArgs, "--older-than-hours", 24*time.Hour,
		); err != nil {
			return err
		}
		_, err := boundedIntOptionValue(
			actionArgs, "--limit", 100, 1000,
		)
		return err
	default:
		return fmt.Errorf("unknown session action %q", action)
	}
}

func validateSessionStateFilter(state session.SessionState) error {
	switch state {
	case "", session.SessionIdle, session.SessionActive,
		session.SessionBlocked, session.SessionArchived:
		return nil
	default:
		return fmt.Errorf(
			"--state must be idle, active, blocked, or archived",
		)
	}
}

func validateSessionRetention(retention session.Retention) error {
	switch retention {
	case session.RetentionEphemeral, session.RetentionStandard,
		session.RetentionPinned:
		return nil
	default:
		return fmt.Errorf(
			"--retention must be ephemeral, standard, or pinned",
		)
	}
}

func parseSessionReconcileOptions(
	args []string,
) (string, session.ReconcileOptions, error) {
	var sessionID string
	var options session.ReconcileOptions
	seen := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		current := args[index]
		if seen[current] {
			return "", options, fmt.Errorf(
				"session reconcile option %s may only be used once",
				current,
			)
		}
		seen[current] = true
		switch current {
		case "--session-id":
			index++
			if index >= len(args) || strings.HasPrefix(args[index], "-") {
				return "", options, fmt.Errorf("--session-id requires value")
			}
			sessionID = args[index]
		case "--terminate":
			options.Terminate = true
		case "--acknowledge-unknown":
			options.AcknowledgeUnknown = true
		default:
			return "", options, fmt.Errorf(
				"unknown session reconcile option %s", current,
			)
		}
	}
	if sessionID == "" {
		return "", options, fmt.Errorf("--session-id is required")
	}
	if options.Terminate && options.AcknowledgeUnknown {
		return "", options, fmt.Errorf(
			"--terminate and --acknowledge-unknown are mutually exclusive",
		)
	}
	return sessionID, options, nil
}

type sessionInvocation struct {
	sessionID      string
	taskID         string
	retention      session.Retention
	profileID      string
	input          string
	model          string
	effort         string
	cwd            string
	modelOptions   contract.GenerateOptions
	tokenLimitFlag string
}

func runSessionExecution(
	paths layout.Paths,
	action string,
	args []string,
	output *cliOutput,
) error {
	invocation, err := parseSessionInvocation(args)
	if err != nil {
		return err
	}
	if action == "run" {
		callerCWD, err := os.Getwd()
		if err != nil {
			return err
		}
		if invocation.cwd != "" && !filepath.IsAbs(invocation.cwd) {
			invocation.cwd = filepath.Join(callerCWD, invocation.cwd)
		}
		services, err := runtimebootstrap.LoadSessionServices(paths, fixedNamespaces...)
		if err != nil {
			return err
		}
		if err := validateSessionProfileOptions(
			invocation, services.Profiles,
		); err != nil {
			return err
		}
		runContext, stop := signal.NotifyContext(
			context.Background(), os.Interrupt, syscall.SIGTERM,
		)
		defer stop()
		result, runtimeErr := services.Sessions.Run(
			runContext,
			session.RunRequest{
				SessionID: invocation.sessionID, TaskID: invocation.taskID,
				ProfileID: invocation.profileID, Input: invocation.input,
				Model: invocation.model, Effort: invocation.effort,
				CWD: invocation.cwd, InvocationBase: callerCWD,
				Retention:    invocation.retention,
				ModelOptions: invocation.modelOptions,
			},
		)
		if runtimeErr != nil {
			return runtimeErr
		}
		if output.JSON() {
			return output.writeJSON(result)
		}
		return renderSessionRunResult(output, result)
	}
	if invocation.sessionID == "" {
		invocation.sessionID, err = session.NewID()
		if err != nil {
			return err
		}
	}
	callerCWD, err := os.Getwd()
	if err != nil {
		return err
	}
	if invocation.cwd != "" && !filepath.IsAbs(invocation.cwd) {
		invocation.cwd = filepath.Join(callerCWD, invocation.cwd)
	}
	services, err := runtimebootstrap.LoadServices(paths, callerCWD, fixedNamespaces...)
	if err != nil {
		return err
	}
	defer services.Runs.Close()
	if err := validateSessionProfileOptions(
		invocation, services.Profiles,
	); err != nil {
		return err
	}
	record, runtimeErr := services.Runs.Submit(
		context.Background(),
		runtime.Request{
			Kind: runtime.KindSession, ProfileID: invocation.profileID,
			Input: invocation.input, SessionID: invocation.sessionID,
			SessionRetention: string(invocation.retention),
			TaskID:           invocation.taskID, Model: invocation.model,
			Effort: invocation.effort, CWD: invocation.cwd,
			InvocationBase: callerCWD,
			ModelOptions:   invocation.modelOptions,
		},
	)
	if runtimeErr != nil {
		return runtimeErr
	}
	if output.JSON() {
		return output.writeJSON(map[string]any{
			"run": record, "session_id": invocation.sessionID,
		})
	}
	return output.line(
		"Submitted run %s (session=%s, state=%s)",
		record.ID, invocation.sessionID, record.State,
	)
}

func parseSessionInvocation(args []string) (sessionInvocation, error) {
	value := sessionInvocation{retention: session.RetentionStandard}
	seen := make(map[string]bool)
	index := 0
	for index < len(args) {
		current := args[index]
		if !strings.HasPrefix(current, "-") {
			value.profileID = current
			index++
			break
		}
		if seen[current] {
			return value, fmt.Errorf("session option %s may only be used once", current)
		}
		seen[current] = true
		switch current {
		case "--session-id":
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return value, fmt.Errorf("--session-id requires value")
			}
			value.sessionID = args[index]
		case "--task-id":
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return value, fmt.Errorf("--task-id requires value")
			}
			value.taskID = args[index]
		case "--retention":
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return value, fmt.Errorf("--retention requires value")
			}
			value.retention = session.Retention(args[index])
			if err := validateSessionRetention(value.retention); err != nil {
				return value, err
			}
		case "--model":
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return value, fmt.Errorf("--model requires value")
			}
			value.model = args[index]
		case "--effort":
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return value, fmt.Errorf("--effort requires value")
			}
			if _, err := runtimecommand.ParseEffort(args[index]); err != nil {
				return value, err
			}
			value.effort = args[index]
		case "--cwd":
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return value, fmt.Errorf("--cwd requires value")
			}
			value.cwd = args[index]
		case "--max-completion-tokens", "--max-tokens":
			if value.tokenLimitFlag != "" {
				return value, fmt.Errorf(
					"%s and %s are mutually exclusive",
					value.tokenLimitFlag, current,
				)
			}
			value.tokenLimitFlag = current
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return value, fmt.Errorf("%s requires value", current)
			}
			tokenLimit, err := strconv.ParseInt(args[index], 10, 64)
			if err != nil || tokenLimit <= 0 {
				return value, fmt.Errorf("%s must be positive", current)
			}
			value.modelOptions.MaxOutputTokens = &tokenLimit
		case "--temperature":
			index++
			if index >= len(args) || args[index] == "" ||
				strings.HasPrefix(args[index], "--") {
				return value, fmt.Errorf("--temperature requires value")
			}
			current, err := strconv.ParseFloat(args[index], 64)
			if err != nil || math.IsNaN(current) || math.IsInf(current, 0) ||
				current < 0 || current > 2 {
				return value, fmt.Errorf("--temperature must be between 0 and 2")
			}
			value.modelOptions.Temperature = &current
		default:
			return value, fmt.Errorf("unknown session option %s", current)
		}
		index++
	}
	if value.profileID == "" {
		return value, fmt.Errorf("session execution requires profile ID")
	}
	if len(args[index:]) > 1 {
		return value, fmt.Errorf("session input must be one quoted argument")
	}
	stdinInput, err := readDirectStdin()
	if err != nil {
		return value, err
	}
	var positionalInput string
	if len(args[index:]) == 1 {
		positionalInput = args[index]
	}
	value.input = joinNonEmptyInput(stdinInput, positionalInput)
	if strings.TrimSpace(value.input) == "" {
		return value, fmt.Errorf("session input is required")
	}
	return value, nil
}

func joinNonEmptyInput(values ...string) string {
	var fragments []string
	for _, value := range values {
		if value != "" {
			fragments = append(fragments, value)
		}
	}
	return strings.Join(fragments, "\n")
}

func validateSessionProfileOptions(
	invocation sessionInvocation,
	profiles *runtimeprofile.Catalog,
) error {
	entry, exists := profiles.Resolve(invocation.profileID)
	if !exists {
		return fmt.Errorf("unknown profile %q", invocation.profileID)
	}
	hasModelOptions := invocation.modelOptions.MaxOutputTokens != nil ||
		invocation.modelOptions.Temperature != nil
	if entry.Kind == runtimeprofile.KindCommand {
		if hasModelOptions {
			return fmt.Errorf(
				"API model request options are invalid for CLI profile %q",
				invocation.profileID,
			)
		}
		if invocation.effort != "" {
			if _, err := runtimecommand.ParseEffort(invocation.effort); err != nil {
				return err
			}
		}
		return nil
	}
	if invocation.model != "" || invocation.effort != "" ||
		invocation.cwd != "" {
		return fmt.Errorf(
			"--model, --effort, and --cwd are invalid for API profile %q",
			invocation.profileID,
		)
	}
	if !hasModelOptions {
		return nil
	}
	if invocation.tokenLimitFlag == "" {
		return nil
	}
	expected := modelTokenLimitOption(runtimemodel.DriverName(entry.Model.Driver))
	if invocation.tokenLimitFlag != expected {
		return fmt.Errorf(
			"%s is invalid for %s; use %s",
			invocation.tokenLimitFlag, entry.Model.Driver, expected,
		)
	}
	return nil
}

func submitSessionToolResult(
	service *session.Service,
	args []string,
	output *cliOutput,
) error {
	if err := validateManagementArgs(
		args,
		[]string{
			"--session-id", "--turn-id", "--tool-call-id",
			"--idempotency-key", "--content", "--content-file",
		},
		[]string{"--error"},
	); err != nil {
		return err
	}
	sessionID, err := requiredOption(args, "--session-id")
	if err != nil {
		return err
	}
	turnID, err := requiredOption(args, "--turn-id")
	if err != nil {
		return err
	}
	callID, err := requiredOption(args, "--tool-call-id")
	if err != nil {
		return err
	}
	key, err := requiredOption(args, "--idempotency-key")
	if err != nil {
		return err
	}
	content, err := optionString(args, "--content")
	if err != nil {
		return err
	}
	contentFile, err := optionString(args, "--content-file")
	if err != nil {
		return err
	}
	if optionProvided(args, "--content") &&
		optionProvided(args, "--content-file") {
		return fmt.Errorf("--content and --content-file are mutually exclusive")
	}
	if contentFile != "" {
		content, err = readPromptFile(contentFile)
		if err != nil {
			return err
		}
	}
	receipt, runtimeErr := service.SubmitToolResult(
		sessionID, turnID,
		session.ToolResultInput{
			ToolCallID: callID, Content: content,
			IsError: hasFlag(args, "--error"), IdempotencyKey: key,
		},
	)
	if runtimeErr != nil {
		return runtimeErr
	}
	if output.JSON() {
		return output.writeJSON(receipt)
	}
	return output.line(
		"Tool result accepted: call=%s message_sequence=%d",
		receipt.ToolCallID, receipt.MessageSequence,
	)
}

func exportSession(
	service *session.Service,
	args []string,
	output *cliOutput,
) error {
	if err := validateManagementArgs(
		args, []string{"--session-id", "--output"}, nil,
	); err != nil {
		return err
	}
	sessionID, err := requiredOption(args, "--session-id")
	if err != nil {
		return err
	}
	outputPath, err := requiredOption(args, "--output")
	if err != nil {
		return err
	}
	sessionValue, err := service.Get(sessionID)
	if err != nil {
		return err
	}
	messages, err := service.Messages(sessionID, 0)
	if err != nil {
		return err
	}
	events, err := service.Events(sessionID, 0)
	if err != nil {
		return err
	}
	executions, err := service.Executions(sessionID)
	if err != nil {
		return err
	}
	if err := writeJSONFile(outputPath, map[string]any{
		"schema_version": session.SchemaVersion,
		"session":        sessionValue, "messages": messages, "events": events,
		"executions": executions,
	}); err != nil {
		return err
	}
	return renderSessionExportResult(output, sessionID, outputPath)
}

func renderSessionExportResult(
	output *cliOutput,
	sessionID string,
	outputPath string,
) error {
	if output.JSON() {
		return output.writeJSON(map[string]any{
			"session_id": sessionID,
			"output":     outputPath,
			"exported":   true,
		})
	}
	return output.line("Exported session %s to %s", sessionID, outputPath)
}

func renderSessionSummary(output *cliOutput, value session.Session) error {
	if err := output.line("Session: %s", value.ID); err != nil {
		return err
	}
	if err := output.line(
		"State: %s, retention: %s", value.State, value.Retention,
	); err != nil {
		return err
	}
	return output.line(
		"Messages: %d, events: %d", value.MessageCount, value.EventCount,
	)
}

func renderSessionRunResult(
	output *cliOutput,
	result session.RunResult,
) error {
	if result.Message != nil && strings.TrimSpace(result.Message.Content) != "" {
		if err := output.text(result.Message.Content); err != nil {
			return err
		}
	}
	if len(result.PendingActions) > 0 {
		if err := output.line(
			"Requires action: %d tool call(s)", len(result.PendingActions),
		); err != nil {
			return err
		}
		for _, call := range result.PendingActions {
			if err := output.line("  %s  %s", call.Name, call.ID); err != nil {
				return err
			}
		}
	}
	return output.line(
		"Session %s, turn %s: %s",
		result.SessionID, result.TurnID, result.State,
	)
}

func readPromptFile(path string) (string, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() > 1<<20 {
		return "", fmt.Errorf("prompt file must be regular, not a symlink, and no larger than 1048576 bytes")
	}
	value, err := os.ReadFile(path)
	return string(value), err
}

func writeJSONFile(path string, value any) error {
	path = filepath.Clean(path)
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("output must not be a symlink")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	parent := filepath.Dir(path)
	temp, err := os.CreateTemp(parent, ".sn-export-*.tmp")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	if _, err := temp.Write(append(data, '\n')); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	return os.Rename(tempPath, path)
}

func requiredOption(args []string, name string) (string, error) {
	value, err := optionString(args, name)
	if err != nil {
		return "", err
	}
	if value == "" {
		return "", fmt.Errorf("%s is required", name)
	}
	return value, nil
}

func validateManagementArgs(
	args []string,
	valueOptions []string,
	flagOptions []string,
) error {
	values := make(map[string]struct{}, len(valueOptions))
	for _, name := range valueOptions {
		values[name] = struct{}{}
	}
	flags := make(map[string]struct{}, len(flagOptions))
	for _, name := range flagOptions {
		flags[name] = struct{}{}
	}
	seen := make(map[string]bool, len(args))
	for index := 0; index < len(args); index++ {
		name := args[index]
		if seen[name] {
			return fmt.Errorf(
				"option %s may only be used once", name,
			)
		}
		if _, ok := flags[name]; ok {
			seen[name] = true
			continue
		}
		if _, ok := values[name]; !ok {
			return fmt.Errorf("unknown option %s", name)
		}
		seen[name] = true
		index++
		if index >= len(args) ||
			strings.HasPrefix(args[index], "--") {
			return fmt.Errorf("%s requires value", name)
		}
	}
	return nil
}

func optionString(args []string, name string) (string, error) {
	for index, value := range args {
		if value == name {
			if index+1 >= len(args) {
				return "", fmt.Errorf("%s requires value", name)
			}
			return args[index+1], nil
		}
	}
	return "", nil
}

func uintOption(args []string, name string, fallback uint64) (uint64, error) {
	value, err := optionString(args, name)
	if err != nil {
		return fallback, err
	}
	if value == "" {
		if optionProvided(args, name) {
			return 0, fmt.Errorf("%s requires value", name)
		}
		return fallback, nil
	}
	return strconv.ParseUint(value, 10, 64)
}

func intOptionValue(args []string, name string, fallback int) (int, error) {
	value, err := optionString(args, name)
	if err != nil {
		return fallback, err
	}
	if value == "" {
		if optionProvided(args, name) {
			return 0, fmt.Errorf("%s requires value", name)
		}
		return fallback, nil
	}
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, fmt.Errorf("%s must be a positive integer", name)
	}
	return parsed, nil
}

func boundedIntOptionValue(
	args []string,
	name string,
	fallback int,
	maximum int,
) (int, error) {
	value, err := intOptionValue(args, name, fallback)
	if err != nil {
		return 0, err
	}
	if value > maximum {
		return 0, fmt.Errorf(
			"%s must be between 1 and %d", name, maximum,
		)
	}
	return value, nil
}

func durationHoursOption(
	args []string,
	name string,
	fallback time.Duration,
) (time.Duration, error) {
	value, err := optionString(args, name)
	if err != nil {
		return fallback, err
	}
	if value == "" {
		if optionProvided(args, name) {
			return 0, fmt.Errorf("%s requires value", name)
		}
		return fallback, nil
	}
	hours, err := strconv.ParseUint(value, 10, 64)
	if err != nil || hours == 0 {
		return 0, fmt.Errorf(
			"%s must be a positive integer that fits a duration", name,
		)
	}
	duration, err := time.ParseDuration(value + "h")
	if err != nil || duration <= 0 {
		return 0, fmt.Errorf(
			"%s must be a positive integer that fits a duration", name,
		)
	}
	return duration, nil
}

func hasFlag(args []string, name string) bool {
	return optionProvided(args, name)
}

func optionProvided(args []string, name string) bool {
	for _, value := range args {
		if value == name {
			return true
		}
	}
	return false
}
