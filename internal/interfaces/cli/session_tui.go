package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/yy003x/runtime/internal/application/nativeconsole"
	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/activationgate"
	"github.com/yy003x/runtime/internal/infrastructure/executionlog"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
	runtimeprofile "github.com/yy003x/runtime/pkg/profile"
	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
	runtimetmux "github.com/yy003x/runtime/pkg/tmux"
)

type sessionNativeOpenOptions struct {
	profileID    string
	sessionID    string
	retention    session.Retention
	retentionSet bool
	model        string
	effort       string
	cwd          string
	input        string
	attach       bool
	detach       bool
}

type sessionNativeOpenResult struct {
	Session              session.Session            `json:"session"`
	Run                  runtime.Record             `json:"run"`
	Execution            runtime.NativeTUIExecution `json:"execution"`
	Window               runtimetmux.Window         `json:"tmux_window"`
	LaunchAccepted       bool                       `json:"launch_accepted"`
	InitialInputSupplied bool                       `json:"initial_input_supplied"`
}

type sessionNativeActionResult struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id,omitempty"`
	TmuxID    string `json:"tmux_id"`
	Action    string `json:"action"`
	Accepted  bool   `json:"accepted"`
}

type sessionNativeCloseAllResult struct {
	Action      string                      `json:"action"`
	Accepted    bool                        `json:"accepted"`
	ClosedCount int                         `json:"closed_count"`
	Closed      []sessionNativeActionResult `json:"closed"`
}

func runNativeTUISupervisor(args []string) error {
	if len(args) != 4 || args[0] != "--manifest" || args[1] == "" ||
		args[2] != "--digest" || args[3] == "" {
		return fmt.Errorf(
			"usage: %s --manifest <absolute-path> --digest <sha256>",
			nativeconsole.SupervisorCommand,
		)
	}
	paths, err := layout.Resolve()
	if err != nil {
		return err
	}
	if err := activationgate.RequireOpen(paths.StateDir); err != nil {
		return err
	}
	services, err := runtimebootstrap.LoadNativeConsoleServices(
		paths, fixedNamespaces...,
	)
	if err != nil {
		return err
	}
	defer services.Console.Close()
	return services.Console.Supervise(
		context.Background(), args[1], args[3],
	)
}

func runSessionNativeAction(
	paths layout.Paths,
	action string,
	args []string,
	output *cliOutput,
) error {
	manager, err := runtimebootstrap.LoadTmuxService(paths)
	if err != nil {
		return err
	}
	ctx := context.Background()
	switch action {
	case "open":
		options, err := parseSessionNativeOpenOptions(args)
		if err != nil {
			return sessionNativeRequestError(err)
		}
		if output.JSON() && options.attach {
			return sessionNativeRequestError(fmt.Errorf(
				"session open --attach is human-only",
			))
		}
		piped, err := readOptionalTmuxInput(os.Stdin)
		if err != nil {
			return err
		}
		options.input = joinNonEmptyInput(piped, options.input)
		if options.input != "" {
			options.input, err = validateSessionNativeInput(options.input)
			if err != nil {
				return sessionNativeRequestError(err)
			}
		}
		result, err := openSessionNativeTUI(ctx, paths, manager, options)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(result)
		}
		if err := output.line(
			"Native TUI Session %s opened: tmux=%s run=%s execution=%s state=%s",
			result.Session.ID, result.Window.TmuxID, result.Run.ID,
			result.Execution.ID, result.Run.State,
		); err != nil {
			return err
		}
		if !options.attach {
			return nil
		}
		return manager.Attach(ctx, result.Window.TmuxID, runtimetmux.TTYFiles{
			Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		})
	case "send":
		sessionID, positional, err := parseSessionNativeIDAndInput(args)
		if err != nil {
			return sessionNativeRequestError(err)
		}
		piped, err := readOptionalTmuxInput(os.Stdin)
		if err != nil {
			return err
		}
		input, err := validateSessionNativeInput(
			joinNonEmptyInput(piped, positional),
		)
		if err != nil {
			return sessionNativeRequestError(err)
		}
		window, err := findSessionNativeTUI(ctx, manager, sessionID)
		if err != nil {
			return err
		}
		accepted, err := manager.Send(ctx, window.TmuxID, input)
		if err != nil {
			return err
		}
		return renderSessionNativeAction(output, sessionNativeActionResult{
			SessionID: sessionID, TmuxID: window.TmuxID,
			Action: "send", Accepted: accepted.Accepted,
		})
	case "attach":
		if output.JSON() {
			return sessionNativeRequestError(fmt.Errorf(
				"session attach is human-only",
			))
		}
		sessionID, err := parseSessionNativeIDOnly(args)
		if err != nil {
			return sessionNativeRequestError(err)
		}
		window, err := findSessionNativeTUI(ctx, manager, sessionID)
		if err != nil {
			return err
		}
		return manager.Attach(ctx, window.TmuxID, runtimetmux.TTYFiles{
			Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		})
	case "interrupt":
		sessionID, err := parseSessionNativeIDOnly(args)
		if err != nil {
			return sessionNativeRequestError(err)
		}
		window, err := findSessionNativeTUI(ctx, manager, sessionID)
		if err != nil {
			return err
		}
		accepted, err := manager.Interrupt(ctx, window.TmuxID)
		if err != nil {
			return err
		}
		return renderSessionNativeAction(output, sessionNativeActionResult{
			SessionID: sessionID, TmuxID: window.TmuxID,
			Action: "interrupt", Accepted: accepted.Accepted,
		})
	case "close":
		sessionID, err := parseSessionNativeIDOnly(args)
		if err != nil {
			return sessionNativeRequestError(err)
		}
		result, err := closeSessionNativeTUILifecycle(ctx, paths, sessionID)
		if err != nil {
			return err
		}
		return renderSessionNativeAction(output, result)
	case "close-all":
		if len(args) != 0 {
			return sessionNativeRequestError(fmt.Errorf(
				"session close-all does not accept arguments",
			))
		}
		result, err := closeAllSessionNativeTUILifecycles(ctx, paths, manager)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(result)
		}
		return output.line("Native TUI Sessions closed: %d", result.ClosedCount)
	default:
		return sessionNativeRequestError(fmt.Errorf(
			"unknown native Session action %q", action,
		))
	}
}

