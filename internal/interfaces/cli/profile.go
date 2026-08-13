package cli

import (
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/infrastructure/executionlog"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
	runtimemodel "github.com/yy003x/runtime/pkg/model"
	runtimeprofile "github.com/yy003x/runtime/pkg/profile"
)

func runProfileNamespace(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	if len(args) == 0 {
		return cliValidationf("usage: profile list|show|check")
	}
	runtime, err := runtimebootstrap.LoadProfileServices(
		paths, fixedNamespaces...,
	)
	if err != nil {
		return err
	}
	return runLoadedProfile(runtime, args, output)
}

func runLoadedProfile(
	runtime *runtimebootstrap.ProfileServices,
	args []string,
	output *cliOutput,
) error {
	if runtime == nil {
		return fmt.Errorf("Runtime is required")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return cliValidationf("profile list does not accept arguments")
		}
		values := make([]map[string]string, 0, len(runtime.Profiles.Entries()))
		for _, entry := range runtime.Profiles.Entries() {
			values = append(values, map[string]string{"id": entry.ID, "kind": string(entry.Kind)})
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"ok": true, "profiles": values})
		}
		if err := output.line("Profiles (%d)", len(values)); err != nil {
			return err
		}
		for _, value := range values {
			if err := output.line("  %s  %s", value["id"], value["kind"]); err != nil {
				return err
			}
		}
		return nil
	case "show":
		if len(args) != 2 {
			return cliValidationf("profile show requires one profile ID")
		}
		entry, exists := runtime.Profiles.Resolve(args[1])
		if !exists {
			return cliValidationf("unknown profile %q", args[1])
		}
		value := any(entry.Model)
		if entry.Kind == runtimeprofile.KindCommand {
			value = entry.Command
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{
				"ok": true, "id": entry.ID, "kind": entry.Kind, "profile": value,
			})
		}
		if err := output.line("Profile: %s (%s)", entry.ID, entry.Kind); err != nil {
			return err
		}
		if entry.Command != nil {
			if err := output.line("Command: %s", entry.Command.Command); err != nil {
				return err
			}
			return output.line(
				"Model: %s, effort: %s, cwd: %s",
				entry.Command.Model, entry.Command.Effort, entry.Command.CWD,
			)
		}
		if err := output.line(
			"Driver: %s, model: %s", entry.Model.Driver, entry.Model.Model,
		); err != nil {
			return err
		}
		endpoint, err := entry.Model.ResolvedEndpoint()
		if err != nil {
			return err
		}
		return output.line("Endpoint: %s", endpoint)
	case "check":
		if len(args) > 2 {
			return cliValidationf("profile check accepts at most one profile ID")
		}
		var entries []runtimeprofile.Entry
		if len(args) == 2 {
			entry, exists := runtime.Profiles.Resolve(args[1])
			if !exists {
				return cliValidationf("unknown profile %q", args[1])
			}
			entries = []runtimeprofile.Entry{entry}
		} else {
			entries = runtime.Profiles.Entries()
		}
		checked := make([]string, 0, len(entries))
		for _, entry := range entries {
			if entry.Kind == runtimeprofile.KindCommand {
				if err := runtimecommand.CheckProfile(*entry.Command); err != nil {
					return fmt.Errorf("profile %q: %w", entry.ID, err)
				}
			} else if err := entry.Model.Validate(); err != nil {
				return fmt.Errorf("profile %q: %w", entry.ID, err)
			}
			checked = append(checked, entry.ID)
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"ok": true, "checked": checked})
		}
		return output.line("Profiles OK: %s", strings.Join(checked, ", "))
	default:
		return cliValidationf(
			"unknown profile action %q; usage: profile list|show|check",
			args[0],
		)
	}
}

func runProfileExecutionNamespace(
	paths layout.Paths,
	args []string,
	expectedKind runtimeprofile.Kind,
	commandMode runtimecommand.Mode,
	namespace string,
	output *cliOutput,
) error {
	if len(args) == 0 {
		return cliValidationf("usage: %s <profile-id> [options] [input]", namespace)
	}
	runtime, err := runtimebootstrap.LoadProfileServices(
		paths, fixedNamespaces...,
	)
	if err != nil {
		return err
	}
	return runLoadedProfileID(
		runtime, paths.LogsDir, args[0], args[1:], expectedKind, commandMode,
		namespace, output,
	)
}

