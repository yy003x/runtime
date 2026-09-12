package cli

import (
	"bufio"
	"context"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/internal/infrastructure/runtimeconfig"
	"github.com/yy003x/runtime/pkg/contract"
	runtimeprofile "github.com/yy003x/runtime/pkg/profile"
	runtime "github.com/yy003x/runtime/pkg/run"
)

func runAgentNamespace(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	if len(args) == 0 {
		return cliValidationf("usage: agent <api-profile-id> [options] [input]")
	}
	options, err := parseAgentRun(args)
	if options.stream {
		output.beginStream()
	}
	if err != nil {
		return err
	}
	core, err := runtimebootstrap.LoadProfileServices(
		paths, fixedNamespaces...,
	)
	if err != nil {
		return err
	}
	entry, exists := core.Profiles.Resolve(options.profileID)
	if !exists {
		return cliValidationf("unknown profile %q", options.profileID)
	}
	if entry.Kind != runtimeprofile.KindModel {
		return cliValidationf(
			"agent requires an API model profile; %q is a command profile",
			options.profileID,
		)
	}
	cwd, err := resolveAgentCWD(options.cwd)
	if err != nil {
		return err
	}
	services, err := runtimebootstrap.LoadServices(paths, cwd, fixedNamespaces...)
	if err != nil {
		return err
	}
	defer services.Runs.Close()
	if options.autoSession && options.sessionID == "" {
		sessionID, sessionErr := identity.New("session")
		if sessionErr != nil {
			return sessionErr
		}
		options.sessionID = sessionID
	}
	budget := services.Config.AgentBudget()
	if options.maxRounds > 0 {
		budget.MaxRounds = options.maxRounds
	}
	if options.maxToolCalls > 0 {
		budget.MaxToolCalls = options.maxToolCalls
	}
	if options.maxTotalTokens > 0 {
		budget.MaxTotalTokens = options.maxTotalTokens
	}
	if options.maxWallTime > 0 {
		budget.MaxWallTime = options.maxWallTime
	}
	var sink contract.EventSink
	if options.stream {
		sink = func(event contract.Event) error {
			return output.writeEvent(event)
		}
	}
	parts, err := buildAgentInputParts(options.input, options.imagePaths)
	if err != nil {
		return err
	}
	request := runtime.Request{
		Kind: runtime.KindAgent, ProfileID: options.profileID,
		Input: options.input, InputParts: parts, SessionID: options.sessionID,
		TaskID: options.taskID, AgentBudget: budget,
		Labels:       options.labels,
		ModelOptions: contract.GenerateOptions{Stream: options.providerStream},
	}
	if options.repl {
		return runAgentREPL(services, request, options, sink, output)
	}
	return executeAgentRun(services, request, options, sink, output)
}

func executeAgentRun(
	services *runtimebootstrap.Services,
	request runtime.Request,
	options agentRunOptions,
	sink contract.EventSink,
	output *cliOutput,
) error {
	var record runtime.Record
	var runtimeErr *contract.RuntimeError
	if options.queue {
		record, runtimeErr = services.Runs.Submit(
			context.Background(), request,
		)
	} else {
		record, runtimeErr = services.Runs.RunNow(
			context.Background(), request, sink,
		)
	}
	if runtimeErr != nil {
		return runtimeErr
	}
	payload := map[string]any{"run": record}
	if request.SessionID != "" {
		payload["session_id"] = request.SessionID
	}
	if options.stream {
		return output.writeFinal(payload)
	}
	if output.JSON() {
		return output.writeJSON(payload)
	}
	if request.SessionID != "" {
		if err := output.line("Session %s", request.SessionID); err != nil {
			return err
		}
	}
	return renderAgentRun(output, record)
}

