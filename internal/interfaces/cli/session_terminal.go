package cli

import (
	"bufio"
	"context"
	"crypto/sha256"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/signal"
	"path/filepath"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/domain/profileid"
	"github.com/yy003x/runtime/internal/infrastructure/activationgate"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
	runtimetmux "github.com/yy003x/runtime/pkg/tmux"
)

const (
	sessionTerminalHelperCommandName = "__sn_session_terminal_helper"
	sessionTerminalInputPrefix       = "__SN_SESSION_INPUT_V1__:"
	sessionTerminalControlPrefix     = "__SN_SESSION_CONTROL_V1__:"
	sessionTerminalCloseFrame        = sessionTerminalControlPrefix + "close"
	sessionTerminalFrameLimit        = 2 << 20
	sessionTerminalCloseTimeout      = 15 * time.Second
)

type sessionTerminalOpenOptions struct {
	profileID    string
	sessionID    string
	retention    session.Retention
	retentionSet bool
	model        string
	effort       string
	cwd          string
	input        string
}

type sessionTerminalHelperConfig struct {
	SessionID        string            `json:"session_id"`
	ProfileID        string            `json:"profile_id"`
	Retention        session.Retention `json:"retention"`
	Model            string            `json:"model,omitempty"`
	Effort           string            `json:"effort,omitempty"`
	CWD              string            `json:"cwd"`
	InvocationBase   string            `json:"invocation_base"`
	ConfigDigest     string            `json:"config_digest"`
	BasePromptDigest string            `json:"base_prompt_digest,omitempty"`
}

type sessionTerminalOpenResult struct {
	Session         session.Session    `json:"session"`
	Window          runtimetmux.Window `json:"tmux_window"`
	LaunchAccepted  bool               `json:"launch_accepted"`
	InitialAccepted bool               `json:"initial_input_accepted"`
}

type sessionTerminalActionResult struct {
	SessionID string `json:"session_id"`
	TmuxID    string `json:"tmux_id"`
	RunID     string `json:"run_id,omitempty"`
	Action    string `json:"action"`
	Accepted  bool   `json:"accepted"`
}

