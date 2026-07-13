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
	"time"

	"agent-arch/internal/agentrun"
	"agent-arch/internal/capability"
	"agent-arch/internal/provider"
	"agent-arch/sncli/internal/config"
)

func runRuntimeDoctor(cfg *config.Config, _ []string) error {
	service := agentrun.New(cfg.Root)
	profiles, err := service.Profiles()
	if err != nil {
		return err
	}
	items := make(map[string]any, len(profiles))
	ok := true
	for id, profile := range profiles {
		validation := validateProfile(profile, false)
		items[id] = validation
		if valid, _ := validation["ok"].(bool); !valid {
			ok = false
		}
	}
	return printJSON(map[string]any{
		"ok": ok, "version": service.RuntimeVersion, "runs_dir": service.RunsDir,
		"default_profile": service.DefaultProfile, "profiles": len(profiles), "providers": items,
	})
}

func runConfigCommand(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: config choices|validate")
	}
	service := agentrun.New(cfg.Root)
	profiles, err := service.Profiles()
	if err != nil {
		return err
	}
	switch args[0] {
	case "choices":
		includeAll := containsArg(args[1:], "--all")
		var choices []map[string]any
		for _, profile := range profiles {
			validation := validateProfile(profile, false)
			valid, _ := validation["ok"].(bool)
			if !includeAll && !valid {
				continue
			}
			choices = append(choices, map[string]any{
				"id": profile.ID, "label": profile.Label, "provider_type": profile.Type,
				"transport": profile.Transport(), "validated": valid, "validation": validation,
			})
		}
		return printJSON(map[string]any{"ok": true, "only_valid": !includeAll, "choices": choices})
	case "validate":
		name := optionValue(args[1:], "--name")
		profileID := optionValue(args[1:], "--profile")
		providerType := optionValue(args[1:], "--provider")
		live := containsArg(args[1:], "--live")
		var results []map[string]any
		allOK := true
		for _, profile := range profiles {
			if name != "" && !profileHasName(profile, name) {
				continue
			}
			if profileID != "" && profile.ID != profileID {
				continue
			}
			if providerType != "" && profile.Transport() != providerType && profile.Type != providerType {
				continue
			}
			result := validateProfile(profile, live)
			results = append(results, result)
			if valid, _ := result["ok"].(bool); !valid {
				allOK = false
			}
		}
		if len(results) == 0 {
			allOK = false
		}
		return printJSON(map[string]any{"ok": allOK, "validated": len(results), "results": results})
	default:
		return fmt.Errorf("unknown config command: %s", args[0])
	}
}

func profileHasName(profile provider.Config, name string) bool {
	if profile.ID == name {
		return true
	}
	for _, alias := range profile.Aliases {
		if alias == name {
			return true
		}
	}
	return false
}

func validateProfile(profile provider.Config, live bool) map[string]any {
	result := map[string]any{"profile": profile.ID, "provider_type": profile.Type, "transport": profile.Transport(), "ok": false}
	if profile.Type == provider.TypeAPI {
		if profile.API.Mock {
			result["ok"], result["message"] = true, "mock API profile 可用"
			return result
		}
		if os.Getenv(profile.API.APIKeyEnv) == "" {
			result["message"] = "环境变量未设置: " + profile.API.APIKeyEnv
			return result
		}
		if live {
			result["message"] = "配置有效；真实 API 验证请执行 profile smoke，避免 doctor 产生费用"
		} else {
			result["message"] = "API key 环境变量已设置"
		}
		result["ok"] = true
		return result
	}
	if profile.Transport() == provider.ExecutorTmux {
		if _, err := exec.LookPath("tmux"); err != nil {
			result["message"] = "tmux 不可用"
			return result
		}
	}
	binary := profile.CLI.Command.Binary
	if _, err := exec.LookPath(binary); err != nil {
		if info, statErr := os.Stat(binary); statErr != nil || info.IsDir() {
			result["message"] = "命令不可用: " + binary
			return result
		}
	}
	result["ok"], result["message"] = true, "命令可用"
	return result
}

