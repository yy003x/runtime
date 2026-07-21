package cli

import (
	"context"
	"fmt"
	"strings"
	"time"

	"agent-runtime/internal/agentrun"
	"agent-runtime/internal/cli/config"
	"agent-runtime/internal/installbundle"
	"agent-runtime/internal/provider"
)

func runRunNamespace(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: run list|show|logs|result|watch|cancel|reconcile")
	}
	service := agentrun.New(cfg.Home)
	switch args[0] {
	case "list", "reconcile":
		return runRunRegistryAction(cfg, args)
	case "show":
		runID, runType, err := resolvePublicRun(service, args[1:], nil)
		if err != nil {
			return err
		}
		value, _, err := publicRunStatus(context.Background(), service, runType, runID)
		if err != nil {
			return err
		}
		return printJSON(value)
	case "logs":
		runID, runType, err := resolvePublicRun(service, args[1:], map[string]bool{"--tail": true})
		if err != nil {
			return err
		}
		logs, err := publicRunLogs(context.Background(), service, runType, runID, intOption(args[1:], "--tail", 120))
		if err != nil {
			return err
		}
		return printJSON(logs)
	case "result":
		runID, runType, err := resolvePublicRun(service, args[1:], nil)
		if err != nil {
			return err
		}
		result, err := service.ReadResult(runType, runID)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "watch":
		runID, runType, err := resolvePublicRun(service, args[1:], map[string]bool{"--seconds": true, "--poll-seconds": true, "--tail": true})
		if err != nil {
			return err
		}
		return watchPublicRun(service, runType, runID, args[1:])
	case "cancel":
		runID, runType, err := resolvePublicRun(service, args[1:], nil)
		if err != nil {
			return err
		}
		value, err := cancelPublicRun(context.Background(), service, runType, runID)
		_ = printJSON(value)
		return err
	default:
		return fmt.Errorf("unknown run action: %s", args[0])
	}
}

func resolvePublicRun(service *agentrun.Service, args []string, valueOptions map[string]bool) (string, string, error) {
	runID, err := parseRequiredID(args, "--run-id", valueOptions, nil)
	if err != nil {
		return "", "", err
	}
	runType, err := service.ResolveRunType(runID)
	if err != nil {
		return "", "", err
	}
	if runType == "loop" {
		return "", "", fmt.Errorf("run %s belongs to loop; use the loop namespace", runID)
	}
	return runID, runType, nil
}

func publicRunStatus(ctx context.Context, service *agentrun.Service, runType, runID string) (any, string, error) {
	switch runType {
	case agentrun.RunTask, agentrun.RunTurn:
		status, err := service.Status(runType, runID)
		return status, status.State, err
	case agentrun.RunCommand:
		status, err := service.CommandStatus(ctx, runID)
		return status, status.State, err
	case agentrun.RunSession:
		status, err := service.SessionStatus(ctx, runID)
		return status, status.State, err
	default:
		return nil, "", fmt.Errorf("unsupported run type: %s", runType)
	}
}

func publicRunLogs(ctx context.Context, service *agentrun.Service, runType, runID string, tail int) (agentrun.Logs, error) {
	switch runType {
	case agentrun.RunTask, agentrun.RunTurn:
		return service.Logs(runType, runID, tail)
	case agentrun.RunCommand:
		return service.CommandLogs(ctx, runID, tail)
	case agentrun.RunSession:
		return service.SessionLogs(ctx, runID, tail)
	default:
		return agentrun.Logs{}, fmt.Errorf("unsupported run type: %s", runType)
	}
}

func cancelPublicRun(ctx context.Context, service *agentrun.Service, runType, runID string) (any, error) {
	switch runType {
	case agentrun.RunTask, agentrun.RunTurn:
		return service.Cancel(runType, runID)
	case agentrun.RunCommand:
		return service.CommandStop(ctx, runID)
	case agentrun.RunSession:
		return service.SessionStop(ctx, runID)
	default:
		return nil, fmt.Errorf("unsupported run type: %s", runType)
	}
}

func watchPublicRun(service *agentrun.Service, runType, runID string, args []string) error {
	seconds := intOption(args, "--seconds", 0)
	poll := floatOption(args, "--poll-seconds", 1)
	deadline := time.Time{}
	if seconds > 0 {
		deadline = time.Now().Add(time.Duration(seconds) * time.Second)
	}
	for {
		status, state, err := publicRunStatus(context.Background(), service, runType, runID)
		if err != nil {
			return err
		}
		if terminalState(state) || (!deadline.IsZero() && time.Now().After(deadline)) {
			logs, _ := publicRunLogs(context.Background(), service, runType, runID, intOption(args, "--tail", 120))
			return printJSON(map[string]any{"status": status, "logs": logs})
		}
		time.Sleep(time.Duration(poll * float64(time.Second)))
	}
}

