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
	transportcli "github.com/yy003x/runtime/transport/cli"
)

func runVNextProfileNamespace(paths layout.Paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: profile <profile-id> [input...] | profile list|show|check")
	}
	runtime, err := runtimebootstrap.LoadVNext(paths, fixedNamespaces...)
	if err != nil {
		return err
	}
	return runLoadedVNextProfile(runtime, args)
}

func runLoadedVNextProfile(runtime *runtimebootstrap.VNext, args []string) error {
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
		return printJSON(map[string]any{"ok": true, "profiles": values})
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
		return printJSON(map[string]any{
			"ok": true, "id": entry.ID, "kind": entry.Kind, "profile": value,
		})
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
		return printJSON(map[string]any{"ok": true, "checked": checked})
	default:
		entry, exists := runtime.Profiles.Resolve(args[0])
		if !exists {
			return fmt.Errorf("unknown profile %q", args[0])
		}
		if entry.Kind == runtimeprofile.KindCommand {
			prompt, nativeArgs, err := parseCommandProfileInput(*entry.Command, args[1:])
			if err != nil {
				return err
			}
			if entry.Command.Transport == runtimecommand.TransportTTY {
				if entry.Command.PromptDelivery == runtimecommand.PromptManual {
					return runtimecommand.ReplaceProcess(*entry.Command, nativeArgs)
				}
				return runtimecommand.ReplaceProcessPrompt(*entry.Command, prompt)
			}
			result, err := runtimecommand.NewRunner().Execute(
				context.Background(), *entry.Command,
				runtimecommand.ExecutionRequest{
					Args: nativeArgs, Prompt: prompt,
					TerminalDriver: runtime.Config.Terminal.Driver,
				},
			)
			if err != nil {
				return err
			}
			return printJSON(result)
		}
		request, stream, err := parseDirectModelInput(entry.ID, *entry.Model, args[1:])
		if err != nil {
			return err
		}
		if stream {
			if runtimeErr := transportcli.Generate(
				context.Background(), runtime.Models, request, true, os.Stdout,
			); runtimeErr != nil {
				return runtimeErr
			}
			return nil
		}
		result, runtimeErr := runtime.Models.Generate(context.Background(), request)
		if runtimeErr != nil {
			return runtimeErr
		}
		state := "completed"
		if result.FinishReason == contract.FinishToolCall {
			state = "requires_action"
		}
		return printJSON(map[string]any{"state": state, "result": result})
	}
}

func parseCommandProfileInput(
	profile runtimecommand.Profile,
	args []string,
) (string, []string, error) {
	switch profile.PromptDelivery {
	case runtimecommand.PromptManual:
		return "", append([]string(nil), args...), nil
	case runtimecommand.PromptArgv, runtimecommand.PromptStdin,
		runtimecommand.PromptPaste:
		if len(args) > 1 {
			return "", nil, fmt.Errorf(
				"automatic command input must be one quoted argument",
			)
		}
		if len(args) == 1 {
			return args[0], nil, nil
		}
		value, err := readDirectStdin()
		if err != nil {
			return "", nil, err
		}
		if profile.PromptDelivery == runtimecommand.PromptPaste &&
			strings.TrimSpace(value) == "" {
			return "", nil, fmt.Errorf("paste prompt is required")
		}
		return value, nil, nil
	default:
		return "", nil, fmt.Errorf(
			"unsupported prompt delivery %q", profile.PromptDelivery,
		)
	}
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
				return contract.GenerateRequest{}, false, fmt.Errorf("--request-file requires value")
			}
			requestFile = args[index]
		case "--system":
			index++
			if index >= len(args) {
				return contract.GenerateRequest{}, false, fmt.Errorf("--system requires value")
			}
			system = args[index]
		case "--max-completion-tokens", "--max-tokens":
			expected := modelTokenLimitOption(modelProfile.Driver)
			if args[index] != expected {
				return contract.GenerateRequest{}, false, fmt.Errorf(
					"%s is invalid for %s; use %s",
					args[index], modelProfile.Driver, expected,
				)
			}
			index++
			if index >= len(args) {
				return contract.GenerateRequest{}, false, fmt.Errorf("%s requires value", expected)
			}
			value, err := strconv.ParseInt(args[index], 10, 64)
			if err != nil || value <= 0 {
				return contract.GenerateRequest{}, false, fmt.Errorf("%s must be positive", expected)
			}
			request.Input.Options.MaxOutputTokens = &value
		case "--temperature":
			index++
			if index >= len(args) {
				return contract.GenerateRequest{}, false, fmt.Errorf("--temperature requires value")
			}
			value, err := strconv.ParseFloat(args[index], 64)
			if err != nil || value < 0 || value > 2 {
				return contract.GenerateRequest{}, false, fmt.Errorf("--temperature must be between 0 and 2")
			}
			request.Input.Options.Temperature = &value
		default:
			if strings.HasPrefix(args[index], "-") {
				return contract.GenerateRequest{}, false, fmt.Errorf("unknown model input option: %s", args[index])
			}
			if prompt != "" {
				return contract.GenerateRequest{}, false, fmt.Errorf("model prompt must be one quoted argument")
			}
			prompt = args[index]
		}
	}
	if requestFile != "" {
		if prompt != "" || system != "" || request.Input.Options.MaxOutputTokens != nil ||
			request.Input.Options.Temperature != nil {
			return contract.GenerateRequest{}, false, fmt.Errorf(
				"--request-file cannot be combined with prompt or request options",
			)
		}
		value, err := readModelRequest(requestFile)
		if err != nil {
			return contract.GenerateRequest{}, false, err
		}
		request.Input = value
	} else {
		if prompt == "" {
			value, err := readDirectStdin()
			if err != nil {
				return contract.GenerateRequest{}, false, err
			}
			prompt = value
		}
		if strings.TrimSpace(prompt) == "" {
			return contract.GenerateRequest{}, false, fmt.Errorf("model prompt is required")
		}
		request.Input.System = system
		request.Input.Messages = []contract.Message{{
			Role: contract.RoleUser, Content: prompt,
		}}
	}
	if err := request.Validate(); err != nil {
		return contract.GenerateRequest{}, false, err
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