func runTaskCommand(cfg *config.Config, command string, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: %s run|status|logs|watch|cancel", command)
	}
	service := agentrun.New(cfg.Root)
	runType := agentrun.RunTask
	if command == "turn" {
		runType = agentrun.RunTurn
	}
	switch args[0] {
	case "run":
		options, err := parseRunOptions(runType, args[1:])
		if err != nil {
			return err
		}
		result, runErr := service.Run(context.Background(), options)
		_ = printJSON(result)
		return runErr
	case "status":
		if len(args) < 2 {
			return fmt.Errorf("usage: %s status <run_id>", command)
		}
		status, err := service.Status(runType, args[1])
		if err != nil {
			return err
		}
		return printJSON(status)
	case "logs":
		if len(args) < 2 {
			return fmt.Errorf("usage: %s logs <run_id>", command)
		}
		logs, err := service.Logs(runType, args[1], intOption(args[2:], "--tail", 120))
		if err != nil {
			return err
		}
		return printJSON(logs)
	case "watch":
		if len(args) < 2 {
			return fmt.Errorf("usage: %s watch <run_id>", command)
		}
		return watchRun(service, runType, args[1], args[2:])
	case "cancel":
		if len(args) < 2 {
			return fmt.Errorf("usage: %s cancel <run_id>", command)
		}
		result, err := service.Cancel(runType, args[1])
		_ = printJSON(result)
		return err
	default:
		return fmt.Errorf("unknown %s command: %s", command, args[0])
	}
}

func parseRunOptions(runType string, args []string) (agentrun.RunOptions, error) {
	options := agentrun.RunOptions{RunType: runType, ExecutionMode: agentrun.ModeManaged, ProviderOverrides: map[string]any{}}
	var promptParts []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--profile":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--profile requires value")
			}
			options.Profile = args[i]
		case "--project":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--project requires value")
			}
			options.ProjectID = args[i]
		case "--run-id":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--run-id requires value")
			}
			options.RunID = args[i]
		case "--cwd":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--cwd requires value")
			}
			options.CWD = args[i]
		case "--prompt-file", "--prompt_file":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			options.PromptFile = args[i]
		case "--mode":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--mode requires value")
			}
			options.ExecutionMode = args[i]
		case "--deadline", "--deadline-seconds", "--timeout":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			value, err := strconv.Atoi(args[i])
			if err != nil {
				return options, err
			}
			options.DeadlineSeconds = value
		case "--result-schema":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--result-schema requires value")
			}
			options.ResultSchema = args[i]
		case "--model":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--model requires value")
			}
			options.ProviderOverrides["model"] = args[i]
		case "--reasoning-effort", "--codex-reasoning-effort":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			options.ProviderOverrides["reasoning_effort"] = args[i]
		case "--image":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--image requires value")
			}
			images, _ := options.ProviderOverrides["images"].([]string)
			options.ProviderOverrides["images"] = append(images, args[i])
		case "--provider-overrides":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--provider-overrides requires value")
			}
			var overrides map[string]any
			if err := json.Unmarshal([]byte(args[i]), &overrides); err != nil {
				return options, err
			}
			for key, value := range overrides {
				options.ProviderOverrides[key] = value
			}
		case "--allowed-action":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--allowed-action requires value")
			}
			options.AllowedActions = append(options.AllowedActions, args[i])
		case "--forbidden-action":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--forbidden-action requires value")
			}
			options.ForbiddenActions = append(options.ForbiddenActions, args[i])
		case "--force":
			options.Force = true
		case "--json":
		default:
			promptParts = append(promptParts, args[i])
		}
	}
	options.Prompt = strings.TrimSpace(strings.Join(promptParts, " "))
	return options, nil
}

