package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"strconv"
	"strings"
	"time"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
	runtime "github.com/yy003x/runtime/run"
)

func runAgentNamespace(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	if len(args) == 0 || args[0] != "run" {
		return fmt.Errorf("usage: agent run --profile <model-profile-id> [options] <input>")
	}
	options, err := parseAgentRun(args[1:])
	if options.stream {
		output.beginStream()
	}
	if err != nil {
		return err
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
			TaskID: options.taskID, CWD: cwd, AgentBudget: budget,
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
	var positional []string
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--profile":
			index++
			if index >= len(args) {
				return value, fmt.Errorf("--profile requires value")
			}
			value.profileID = args[index]
		case "--session-id":
			index++
			if index >= len(args) {
				return value, fmt.Errorf("--session-id requires value")
			}
			value.sessionID = args[index]
		case "--task-id":
			index++
			if index >= len(args) {
				return value, fmt.Errorf("--task-id requires value")
			}
			value.taskID = args[index]
		case "--stream":
			value.stream = true
		case "--max-rounds":
			index++
			current, err := parsePositiveInt(args, index, "--max-rounds")
			if err != nil {
				return value, err
			}
			value.maxRounds = current
		case "--max-tool-calls":
			index++
			current, err := parsePositiveInt(args, index, "--max-tool-calls")
			if err != nil {
				return value, err
			}
			value.maxToolCalls = current
		case "--max-total-tokens":
			index++
			if index >= len(args) {
				return value, fmt.Errorf("--max-total-tokens requires value")
			}
			current, err := strconv.ParseInt(args[index], 10, 64)
			if err != nil || current <= 0 {
				return value, fmt.Errorf("--max-total-tokens must be positive")
			}
			value.maxTotalTokens = current
		case "--max-wall-time":
			index++
			if index >= len(args) {
				return value, fmt.Errorf("--max-wall-time requires value")
			}
			current, err := time.ParseDuration(args[index])
			if err != nil || current <= 0 {
				return value, fmt.Errorf("--max-wall-time must be a positive duration")
			}
			value.maxWallTime = current
		case "--label":
			index++
			if index >= len(args) {
				return value, fmt.Errorf("--label requires key=value")
			}
			key, current, exists := strings.Cut(args[index], "=")
			if !exists || key == "" {
				return value, fmt.Errorf("--label requires key=value")
			}
			value.labels[key] = current
		default:
			if strings.HasPrefix(args[index], "-") {
				return value, fmt.Errorf("unknown agent option %s", args[index])
			}
			positional = append(positional, args[index])
		}
	}
	if value.profileID == "" {
		return value, fmt.Errorf("--profile is required")
	}
	if len(positional) > 1 {
		return value, fmt.Errorf("agent input must be one quoted argument")
	}
	if len(positional) == 1 {
		value.input = positional[0]
	} else {
		input, err := readDirectStdin()
		if err != nil {
			return value, err
		}
		value.input = input
	}
	if strings.TrimSpace(value.input) == "" {
		return value, fmt.Errorf("agent input is required")
	}
	return value, nil
}

func parsePositiveInt(args []string, index int, name string) (int, error) {
	if index >= len(args) {
		return 0, fmt.Errorf("%s requires value", name)
	}
	value, err := strconv.Atoi(args[index])
	if err != nil || value <= 0 {
		return 0, fmt.Errorf("%s must be positive", name)
	}
	return value, nil
}