func runLoadedProfileID(
	runtime *runtimebootstrap.ProfileServices,
	logsDir string,
	profileID string,
	args []string,
	expectedKind runtimeprofile.Kind,
	commandMode runtimecommand.Mode,
	namespace string,
	output *cliOutput,
) error {
	if runtime == nil {
		return fmt.Errorf("Runtime is required")
	}
	entry, exists := runtime.Profiles.Resolve(profileID)
	if !exists {
		return cliValidationf("unknown profile %q", profileID)
	}
	if entry.Kind != expectedKind {
		if expectedKind == runtimeprofile.KindCommand {
			return cliValidationf(
				"%s requires a CLI profile; %q is an API profile",
				namespace, profileID,
			)
		}
		return cliValidationf(
			"%s requires an API profile; %q is a CLI profile",
			namespace, profileID,
		)
	}
	if expectedKind == runtimeprofile.KindCommand {
		invocationBase, err := os.Getwd()
		if err != nil {
			return err
		}
		pipedInput := ""
		if !runtimecommand.IsTerminal(os.Stdin) {
			pipedInput, err = runtimecommand.ReadPrompt(os.Stdin)
			if err != nil {
				return err
			}
		}
		invocation, err := buildCommandProfileInvocation(
			*entry.Command, args, pipedInput,
			invocationBase, os.Environ(), commandMode,
		)
		if err != nil {
			return err
		}
		_ = executionlog.AppendCLI(logsDir, executionlog.CLIRecord{
			Time: time.Now(), Namespace: executionlog.Namespace(namespace), Profile: profileID,
			Source:  executionlog.SourceFromArgs(os.Args),
			Command: executionlog.FormatCommand(entry.Command.Env, invocation.CWD, invocation.Path, invocation.Argv),
		})
		stdinMode := runtimecommand.StdinTTY
		if commandMode == runtimecommand.ModeExec {
			stdinMode = runtimecommand.StdinNull
		}
		return runtimecommand.ReplaceProcess(invocation, stdinMode)
	}
	request, stream, err := parseDirectModelInput(entry.ID, *entry.Model, args)
	if stream {
		output.beginStream()
	}
	if err != nil {
		return err
	}
	callContext := runtimemodel.WithAttemptOrigin(
		context.Background(),
		runtimemodel.AttemptOrigin{
			Namespace: runtimemodel.AttemptNamespaceRequest,
			Source:    executionlog.SourceFromArgs(os.Args),
		},
	)
	if stream {
		result, runtimeErr := runtime.Models.GenerateStream(
			callContext, request,
			func(event contract.Event) error {
				return output.writeEvent(event)
			},
		)
		if runtimeErr != nil {
			return runtimeErr
		}
		return output.writeFinal(directModelResult(result))
	}
	result, runtimeErr := runtime.Models.Generate(callContext, request)
	if runtimeErr != nil {
		return runtimeErr
	}
	payload := directModelResult(result)
	if output.JSON() {
		return output.writeJSON(payload)
	}
	return renderDirectModelResult(output, result)
}

func directModelResult(result contract.ModelResult) map[string]any {
	state := "completed"
	if result.FinishReason == contract.FinishToolCall {
		state = "requires_action"
	}
	return map[string]any{"state": state, "result": result}
}

func renderDirectModelResult(
	output *cliOutput,
	result contract.ModelResult,
) error {
	if strings.TrimSpace(result.Message.Content) != "" {
		return output.text(result.Message.Content)
	}
	if len(result.Message.ToolCalls) > 0 {
		if err := output.line(
			"Model requires action: %d tool call(s)", len(result.Message.ToolCalls),
		); err != nil {
			return err
		}
		for _, call := range result.Message.ToolCalls {
			if err := output.line("  %s  %s", call.Name, call.ID); err != nil {
				return err
			}
		}
		return nil
	}
	return output.line(
		"Model returned no assistant text (finish_reason=%s)",
		result.FinishReason,
	)
}