func watchRun(service *agentrun.Service, runType, runID string, args []string) error {
	seconds := intOption(args, "--seconds", 0)
	poll := floatOption(args, "--poll-seconds", 1)
	deadline := time.Time{}
	if seconds > 0 {
		deadline = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	for {
		status, err := service.Status(runType, runID)
		if err != nil {
			return err
		}
		if terminalState(status.State) || (!deadline.IsZero() && time.Now().After(deadline)) {
			logs, _ := service.Logs(runType, runID, intOption(args, "--tail", 120))
			return printJSON(map[string]any{"status": status, "logs": logs})
		}
		time.Sleep(time.Duration(poll * float64(time.Second)))
	}
}

func runRuntimeSession(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: session start|status|logs|send|interrupt|stop|attach")
	}
	service := agentrun.New(cfg.Root)
	ctx := context.Background()
	switch args[0] {
	case "start":
		profile := optionValue(args[1:], "--profile")
		if profile == "" {
			profile = "tcx"
		}
		cwd := optionValue(args[1:], "--cwd")
		result, err := service.StartSessionWithOptions(ctx, agentrun.SessionOptions{
			Profile: profile, ProjectID: optionValue(args[1:], "--project"), CWD: cwd,
			RunID: optionValue(args[1:], "--run-id"), AllowedActions: repeatOption(args[1:], "--allowed-action"),
			ForbiddenActions: repeatOption(args[1:], "--forbidden-action"), Force: containsArg(args[1:], "--force"),
		})
		_ = printJSON(result)
		return err
	case "status":
		if len(args) < 2 {
			return fmt.Errorf("usage: session status <run_id>")
		}
		result, err := service.SessionStatus(ctx, args[1])
		if err != nil {
			return err
		}
		return printJSON(result)
	case "logs":
		if len(args) < 2 {
			return fmt.Errorf("usage: session logs <run_id>")
		}
		result, err := service.SessionLogs(ctx, args[1], intOption(args[2:], "--tail", 120))
		if err != nil {
			return err
		}
		return printJSON(result)
	case "watch":
		if len(args) < 2 {
			return fmt.Errorf("usage: session watch <run_id>")
		}
		return watchSession(service, args[1], args[2:])
	case "send":
		if len(args) < 2 {
			return fmt.Errorf("usage: session send <run_id> --text <text>")
		}
		submit := !containsArg(args[2:], "--no-submit")
		textValue := optionValue(args[2:], "--text")
		if textValue == "" {
			textArgs := make([]string, 0, len(args)-2)
			for _, arg := range args[2:] {
				if arg != "--no-submit" {
					textArgs = append(textArgs, arg)
				}
			}
			textValue = strings.Join(textArgs, " ")
		}
		if strings.TrimSpace(textValue) == "" {
			return fmt.Errorf("session send text is required")
		}
		result, err := service.SessionSend(ctx, args[1], textValue, submit)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "interrupt":
		if len(args) < 2 {
			return fmt.Errorf("usage: session interrupt <run_id>")
		}
		result, err := service.SessionInterrupt(ctx, args[1])
		if err != nil {
			return err
		}
		return printJSON(result)
	case "stop":
		if len(args) < 2 {
			return fmt.Errorf("usage: session stop <run_id>")
		}
		result, err := service.SessionStop(ctx, args[1])
		_ = printJSON(result)
		return err
	case "attach":
		if len(args) < 2 {
			return fmt.Errorf("usage: session attach <run_id>")
		}
		return service.SessionAttach(ctx, args[1])
	default:
		return fmt.Errorf("unknown session command: %s", args[0])
	}
}

func runLoopCommand(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: loop run|start|step|status|logs|cancel")
	}
	service := agentrun.New(cfg.Root)
	switch args[0] {
	case "run", "start":
		options, err := parseLoopOptions(args[1:])
		if err != nil {
			return err
		}
		var status agentrun.PersistentLoopStatus
		if args[0] == "run" {
			status, err = service.LoopRun(context.Background(), options)
		} else {
			status, err = service.LoopStart(options)
		}
		_ = printJSON(status)
		return err
	case "step":
		if len(args) < 2 {
			return fmt.Errorf("usage: loop step <loop_id>")
		}
		status, err := service.LoopStep(context.Background(), args[1])
		_ = printJSON(status)
		return err
	case "status":
		if len(args) < 2 {
			return fmt.Errorf("usage: loop status <loop_id>")
		}
		status, err := service.LoopStatus(args[1])
		if err != nil {
			return err
		}
		return printJSON(status)
	case "logs":
		if len(args) < 2 {
			return fmt.Errorf("usage: loop logs <loop_id>")
		}
		logs, err := service.LoopLogs(args[1], intOption(args[2:], "--tail", 120))
		if err != nil {
			return err
		}
		return printJSON(logs)
	case "cancel":
		if len(args) < 2 {
			return fmt.Errorf("usage: loop cancel <loop_id>")
		}
		status, err := service.LoopCancel(args[1])
		_ = printJSON(status)
		return err
	default:
		return fmt.Errorf("unknown loop command: %s", args[0])
	}
}