func runProfileNamespace(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: profile list|show|validate|command")
	}
	switch args[0] {
	case "list":
		if len(args) != 1 {
			return fmt.Errorf("profile list does not accept arguments")
		}
		return printProviders(cfg.Home)
	case "show":
		if len(args) != 2 || strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("profile show requires a profile as its third argument")
		}
		profiles, err := agentrun.New(cfg.Home).Profiles()
		if err != nil {
			return err
		}
		profile, ok := provider.Resolve(profiles, args[1])
		if !ok {
			return fmt.Errorf("unknown profile %q", args[1])
		}
		return printJSON(profilePublicView(profile, profiles))
	case "validate":
		rest := args[1:]
		profileID := ""
		if len(rest) > 0 && !strings.HasPrefix(rest[0], "-") {
			profileID = rest[0]
			rest = rest[1:]
		}
		live := false
		for _, arg := range rest {
			if arg != "--live" {
				return fmt.Errorf("unknown profile validate option: %s", arg)
			}
			live = true
		}
		return runProfileValidation(cfg, profileID, live)
	case "command":
		if len(args) < 2 || strings.HasPrefix(args[1], "-") {
			return fmt.Errorf("profile command requires a profile as its third argument")
		}
		jsonOutput := false
		for _, arg := range args[2:] {
			if arg != "--json" {
				return fmt.Errorf("unknown profile command option: %s", arg)
			}
			jsonOutput = true
		}
		return printProfileCommand(cfg, args[1], jsonOutput)
	default:
		return fmt.Errorf("unknown profile action: %s", args[0])
	}
}

func profilePublicView(profile provider.Config, profiles map[string]provider.Config) map[string]any {
	view := map[string]any{
		"id": profile.ID, "label": profile.Label, "type": profile.Type, "transport": profile.Transport(),
		"timeout_seconds": profile.TimeoutSeconds,
		"validation":      validateProfile(profile, profiles, false),
	}
	if profile.CLI != nil {
		view["driver"] = profile.CLI.Driver
		view["executor"] = profile.CLI.Executor
	}
	if profile.API != nil {
		view["protocol"] = profile.API.Protocol
		view["model"] = profile.API.Model
	}
	if profile.Native != nil {
		view["model_profile"] = profile.Native.ModelProfile
		view["persona"] = profile.Native.Persona
	}
	return view
}

func runSystemNamespace(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: system doctor|start|status|stop|restart|migrate-config|update")
	}
	switch args[0] {
	case "doctor":
		for _, arg := range args[1:] {
			if arg != "--json" {
				return fmt.Errorf("unknown system doctor option: %s", arg)
			}
		}
		return runRuntimeDoctor(cfg, args[1:])
	case "serve", "start", "status", "stop", "restart":
		if len(args) != 1 {
			return fmt.Errorf("system %s does not accept arguments", args[0])
		}
		return runDaemonCommand(cfg, []string{args[0]})
	case "migrate-config":
		if len(args) != 1 {
			return fmt.Errorf("system migrate-config does not accept arguments")
		}
		result, err := installbundle.MigrateProfileConfigs(cfg.Paths.ConfigDir)
		if err != nil {
			return err
		}
		return printJSON(result)
	case "update":
		return runUpdate(cfg, args[1:])
	default:
		return fmt.Errorf("unknown system action: %s", args[0])
	}
}

func runSessionNamespace(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: session run|submit|open|list|show|messages|events|logs|send|interrupt|stop|attach|configure|export|delete")
	}
	switch args[0] {
	case "run", "submit":
		return runManagedSessionAction(cfg, args[0], args[1:])
	case "open":
		return runSessionOpen(cfg, args[1:])
	case "list", "show", "messages", "events", "configure", "export", "delete":
		return runSessionHistory(cfg, args)
	case "logs", "send", "interrupt", "stop", "attach":
		return runSessionCarrierAction(cfg, args[0], args[1:])
	default:
		return fmt.Errorf("unknown session action: %s", args[0])
	}
}

