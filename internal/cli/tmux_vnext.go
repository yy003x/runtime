package cli

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"strings"

	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
	runtimeprofile "github.com/yy003x/runtime/profile"
	runtimetmux "github.com/yy003x/runtime/tmux"
)

const tmuxInputLimit = 1 << 20

type tmuxManager interface {
	Start(context.Context, runtimetmux.StartRequest) (runtimetmux.StartResult, error)
	List(context.Context) ([]runtimetmux.Window, error)
	Show(context.Context, string) (runtimetmux.Window, error)
	Send(context.Context, string, string) (runtimetmux.ActionResult, error)
	Attach(context.Context, string, runtimetmux.TTYFiles) error
	Interrupt(context.Context, string) (runtimetmux.ActionResult, error)
	Stop(context.Context, string) (runtimetmux.ActionResult, error)
}

type tmuxStartResolver func(
	context.Context,
	tmuxStartOptions,
	string,
) (runtimetmux.Invocation, error)

type tmuxStartOptions struct {
	profileID string
	model     *string
	effort    *runtimecommand.Effort
	prompt    *string
	cwd       *string
	input     *string
}

// runTmuxHelperVNext is intentionally separate from namespace dispatch. root
// must call it before layout or Profile loading when it sees the private helper
// token.
func runTmuxHelperVNext(args []string) error {
	return runtimetmux.RunHelper(args)
}

func runTmuxNamespaceVNext(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	if len(args) == 0 {
		return tmuxRequestError(fmt.Errorf(
			"usage: tmux start|list|show|send|attach|interrupt|stop",
		))
	}
	manager, err := runtimebootstrap.LoadTmuxService(paths)
	if err != nil {
		return err
	}
	var resolver tmuxStartResolver
	if args[0] == "start" {
		if _, err := parseTmuxStartOptions(args[1:]); err != nil {
			return tmuxRequestError(err)
		}
		invocationBase, err := os.Getwd()
		if err != nil {
			return fmt.Errorf("resolve invocation cwd: %w", err)
		}
		catalog, err := runtimeprofile.Load(
			paths.ConfigDir, fixedNamespaces...,
		)
		if err != nil {
			return err
		}
		inheritedEnvironment := os.Environ()
		resolver = func(
			_ context.Context,
			options tmuxStartOptions,
			pipedInput string,
		) (runtimetmux.Invocation, error) {
			return resolveTmuxStartInvocation(
				catalog, options, pipedInput,
				invocationBase, inheritedEnvironment,
			)
		}
	}
	return runTmuxNamespaceWith(
		context.Background(), manager, resolver, args, output,
		os.Stdin, os.Stdout, os.Stderr,
	)
}

