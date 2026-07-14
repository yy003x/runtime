package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"

	"agent-runtime/internal/agentrun"
	"agent-runtime/internal/cli/config"
	snupdate "agent-runtime/internal/cli/update"
	"agent-runtime/internal/cli/version"
	"agent-runtime/internal/provider"
)

func Main(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	if len(args) == 0 {
		printHelp()
		return 0
	}
	switch args[0] {
	case "-h", "--help", "help":
		printHelp()
	case "--version", "version":
		fmt.Println(version.String())
	case "profiles", "providers":
		return exit(printProviders(cfg.Home))
	case "config":
		return exit(runConfigCommand(cfg, args[1:]))
	case "doctor":
		return exit(runRuntimeDoctor(cfg, args[1:]))
	case "daemon":
		return exit(runDaemonCommand(cfg, args[1:]))
	case "task", "turn":
		return exit(runTaskCommand(cfg, args[0], args[1:]))
	case "prompt":
		return exit(runManagedPrompt(cfg, args[1:]))
	case "loop":
		return exit(runLoopCommand(cfg, args[1:]))
	case "capabilities":
		return exit(runCapabilitiesCommand(cfg, args[1:]))
	case "tools":
		return exit(runCapabilitiesCommand(cfg, append([]string{"tools"}, args[1:]...)))
	case "session":
		return exit(runRuntimeSession(cfg, args[1:]))
	case "command":
		return exit(runCommandCommand(cfg, args[1:]))
	case "prune":
		return exit(runPruneCommand(cfg, args[1:]))
	case "update", "upgrade":
		return exit(runUpdate(cfg, args[1:]))
	default:
		if profile, ok := resolveProfile(cfg.Home, args[0]); ok {
			if profile.Type == provider.TypeCLI && profile.CLI != nil && profile.CLI.Executor == provider.ExecutorCommand {
				code, runErr := runInteractiveProfile(cfg, profile, args[1:])
				if runErr != nil {
					return fail(runErr)
				}
				return code
			}
			return exit(runProfile(cfg, args))
		}
		return fail(fmt.Errorf("unknown command %q", args[0]))
	}
	return 0
}

func runInteractiveProfile(cfg *config.Config, profile provider.Config, rawArgs []string) (int, error) {
	request, err := provider.PrepareInteractiveCLI(profile, rawArgs)
	if err != nil {
		return 1, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return 1, err
	}
	result, err := provider.ExecuteCLIInteractive(context.Background(), profile, request, cwd, agentrun.New(cfg.Home).DaemonClient())
	if err != nil {
		return 1, err
	}
	if result.ExitCode < 0 {
		return 1, nil
	}
	return result.ExitCode, nil
}

func runManagedPrompt(cfg *config.Config, args []string) error {
	profile := ""
	remaining := make([]string, 0, len(args))
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--":
			remaining = append(remaining, args[i:]...)
			i = len(args)
		case "-e", "--profile":
			i++
			if i >= len(args) {
				return fmt.Errorf("%s requires value", args[i-1])
			}
			profile = args[i]
		default:
			remaining = append(remaining, args[i])
		}
	}
	if profile == "" {
		return fmt.Errorf("prompt requires -e <profile>")
	}
	invocationArgs := append([]string{profile}, remaining...)
	invocation, err := parseProfileInvocation(invocationArgs)
	if err != nil {
		return err
	}
	if invocation.Prompt == "" && invocation.PromptFile == "" {
		info, statErr := os.Stdin.Stat()
		if statErr != nil || info.Mode()&os.ModeCharDevice != 0 {
			return fmt.Errorf("prompt is required")
		}
		data, readErr := io.ReadAll(os.Stdin)
		if readErr != nil {
			return readErr
		}
		if strings.TrimSpace(string(data)) == "" {
			return fmt.Errorf("prompt is required")
		}
		invocationArgs = append(invocationArgs, "--", string(data))
	}
	return runProfile(cfg, invocationArgs)
}

func printProviders(root string) error {
	profiles, err := agentrun.New(root).Profiles()
	if err != nil {
		return err
	}
	items := make([]map[string]any, 0, len(profiles))
	for _, profile := range profiles {
		items = append(items, map[string]any{
			"id": profile.ID, "type": profile.Type, "transport": profile.Transport(),
			"label": profile.Label, "result_contract": profile.ResultContract(),
		})
	}
	sort.Slice(items, func(i, j int) bool { return fmt.Sprint(items[i]["id"]) < fmt.Sprint(items[j]["id"]) })
	return printJSON(map[string]any{"ok": true, "source": "configs", "profiles": items})
}