func runSessionCarrierAction(cfg *config.Config, action string, args []string) error {
	service := agentrun.New(cfg.Home)
	ctx := context.Background()
	if action == "send" {
		return runSessionSendByID(ctx, service, args)
	}
	valueOptions := map[string]bool{}
	activeOnly := action != "logs"
	if action == "logs" {
		valueOptions["--tail"] = true
	}
	sessionID, err := parseRequiredID(args, "--session-id", valueOptions, nil)
	if err != nil {
		return err
	}
	runID, err := service.ResolveSessionCarrierRun(ctx, sessionID, activeOnly)
	if err != nil {
		return err
	}
	switch action {
	case "logs":
		logs, logsErr := service.SessionLogs(ctx, runID, intOption(args, "--tail", 120))
		if logsErr != nil {
			return logsErr
		}
		return printJSON(logs)
	case "interrupt":
		value, interruptErr := service.SessionInterrupt(ctx, runID)
		if interruptErr != nil {
			return interruptErr
		}
		return printJSON(value)
	case "stop":
		value, stopErr := service.SessionStop(ctx, runID)
		_ = printJSON(value)
		return stopErr
	case "attach":
		return service.SessionAttach(ctx, runID)
	default:
		return fmt.Errorf("unknown session carrier action: %s", action)
	}
}

func runSessionSendByID(ctx context.Context, service *agentrun.Service, args []string) error {
	sessionID, textValue, promptFile, submit := "", "", "", true
	var promptParts []string
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--session-id":
			index++
			if index >= len(args) {
				return fmt.Errorf("--session-id requires value")
			}
			sessionID = args[index]
		case "--prompt-file":
			index++
			if index >= len(args) {
				return fmt.Errorf("--prompt-file requires value")
			}
			promptFile = args[index]
		case "--no-submit":
			submit = false
		default:
			if strings.HasPrefix(args[index], "-") {
				return fmt.Errorf("unknown session send option: %s", args[index])
			}
			promptParts = append(promptParts, args[index])
		}
	}
	if sessionID == "" {
		return fmt.Errorf("--session-id is required")
	}
	textValue = strings.TrimSpace(strings.Join(promptParts, " "))
	if textValue != "" && strings.TrimSpace(promptFile) != "" {
		return fmt.Errorf("positional prompt and --prompt-file are mutually exclusive")
	}
	if err := applyStdinPrompt(&textValue, promptFile); err != nil {
		return err
	}
	if promptFile != "" {
		resolved, _, resolveErr := resolvePromptForCLI(promptFile)
		if resolveErr != nil {
			return resolveErr
		}
		textValue = resolved
	}
	if strings.TrimSpace(textValue) == "" {
		return fmt.Errorf("session send prompt is required")
	}
	runID, err := service.ResolveSessionCarrierRun(ctx, sessionID, true)
	if err != nil {
		return err
	}
	value, err := service.SessionSend(ctx, runID, textValue, submit)
	if err != nil {
		return err
	}
	return printJSON(value)
}

func runManagedSessionAction(cfg *config.Config, action string, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("session %s requires a profile as its third argument", action)
	}
	for _, option := range []string{"-c", "--config"} {
		if hasOptionBeforeSeparator(args[1:], option) {
			return fmt.Errorf("%s is not supported here; the profile is already %q", option, args[0])
		}
	}
	parseArgs := []string{"-c", args[0]}
	parseArgs = append(parseArgs, args[1:]...)
	options, err := parseRunOptions(agentrun.RunTurn, parseArgs)
	if err != nil {
		return err
	}
	if err := applyStdinPrompt(&options.Prompt, options.PromptFile); err != nil {
		return err
	}
	if options.RecordMode == agentrun.RecordOff {
		return fmt.Errorf("session %s does not allow --record-mode off; use '<profile> <prompt>' for direct execution", action)
	}
	options.Caller = "cli.session." + action
	options.CreateSession = true
	options.ExecutionMode = agentrun.ModeManaged
	if options.Retention == "" {
		options.Retention = agentrun.RetentionStandard
	}
	service := agentrun.New(cfg.Home)
	var result agentrun.RunSummary
	if action == "submit" {
		result, err = service.Submit(context.Background(), options)
	} else {
		result, err = runWithFollow(context.Background(), service, options)
	}
	_ = printJSON(result)
	return err
}

func runSessionOpen(cfg *config.Config, args []string) error {
	if len(args) == 0 || strings.HasPrefix(args[0], "-") {
		return fmt.Errorf("session open requires a profile as its third argument")
	}
	options, err := parseSessionStartOptions(args)
	if err != nil {
		return err
	}
	if err := applyStdinPrompt(&options.Prompt, options.PromptFile); err != nil {
		return err
	}
	if options.Retention == "" {
		options.Retention = agentrun.RetentionStandard
	}
	result, err := agentrun.New(cfg.Home).StartSessionWithOptions(context.Background(), options)
	_ = printJSON(result)
	return err
}