func runSessionTerminalAction(
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
		options, err := parseSessionTerminalOpenOptions(args)
		if err != nil {
			return sessionTerminalRequestError(err)
		}
		piped, err := readOptionalTmuxInput(os.Stdin)
		if err != nil {
			return err
		}
		options.input = joinNonEmptyInput(piped, options.input)
		if options.input != "" {
			options.input, err = mergeSessionTerminalInput("", options.input)
			if err != nil {
				return sessionTerminalRequestError(err)
			}
		}
		result, err := openSessionTerminal(ctx, paths, manager, options)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(result)
		}
		if err := output.line(
			"Session %s terminal opened: tmux=%s state=%s",
			result.Session.ID, result.Window.TmuxID, result.Window.State,
		); err != nil {
			return err
		}
		if result.InitialAccepted {
			return output.line("Initial input accepted; completion is recorded by its Run")
		}
		return nil
	case "send":
		sessionID, positional, err := parseSessionTerminalIDAndInput(args)
		if err != nil {
			return sessionTerminalRequestError(err)
		}
		piped, err := readOptionalTmuxInput(os.Stdin)
		if err != nil {
			return err
		}
		input, err := mergeSessionTerminalInput(piped, positional)
		if err != nil {
			return sessionTerminalRequestError(err)
		}
		window, err := findSessionTerminal(ctx, manager, sessionID)
		if err != nil {
			return err
		}
		frame, err := encodeSessionTerminalInput(input)
		if err != nil {
			return sessionTerminalRequestError(err)
		}
		accepted, err := manager.SendFramed(ctx, window.TmuxID, frame)
		if err != nil {
			return err
		}
		return renderSessionTerminalAction(output, sessionTerminalActionResult{
			SessionID: sessionID, TmuxID: window.TmuxID,
			Action: "send", Accepted: accepted.Accepted,
		})
	case "attach":
		if output.JSON() {
			return sessionTerminalRequestError(fmt.Errorf("session attach is human-only"))
		}
		sessionID, err := parseSessionTerminalIDOnly(args)
		if err != nil {
			return sessionTerminalRequestError(err)
		}
		window, err := findSessionTerminal(ctx, manager, sessionID)
		if err != nil {
			return err
		}
		return manager.Attach(ctx, window.TmuxID, runtimetmux.TTYFiles{
			Stdin: os.Stdin, Stdout: os.Stdout, Stderr: os.Stderr,
		})
	case "interrupt":
		sessionID, err := parseSessionTerminalIDOnly(args)
		if err != nil {
			return sessionTerminalRequestError(err)
		}
		window, err := findSessionTerminal(ctx, manager, sessionID)
		if err != nil {
			return err
		}
		record, found, err := cancelActiveSessionTerminalRun(ctx, paths, sessionID)
		if err != nil {
			return err
		}
		if !found {
			return sessionTerminalConflict("Session %s has no active terminal Run", sessionID)
		}
		return renderSessionTerminalAction(output, sessionTerminalActionResult{
			SessionID: sessionID, TmuxID: window.TmuxID, RunID: record.ID,
			Action: "interrupt", Accepted: true,
		})
	case "close":
		sessionID, err := parseSessionTerminalIDOnly(args)
		if err != nil {
			return sessionTerminalRequestError(err)
		}
		window, err := findSessionTerminal(ctx, manager, sessionID)
		if err != nil {
			return err
		}
		if window.State == runtimetmux.StateRunning {
			if _, err := manager.SendFramed(
				ctx, window.TmuxID, sessionTerminalCloseFrame,
			); err != nil {
				return err
			}
		}
		record, found, err := cancelActiveSessionTerminalRun(ctx, paths, sessionID)
		if err != nil {
			return err
		}
		if found {
			if err := waitSessionTerminalRunSettled(
				ctx, paths, sessionID, record.ID, sessionTerminalCloseTimeout,
			); err != nil {
				return err
			}
		}
		if window.State != runtimetmux.StateExited {
			if err := waitSessionTerminalWindowExited(
				ctx, manager, window.TmuxID, sessionTerminalCloseTimeout,
			); err != nil {
				return err
			}
		}
		accepted, err := manager.Stop(ctx, window.TmuxID)
		if err != nil {
			return err
		}
		result := sessionTerminalActionResult{
			SessionID: sessionID, TmuxID: window.TmuxID,
			Action: "close", Accepted: accepted.Accepted,
		}
		if found {
			result.RunID = record.ID
		}
		return renderSessionTerminalAction(output, result)
	default:
		return sessionTerminalRequestError(fmt.Errorf(
			"unknown Session terminal action %q", action,
		))
	}
}

