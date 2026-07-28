package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
	runtimemodel "github.com/yy003x/runtime/model"
	runtimeprofile "github.com/yy003x/runtime/profile"
)

func runVNextProfileNamespace(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: profile <profile-id> [input...] | profile list|show|check")
	}
	runtime, err := runtimebootstrap.LoadVNext(paths, fixedNamespaces...)
	if err != nil {
		return err
	}
	return runLoadedVNextProfile(runtime, args, output)
}

func runLoadedVNextProfile(
	runtime *runtimebootstrap.VNext,
	args []string,
	output *cliOutput,
) error {
	if runtime == nil {
		return fmt.Errorf("Runtime is required")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("profile list does not accept arguments")
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
			return fmt.Errorf("profile show requires one profile ID")
		}
		entry, exists := runtime.Profiles.Resolve(args[1])
		if !exists {
			return fmt.Errorf("unknown profile %q", args[1])
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
			if err := output.line("Binary: %s", entry.Command.Binary); err != nil {
				return err
			}
			if err := output.line(
				"Transport: %s, prompt_delivery: %s",
				entry.Command.Transport, entry.Command.PromptDelivery,
			); err != nil {
				return err
			}
			return nil
		}
		if err := output.line(
			"Driver: %s, model: %s", entry.Model.Driver, entry.Model.Model,
		); err != nil {
			return err
		}
		return output.line("Endpoint: %s", entry.Model.Endpoint)
	case "check":
		if len(args) > 2 {
			return fmt.Errorf("profile check accepts at most one profile ID")
		}
		var checked []string
		if len(args) == 2 {
			if _, exists := runtime.Profiles.Resolve(args[1]); !exists {
				return fmt.Errorf("unknown profile %q", args[1])
			}
			checked = []string{args[1]}
		} else {
			for _, entry := range runtime.Profiles.Entries() {
				checked = append(checked, entry.ID)
			}
		}
		if output.JSON() {
			return output.writeJSON(map[string]any{"ok": true, "checked": checked})
		}
		return output.line("Profiles OK: %s", strings.Join(checked, ", "))
	default:
		entry, exists := runtime.Profiles.Resolve(args[0])
		if !exists {
			return fmt.Errorf("unknown profile %q", args[0])
		}
		if entry.Kind == runtimeprofile.KindCommand {
			commandProfile, prompt, nativeArgs, err := parseCommandProfileInput(
				*entry.Command,
				args[1:],
			)
			if err != nil {
				return err
			}
			if commandProfile.Transport == runtimecommand.TransportTTY {
				if commandProfile.PromptDelivery == runtimecommand.PromptManual {
					return runtimecommand.ReplaceProcess(commandProfile, nativeArgs)
				}
				return runtimecommand.ReplaceProcessPrompt(commandProfile, prompt)
			}
			result, err := runtimecommand.NewRunner().Execute(
				context.Background(), commandProfile,
				runtimecommand.ExecutionRequest{
					Args: nativeArgs, Prompt: prompt,
					TerminalDriver: runtime.Config.Terminal.Driver,
				},
			)
			if err != nil {
				return err
			}
			if output.JSON() {
				return output.writeJSON(result)
			}
			if err := output.text(result.Stdout); err != nil {
				return err
			}
			if err := output.diagnostic(result.Stderr); err != nil {
				return err
			}
			if result.LaunchHandle != "" {
				return output.line(
					"Submitted %s carrier: %s",
					entry.Command.Transport, result.LaunchHandle,
				)
			}
			if result.Stdout == "" && result.Stderr == "" {
				return output.line(
					"Command completed (exit=%d, capture=%s)",
					result.ExitCode, result.CaptureQuality,
				)
			}
			return nil
		}
		request, stream, err := parseDirectModelInput(entry.ID, *entry.Model, args[1:])
		if stream {
			output.beginStream()
		}
		if err != nil {
			return err
		}
		if stream {
			result, runtimeErr := runtime.Models.GenerateStream(
				context.Background(), request,
				func(event contract.Event) error {
					return output.writeEvent(event)
				},
			)
			if runtimeErr != nil {
				return runtimeErr
			}
			return output.writeFinal(directModelResult(result))
		}
		result, runtimeErr := runtime.Models.Generate(context.Background(), request)
		if runtimeErr != nil {
			return runtimeErr
		}
		payload := directModelResult(result)
		if output.JSON() {
			return output.writeJSON(payload)
		}
		return renderDirectModelResult(output, result)
	}
}