type commandProfileOptions struct {
	model      *string
	effort     *runtimecommand.Effort
	prompt     *string
	cwd        *string
	positional *string
	resume     *string
}

func buildCommandProfileInvocation(
	profile runtimecommand.Profile,
	args []string,
	pipedInput string,
	invocationBase string,
	inheritedEnvironment []string,
	mode runtimecommand.Mode,
) (runtimecommand.Invocation, error) {
	options, err := parseCommandProfileOptions(args)
	if err != nil {
		return runtimecommand.Invocation{}, commandProfileRequestError(err)
	}
	basePrompt, err := runtimecommand.ResolvePrompt(
		profile.Prompt, invocationBase,
	)
	if err != nil {
		return runtimecommand.Invocation{}, commandProfileError(err)
	}
	typedPrompt := ""
	if options.prompt != nil {
		typedPrompt, err = runtimecommand.ResolvePrompt(
			*options.prompt, invocationBase,
		)
		if err != nil {
			return runtimecommand.Invocation{}, commandProfileError(err)
		}
	}
	positional := ""
	if options.positional != nil {
		positional = *options.positional
	}
	prompt, err := runtimecommand.MergePrompt(
		basePrompt, typedPrompt, pipedInput, positional,
	)
	if err != nil {
		return runtimecommand.Invocation{}, commandProfileError(err)
	}
	if mode != runtimecommand.ModeInteractive && mode != runtimecommand.ModeExec {
		return runtimecommand.Invocation{}, commandProfileError(fmt.Errorf(
			"command mode must be interactive or exec",
		))
	}
	if mode == runtimecommand.ModeExec {
		if prompt == "" {
			return runtimecommand.Invocation{}, commandProfileError(fmt.Errorf(
				"exec Profile prompt is required",
			))
		}
		if options.resume != nil {
			return runtimecommand.Invocation{}, commandProfileError(fmt.Errorf(
				"resume is only supported in interactive mode",
			))
		}
	}
	var argvPrompt *string
	if prompt != "" {
		argvPrompt = &prompt
	}
	invocation, err := runtimecommand.Build(runtimecommand.BuildRequest{
		Mode: mode, OutputProtocol: runtimecommand.OutputNative,
		Profile: profile,
		Overrides: runtimecommand.Overrides{
			Model: options.model, Effort: options.effort, CWD: options.cwd,
		},
		ArgvPrompt:           argvPrompt,
		InheritedEnvironment: inheritedEnvironment,
		InvocationBase:       invocationBase,
		Resume:               options.resume,
	})
	if err != nil {
		return runtimecommand.Invocation{}, commandProfileError(err)
	}
	return invocation, nil
}

func commandProfileError(err error) error {
	if err == nil {
		return nil
	}
	var runtimeErr *contract.RuntimeError
	if errors.As(err, &runtimeErr) {
		return err
	}
	return &contract.RuntimeError{
		Code: contract.ErrorInvalidRequest, Phase: contract.PhaseProfile,
		Message: err.Error(),
	}
}