func openSessionNativeTUI(
	ctx context.Context,
	paths layout.Paths,
	manager tmuxManager,
	options sessionNativeOpenOptions,
) (sessionNativeOpenResult, error) {
	callerCWD, err := os.Getwd()
	if err != nil {
		return sessionNativeOpenResult{}, err
	}
	if options.cwd != "" && !filepath.IsAbs(options.cwd) {
		options.cwd = filepath.Join(callerCWD, options.cwd)
	}
	services, err := runtimebootstrap.LoadNativeConsoleServicesWithTmux(
		paths, manager, fixedNamespaces...,
	)
	if err != nil {
		return sessionNativeOpenResult{}, err
	}
	entry, exists := services.Profiles.Resolve(options.profileID)
	if !exists {
		return sessionNativeOpenResult{}, cliValidationf(
			"unknown profile %q", options.profileID,
		)
	}
	if entry.Kind != runtimeprofile.KindCommand || entry.Command == nil {
		return sessionNativeOpenResult{}, cliValidationf(
			"session open requires a CLI profile; %q is an API profile",
			options.profileID,
		)
	}
	if options.sessionID == "" {
		options.sessionID, err = session.NewID()
		if err != nil {
			return sessionNativeOpenResult{}, err
		}
	}
	model := optionalString(options.model)
	effort := optionalEffort(options.effort)
	cwd := optionalString(options.cwd)
	input := optionalString(options.input)
	tmuxOptions := tmuxOpenOptions{
		profileID: options.profileID, model: model,
		effort: effort, cwd: cwd, input: input,
	}
	invocation, err := resolveTmuxOpenInvocationWithNamespace(
		services.Profiles, tmuxOptions, "", callerCWD,
		os.Environ(), paths.LogsDir, executionlog.NamespaceSession,
	)
	if err != nil {
		return sessionNativeOpenResult{}, err
	}
	opened, err := services.Console.Open(ctx, nativeConsoleOpenRequest(
		options, invocation,
	))
	if err != nil {
		_ = services.Console.Close()
		return sessionNativeOpenResult{}, err
	}
	if err := services.Console.Close(); err != nil {
		return sessionNativeOpenResult{}, err
	}
	return sessionNativeOpenResult{
		Session: opened.Session, Run: opened.Run,
		Execution: opened.Execution, Window: opened.Window,
		LaunchAccepted:       opened.LaunchAccepted,
		InitialInputSupplied: opened.InitialInputSupplied,
	}, nil
}

func closeSessionNativeTUILifecycle(
	ctx context.Context,
	paths layout.Paths,
	sessionID string,
) (sessionNativeActionResult, error) {
	services, err := runtimebootstrap.LoadNativeConsoleServices(
		paths, fixedNamespaces...,
	)
	if err != nil {
		return sessionNativeActionResult{}, err
	}
	defer services.Console.Close()
	closed, err := services.Console.CloseSession(ctx, sessionID)
	if err != nil {
		return sessionNativeActionResult{}, err
	}
	return sessionNativeActionResult{
		SessionID: closed.SessionID, RunID: closed.RunID,
		TmuxID: closed.TmuxID, Action: closed.Action,
		Accepted: closed.Accepted,
	}, nil
}