func watchSession(service *agentrun.Service, runID string, args []string) error {
	seconds := intOption(args, "--seconds", 0)
	poll := floatOption(args, "--poll-seconds", 1)
	deadline := time.Time{}
	if seconds > 0 {
		deadline = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	for {
		status, err := service.SessionStatus(context.Background(), runID)
		if err != nil {
			return err
		}
		if terminalState(status.State) || !status.Alive || (!deadline.IsZero() && time.Now().After(deadline)) {
			logs, _ := service.SessionLogs(context.Background(), runID, intOption(args, "--tail", 120))
			return printJSON(map[string]any{"status": status, "logs": logs})
		}
		time.Sleep(time.Duration(poll * float64(time.Second)))
	}
}

func runCommandCommand(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: command start|status|logs|watch|interrupt|stop|attach")
	}
	service := agentrun.New(cfg.Root)
	ctx := context.Background()
	switch args[0] {
	case "start":
		options, err := parseCommandOptions(args[1:])
		if err != nil {
			return err
		}
		result, err := service.StartCommand(ctx, options)
		_ = printJSON(result)
		return err
	case "status":
		if len(args) < 2 {
			return fmt.Errorf("usage: command status <run_id>")
		}
		result, err := service.CommandStatus(ctx, args[1])
		if err != nil {
			return err
		}
		return printJSON(result)
	case "logs":
		if len(args) < 2 {
			return fmt.Errorf("usage: command logs <run_id>")
		}
		result, err := service.CommandLogs(ctx, args[1], intOption(args[2:], "--tail", 120))
		if err != nil {
			return err
		}
		return printJSON(result)
	case "watch":
		if len(args) < 2 {
			return fmt.Errorf("usage: command watch <run_id>")
		}
		return watchCommand(service, args[1], args[2:])
	case "interrupt":
		if len(args) < 2 {
			return fmt.Errorf("usage: command interrupt <run_id>")
		}
		result, err := service.CommandInterrupt(ctx, args[1])
		_ = printJSON(result)
		return err
	case "stop":
		if len(args) < 2 {
			return fmt.Errorf("usage: command stop <run_id>")
		}
		result, err := service.CommandStop(ctx, args[1])
		_ = printJSON(result)
		return err
	case "attach":
		if len(args) < 2 {
			return fmt.Errorf("usage: command attach <run_id>")
		}
		return service.CommandAttach(ctx, args[1])
	default:
		return fmt.Errorf("unknown command lifecycle action: %s", args[0])
	}
}

func parseCommandOptions(args []string) (agentrun.CommandOptions, error) {
	options := agentrun.CommandOptions{Profile: "tcx", Label: "command"}
	for i := 0; i < len(args); i++ {
		if args[i] == "--" {
			options.Argv = append(options.Argv, args[i+1:]...)
			break
		}
		switch args[i] {
		case "--profile":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--profile requires value")
			}
			options.Profile = args[i]
		case "--project":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--project requires value")
			}
			options.ProjectID = args[i]
		case "--run-id":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--run-id requires value")
			}
			options.RunID = args[i]
		case "--cwd":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--cwd requires value")
			}
			options.CWD = args[i]
		case "--label":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--label requires value")
			}
			options.Label = args[i]
		case "--input":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--input requires value")
			}
			options.Input = args[i]
		case "--input-file":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--input-file requires value")
			}
			data, err := os.ReadFile(args[i])
			if err != nil {
				return options, err
			}
			options.Input = string(data)
		case "--deadline-seconds", "--deadline", "--timeout":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			value, err := strconv.Atoi(args[i])
			if err != nil {
				return options, err
			}
			options.DeadlineSeconds = value
		case "--force":
			options.Force = true
		default:
			options.Argv = append(options.Argv, args[i])
		}
	}
	if len(options.Argv) == 0 {
		return options, fmt.Errorf("command argv is required; use -- <command> [args...]")
	}
	return options, nil
}