func openSessionTerminal(
	ctx context.Context,
	paths layout.Paths,
	manager sessionTerminalTmuxManager,
	options sessionTerminalOpenOptions,
) (sessionTerminalOpenResult, error) {
	callerCWD, err := os.Getwd()
	if err != nil {
		return sessionTerminalOpenResult{}, err
	}
	if options.cwd != "" && !filepath.IsAbs(options.cwd) {
		options.cwd = filepath.Join(callerCWD, options.cwd)
	}
	services, err := runtimebootstrap.LoadSessionServices(paths, fixedNamespaces...)
	if err != nil {
		return sessionTerminalOpenResult{}, err
	}
	if err := validateSessionProfileOptions(
		"exec",
		sessionInvocation{
			profileID: options.profileID, model: options.model,
			effort: options.effort, cwd: options.cwd,
		},
		services.Profiles,
	); err != nil {
		return sessionTerminalOpenResult{}, err
	}
	newSession := options.sessionID == ""
	if newSession {
		options.sessionID, err = session.NewID()
		if err != nil {
			return sessionTerminalOpenResult{}, err
		}
	} else {
		value, getErr := services.Sessions.Get(options.sessionID)
		if getErr != nil {
			return sessionTerminalOpenResult{}, canonicalSessionResourceError(
				getErr, "session", options.sessionID,
			)
		}
		if value.State != session.SessionIdle || value.ActiveTurnID != "" {
			return sessionTerminalOpenResult{}, sessionTerminalConflict(
				"Session %s must be idle before opening a terminal", options.sessionID,
			)
		}
		if options.retentionSet && value.Retention != options.retention {
			return sessionTerminalOpenResult{}, sessionTerminalConflict(
				"Session %s retention is %s, not %s",
				options.sessionID, value.Retention, options.retention,
			)
		}
		options.retention = value.Retention
	}
	if options.retention == "" {
		options.retention = session.RetentionStandard
	}
	existing, err := findSessionTerminalOptional(ctx, manager, options.sessionID)
	if err != nil {
		return sessionTerminalOpenResult{}, err
	}
	if existing != nil {
		return sessionTerminalOpenResult{}, sessionTerminalConflict(
			"Session %s is already bound to terminal %s",
			options.sessionID, existing.TmuxID,
		)
	}
	prepared, runtimeErr := services.Sessions.PrepareRunRequest(session.RunRequest{
		SessionID: options.sessionID, ProfileID: options.profileID,
		Input: "Session terminal preflight", Model: options.model,
		Effort: options.effort, CWD: options.cwd, InvocationBase: callerCWD,
		Retention: options.retention,
	})
	if runtimeErr != nil {
		return sessionTerminalOpenResult{}, runtimeErr
	}
	config := sessionTerminalHelperConfig{
		SessionID: options.sessionID, ProfileID: options.profileID,
		Retention: options.retention, Model: prepared.Model,
		Effort: prepared.Effort, CWD: prepared.CWD,
		InvocationBase: callerCWD, ConfigDigest: prepared.ConfigDigest(),
		BasePromptDigest: prepared.BasePromptDigest(),
	}
	invocation, err := buildSessionTerminalInvocation(config)
	if err != nil {
		return sessionTerminalOpenResult{}, err
	}
	started, err := manager.Start(ctx, runtimetmux.StartRequest{Invocation: invocation})
	if err != nil {
		return sessionTerminalOpenResult{}, err
	}
	if !started.LaunchAccepted {
		_, _ = manager.Stop(context.Background(), started.Window.TmuxID)
		message := "Session terminal helper did not start"
		if started.Window.LaunchError != nil {
			message = started.Window.LaunchError.Message
		}
		return sessionTerminalOpenResult{}, sessionTerminalConflict("%s", message)
	}
	var value session.Session
	if newSession {
		value, err = services.Sessions.CreateWithID(options.sessionID, options.retention)
		if err != nil {
			_, _ = manager.Stop(context.Background(), started.Window.TmuxID)
			return sessionTerminalOpenResult{}, err
		}
	} else {
		value, err = services.Sessions.Get(options.sessionID)
		if err != nil {
			_, _ = manager.Stop(context.Background(), started.Window.TmuxID)
			return sessionTerminalOpenResult{}, err
		}
	}
	result := sessionTerminalOpenResult{
		Session: value, Window: started.Window,
		LaunchAccepted: started.LaunchAccepted,
	}
	if options.input != "" {
		frame, encodeErr := encodeSessionTerminalInput(options.input)
		if encodeErr != nil {
			return result, sessionTerminalRequestError(encodeErr)
		}
		accepted, sendErr := manager.SendFramed(ctx, started.Window.TmuxID, frame)
		if sendErr != nil {
			_, stopErr := manager.Stop(context.Background(), started.Window.TmuxID)
			if stopErr == nil && newSession {
				_, _ = services.Sessions.Delete(options.sessionID)
			}
			if stopErr != nil {
				return result, fmt.Errorf(
					"Session %s terminal opened as %s, initial input failed, and cleanup failed: %v; original error: %w",
					options.sessionID, started.Window.TmuxID, stopErr, sendErr,
				)
			}
			return result, fmt.Errorf(
				"Session terminal initial input was not accepted and the new binding was cleaned up: %w",
				sendErr,
			)
		}
		result.InitialAccepted = accepted.Accepted
	}
	return result, nil
}

