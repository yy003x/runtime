package cli

import (
	"context"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
	runtimemodel "github.com/yy003x/runtime/model"
	runtimeprofile "github.com/yy003x/runtime/profile"
	runtime "github.com/yy003x/runtime/run"
	"github.com/yy003x/runtime/session"
)

func runSessionNamespaceVNext(paths layout.Paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: session run|submit|list|show|messages|events|logs|tool-result|send|interrupt|stop|attach|configure|export|delete|gc")
	}
	switch args[0] {
	case "run", "submit":
		return runSessionExecution(paths, args[0], args[1:])
	}
	services, err := runtimebootstrap.LoadSessionServices(paths, fixedNamespaces...)
	if err != nil {
		return err
	}
	switch args[0] {
	case "list":
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
		return printJSON(map[string]any{"sessions": values})
	case "show":
		sessionID, err := requiredOption(args[1:], "--session-id")
		if err != nil {
			return err
		}
		value, err := services.Sessions.Get(sessionID)
		if err != nil {
			return err
		}
		return printJSON(value)
	case "messages":
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
		return printJSON(map[string]any{"messages": values})
	case "events", "logs":
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
		return printJSON(map[string]any{"events": values})
	case "tool-result":
		return submitSessionToolResult(services.Sessions, args[1:])
	case "configure":
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
		return printJSON(value)
	case "export":
		return exportSession(services.Sessions, args[1:])
	case "delete":
		sessionID, err := requiredOption(args[1:], "--session-id")
		if err != nil {
			return err
		}
		target, err := services.Sessions.Delete(sessionID)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{
			"session_id": sessionID, "moved_to": target, "recoverable": true,
		})
	case "gc":
		hours, err := intOptionValue(args[1:], "--older-than-hours", 24)
		if err != nil {
			return err
		}
		limit, err := intOptionValue(args[1:], "--limit", 100)
		if err != nil {
			return err
		}
		value, err := services.Sessions.GC(session.GCOptions{
			OlderThan: time.Duration(hours) * time.Hour,
			Limit:     limit, Apply: hasFlag(args[1:], "--apply"),
		})
		if err != nil {
			return err
		}
		return printJSON(value)
	case "attach", "send", "interrupt", "stop":
		return operateSessionCarrier(services.Sessions, args[0], args[1:])
	default:
		return fmt.Errorf("unknown session action %q", args[0])
	}
}

type sessionInvocation struct {
	sessionID      string
	taskID         string
	retention      session.Retention
	profileID      string
	input          string
	promptFile     string
	cwd            string
	terminalDriver string
	commandArgs    []string
	modelOptions   contract.GenerateOptions
	tokenLimitFlag string
}

func runSessionExecution(paths layout.Paths, action string, args []string) error {
	invocation, err := parseSessionInvocation(args)
	if err != nil {
		return err
	}
	if action == "run" {
		services, err := runtimebootstrap.LoadSessionServices(paths, fixedNamespaces...)
		if err != nil {
			return err
		}
		if invocation.terminalDriver == "" {
			invocation.terminalDriver = services.Config.Terminal.Driver
		}
		if err := validateSessionProfileOptions(
			invocation, services.Profiles,
		); err != nil {
			return err
		}
		result, runtimeErr := services.Sessions.Run(
			context.Background(),
			session.RunRequest{
				SessionID: invocation.sessionID, TaskID: invocation.taskID,
				ProfileID: invocation.profileID, Input: invocation.input,
				CommandArgs: invocation.commandArgs, CWD: invocation.cwd,
				Retention:      invocation.retention,
				ModelOptions:   invocation.modelOptions,
				TerminalDriver: invocation.terminalDriver,
			},
		)
		if err := printJSON(result); err != nil {
			return err
		}
		if runtimeErr != nil {
			return runtimeErr
		}
		return nil
	}
	if invocation.sessionID == "" {
		invocation.sessionID, err = session.NewID()
		if err != nil {
			return err
		}
	}
	cwd := invocation.cwd
	if cwd == "" {
		cwd, err = os.Getwd()
		if err != nil {
			return err
		}
	}
	services, err := runtimebootstrap.LoadServices(paths, cwd, fixedNamespaces...)
	if err != nil {
		return err
	}
	defer services.Runs.Close()
	if invocation.terminalDriver == "" {
		invocation.terminalDriver = services.Config.Terminal.Driver
	}
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
			TaskID:           invocation.taskID, CommandArgs: invocation.commandArgs,
			CWD: cwd, TerminalDriver: invocation.terminalDriver,
			ModelOptions: invocation.modelOptions,
		},
	)
	if runtimeErr != nil {
		return runtimeErr
	}
	return printJSON(map[string]any{
		"run": record, "session_id": invocation.sessionID,
	})
}