func watchCommand(service *agentrun.Service, runID string, args []string) error {
	seconds := intOption(args, "--seconds", 0)
	poll := floatOption(args, "--poll-seconds", 1)
	deadline := time.Time{}
	if seconds > 0 {
		deadline = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	for {
		status, err := service.CommandStatus(context.Background(), runID)
		if err != nil {
			return err
		}
		if terminalState(status.State) || !status.Alive || (!deadline.IsZero() && time.Now().After(deadline)) {
			logs, _ := service.CommandLogs(context.Background(), runID, intOption(args, "--tail", 120))
			return printJSON(map[string]any{"status": status, "logs": logs})
		}
		time.Sleep(time.Duration(poll * float64(time.Second)))
	}
}

func runPruneCommand(cfg *config.Config, args []string) error {
	for _, arg := range args {
		if arg != "--apply" && arg != "--json" {
			return fmt.Errorf("unknown prune argument: %s", arg)
		}
	}
	result, err := agentrun.New(cfg.Root).Prune(!containsArg(args, "--apply"))
	if err != nil {
		return err
	}
	return printJSON(result)
}

func parseLoopOptions(args []string) (agentrun.LoopStartOptions, error) {
	options := agentrun.LoopStartOptions{MaxSteps: 10}
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--input":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--input requires value")
			}
			options.Input = args[i]
		case "--input-file":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--input-file requires value")
			}
			options.InputFile = args[i]
		case "--actions-file":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			data, err := os.ReadFile(args[i])
			if err != nil {
				return options, err
			}
			if err := json.Unmarshal(data, &options.Actions); err != nil {
				return options, err
			}
		case "--actions", "--actions-json":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--actions-json requires value")
			}
			if err := json.Unmarshal([]byte(args[i]), &options.Actions); err != nil {
				return options, err
			}
		case "--planner-profile":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--planner-profile requires value")
			}
			options.PlannerProfile = args[i]
		case "--max-steps":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--max-steps requires value")
			}
			value, err := strconv.Atoi(args[i])
			if err != nil {
				return options, err
			}
			options.MaxSteps = value
		case "--result-schema":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--result-schema requires value")
			}
			options.ResultSchema = args[i]
		case "--deadline-seconds", "--deadline", "--timeout":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("%s requires value", args[i-1])
			}
			value, err := strconv.Atoi(args[i])
			if err != nil {
				return options, err
			}
			options.DeadlineSeconds = value
		case "--capability":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--capability requires value")
			}
			options.Capabilities = append(options.Capabilities, args[i])
		case "--forbidden-action":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--forbidden-action requires value")
			}
			options.Forbidden = append(options.Forbidden, args[i])
		case "--project":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--project requires value")
			}
			options.ProjectID = args[i]
		case "--cwd":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--cwd requires value")
			}
			options.CWD = args[i]
		case "--loop-id":
			i++
			if i >= len(args) {
				return options, fmt.Errorf("--loop-id requires value")
			}
			options.LoopID = args[i]
		case "--force":
			options.Force = true
		default:
			if options.Input == "" {
				options.Input = args[i]
			} else {
				options.Input += " " + args[i]
			}
		}
	}
	return options, nil
}

func runCapabilitiesCommand(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: capabilities skills|tools|memory")
	}
	switch args[0] {
	case "tools":
		return runCapabilityTools(args[1:])
	case "skills":
		return runCapabilitySkills(cfg, args[1:])
	case "memory":
		return runCapabilityMemory(cfg, args[1:])
	default:
		return fmt.Errorf("unknown capability: %s", args[0])
	}
}