func buildSessionTerminalInvocation(
	config sessionTerminalHelperConfig,
) (runtimetmux.Invocation, error) {
	if err := validateSessionTerminalHelperConfig(config); err != nil {
		return runtimetmux.Invocation{}, err
	}
	executable, err := os.Executable()
	if err != nil {
		return runtimetmux.Invocation{}, fmt.Errorf("resolve sn-cli executable: %w", err)
	}
	argv := []string{
		executable, sessionTerminalHelperCommandName,
		"--session-id", config.SessionID,
		"--profile-id", config.ProfileID,
		"--retention", string(config.Retention),
		"--cwd", config.CWD,
		"--invocation-base", config.InvocationBase,
		"--config-digest", config.ConfigDigest,
	}
	if config.BasePromptDigest != "" {
		argv = append(argv, "--base-prompt-digest", config.BasePromptDigest)
	}
	if config.Model != "" {
		argv = append(argv, "--model", config.Model)
	}
	if config.Effort != "" {
		argv = append(argv, "--effort", config.Effort)
	}
	data, err := json.Marshal(config)
	if err != nil {
		return runtimetmux.Invocation{}, fmt.Errorf("encode Session terminal config: %w", err)
	}
	digest := sha256.Sum256(data)
	return runtimetmux.Invocation{
		ProfileID: config.ProfileID, Path: executable, Argv: argv,
		Environment: canonicalEnvironment(os.Environ()), CWD: config.CWD,
		ConfigDigest:     hex.EncodeToString(digest[:]),
		Binding:          &runtimetmux.Binding{Kind: "session", ID: config.SessionID},
		CooperativeReady: true,
	}, nil
}

func runSessionTerminalHelperVNext(args []string) error {
	if err := runtimetmux.AcknowledgeTargetReady(); err != nil {
		return err
	}
	config, err := parseSessionTerminalHelperConfig(args)
	if err != nil {
		return err
	}
	paths, err := layout.Resolve()
	if err != nil {
		return err
	}
	if err := activationgate.RequireOpen(paths.StateDir); err != nil {
		return err
	}
	return serveSessionTerminal(
		context.Background(), paths, config, os.Stdin, os.Stdout, os.Stderr,
	)
}