func parseSessionInvocation(args []string) (sessionInvocation, error) {
	value := sessionInvocation{retention: session.RetentionStandard}
	index := 0
	for index < len(args) {
		current := args[index]
		if !strings.HasPrefix(current, "-") {
			value.profileID = current
			index++
			break
		}
		switch current {
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
		case "--retention":
			index++
			if index >= len(args) {
				return value, fmt.Errorf("--retention requires value")
			}
			value.retention = session.Retention(args[index])
		case "--prompt-file":
			index++
			if index >= len(args) {
				return value, fmt.Errorf("--prompt-file requires value")
			}
			value.promptFile = args[index]
		case "--cwd":
			index++
			if index >= len(args) {
				return value, fmt.Errorf("--cwd requires value")
			}
			value.cwd = args[index]
		case "--terminal-driver":
			index++
			if index >= len(args) {
				return value, fmt.Errorf("--terminal-driver requires value")
			}
			value.terminalDriver = args[index]
		case "--command-arg":
			index++
			if index >= len(args) {
				return value, fmt.Errorf("--command-arg requires value")
			}
			value.commandArgs = append(value.commandArgs, args[index])
		case "--max-completion-tokens", "--max-tokens":
			if value.tokenLimitFlag != "" {
				return value, fmt.Errorf(
					"%s and %s are mutually exclusive",
					value.tokenLimitFlag, current,
				)
			}
			value.tokenLimitFlag = current
			index++
			if index >= len(args) {
				return value, fmt.Errorf("%s requires value", current)
			}
			tokenLimit, err := strconv.ParseInt(args[index], 10, 64)
			if err != nil || tokenLimit <= 0 {
				return value, fmt.Errorf("%s must be positive", current)
			}
			value.modelOptions.MaxOutputTokens = &tokenLimit
		case "--temperature":
			index++
			if index >= len(args) {
				return value, fmt.Errorf("--temperature requires value")
			}
			current, err := strconv.ParseFloat(args[index], 64)
			if err != nil || current < 0 || current > 2 {
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
	if len(args[index:]) == 1 {
		value.input = args[index]
	}
	if value.promptFile != "" {
		if value.input != "" {
			return value, fmt.Errorf("--prompt-file and positional input are mutually exclusive")
		}
		input, err := readPromptFile(value.promptFile)
		if err != nil {
			return value, err
		}
		value.input = input
	}
	if value.input == "" {
		input, err := readDirectStdin()
		if err != nil {
			return value, err
		}
		value.input = input
	}
	if strings.TrimSpace(value.input) == "" {
		return value, fmt.Errorf("session input is required")
	}
	return value, nil
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
	if !hasModelOptions {
		return nil
	}
	if entry.Kind != runtimeprofile.KindModel || entry.Model == nil {
		return fmt.Errorf(
			"model request options are invalid for command profile %q",
			invocation.profileID,
		)
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

func submitSessionToolResult(service *session.Service, args []string) error {
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
	if content != "" && contentFile != "" {
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
	return printJSON(receipt)
}

func exportSession(service *session.Service, args []string) error {
	sessionID, err := requiredOption(args, "--session-id")
	if err != nil {
		return err
	}
	output, err := requiredOption(args, "--output")
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
	return writeJSONFile(output, map[string]any{
		"schema_version": session.SchemaVersion,
		"session":        sessionValue, "messages": messages, "events": events,
	})
}

func operateSessionCarrier(
	service *session.Service,
	action string,
	args []string,
) error {
	sessionID, err := requiredOption(args, "--session-id")
	if err != nil {
		return err
	}
	execution, err := service.LatestExecution(sessionID)
	if err != nil {
		return err
	}
	if execution.Transport != "tmux" || execution.LaunchHandle == "" {
		return fmt.Errorf(
			"session %s does not have a controllable tmux carrier", sessionID,
		)
	}
	target := execution.LaunchHandle
	sessionTarget := strings.SplitN(target, ":", 2)[0]
	switch action {
	case "attach":
		path, err := exec.LookPath("tmux")
		if err != nil {
			return err
		}
		return syscall.Exec(path, []string{"tmux", "attach-session", "-t", sessionTarget}, os.Environ())
	case "send":
		input, err := carrierSendInput(args)
		if err != nil {
			return err
		}
		if input == "" {
			return fmt.Errorf("session send requires input")
		}
		buffer := "sn-session-" + strconv.Itoa(os.Getpid())
		for _, commandArgs := range [][]string{
			{"set-buffer", "-b", buffer, "--", input},
			{"paste-buffer", "-d", "-b", buffer, "-t", target},
			{"send-keys", "-t", target, "Enter"},
		} {
			if output, err := exec.Command("tmux", commandArgs...).CombinedOutput(); err != nil {
				return fmt.Errorf("tmux %s: %w: %s", commandArgs[0], err, strings.TrimSpace(string(output)))
			}
		}
	case "interrupt":
		if output, err := exec.Command("tmux", "send-keys", "-t", target, "C-c").CombinedOutput(); err != nil {
			return fmt.Errorf("tmux interrupt: %w: %s", err, strings.TrimSpace(string(output)))
		}
	case "stop":
		if output, err := exec.Command("tmux", "kill-session", "-t", sessionTarget).CombinedOutput(); err != nil {
			return fmt.Errorf("tmux stop: %w: %s", err, strings.TrimSpace(string(output)))
		}
	}
	return printJSON(map[string]any{
		"session_id": sessionID, "action": action, "ok": true,
	})
}

func carrierSendInput(args []string) (string, error) {
	var positional []string
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--session-id":
			index++
			if index >= len(args) {
				return "", fmt.Errorf("--session-id requires value")
			}
		default:
			if strings.HasPrefix(args[index], "-") {
				return "", fmt.Errorf("unknown session send option %s", args[index])
			}
			positional = append(positional, args[index])
		}
	}
	if len(positional) != 1 {
		return "", fmt.Errorf("session send requires one quoted input")
	}
	return positional[0], nil
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
	if err != nil || value == "" {
		return fallback, err
	}
	return strconv.ParseUint(value, 10, 64)
}

func intOptionValue(args []string, name string, fallback int) (int, error) {
	value, err := optionString(args, name)
	if err != nil || value == "" {
		return fallback, err
	}
	return strconv.Atoi(value)
}

func hasFlag(args []string, name string) bool {
	for _, value := range args {
		if value == name {
			return true
		}
	}
	return false
}