func runTmuxNamespaceWith(
	ctx context.Context,
	manager tmuxManager,
	resolver tmuxStartResolver,
	args []string,
	output *cliOutput,
	stdin *os.File,
	stdout *os.File,
	stderr *os.File,
) error {
	if manager == nil {
		return fmt.Errorf("Tmux manager is required")
	}
	switch args[0] {
	case "start":
		options, err := parseTmuxStartOptions(args[1:])
		if err != nil {
			return tmuxRequestError(err)
		}
		if resolver == nil {
			return fmt.Errorf("Tmux start resolver is required")
		}
		pipedInput, err := readOptionalPromptInput(stdin)
		if err != nil {
			return err
		}
		invocation, err := resolver(ctx, options, pipedInput)
		if err != nil {
			return err
		}
		result, err := manager.Start(
			ctx, runtimetmux.StartRequest{Invocation: invocation},
		)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{
				"tmux_window": result.Window, "launch_accepted": result.LaunchAccepted,
			})
		}
		return output.line(
			"Tmux %s: state=%s launch_accepted=%t window=%s",
			result.Window.TmuxID, result.Window.State,
			result.LaunchAccepted, result.Window.WindowID,
		)
	case "list":
		if len(args) != 1 {
			return tmuxRequestError(fmt.Errorf(
				"tmux list does not accept arguments",
			))
		}
		values, err := manager.List(ctx)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"tmux_windows": values})
		}
		if err := output.line("Tmux windows (%d)", len(values)); err != nil {
			return err
		}
		for _, value := range values {
			if err := output.line(
				"  %s  %s  %s  %s",
				value.TmuxID, value.State, value.ProfileID, value.WindowID,
			); err != nil {
				return err
			}
		}
		return nil
	case "show":
		tmuxID, err := parseTmuxIDOnly(args[1:])
		if err != nil {
			return tmuxRequestError(err)
		}
		value, err := manager.Show(ctx, tmuxID)
		if err != nil {
			return err
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"tmux_window": value})
		}
		return renderTmuxWindow(output, value)
	case "send":
		tmuxID, positional, err := parseTmuxIDAndInput(args[1:])
		if err != nil {
			return tmuxRequestError(err)
		}
		pipedInput, err := readOptionalTmuxInput(stdin)
		if err != nil {
			return tmuxRequestError(err)
		}
		input, err := mergeTmuxInput(pipedInput, positional)
		if err != nil {
			return tmuxRequestError(err)
		}
		result, err := manager.Send(ctx, tmuxID, input)
		if err != nil {
			return err
		}
		return renderTmuxAction(output, result)
	case "attach":
		if output.JSON() {
			return tmuxRequestError(fmt.Errorf("tmux attach is human-only"))
		}
		tmuxID, err := parseTmuxIDOnly(args[1:])
		if err != nil {
			return tmuxRequestError(err)
		}
		return manager.Attach(ctx, tmuxID, runtimetmux.TTYFiles{
			Stdin: stdin, Stdout: stdout, Stderr: stderr,
		})
	case "interrupt":
		tmuxID, err := parseTmuxIDOnly(args[1:])
		if err != nil {
			return tmuxRequestError(err)
		}
		result, err := manager.Interrupt(ctx, tmuxID)
		if err != nil {
			return err
		}
		return renderTmuxAction(output, result)
	case "stop":
		tmuxID, err := parseTmuxIDOnly(args[1:])
		if err != nil {
			return tmuxRequestError(err)
		}
		result, err := manager.Stop(ctx, tmuxID)
		if err != nil {
			return err
		}
		return renderTmuxAction(output, result)
	default:
		return tmuxRequestError(fmt.Errorf(
			"unknown tmux command %q", args[0],
		))
	}
}