func nativeConsoleOpenRequest(
	options sessionNativeOpenOptions,
	invocation runtimetmux.Invocation,
) nativeconsole.OpenRequest {
	return nativeconsole.OpenRequest{
		SessionID: options.sessionID, Retention: options.retention,
		Target: invocation, Input: options.input,
		Model: options.model, Effort: options.effort,
		InitialInput: options.input != "",
	}
}

func closeAllSessionNativeTUILifecycles(
	ctx context.Context,
	paths layout.Paths,
	manager tmuxManager,
) (sessionNativeCloseAllResult, error) {
	result := sessionNativeCloseAllResult{
		Action: "close-all", Accepted: true,
		Closed: []sessionNativeActionResult{},
	}
	windows, err := manager.List(ctx)
	if err != nil {
		return result, err
	}
	for _, window := range windows {
		if window.Binding == nil || window.Binding.Kind != "session" {
			continue
		}
		closed, closeErr := closeSessionNativeTUILifecycle(
			ctx, paths, window.Binding.ID,
		)
		if closeErr != nil {
			return result, fmt.Errorf(
				"close native TUI Session %s after %d successful close(s): %w",
				window.Binding.ID, result.ClosedCount, closeErr,
			)
		}
		result.Closed = append(result.Closed, closed)
		result.ClosedCount++
	}
	return result, nil
}

func closeSessionNativeTUI(
	ctx context.Context,
	manager tmuxManager,
	sessionID string,
) (sessionNativeActionResult, error) {
	window, err := findSessionNativeTUI(ctx, manager, sessionID)
	if err != nil {
		return sessionNativeActionResult{}, err
	}
	accepted, err := manager.Stop(ctx, window.TmuxID)
	if err != nil {
		return sessionNativeActionResult{}, err
	}
	return sessionNativeActionResult{
		SessionID: sessionID, TmuxID: window.TmuxID,
		Action: "close", Accepted: accepted.Accepted,
	}, nil
}

func closeAllSessionNativeTUIs(
	ctx context.Context,
	manager tmuxManager,
) (sessionNativeCloseAllResult, error) {
	result := sessionNativeCloseAllResult{
		Action: "close-all", Accepted: true,
		Closed: []sessionNativeActionResult{},
	}
	windows, err := manager.List(ctx)
	if err != nil {
		return result, err
	}
	for _, window := range windows {
		if window.Binding == nil || window.Binding.Kind != "session" {
			continue
		}
		accepted, stopErr := manager.Stop(ctx, window.TmuxID)
		if stopErr != nil {
			return result, fmt.Errorf(
				"close native TUI Session %s after %d successful close(s): %w",
				window.Binding.ID, result.ClosedCount, stopErr,
			)
		}
		if !accepted.Accepted {
			return result, sessionNativeConflict(
				"native TUI Session %s close was not accepted after %d successful close(s)",
				window.Binding.ID, result.ClosedCount,
			)
		}
		result.Closed = append(result.Closed, sessionNativeActionResult{
			SessionID: window.Binding.ID, TmuxID: window.TmuxID,
			Action: "close", Accepted: true,
		})
		result.ClosedCount++
	}
	return result, nil
}

func parseSessionNativeOpenOptions(
	args []string,
) (sessionNativeOpenOptions, error) {
	result := sessionNativeOpenOptions{retention: session.RetentionStandard}
	if len(args) == 0 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return result, fmt.Errorf(
			"session open requires CLI profile ID immediately after open",
		)
	}
	result.profileID = args[0]
	seen := make(map[string]bool)
	for index := 1; index < len(args); index++ {
		current := args[index]
		if current == "--" {
			remaining := args[index+1:]
			if result.input != "" || len(remaining) > 1 {
				return result, fmt.Errorf(
					"session open accepts at most one quoted input",
				)
			}
			if len(remaining) == 1 {
				result.input = remaining[0]
			}
			break
		}
		if !strings.HasPrefix(current, "-") {
			if result.input != "" || index != len(args)-1 {
				return result, fmt.Errorf(
					"session open input must be the final argument",
				)
			}
			result.input = current
			continue
		}
		if seen[current] {
			return result, fmt.Errorf(
				"session open option %s may only be used once", current,
			)
		}
		seen[current] = true
		switch current {
		case "--attach":
			result.attach = true
			continue
		case "--detach":
			result.detach = true
			continue
		}
		index++
		if index >= len(args) || args[index] == "" {
			return result, fmt.Errorf("%s requires value", current)
		}
		switch current {
		case "--session-id":
			result.sessionID = args[index]
		case "--retention":
			result.retention = session.Retention(args[index])
			result.retentionSet = true
			if err := validateSessionRetention(result.retention); err != nil {
				return result, err
			}
		case "--model":
			result.model = args[index]
		case "--effort":
			if _, err := runtimecommand.ParseEffort(args[index]); err != nil {
				return result, err
			}
			result.effort = args[index]
		case "--cwd":
			result.cwd = args[index]
		default:
			return result, fmt.Errorf(
				"unknown session open option %s", current,
			)
		}
	}
	if result.attach && result.detach {
		return result, fmt.Errorf(
			"session open --attach and --detach are mutually exclusive",
		)
	}
	if result.sessionID != "" {
		if err := identity.Validate(result.sessionID, "session"); err != nil {
			return result, err
		}
	}
	return result, nil
}