func runLoadedVNextShortcut(
	runtime *runtimebootstrap.VNext,
	profileID string,
	nativeArgs []string,
) error {
	if runtime == nil {
		return fmt.Errorf("Runtime is required")
	}
	entry, exists := runtime.Profiles.Resolve(profileID)
	if !exists {
		return fmt.Errorf("unknown profile %q", profileID)
	}
	if entry.Kind != runtimeprofile.KindCommand || entry.Command == nil {
		return fmt.Errorf(
			"shortcut profile %q must be type=cli", profileID,
		)
	}
	if entry.Command.Transport != runtimecommand.TransportTTY {
		return fmt.Errorf(
			"shortcut profile %q must use transport=tty", profileID,
		)
	}
	return runtimecommand.ReplaceProcess(*entry.Command, nativeArgs)
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

func parseCommandProfileInput(
	profile runtimecommand.Profile,
	args []string,
) (runtimecommand.Profile, string, []string, error) {
	effort, remaining, err := parseCommandProfileOptions(args)
	if err != nil {
		return runtimecommand.Profile{}, "", nil, err
	}
	resolved, err := profile.WithEffort(effort)
	if err != nil {
		return runtimecommand.Profile{}, "", nil, err
	}
	switch profile.PromptDelivery {
	case runtimecommand.PromptManual:
		return resolved, "", append([]string(nil), remaining...), nil
	case runtimecommand.PromptArgv, runtimecommand.PromptStdin,
		runtimecommand.PromptPaste:
		if len(remaining) > 1 {
			return runtimecommand.Profile{}, "", nil, fmt.Errorf(
				"automatic command input must be one quoted argument",
			)
		}
		if len(remaining) == 1 {
			return resolved, remaining[0], nil, nil
		}
		value, err := readDirectStdin()
		if err != nil {
			return runtimecommand.Profile{}, "", nil, err
		}
		if profile.PromptDelivery == runtimecommand.PromptPaste &&
			strings.TrimSpace(value) == "" {
			return runtimecommand.Profile{}, "", nil, fmt.Errorf(
				"paste prompt is required",
			)
		}
		return resolved, value, nil, nil
	default:
		return runtimecommand.Profile{}, "", nil, fmt.Errorf(
			"unsupported prompt delivery %q", profile.PromptDelivery,
		)
	}
}

func parseCommandProfileOptions(
	args []string,
) (runtimecommand.Effort, []string, error) {
	var effort runtimecommand.Effort
	remaining := make([]string, 0, len(args))
	for index := 0; index < len(args); index++ {
		argument := args[index]
		if argument == "--" {
			remaining = append(remaining, args[index+1:]...)
			break
		}
		value := ""
		switch {
		case argument == "--effort":
			index++
			if index >= len(args) {
				return "", nil, fmt.Errorf("--effort requires value")
			}
			value = args[index]
		case strings.HasPrefix(argument, "--effort="):
			value = strings.TrimPrefix(argument, "--effort=")
		default:
			remaining = append(remaining, argument)
			continue
		}
		if effort != "" {
			return "", nil, fmt.Errorf("--effort may only be specified once")
		}
		parsed, err := runtimecommand.ParseEffort(value)
		if err != nil {
			return "", nil, err
		}
		effort = parsed
	}
	return effort, remaining, nil
}

func parseDirectModelInput(
	profileID string,
	modelProfile runtimemodel.Profile,
	args []string,
) (contract.GenerateRequest, bool, error) {
	request := contract.GenerateRequest{ModelProfile: profileID}
	stream := false
	requestFile := ""
	system := ""
	var prompt string
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--stream":
			stream = true
		case "--request-file":
			index++
			if index >= len(args) {
				return contract.GenerateRequest{}, stream, fmt.Errorf("--request-file requires value")
			}
			requestFile = args[index]
		case "--system":
			index++
			if index >= len(args) {
				return contract.GenerateRequest{}, stream, fmt.Errorf("--system requires value")
			}
			system = args[index]
		case "--max-completion-tokens", "--max-tokens":
			expected := modelTokenLimitOption(modelProfile.Driver)
			if args[index] != expected {
				return contract.GenerateRequest{}, stream, fmt.Errorf(
					"%s is invalid for %s; use %s",
					args[index], modelProfile.Driver, expected,
				)
			}
			index++
			if index >= len(args) {
				return contract.GenerateRequest{}, stream, fmt.Errorf("%s requires value", expected)
			}
			value, err := strconv.ParseInt(args[index], 10, 64)
			if err != nil || value <= 0 {
				return contract.GenerateRequest{}, stream, fmt.Errorf("%s must be positive", expected)
			}
			request.Input.Options.MaxOutputTokens = &value
		case "--temperature":
			index++
			if index >= len(args) {
				return contract.GenerateRequest{}, stream, fmt.Errorf("--temperature requires value")
			}
			value, err := strconv.ParseFloat(args[index], 64)
			if err != nil || value < 0 || value > 2 {
				return contract.GenerateRequest{}, stream, fmt.Errorf("--temperature must be between 0 and 2")
			}
			request.Input.Options.Temperature = &value
		default:
			if args[index] == "--effort" ||
				strings.HasPrefix(args[index], "--effort=") {
				value := strings.TrimPrefix(args[index], "--effort=")
				if args[index] == "--effort" {
					index++
					if index >= len(args) {
						return contract.GenerateRequest{}, stream, fmt.Errorf(
							"--effort requires value",
						)
					}
					value = args[index]
				}
				if _, err := runtimecommand.ParseEffort(value); err != nil {
					return contract.GenerateRequest{}, stream, err
				}
				return contract.GenerateRequest{}, stream, fmt.Errorf(
					"profile %q does not declare an API effort adapter",
					profileID,
				)
			}
			if strings.HasPrefix(args[index], "-") {
				return contract.GenerateRequest{}, stream, fmt.Errorf("unknown model input option: %s", args[index])
			}
			if prompt != "" {
				return contract.GenerateRequest{}, stream, fmt.Errorf("model prompt must be one quoted argument")
			}
			prompt = args[index]
		}
	}
	if requestFile != "" {
		if prompt != "" || system != "" || request.Input.Options.MaxOutputTokens != nil ||
			request.Input.Options.Temperature != nil {
			return contract.GenerateRequest{}, stream, fmt.Errorf(
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
			return contract.GenerateRequest{}, stream, fmt.Errorf("model prompt is required")
		}
		request.Input.System = system
		request.Input.Messages = []contract.Message{{
			Role: contract.RoleUser, Content: prompt,
		}}
	}
	if err := request.Validate(); err != nil {
		return contract.GenerateRequest{}, stream, err
	}
	return request, stream, nil
}

func modelTokenLimitOption(driver runtimemodel.DriverName) string {
	switch driver {
	case runtimemodel.DriverOpenAICompatible:
		return "--max-completion-tokens"
	case runtimemodel.DriverAnthropicCompatible:
		return "--max-tokens"
	default:
		return "--max-tokens"
	}
}

func readModelRequest(path string) (contract.ModelRequest, error) {
	var reader io.Reader
	var file *os.File
	if path == "-" {
		reader = os.Stdin
	} else {
		info, err := os.Lstat(path)
		if err != nil {
			return contract.ModelRequest{}, err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return contract.ModelRequest{}, fmt.Errorf("request file must be a regular file, not a symlink")
		}
		file, err = os.Open(filepath.Clean(path))
		if err != nil {
			return contract.ModelRequest{}, err
		}
		defer file.Close()
		reader = file
	}
	var value contract.ModelRequest
	decoder := json.NewDecoder(io.LimitReader(reader, (1<<20)+1))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return contract.ModelRequest{}, fmt.Errorf("decode model request: %w", err)
	}
	if err := value.Validate(); err != nil {
		return contract.ModelRequest{}, err
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
		return "", fmt.Errorf("stdin exceeds 1048576 bytes")
	}
	return string(value), nil
}