func runAgentREPL(
	services *runtimebootstrap.Services,
	request runtime.Request,
	options agentRunOptions,
	sink contract.EventSink,
	output *cliOutput,
) error {
	if err := output.line("Session %s  (empty line or /exit to quit)", request.SessionID); err != nil {
		return err
	}
	scanner := bufio.NewScanner(os.Stdin)
	scanner.Buffer(make([]byte, 0, 64*1024), 1<<20)
	pending := request
	hasPending := strings.TrimSpace(request.Input) != "" || len(request.InputParts) > 0
	for {
		var turn runtime.Request
		if hasPending {
			turn = pending
			hasPending = false
		} else {
			if err := output.text("> "); err != nil {
				return err
			}
			if !scanner.Scan() {
				break
			}
			input := strings.TrimSpace(scanner.Text())
			if input == "" {
				continue
			}
			if input == "/exit" || input == "/quit" {
				break
			}
			turn = request
			turn.Input = input
			turn.InputParts = nil
		}
		if err := executeAgentRun(services, turn, options, sink, output); err != nil {
			if writeErr := output.line("%s", err.Error()); writeErr != nil {
				return writeErr
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return fmt.Errorf("read agent repl: %w", err)
	}
	return nil
}

func buildAgentInputParts(prompt string, imagePaths []string) ([]contract.ContentPart, error) {
	if len(imagePaths) == 0 {
		return nil, nil
	}
	message, err := buildUserMessage(prompt, imagePaths)
	if err != nil {
		return nil, err
	}
	return message.ContentParts, nil
}

func resolveAgentCWD(value string) (string, error) {
	if strings.TrimSpace(value) == "" {
		cwd, err := os.Getwd()
		if err != nil {
			return "", err
		}
		return cwd, nil
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", cliValidation(err)
	}
	info, err := os.Stat(absolute)
	if err != nil {
		return "", cliValidation(fmt.Errorf("--cwd: %w", err))
	}
	if !info.IsDir() {
		return "", cliValidationf("--cwd must be a directory")
	}
	return absolute, nil
}

func renderAgentRun(output *cliOutput, record runtime.Record) error {
	var result struct {
		Outcome struct {
			State      string            `json:"state"`
			StopReason string            `json:"stop_reason"`
			Message    *contract.Message `json:"message"`
		} `json:"outcome"`
	}
	if len(record.Result) > 0 && json.Unmarshal(record.Result, &result) == nil &&
		result.Outcome.Message != nil &&
		strings.TrimSpace(result.Outcome.Message.Content) != "" {
		if err := output.text(result.Outcome.Message.Content); err != nil {
			return err
		}
	}
	return output.line("Run %s: %s", record.ID, record.State)
}

type agentRunOptions struct {
	profileID      string
	sessionID      string
	taskID         string
	input          string
	stream         bool
	providerStream *bool
	queue          bool
	maxRounds      int
	maxToolCalls   int
	maxTotalTokens int64
	maxWallTime    time.Duration
	labels         map[string]string
	cwd            string
	imagePaths     []string
	autoSession    bool
	repl           bool
}

func parseAgentRun(args []string) (agentRunOptions, error) {
	value := agentRunOptions{labels: make(map[string]string)}
	if len(args) == 0 || args[0] == "" || strings.HasPrefix(args[0], "-") {
		return value, cliValidationf(
			"agent requires API profile ID immediately after agent",
		)
	}
	value.profileID = args[0]
	seen := make(map[string]bool)
	inputSet := false
	for index := 1; index < len(args); index++ {
		current := args[index]
		if current == "--" {
			if inputSet {
				return value, cliValidationf(
					"agent input terminator cannot follow positional input",
				)
			}
			remaining := args[index+1:]
			if len(remaining) > 1 {
				return value, cliValidationf(
					"`--` accepts at most one agent input",
				)
			}
			if len(remaining) == 1 {
				value.input = remaining[0]
				inputSet = true
			}
			break
		}
		if inputSet {
			return value, cliValidationf("agent input must be the final argument")
		}
		if !strings.HasPrefix(current, "-") {
			value.input = current
			inputSet = true
			continue
		}
		if current != "--label" && current != "--image" {
			if seen[current] {
				return value, cliValidationf(
					"agent option %s may only be used once", current,
				)
			}
			seen[current] = true
		}
		switch current {
		case "--session-id":
			optionValue, next, err := agentOptionValue(
				args, index, "--session-id",
			)
			if err != nil {
				return value, err
			}
			value.sessionID = optionValue
			index = next
		case "--task-id":
			optionValue, next, err := agentOptionValue(
				args, index, "--task-id",
			)
			if err != nil {
				return value, err
			}
			value.taskID = optionValue
			index = next
		case "--stream":
			if override, next, ok := takeStreamOverride(args, index); ok {
				value.providerStream = override
				index = next
				break
			}
			value.stream = true
		case "--queue":
			value.queue = true
		case "--repl":
			value.repl = true
			value.autoSession = true
		case "--session":
			value.autoSession = true
		case "--cwd":
			optionValue, next, err := agentOptionValue(
				args, index, "--cwd",
			)
			if err != nil {
				return value, err
			}
			value.cwd = optionValue
			index = next
		case "--image":
			optionValue, next, err := agentOptionValue(
				args, index, "--image",
			)
			if err != nil {
				return value, err
			}
			value.imagePaths = append(value.imagePaths, optionValue)
			index = next
		case "--max-rounds":
			optionValue, next, err := agentOptionValue(
				args, index, "--max-rounds",
			)
			if err != nil {
				return value, err
			}
			parsed, err := parsePositiveInt(optionValue, "--max-rounds")
			if err != nil {
				return value, err
			}
			value.maxRounds = parsed
			index = next
		case "--max-tool-calls":
			optionValue, next, err := agentOptionValue(
				args, index, "--max-tool-calls",
			)
			if err != nil {
				return value, err
			}
			parsed, err := parsePositiveInt(optionValue, "--max-tool-calls")
			if err != nil {
				return value, err
			}
			value.maxToolCalls = parsed
			index = next
		case "--max-total-tokens":
			optionValue, next, err := agentOptionValue(
				args, index, "--max-total-tokens",
			)
			if err != nil {
				return value, err
			}
			parsed, err := strconv.ParseInt(optionValue, 10, 64)
			if err != nil || parsed <= 0 {
				return value, cliValidationf("--max-total-tokens must be positive")
			}
			value.maxTotalTokens = parsed
			index = next
		case "--max-wall-time":
			optionValue, next, err := agentOptionValue(
				args, index, "--max-wall-time",
			)
			if err != nil {
				return value, err
			}
			parsed, err := time.ParseDuration(optionValue)
			if err != nil || parsed <= 0 {
				return value, cliValidationf("--max-wall-time must be a positive duration")
			}
			value.maxWallTime = parsed
			index = next
		case "--label":
			optionValue, next, err := agentOptionValue(
				args, index, "--label",
			)
			if err != nil {
				return value, err
			}
			key, labelValue, exists := strings.Cut(optionValue, "=")
			if !exists || key == "" {
				return value, cliValidationf("--label requires key=value")
			}
			if _, exists := value.labels[key]; exists {
				return value, cliValidationf(
					"--label key %q may only be used once", key,
				)
			}
			value.labels[key] = labelValue
			index = next
		default:
			return value, cliValidationf("unknown agent option %s", current)
		}
	}
	if value.queue && value.stream {
		return value, cliValidationf("--queue and --stream are mutually exclusive")
	}
	if value.queue && value.repl {
		return value, cliValidationf("--queue and --repl are mutually exclusive")
	}
	if value.queue && value.cwd != "" {
		return value, cliValidationf("--cwd cannot be combined with --queue")
	}
	if value.autoSession && value.sessionID != "" && !value.repl {
		return value, cliValidationf("--session cannot be combined with --session-id")
	}
	if err := validateAgentBudgetOverrides(value); err != nil {
		return value, err
	}
	if !inputSet && !value.repl {
		input, err := readDirectStdin()
		if err != nil {
			return value, err
		}
		value.input = input
	}
	if strings.TrimSpace(value.input) == "" && len(value.imagePaths) == 0 && !value.repl {
		return value, cliValidationf("agent input is required")
	}
	return value, nil
}

func agentOptionValue(
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

func parsePositiveInt(value string, name string) (int, error) {
	parsed, err := strconv.Atoi(value)
	if err != nil || parsed <= 0 {
		return 0, cliValidationf("%s must be positive", name)
	}
	return parsed, nil
}

func validateAgentBudgetOverrides(value agentRunOptions) error {
	config := runtimeconfig.Default()
	if value.maxRounds > 0 {
		config.Agent.MaxRounds = value.maxRounds
	}
	if value.maxToolCalls > 0 {
		config.Agent.MaxToolCalls = value.maxToolCalls
	}
	if value.maxTotalTokens > 0 {
		config.Agent.MaxTotalTokens = value.maxTotalTokens
	}
	if value.maxWallTime > 0 {
		config.Agent.MaxWallTime = value.maxWallTime.String()
	}
	if err := config.Validate(); err != nil {
		return cliValidationf("invalid Agent budget override: %v", err)
	}
	return nil
}
