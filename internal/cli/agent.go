package cli

import (
	"context"
	"encoding/json"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
	"github.com/yy003x/runtime/internal/runtimeconfig"
	runtimeprofile "github.com/yy003x/runtime/profile"
	runtime "github.com/yy003x/runtime/run"
)

func runAgentNamespace(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	if len(args) == 0 || args[0] != "run" {
		return cliValidationf("usage: agent run --profile <model-profile-id> [options] [input]")
	}
	options, err := parseAgentRun(args[1:])
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
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	services, err := runtimebootstrap.LoadServices(paths, cwd, fixedNamespaces...)
	if err != nil {
		return err
	}
	defer services.Runs.Close()
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
	record, runtimeErr := services.Runs.RunNow(
		context.Background(),
		runtime.Request{
			Kind: runtime.KindAgent, ProfileID: options.profileID,
			Input: options.input, SessionID: options.sessionID,
			TaskID: options.taskID, AgentBudget: budget,
			Labels: options.labels,
		},
		sink,
	)
	if runtimeErr != nil {
		return runtimeErr
	}
	payload := map[string]any{"run": record}
	if options.stream {
		return output.writeFinal(payload)
	}
	if output.JSON() {
		return output.writeJSON(payload)
	}
	return renderAgentRun(output, record)
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
	maxRounds      int
	maxToolCalls   int
	maxTotalTokens int64
	maxWallTime    time.Duration
	labels         map[string]string
}

func parseAgentRun(args []string) (agentRunOptions, error) {
	value := agentRunOptions{labels: make(map[string]string)}
	seen := make(map[string]bool)
	inputSet := false
	for index := 0; index < len(args); index++ {
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
		if current != "--label" {
			if seen[current] {
				return value, cliValidationf(
					"agent option %s may only be used once", current,
				)
			}
			seen[current] = true
		}
		switch current {
		case "--profile":
			optionValue, next, err := agentOptionValue(
				args, index, "--profile",
			)
			if err != nil {
				return value, err
			}
			value.profileID = optionValue
			index = next
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
			value.stream = true
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
	if value.profileID == "" {
		return value, cliValidationf("--profile is required")
	}
	if err := validateAgentBudgetOverrides(value); err != nil {
		return value, err
	}
	if !inputSet {
		input, err := readDirectStdin()
		if err != nil {
			return value, err
		}
		value.input = input
	}
	if strings.TrimSpace(value.input) == "" {
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