func serveSessionTerminal(
	ctx context.Context,
	paths layout.Paths,
	config sessionTerminalHelperConfig,
	input io.Reader,
	stdout io.Writer,
	stderr io.Writer,
) error {
	services, err := runtimebootstrap.LoadSessionRunServices(paths, fixedNamespaces...)
	if err != nil {
		return err
	}
	defer services.Runs.Close()
	if err := waitForBoundSession(ctx, services.Sessions, config.SessionID); err != nil {
		return err
	}
	if _, err := fmt.Fprintf(
		stdout,
		"SN Session terminal\nsession=%s profile=%s\nEach input creates one durable Run and canonical Turn.\n",
		config.SessionID, config.ProfileID,
	); err != nil {
		return err
	}
	signal.Ignore(os.Interrupt)
	defer signal.Reset(os.Interrupt)
	scanner := bufio.NewScanner(input)
	scanner.Buffer(make([]byte, 64<<10), sessionTerminalFrameLimit)
	for {
		if _, err := fmt.Fprint(stdout, "sn> "); err != nil {
			return err
		}
		if !scanner.Scan() {
			if err := scanner.Err(); err != nil {
				return fmt.Errorf("read Session terminal input: %w", err)
			}
			_, _ = fmt.Fprintln(stdout)
			return nil
		}
		line := strings.TrimSuffix(scanner.Text(), "\r")
		prompt, closeRequested, err := decodeSessionTerminalLine(line)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "Input rejected: %v\n", err)
			continue
		}
		if closeRequested {
			_, _ = fmt.Fprintln(stdout, "Session terminal closed; canonical Session retained.")
			return nil
		}
		if strings.TrimSpace(prompt) == "" {
			continue
		}
		request := session.RunRequest{
			SessionID: config.SessionID, ProfileID: config.ProfileID,
			Input: prompt, Model: config.Model, Effort: config.Effort,
			CWD: config.CWD, InvocationBase: config.InvocationBase,
			Retention: config.Retention,
		}
		prepared, prepareErr := services.Sessions.PrepareRunRequest(request)
		if prepareErr != nil {
			_, _ = fmt.Fprintf(stderr, "Turn rejected: %s\n", prepareErr.Message)
			continue
		}
		if prepared.ConfigDigest() != config.ConfigDigest ||
			prepared.BasePromptDigest() != config.BasePromptDigest {
			_, _ = fmt.Fprintln(
				stderr,
				"Turn rejected: Profile or base prompt changed; close and reopen the Session terminal",
			)
			continue
		}
		if prepared.Snapshot == nil {
			_, _ = fmt.Fprintln(
				stderr,
				"Turn rejected: CLI execution snapshot is unavailable",
			)
			continue
		}
		privateSnapshot, err := json.Marshal(prepared.Snapshot)
		if err != nil {
			_, _ = fmt.Fprintf(stderr, "Turn rejected: encode CLI snapshot: %v\n", err)
			continue
		}
		turnCtx, stop := signal.NotifyContext(ctx, os.Interrupt)
		record, runtimeErr := services.Runs.RunNow(
			turnCtx,
			runtime.Request{
				Kind: runtime.KindSession, ProfileID: config.ProfileID,
				Input: prompt, SessionID: config.SessionID,
				SessionRetention: string(config.Retention),
				Model:            config.Model, Effort: config.Effort, CWD: config.CWD,
				InvocationBase: config.InvocationBase,
				Labels:         map[string]string{"interface": "session_terminal"},
				PrivateRequest: privateSnapshot,
			},
			nil,
		)
		stop()
		renderSessionTerminalRun(stdout, stderr, record, runtimeErr)
	}
}

func waitForBoundSession(
	ctx context.Context,
	service *session.Service,
	sessionID string,
) error {
	waitCtx, cancel := context.WithTimeout(ctx, 10*time.Second)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		if _, err := service.Get(sessionID); err == nil {
			return nil
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("load bound Session %s: %w", sessionID, err)
		}
		select {
		case <-waitCtx.Done():
			return fmt.Errorf("bound Session %s was not created", sessionID)
		case <-ticker.C:
		}
	}
}

func renderSessionTerminalRun(
	stdout io.Writer,
	stderr io.Writer,
	record runtime.Record,
	runtimeErr *contract.RuntimeError,
) {
	if record.ID == "" {
		if runtimeErr != nil {
			_, _ = fmt.Fprintf(stderr, "Run rejected: %s\n", runtimeErr.Message)
		}
		return
	}
	_, _ = fmt.Fprintf(stdout, "\n[run %s] %s\n", record.ID, record.State)
	var result session.RunResult
	if len(record.Result) > 0 {
		if err := json.Unmarshal(record.Result, &result); err != nil {
			_, _ = fmt.Fprintf(stderr, "Result decode failed: %v\n", err)
		} else {
			if result.Message != nil && result.Message.Content != "" {
				_, _ = fmt.Fprintln(stdout, result.Message.Content)
			}
			if len(result.PendingActions) > 0 {
				_, _ = fmt.Fprintf(
					stdout, "Pending actions: %d\n", len(result.PendingActions),
				)
			}
		}
	}
	if runtimeErr != nil {
		_, _ = fmt.Fprintf(
			stderr, "Run error: %s/%s: %s\n",
			runtimeErr.Code, runtimeErr.Phase, runtimeErr.Message,
		)
	}
}