func runProfile(cfg *config.Config, args []string) error {
	invocation, err := parseProfileInvocation(args)
	if err != nil {
		return err
	}

	cwd, _ := os.Getwd()
	overrides := cloneAnyMap(invocation.ProviderOverrides)
	if len(invocation.Images) > 0 {
		overrides["images"] = invocation.Images
	}
	if invocation.Model != "" {
		overrides["model"] = invocation.Model
	}
	if invocation.ReasoningEffort != "" {
		overrides["reasoning_effort"] = invocation.ReasoningEffort
	}
	service := agentrun.New(cfg.Home)
	result, err := service.Run(context.Background(), agentrun.RunOptions{
		RunType: agentrun.RunTask, RunID: invocation.RunID, Profile: invocation.Profile,
		Prompt: invocation.Prompt, PromptFile: invocation.PromptFile, CWD: cwd, Caller: invocation.SessionID,
		ExecutionMode: invocation.Mode, DeadlineSeconds: invocation.Timeout,
		ProviderOverrides: overrides, Force: invocation.Force,
	})
	if result.RunID != "" {
		if contract, readErr := service.ReadResult(agentrun.RunTask, result.RunID); readErr == nil && contract.Summary != "" {
			fmt.Println(contract.Summary)
		}
		fmt.Fprintf(os.Stderr, "[run:%s] %s\n", result.RunID, result.RunDir)
	}
	return err
}

type profileInvocation struct {
	Profile           string
	Prompt            string
	PromptFile        string
	Images            []string
	SessionID         string
	RunID             string
	Mode              string
	Model             string
	ReasoningEffort   string
	Timeout           int
	Force             bool
	ProviderOverrides map[string]any
}

func parseProfileInvocation(args []string) (profileInvocation, error) {
	if len(args) == 0 {
		return profileInvocation{}, fmt.Errorf("profile is required")
	}
	invocation := profileInvocation{Profile: args[0], Mode: agentrun.ModeManaged, ProviderOverrides: map[string]any{}}
	var promptParts []string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--session", "--session-id", "--session_id":
			i++
			if i >= len(args) {
				return profileInvocation{}, fmt.Errorf("%s requires value", args[i-1])
			}
			invocation.SessionID = args[i]
		case "--run-id":
			i++
			if i >= len(args) {
				return profileInvocation{}, fmt.Errorf("--run-id requires value")
			}
			invocation.RunID = args[i]
		case "--mode":
			i++
			if i >= len(args) {
				return profileInvocation{}, fmt.Errorf("--mode requires value")
			}
			invocation.Mode = args[i]
		case "--model":
			i++
			if i >= len(args) {
				return profileInvocation{}, fmt.Errorf("--model requires value")
			}
			invocation.Model = args[i]
		case "--reasoning-effort", "--codex-reasoning-effort":
			i++
			if i >= len(args) {
				return profileInvocation{}, fmt.Errorf("%s requires value", args[i-1])
			}
			invocation.ReasoningEffort = args[i]
		case "--timeout":
			i++
			if i >= len(args) {
				return profileInvocation{}, fmt.Errorf("--timeout requires value")
			}
			value, parseErr := strconv.Atoi(args[i])
			if parseErr != nil {
				return profileInvocation{}, fmt.Errorf("invalid --timeout: %w", parseErr)
			}
			invocation.Timeout = value
		case "--provider-overrides":
			i++
			if i >= len(args) {
				return profileInvocation{}, fmt.Errorf("--provider-overrides requires value")
			}
			if err := json.Unmarshal([]byte(args[i]), &invocation.ProviderOverrides); err != nil {
				return profileInvocation{}, fmt.Errorf("parse --provider-overrides: %w", err)
			}
		case "--force":
			invocation.Force = true
		case "--prompt-file", "--prompt_file":
			i++
			if i >= len(args) {
				return profileInvocation{}, fmt.Errorf("%s requires value", args[i-1])
			}
			invocation.PromptFile = args[i]
		case "--image":
			i++
			if i >= len(args) {
				return profileInvocation{}, fmt.Errorf("--image requires value")
			}
			invocation.Images = append(invocation.Images, args[i])
		case "--":
			promptParts = append(promptParts, args[i+1:]...)
			i = len(args)
		default:
			promptParts = append(promptParts, args[i])
		}
	}
	invocation.Prompt = strings.TrimSpace(strings.Join(promptParts, " "))
	if invocation.Prompt != "" && strings.TrimSpace(invocation.PromptFile) != "" {
		return profileInvocation{}, fmt.Errorf("inline prompt and --prompt-file cannot be used together")
	}
	return invocation, nil
}