func commandProfileRequestError(err error) error {
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

func parseCommandProfileOptions(args []string) (commandProfileOptions, error) {
	var result commandProfileOptions
	seen := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			if result.positional != nil {
				return commandProfileOptions{}, fmt.Errorf(
					"prompt terminator cannot follow positional input",
				)
			}
			remaining := args[index+1:]
			if len(remaining) > 1 {
				return commandProfileOptions{}, fmt.Errorf(
					"`--` accepts at most one input",
				)
			}
			if len(remaining) == 1 {
				value := remaining[0]
				result.positional = &value
			}
			return result, nil
		}
		if result.positional != nil {
			if strings.HasPrefix(argument, "-") {
				return commandProfileOptions{}, fmt.Errorf(
					"typed options cannot follow positional input",
				)
			}
			return commandProfileOptions{}, fmt.Errorf(
				"Profile input must be one quoted argument",
			)
		}
		if argument == "resume" {
			if result.resume != nil {
				return commandProfileOptions{}, fmt.Errorf(
					"resume is configured multiple times",
				)
			}
			id := ""
			// resume 后下一个非 typed option、非 `--` 的 bare token 当作底层
			// CLI 的原生 session id 透传；缺省为 bare resume（恢复最近/picker）。
			if index+1 < len(args) &&
				!isCommandProfileTypedOption(args[index+1]) &&
				args[index+1] != "--" {
				index++
				id = args[index]
			}
			result.resume = &id
			continue
		}
		name, value, attached := splitTypedOption(argument)
		switch name {
		case "--model", "--effort", "--prompt", "--cwd":
			if seen[name] {
				return commandProfileOptions{}, fmt.Errorf(
					"%s may only be specified once", name,
				)
			}
			seen[name] = true
			if !attached {
				index++
				if index >= len(args) ||
					isCommandProfileTypedOption(args[index]) {
					return commandProfileOptions{}, fmt.Errorf(
						"%s requires value", name,
					)
				}
				value = args[index]
			}
			if value == "" {
				return commandProfileOptions{}, fmt.Errorf(
					"%s requires value", name,
				)
			}
			switch name {
			case "--model":
				result.model = &value
			case "--effort":
				effort, err := runtimecommand.ParseEffort(value)
				if err != nil {
					return commandProfileOptions{}, err
				}
				result.effort = &effort
			case "--prompt":
				result.prompt = &value
			case "--cwd":
				result.cwd = &value
			}
		default:
			if strings.HasPrefix(argument, "-") {
				return commandProfileOptions{}, fmt.Errorf(
					"unknown command Profile option: %s", argument,
				)
			}
			if result.resume != nil {
				return commandProfileOptions{}, fmt.Errorf(
					"resume does not accept positional input; use --prompt",
				)
			}
			value := argument
			result.positional = &value
		}
	}
	return result, nil
}