func runCapabilityTools(args []string) error {
	manager := capability.NewToolManager()
	dir := optionValue(args, "--dir")
	if dir == "" {
		dir = optionValue(args, "--tools-dir")
	}
	if dir != "" {
		manager.RegisterDir(dir)
	}
	command := "schemas"
	if len(args) > 0 && !strings.HasPrefix(args[0], "--") {
		command = args[0]
	}
	switch command {
	case "schemas":
		return printJSON(map[string]any{"ok": true, "tools": manager.Schemas(), "doctor": manager.Doctor()})
	case "call", "describe-external":
		if len(args) < 2 {
			return fmt.Errorf("usage: capabilities tools %s <name>", command)
		}
		arguments := map[string]any{}
		if raw := optionValue(args[2:], "--args"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
				return err
			}
		}
		var output any
		var err error
		if command == "describe-external" {
			output, err = manager.DescribeExternal(args[1], arguments, repeatOption(args[2:], "--capability"), repeatOption(args[2:], "--forbidden-action"))
		} else {
			output, err = manager.Call(args[1], arguments, repeatOption(args[2:], "--capability"), repeatOption(args[2:], "--forbidden-action"))
		}
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true, "output": output})
	default:
		return fmt.Errorf("unknown tools command: %s", command)
	}
}

func runCapabilitySkills(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: capabilities skills list|route|run")
	}
	manager := capability.NewSkillManager()
	dir := optionValue(args[1:], "--dir")
	if dir == "" {
		dir = optionValue(args[1:], "--skills-dir")
	}
	if dir == "" {
		dir = filepath.Join(cfg.Root, "skills")
	}
	manager.RegisterDir(dir)
	switch args[0] {
	case "list":
		return printJSON(map[string]any{"skills": manager.List(), "doctor": manager.Doctor()})
	case "route":
		query := optionValue(args[1:], "--query")
		if query == "" {
			query = strings.Join(nonOptionArgs(args[1:]), " ")
		}
		if strings.TrimSpace(query) == "" {
			return fmt.Errorf("--query is required")
		}
		skill, ok := manager.Route(query)
		return printJSON(map[string]any{"ok": true, "query": query, "matched": nullableSkill(skill, ok), "doctor": manager.Doctor()})
	case "run", "run-auto":
		query := optionValue(args[1:], "--query")
		var skill capability.Skill
		var err error
		if args[0] == "run-auto" {
			var ok bool
			skill, ok = manager.Route(query)
			if !ok {
				return fmt.Errorf("未命中 skill: %s", query)
			}
		} else {
			if len(args) < 2 {
				return fmt.Errorf("skill name is required")
			}
			skill, err = manager.Get(args[1])
			if err != nil {
				return err
			}
		}
		input := optionValue(args[1:], "--input")
		if inputFile := optionValue(args[1:], "--input-file"); inputFile != "" {
			data, readErr := os.ReadFile(inputFile)
			if readErr != nil {
				return readErr
			}
			input = string(data)
		}
		variables := map[string]any{}
		if raw := optionValue(args[1:], "--vars"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &variables); err != nil {
				return fmt.Errorf("parse --vars: %w", err)
			}
		}
		prompt, err := skill.Render(input, query, variables)
		if err != nil {
			return err
		}
		profile := optionValue(args[1:], "--profile")
		if profile == "" {
			profile = skill.DefaultProfile
		}
		service := agentrun.New(cfg.Root)
		run, runErr := service.Run(context.Background(), agentrun.RunOptions{
			RunType: agentrun.RunTask, Profile: profile, ProjectID: optionValue(args[1:], "--project"),
			RunID: optionValue(args[1:], "--run-id"), CWD: optionValue(args[1:], "--cwd"), Prompt: prompt,
			ExecutionMode: agentrun.ModeManaged, ResultSchema: optionValue(args[1:], "--result-schema"),
			DeadlineSeconds: intOption(args[1:], "--deadline-seconds", 0),
			AllowedActions:  repeatOption(args[1:], "--allowed-action"), ForbiddenActions: repeatOption(args[1:], "--forbidden-action"),
			Force: containsArg(args[1:], "--force"),
		})
		_ = printJSON(run)
		return runErr
	default:
		return fmt.Errorf("unknown skills command: %s", args[0])
	}
}