func runUpdate(cfg *config.Config, args []string) error {
	checkOnly := false
	jsonOutput := false
	dryRun := false
	targetVersion := ""
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "-h", "--help":
			printUpdateHelp()
			return nil
		case "--check":
			checkOnly = true
		case "--json":
			jsonOutput = true
		case "--dry-run":
			dryRun = true
		case "--version":
			i++
			if i >= len(args) {
				return fmt.Errorf("--version requires value")
			}
			targetVersion = args[i]
		default:
			return fmt.Errorf("unknown update argument: %s", args[i])
		}
	}
	if dryRun {
		versionLabel := targetVersion
		if versionLabel == "" {
			versionLabel = "<latest-version>"
		}
		archive, archiveURL, checksumURL, err := snupdate.Plan(cfg, versionLabel)
		if err != nil {
			return err
		}
		fmt.Printf("home: %s\nversion: %s\narchive: %s\narchive url: %s\nchecksums url: %s\n", cfg.Home, versionLabel, archive, archiveURL, checksumURL)
		return nil
	}
	var status snupdate.Status
	if checkOnly || targetVersion == "" {
		status = snupdate.Check(context.Background(), cfg, version.Version)
	}
	if jsonOutput && (checkOnly || targetVersion == "") {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(status); err != nil {
			return err
		}
	}
	if checkOnly {
		if !jsonOutput {
			fmt.Println(status.Message)
			if status.CurrentVersion != "" {
				fmt.Printf("current: %s\n", status.CurrentVersion)
			}
			if status.LatestVersion != "" {
				fmt.Printf("latest:  %s\n", status.LatestVersion)
			}
		}
		if status.Error != "" {
			return errors.New(status.Error)
		}
		return nil
	}
	if targetVersion == "" {
		if status.Error != "" {
			return errors.New(status.Error)
		}
		if !status.UpdateAvailable {
			if !jsonOutput {
				fmt.Println("sn-cli already up to date")
			}
			return nil
		}
		targetVersion = status.LatestVersion
	}
	result, err := snupdate.Apply(context.Background(), cfg, targetVersion)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"ok": true, "update": result})
}

func printHelp() {
	fmt.Println(`sn-cli - Go Agent Runtime

Usage:
  sn-cli cx [args...]
  sn-cli cc [args...]
  sn-cli prompt -e <profile> [prompt...]
  sn-cli task run|status|logs|watch|block|continue|patch-resume|stop|cancel
  sn-cli turn run|status|logs|cancel
  sn-cli loop run|start|step|status|logs|cancel
  sn-cli session start|status|logs|send|interrupt|stop|attach
  sn-cli command start|status|logs|watch|interrupt|stop|attach
  sn-cli prune [--apply]
  sn-cli capabilities skills|tools|memory
  sn-cli profiles
  sn-cli config choices|validate
  sn-cli doctor [--json]
  sn-cli doctor daemon --json
  sn-cli daemon start|status|stop|restart
  sn-cli update [--check|--dry-run|--version VERSION]
  sn-cli version

Installed binary: ~/.sn/bin/sn-cli
Configuration:    ~/.sn/configs`)
}

func printUpdateHelp() {
	fmt.Println(`sn-cli update - check and upgrade sn-cli

Usage:
  sn-cli update --check
  sn-cli update --dry-run
  sn-cli update [--version VERSION]

Options:
  --check             Check remote version without upgrading.
  --json              Print check result as JSON.
  --dry-run           Print release download plan without changing files.
  --version VERSION   Install a specific GitHub Release tag.`)
}

func exit(err error) int {
	if err != nil {
		return fail(err)
	}
	return 0
}

func fail(err error) int {
	fmt.Fprintf(os.Stderr, "error: %v\n", err)
	return 1
}

func profileConfigExists(root, name string) bool {
	_, ok := resolveProfile(root, name)
	return ok
}

func resolveProfile(root, name string) (provider.Config, bool) {
	profileName := strings.TrimSpace(name)
	if profileName == "" {
		return provider.Config{}, false
	}
	if filepath.Base(profileName) != profileName || strings.Contains(profileName, "..") {
		return provider.Config{}, false
	}
	profiles, err := agentrun.New(root).Profiles()
	if err != nil {
		return provider.Config{}, false
	}
	return provider.Resolve(profiles, profileName)
}

func printJSON(value any) error {
	encoder := json.NewEncoder(os.Stdout)
	encoder.SetIndent("", "  ")
	return encoder.Encode(value)
}

func cloneAnyMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = value
	}
	return out
}