func tmuxRequestError(err error) error {
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

func resolveTmuxStartInvocation(
	catalog *runtimeprofile.Catalog,
	options tmuxStartOptions,
	pipedInput string,
	invocationBase string,
	inheritedEnvironment []string,
) (runtimetmux.Invocation, error) {
	if catalog == nil {
		return runtimetmux.Invocation{}, fmt.Errorf("Profile catalog is required")
	}
	entry, exists := catalog.Resolve(options.profileID)
	if !exists {
		return runtimetmux.Invocation{}, fmt.Errorf(
			"unknown profile %q", options.profileID,
		)
	}
	if entry.Kind != runtimeprofile.KindCommand || entry.Command == nil {
		return runtimetmux.Invocation{}, fmt.Errorf(
			"tmux start profile %q must be type=cli", options.profileID,
		)
	}
	basePrompt, err := runtimecommand.ResolvePrompt(
		entry.Command.Prompt, invocationBase,
	)
	if err != nil {
		return runtimetmux.Invocation{}, err
	}
	typedPrompt := ""
	if options.prompt != nil {
		typedPrompt, err = runtimecommand.ResolvePrompt(
			*options.prompt, invocationBase,
		)
		if err != nil {
			return runtimetmux.Invocation{}, err
		}
	}
	positional := ""
	if options.input != nil {
		positional = *options.input
	}
	prompt, err := runtimecommand.MergePrompt(
		basePrompt, typedPrompt, pipedInput, positional,
	)
	if err != nil {
		return runtimetmux.Invocation{}, err
	}
	var argvPrompt *string
	if prompt != "" {
		argvPrompt = &prompt
	}
	invocation, err := runtimecommand.Build(runtimecommand.BuildRequest{
		Mode: runtimecommand.ModeInteractive, OutputProtocol: runtimecommand.OutputNative,
		Profile: *entry.Command,
		Overrides: runtimecommand.Overrides{
			Model: options.model, Effort: options.effort, CWD: options.cwd,
		},
		ArgvPrompt: argvPrompt, InheritedEnvironment: inheritedEnvironment,
		InvocationBase: invocationBase,
	})
	if err != nil {
		return runtimetmux.Invocation{}, err
	}
	digest, err := tmuxConfigDigest(options.profileID, *entry.Command, options)
	if err != nil {
		return runtimetmux.Invocation{}, err
	}
	return runtimetmux.Invocation{
		ProfileID: options.profileID, Path: invocation.Path,
		Argv: invocation.Argv, Environment: invocation.Environment,
		CWD: invocation.CWD, ConfigDigest: digest,
	}, nil
}

func tmuxConfigDigest(
	profileID string,
	profile runtimecommand.Profile,
	options tmuxStartOptions,
) (string, error) {
	value := struct {
		ProfileID string                   `json:"profile_id"`
		Mode      runtimecommand.Mode      `json:"mode"`
		Profile   runtimecommand.Profile   `json:"profile"`
		Overrides runtimecommand.Overrides `json:"overrides"`
	}{
		ProfileID: profileID, Mode: runtimecommand.ModeInteractive,
		Profile: profile,
		Overrides: runtimecommand.Overrides{
			Model: options.model, Effort: options.effort, CWD: options.cwd,
		},
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", fmt.Errorf("encode Tmux effective config: %w", err)
	}
	sum := sha256.Sum256(data)
	return hex.EncodeToString(sum[:]), nil
}

func parseTmuxStartOptions(args []string) (tmuxStartOptions, error) {
	var result tmuxStartOptions
	seen := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			if result.profileID == "" {
				return tmuxStartOptions{}, fmt.Errorf(
					"tmux start profile ID must precede `--`",
				)
			}
			if result.input != nil || len(args[index+1:]) > 1 {
				return tmuxStartOptions{}, fmt.Errorf(
					"tmux start accepts at most one quoted input",
				)
			}
			if len(args[index+1:]) == 1 {
				value := args[index+1]
				result.input = &value
			}
			break
		}
		name, value, attached := splitTypedOption(argument)
		switch name {
		case "--model", "--effort", "--prompt", "--cwd":
			if result.profileID != "" {
				return tmuxStartOptions{}, fmt.Errorf(
					"Tmux typed options must precede profile ID",
				)
			}
			if seen[name] {
				return tmuxStartOptions{}, fmt.Errorf(
					"%s may only be specified once", name,
				)
			}
			seen[name] = true
			if !attached {
				index++
				if index >= len(args) ||
					isTmuxStartTypedOption(args[index]) {
					return tmuxStartOptions{}, fmt.Errorf("%s requires value", name)
				}
				value = args[index]
			}
			if value == "" {
				return tmuxStartOptions{}, fmt.Errorf("%s requires value", name)
			}
			switch name {
			case "--model":
				result.model = &value
			case "--effort":
				effort, err := runtimecommand.ParseEffort(value)
				if err != nil {
					return tmuxStartOptions{}, err
				}
				result.effort = &effort
			case "--prompt":
				result.prompt = &value
			case "--cwd":
				result.cwd = &value
			}
		default:
			if strings.HasPrefix(argument, "-") {
				return tmuxStartOptions{}, fmt.Errorf(
					"unknown Tmux start option: %s", argument,
				)
			}
			if result.profileID == "" {
				result.profileID = argument
				continue
			}
			if result.input != nil {
				return tmuxStartOptions{}, fmt.Errorf(
					"tmux start input must be one quoted argument",
				)
			}
			value := argument
			result.input = &value
		}
	}
	if result.profileID == "" {
		return tmuxStartOptions{}, fmt.Errorf("tmux start requires profile ID")
	}
	return result, nil
}