func parseSessionTerminalOpenOptions(
	args []string,
) (sessionTerminalOpenOptions, error) {
	result := sessionTerminalOpenOptions{retention: session.RetentionStandard}
	if len(args) == 0 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return result, fmt.Errorf("session open requires CLI profile ID immediately after open")
	}
	result.profileID = args[0]
	seen := make(map[string]bool)
	for index := 1; index < len(args); index++ {
		current := args[index]
		if current == "--" {
			remaining := args[index+1:]
			if result.input != "" || len(remaining) > 1 {
				return result, fmt.Errorf("session open accepts at most one quoted input")
			}
			if len(remaining) == 1 {
				result.input = remaining[0]
			}
			break
		}
		if !strings.HasPrefix(current, "-") {
			if result.input != "" || index != len(args)-1 {
				return result, fmt.Errorf("session open input must be the final argument")
			}
			result.input = current
			continue
		}
		if seen[current] {
			return result, fmt.Errorf("session open option %s may only be used once", current)
		}
		seen[current] = true
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
			return result, fmt.Errorf("unknown session open option %s", current)
		}
	}
	if result.sessionID != "" {
		if err := identity.Validate(result.sessionID, "session"); err != nil {
			return result, err
		}
	}
	return result, nil
}

func parseSessionTerminalIDOnly(args []string) (string, error) {
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

func parseSessionTerminalIDAndInput(args []string) (string, string, error) {
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
		return "", "", fmt.Errorf("session send input must be one quoted argument")
	}
	positional := ""
	if len(remaining) == 1 {
		positional = remaining[0]
	}
	return args[1], positional, nil
}

func mergeSessionTerminalInput(piped, positional string) (string, error) {
	value := joinNonEmptyInput(piped, positional)
	if strings.TrimSpace(value) == "" {
		return "", fmt.Errorf("session send requires non-empty input")
	}
	if len(value) > session.MaxInputBytes {
		return "", fmt.Errorf("Session input exceeds %d bytes", session.MaxInputBytes)
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return "", fmt.Errorf("Session input must be UTF-8 without NUL")
	}
	return value, nil
}

func encodeSessionTerminalInput(value string) (string, error) {
	if _, err := mergeSessionTerminalInput("", value); err != nil {
		return "", err
	}
	frame := sessionTerminalInputPrefix + base64.RawStdEncoding.EncodeToString([]byte(value))
	if len(frame) > sessionTerminalFrameLimit {
		return "", fmt.Errorf("Session terminal frame exceeds %d bytes", sessionTerminalFrameLimit)
	}
	return frame, nil
}

func decodeSessionTerminalInput(value string) (string, error) {
	if !strings.HasPrefix(value, sessionTerminalInputPrefix) {
		return value, nil
	}
	encoded := strings.TrimPrefix(value, sessionTerminalInputPrefix)
	data, err := base64.RawStdEncoding.DecodeString(encoded)
	if err != nil {
		return "", fmt.Errorf("invalid Session terminal frame")
	}
	return mergeSessionTerminalInput("", string(data))
}

func decodeSessionTerminalLine(value string) (string, bool, error) {
	if strings.HasPrefix(value, sessionTerminalControlPrefix) {
		if value == sessionTerminalCloseFrame {
			return "", true, nil
		}
		return "", false, fmt.Errorf("invalid Session terminal control frame")
	}
	prompt, err := decodeSessionTerminalInput(value)
	return prompt, false, err
}