func runCapabilityMemory(cfg *config.Config, args []string) error {
	command := "demo"
	if len(args) > 0 {
		command = args[0]
	}
	path := optionValue(args, "--memory-file")
	if path == "" && command != "demo" {
		path = filepath.Join(cfg.Root, "runs", "global", "runtime", "state", "current", "memory.json")
	}
	memory, err := capability.OpenMemory(path)
	if err != nil {
		return err
	}
	switch command {
	case "demo":
		query := optionValue(args[1:], "--query")
		if query == "" {
			return fmt.Errorf("memory demo --query is required")
		}
		items := []capability.MemoryItem{}
		if raw := optionValue(args[1:], "--items"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &items); err != nil {
				return fmt.Errorf("parse --items: %w", err)
			}
		}
		if err := memory.Write(items); err != nil {
			return err
		}
		filters := map[string]any{}
		if raw := optionValue(args[1:], "--filters"); raw != "" {
			if err := json.Unmarshal([]byte(raw), &filters); err != nil {
				return fmt.Errorf("parse --filters: %w", err)
			}
		}
		kind, _ := filters["type"].(string)
		return printJSON(map[string]any{"ok": true, "recall": memory.Recall(query, kind, intOption(args[1:], "--top-k", 5)), "sources": memory.Sources()})
	case "write":
		if len(args) < 3 {
			return fmt.Errorf("usage: memory write <id> <content>")
		}
		err := memory.Write([]capability.MemoryItem{{ID: args[1], Type: optionDefault(args[3:], "--type", "fact"), Content: args[2], Source: optionValue(args[3:], "--source")}})
		if err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true})
	case "recall":
		if len(args) < 2 {
			return fmt.Errorf("usage: memory recall <query>")
		}
		return printJSON(map[string]any{"ok": true, "items": memory.Recall(args[1], optionValue(args[2:], "--type"), intOption(args[2:], "--top-k", 5))})
	case "forget":
		if len(args) < 2 {
			return fmt.Errorf("usage: memory forget <id...>")
		}
		if err := memory.Forget(args[1:]); err != nil {
			return err
		}
		return printJSON(map[string]any{"ok": true})
	case "sources":
		return printJSON(map[string]any{"ok": true, "sources": memory.Sources()})
	default:
		return fmt.Errorf("unknown memory command: %s", command)
	}
}

func terminalState(state string) bool {
	return state == agentrun.StateDone || state == agentrun.StateFailed || state == agentrun.StateBlocked || state == agentrun.StateCancelled
}
func containsArg(args []string, target string) bool {
	for _, arg := range args {
		if arg == target {
			return true
		}
	}
	return false
}
func optionValue(args []string, name string) string {
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			return args[i+1]
		}
	}
	return ""
}
func optionDefault(args []string, name, fallback string) string {
	if value := optionValue(args, name); value != "" {
		return value
	}
	return fallback
}
func intOption(args []string, name string, fallback int) int {
	value := optionValue(args, name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.Atoi(value)
	if err != nil {
		return fallback
	}
	return parsed
}
func floatOption(args []string, name string, fallback float64) float64 {
	value := optionValue(args, name)
	if value == "" {
		return fallback
	}
	parsed, err := strconv.ParseFloat(value, 64)
	if err != nil {
		return fallback
	}
	return parsed
}
func repeatOption(args []string, name string) []string {
	var out []string
	for i := 0; i+1 < len(args); i++ {
		if args[i] == name {
			out = append(out, args[i+1])
			i++
		}
	}
	return out
}
func nonOptionArgs(args []string) []string {
	var out []string
	for i := 0; i < len(args); i++ {
		if strings.HasPrefix(args[i], "--") {
			if i+1 < len(args) && !strings.HasPrefix(args[i+1], "--") {
				i++
			}
			continue
		}
		out = append(out, args[i])
	}
	return out
}
func nullableSkill(skill capability.Skill, ok bool) any {
	if !ok {
		return nil
	}
	return skill
}