func isTmuxStartTypedOption(value string) bool {
	if strings.HasPrefix(value, "--") {
		return true
	}
	name, _, _ := splitTypedOption(value)
	switch name {
	case "--model", "--effort", "--prompt", "--cwd":
		return true
	default:
		return value == "--"
	}
}

func parseTmuxIDOnly(args []string) (string, error) {
	if len(args) == 1 && strings.HasPrefix(args[0], "--tmux-id=") {
		value := strings.TrimPrefix(args[0], "--tmux-id=")
		if value != "" {
			return value, nil
		}
	}
	if len(args) != 2 || args[0] != "--tmux-id" || args[1] == "" {
		return "", fmt.Errorf("command requires exactly --tmux-id <id>")
	}
	return args[1], nil
}

func parseTmuxIDAndInput(args []string) (string, string, error) {
	if len(args) < 2 || args[0] != "--tmux-id" || args[1] == "" {
		return "", "", fmt.Errorf(
			"tmux send requires --tmux-id <id> and non-empty input",
		)
	}
	remaining := args[2:]
	if len(remaining) > 0 && remaining[0] == "--" {
		remaining = remaining[1:]
	}
	if len(remaining) > 1 {
		return "", "", fmt.Errorf("tmux send input must be one quoted argument")
	}
	input := ""
	if len(remaining) == 1 {
		input = remaining[0]
	}
	return args[1], input, nil
}

func readOptionalPromptInput(file *os.File) (string, error) {
	if file == nil || runtimecommand.IsTerminal(file) {
		return "", nil
	}
	return runtimecommand.ReadPrompt(file)
}

func readOptionalTmuxInput(file *os.File) (string, error) {
	if file == nil || runtimecommand.IsTerminal(file) {
		return "", nil
	}
	value, err := io.ReadAll(io.LimitReader(file, tmuxInputLimit+1))
	if err != nil {
		return "", fmt.Errorf("read Tmux input: %w", err)
	}
	if len(value) > tmuxInputLimit {
		return "", fmt.Errorf("Tmux input exceeds %d bytes", tmuxInputLimit)
	}
	return string(value), nil
}

func mergeTmuxInput(pipedInput, positional string) (string, error) {
	fragments := make([]string, 0, 2)
	if pipedInput != "" {
		fragments = append(fragments, pipedInput)
	}
	if positional != "" {
		fragments = append(fragments, positional)
	}
	value := strings.Join(fragments, "\n")
	if value == "" {
		return "", fmt.Errorf("tmux send requires non-empty input")
	}
	if len(value) > tmuxInputLimit {
		return "", fmt.Errorf("Tmux input exceeds %d bytes", tmuxInputLimit)
	}
	return value, nil
}

func renderTmuxAction(output *cliOutput, result runtimetmux.ActionResult) error {
	if output.JSON() {
		return output.writeJSON(result)
	}
	return output.line(
		"Tmux %s: %s accepted=%t",
		result.TmuxID, result.Action, result.Accepted,
	)
}

func renderTmuxWindow(output *cliOutput, value runtimetmux.Window) error {
	if err := output.line(
		"Tmux %s: state=%s window=%s pane=%s",
		value.TmuxID, value.State, value.WindowID, value.PaneID,
	); err != nil {
		return err
	}
	if value.ProfileID != "" {
		if err := output.line(
			"Profile: %s, cwd: %s", value.ProfileID, value.CWD,
		); err != nil {
			return err
		}
	}
	if value.ExitCode != nil {
		return output.line(
			"Exit: code=%d signal=%s", *value.ExitCode, value.Signal,
		)
	}
	return nil
}