func findSessionTerminal(
	ctx context.Context,
	manager tmuxManager,
	sessionID string,
) (runtimetmux.Window, error) {
	value, err := findSessionTerminalOptional(ctx, manager, sessionID)
	if err != nil {
		return runtimetmux.Window{}, err
	}
	if value == nil {
		return runtimetmux.Window{}, &contract.RuntimeError{
			Code: contract.ErrorNotFound, Phase: contract.PhaseRequest,
			Message: fmt.Sprintf("Session %s has no terminal binding", sessionID),
		}
	}
	return *value, nil
}

func findSessionTerminalOptional(
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
		if binding == nil || binding.Kind != "session" || binding.ID != sessionID {
			continue
		}
		if found != nil {
			return nil, sessionTerminalConflict(
				"Session %s has multiple terminal bindings", sessionID,
			)
		}
		current := values[index]
		found = &current
	}
	return found, nil
}

func cancelActiveSessionTerminalRun(
	ctx context.Context,
	paths layout.Paths,
	sessionID string,
) (runtime.Record, bool, error) {
	services, err := runtimebootstrap.LoadRunMaintenanceServices(paths)
	if err != nil {
		return runtime.Record{}, false, err
	}
	defer services.Runs.Close()
	value, err := services.Sessions.Get(sessionID)
	if err != nil {
		return runtime.Record{}, false, canonicalSessionResourceError(
			err, "session", sessionID,
		)
	}
	if value.ActiveTurnID == "" {
		return runtime.Record{}, false, nil
	}
	executions, err := services.Sessions.Executions(sessionID)
	if err != nil {
		return runtime.Record{}, false, err
	}
	var runID string
	for _, execution := range executions {
		if execution.State == session.ExecutionSettled {
			continue
		}
		if runID != "" {
			return runtime.Record{}, false, sessionTerminalConflict(
				"Session %s has multiple nonterminal Executions", sessionID,
			)
		}
		runID = execution.RunID
	}
	if runID == "" {
		return runtime.Record{}, false, sessionTerminalConflict(
			"Session %s active Turn has no nonterminal Execution", sessionID,
		)
	}
	record, err := services.Runs.Cancel(ctx, runID)
	if err != nil {
		return runtime.Record{}, false, err
	}
	return record, true, nil
}

func waitSessionTerminalRunSettled(
	ctx context.Context,
	paths layout.Paths,
	sessionID string,
	runID string,
	timeout time.Duration,
) error {
	services, err := runtimebootstrap.LoadRunMaintenanceServices(paths)
	if err != nil {
		return err
	}
	defer services.Runs.Close()
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(100 * time.Millisecond)
	defer ticker.Stop()
	for {
		record, err := services.Runs.Get(waitCtx, runID)
		if err != nil {
			return err
		}
		value, err := services.Sessions.Get(sessionID)
		if err != nil {
			return err
		}
		if record.State == runtime.StateNeedsReconciliation {
			return sessionTerminalConflict(
				"Run %s needs reconciliation; terminal was left open", runID,
			)
		}
		if record.State.Terminal() && value.ActiveTurnID == "" {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return sessionTerminalConflict(
				"Run %s did not settle before terminal close timeout", runID,
			)
		case <-ticker.C:
		}
	}
}

func waitSessionTerminalWindowExited(
	ctx context.Context,
	manager tmuxManager,
	tmuxID string,
	timeout time.Duration,
) error {
	waitCtx, cancel := context.WithTimeout(ctx, timeout)
	defer cancel()
	ticker := time.NewTicker(25 * time.Millisecond)
	defer ticker.Stop()
	for {
		window, err := manager.Show(waitCtx, tmuxID)
		if err != nil {
			return err
		}
		if window.State == runtimetmux.StateExited {
			return nil
		}
		select {
		case <-waitCtx.Done():
			return sessionTerminalConflict(
				"Tmux terminal %s did not exit before close timeout", tmuxID,
			)
		case <-ticker.C:
		}
	}
}