func parseSessionNativeIDOnly(args []string) (string, error) {
	if len(args) == 1 && strings.HasPrefix(args[0], "--session-id=") {
		value := strings.TrimPrefix(args[0], "--session-id=")
		if err := identity.Validate(value, "session"); err == nil {
			return value, nil
		}
	}
	if len(args) != 2 || args[0] != "--session-id" {
		return "", fmt.Errorf("command requires exactly --session-id <id>")
	}
	if err := identity.Validate(args[1], "session"); err != nil {
		return "", err
	}
	return args[1], nil
}

func parseSessionNativeIDAndInput(args []string) (string, string, error) {
	if len(args) < 2 || args[0] != "--session-id" {
		return "", "", fmt.Errorf(
			"session send requires --session-id <id> and non-empty input",
		)
	}
	if err := identity.Validate(args[1], "session"); err != nil {
		return "", "", err
	}
	remaining := args[2:]
	if len(remaining) > 0 && remaining[0] == "--" {
		remaining = remaining[1:]
	}
	if len(remaining) > 1 {
		return "", "", fmt.Errorf(
			"session send input must be one quoted argument",
		)
	}
	positional := ""
	if len(remaining) == 1 {
		positional = remaining[0]
	}
	return args[1], positional, nil
}

func validateSessionNativeInput(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("session send requires non-empty input")
	}
	if len(value) > session.MaxInputBytes {
		return "", fmt.Errorf(
			"Session native TUI input exceeds %d bytes", session.MaxInputBytes,
		)
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf(
			"Session native TUI input must be UTF-8 without NUL",
		)
	}
	return value, nil
}

func findSessionNativeTUI(
	ctx context.Context,
	manager tmuxManager,
	sessionID string,
) (runtimetmux.Window, error) {
	value, err := findSessionNativeTUIOptional(ctx, manager, sessionID)
	if err != nil {
		return runtimetmux.Window{}, err
	}
	if value == nil {
		return runtimetmux.Window{}, &contract.RuntimeError{
			Code: contract.ErrorNotFound, Phase: contract.PhaseRequest,
			Message: fmt.Sprintf(
				"Session %s has no native TUI binding", sessionID,
			),
		}
	}
	return *value, nil
}

func findSessionNativeTUIOptional(
	ctx context.Context,
	manager tmuxManager,
	sessionID string,
) (*runtimetmux.Window, error) {
	values, err := manager.List(ctx)
	if err != nil {
		return nil, err
	}
	var found *runtimetmux.Window
	for index := range values {
		binding := values[index].Binding
		if binding == nil || binding.Kind != "session" ||
			binding.ID != sessionID {
			continue
		}
		if found != nil {
			return nil, sessionNativeConflict(
				"Session %s has multiple native TUI bindings", sessionID,
			)
		}
		current := values[index]
		found = &current
	}
	return found, nil
}

func renderSessionNativeAction(
	output *cliOutput,
	result sessionNativeActionResult,
) error {
	if output.JSON() {
		return output.writeJSON(result)
	}
	run := ""
	if result.RunID != "" {
		run = " run=" + result.RunID
	}
	return output.line(
		"Native TUI Session %s %s: accepted=%t tmux=%s%s",
		result.SessionID, result.Action, result.Accepted, result.TmuxID, run,
	)
}

func optionalString(value string) *string {
	if value == "" {
		return nil
	}
	return &value
}

func optionalEffort(value string) *runtimecommand.Effort {
	if value == "" {
		return nil
	}
	effort := runtimecommand.Effort(value)
	return &effort
}

func sessionNativeRequestError(err error) error {
	if err == nil {
		return nil
	}
	var runtimeErr *contract.RuntimeError
	if errors.As(err, &runtimeErr) {
		return err
	}
	return &contract.RuntimeError{
		Code: contract.ErrorInvalidRequest, Phase: contract.PhaseRequest,
		Message: err.Error(),
	}
}

func sessionNativeConflict(format string, args ...any) error {
	return &contract.RuntimeError{
		Code: contract.ErrorConflict, Phase: contract.PhaseTransport,
		Message: fmt.Sprintf(format, args...),
	}
}