func isCommandProfileTypedOption(value string) bool {
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

func splitTypedOption(value string) (string, string, bool) {
	if strings.HasPrefix(value, "--") {
		if name, attached, exists := strings.Cut(value, "="); exists {
			return name, attached, true
		}
	}
	return value, "", false
}

func parseDirectModelInput(
	profileID string,
	_ runtimemodel.Profile,
	args []string,
) (contract.GenerateRequest, bool, error) {
	request := contract.GenerateRequest{ModelProfile: profileID}
	stream := false
	requestFile := ""
	system := ""
	prompt := ""
	inputSet := false
	seen := make(map[string]bool)
	for index := 0; index < len(args); index++ {
		current := args[index]
		if current == "--" {
			if inputSet {
				return contract.GenerateRequest{}, stream, cliValidationf(
					"model input terminator cannot follow positional input",
				)
			}
			remaining := args[index+1:]
			if len(remaining) > 1 {
				return contract.GenerateRequest{}, stream, cliValidationf(
					"`--` accepts at most one model input",
				)
			}
			if len(remaining) == 1 {
				prompt = remaining[0]
				inputSet = true
			}
			break
		}
		if inputSet {
			return contract.GenerateRequest{}, stream, cliValidationf(
				"model input must be the final argument",
			)
		}
		if !strings.HasPrefix(current, "-") {
			prompt = current
			inputSet = true
			continue
		}
		name := current
		if strings.HasPrefix(current, "--effort=") {
			name = "--effort"
		}
		if seen[name] {
			return contract.GenerateRequest{}, stream, cliValidationf(
				"model option %s may only be used once", name,
			)
		}
		seen[name] = true
		switch current {
		case "--stream":
			stream = true
		case "--request-file":
			value, next, err := directModelOptionValue(
				args, index, "--request-file",
			)
			if err != nil {
				return contract.GenerateRequest{}, stream, err
			}
			requestFile = value
			index = next
		case "--system":
			value, next, err := directModelOptionValue(
				args, index, "--system",
			)
			if err != nil {
				return contract.GenerateRequest{}, stream, err
			}
			system = value
			index = next
		case "--max-tokens":
			value, next, err := directModelOptionValue(
				args, index, "--max-tokens",
			)
			if err != nil {
				return contract.GenerateRequest{}, stream, err
			}
			parsed, err := strconv.ParseInt(value, 10, 64)
			if err != nil || parsed <= 0 {
				return contract.GenerateRequest{}, stream, cliValidationf("--max-tokens must be positive")
			}
			request.Input.Options.MaxOutputTokens = &parsed
			index = next
		case "--temperature":
			value, next, err := directModelOptionValue(
				args, index, "--temperature",
			)
			if err != nil {
				return contract.GenerateRequest{}, stream, err
			}
			parsed, err := strconv.ParseFloat(value, 64)
			if err != nil {
				return contract.GenerateRequest{}, stream, cliValidationf(
					"--temperature must be between 0 and 2",
				)
			}
			if err := contract.ValidateTemperature(parsed); err != nil {
				return contract.GenerateRequest{}, stream, cliValidationf(
					"--temperature: %v", err,
				)
			}
			request.Input.Options.Temperature = &parsed
			index = next
		default:
			if name == "--effort" {
				value := strings.TrimPrefix(current, "--effort=")
				if current == "--effort" {
					var err error
					value, index, err = directModelOptionValue(
						args, index, "--effort",
					)
					if err != nil {
						return contract.GenerateRequest{}, stream, err
					}
				}
				if _, err := runtimecommand.ParseEffort(value); err != nil {
					return contract.GenerateRequest{}, stream, cliValidation(err)
				}
				return contract.GenerateRequest{}, stream, cliValidationf(
					"--effort is not supported for API profile %q",
					profileID,
				)
			}
			return contract.GenerateRequest{}, stream, cliValidationf(
				"unknown model input option: %s", current,
			)
		}
	}
	if requestFile != "" {
		if prompt != "" || system != "" || request.Input.Options.MaxOutputTokens != nil ||
			request.Input.Options.Temperature != nil {
			return contract.GenerateRequest{}, stream, cliValidationf(
				"--request-file cannot be combined with prompt or request options",
			)
		}
		value, err := readModelRequest(requestFile)
		if err != nil {
			return contract.GenerateRequest{}, stream, err
		}
		request.Input = value
	} else {
		if prompt == "" {
			value, err := readDirectStdin()
			if err != nil {
				return contract.GenerateRequest{}, stream, err
			}
			prompt = value
		}
		if strings.TrimSpace(prompt) == "" {
			return contract.GenerateRequest{}, stream, cliValidationf("model prompt is required")
		}
		request.Input.System = system
		request.Input.Messages = []contract.Message{{
			Role: contract.RoleUser, Content: prompt,
		}}
	}
	if err := request.Validate(); err != nil {
		return contract.GenerateRequest{}, stream, cliValidation(err)
	}
	return request, stream, nil
}

func directModelOptionValue(
	args []string,
	index int,
	name string,
) (string, int, error) {
	index++
	if index >= len(args) || strings.HasPrefix(args[index], "--") {
		return "", index, cliValidationf("%s requires value", name)
	}
	return args[index], index, nil
}

func readModelRequest(path string) (contract.ModelRequest, error) {
	var value contract.ModelRequest
	var err error
	if path == "-" {
		err = strictjson.Decode(os.Stdin, 1<<20, &value)
	} else {
		err = strictjson.ReadRegularFile(path, 1<<20, &value)
	}
	if err != nil {
		err = fmt.Errorf("decode model request: %w", err)
		if strictjson.IsValidation(err) {
			return contract.ModelRequest{}, cliValidation(err)
		}
		return contract.ModelRequest{}, err
	}
	if err := value.Validate(); err != nil {
		return contract.ModelRequest{}, cliValidation(err)
	}
	return value, nil
}

func readDirectStdin() (string, error) {
	info, err := os.Stdin.Stat()
	if err != nil || info.Mode()&os.ModeCharDevice != 0 {
		return "", err
	}
	value, err := io.ReadAll(io.LimitReader(os.Stdin, (1<<20)+1))
	if err != nil {
		return "", err
	}
	if len(value) > 1<<20 {
		return "", cliValidationf("stdin exceeds 1048576 bytes")
	}
	return string(value), nil
}