func parseSessionTerminalHelperConfig(args []string) (sessionTerminalHelperConfig, error) {
	var result sessionTerminalHelperConfig
	values := make(map[string]string)
	for index := 0; index < len(args); index += 2 {
		if index+1 >= len(args) || !strings.HasPrefix(args[index], "--") ||
			args[index+1] == "" {
			return result, fmt.Errorf("invalid private Session terminal helper arguments")
		}
		if _, exists := values[args[index]]; exists {
			return result, fmt.Errorf("duplicate private Session terminal helper option %s", args[index])
		}
		values[args[index]] = args[index+1]
	}
	allowed := map[string]bool{
		"--session-id": true, "--profile-id": true, "--retention": true,
		"--model": true, "--effort": true, "--cwd": true,
		"--invocation-base": true, "--config-digest": true,
		"--base-prompt-digest": true,
	}
	for name := range values {
		if !allowed[name] {
			return result, fmt.Errorf("unknown private Session terminal helper option %s", name)
		}
	}
	result = sessionTerminalHelperConfig{
		SessionID: values["--session-id"], ProfileID: values["--profile-id"],
		Retention: session.Retention(values["--retention"]),
		Model:     values["--model"], Effort: values["--effort"],
		CWD: values["--cwd"], InvocationBase: values["--invocation-base"],
		ConfigDigest:     values["--config-digest"],
		BasePromptDigest: values["--base-prompt-digest"],
	}
	return result, validateSessionTerminalHelperConfig(result)
}

func validateSessionTerminalHelperConfig(config sessionTerminalHelperConfig) error {
	if err := identity.Validate(config.SessionID, "session"); err != nil {
		return err
	}
	if err := profileid.Validate(config.ProfileID); err != nil {
		return err
	}
	if err := validateSessionRetention(config.Retention); err != nil {
		return err
	}
	if !filepath.IsAbs(config.CWD) || !filepath.IsAbs(config.InvocationBase) {
		return fmt.Errorf("private Session terminal paths must be absolute")
	}
	if _, err := runtimecommand.ParseEffort(config.Effort); config.Effort != "" && err != nil {
		return err
	}
	if !validSessionTerminalDigest(config.ConfigDigest) {
		return fmt.Errorf("private Session terminal config digest is invalid")
	}
	if config.BasePromptDigest != "" {
		if !validSessionTerminalDigest(config.BasePromptDigest) {
			return fmt.Errorf("private Session terminal base prompt digest is invalid")
		}
	}
	return nil
}

func validSessionTerminalDigest(value string) bool {
	encoded := strings.TrimPrefix(value, "sha256:")
	if encoded == value {
		return false
	}
	decoded, err := hex.DecodeString(encoded)
	return err == nil && len(decoded) == sha256.Size
}

func canonicalEnvironment(values []string) []string {
	effective := make(map[string]string, len(values))
	for _, value := range values {
		name, current, exists := strings.Cut(value, "=")
		if exists && name != "" {
			effective[name] = current
		}
	}
	result := make([]string, 0, len(effective))
	for name, value := range effective {
		result = append(result, name+"="+value)
	}
	sort.Strings(result)
	return result
}

func renderSessionTerminalAction(
	output *cliOutput,
	result sessionTerminalActionResult,
) error {
	if output.JSON() {
		return output.writeJSON(result)
	}
	if result.RunID != "" {
		return output.line(
			"Session %s terminal %s: accepted=%t tmux=%s run=%s",
			result.SessionID, result.Action, result.Accepted,
			result.TmuxID, result.RunID,
		)
	}
	return output.line(
		"Session %s terminal %s: accepted=%t tmux=%s",
		result.SessionID, result.Action, result.Accepted, result.TmuxID,
	)
}

func sessionTerminalRequestError(err error) error {
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

func sessionTerminalConflict(format string, args ...any) error {
	return &contract.RuntimeError{
		Code: contract.ErrorConflict, Phase: contract.PhaseRun,
		Message: fmt.Sprintf(format, args...),
	}
}
