package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"text/tabwriter"

	"agent-arch/internal/agentrun"
	"agent-arch/sncli/internal/config"
	"agent-arch/sncli/internal/repl"
	legacyruntime "agent-arch/sncli/internal/runtime"
	"agent-arch/sncli/internal/session"
	"agent-arch/sncli/internal/tool"
	snupdate "agent-arch/sncli/internal/update"
	"agent-arch/sncli/internal/version"
)

func Main(args []string) int {
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	app := newApp(cfg)
	if len(args) == 0 {
		snupdate.MaybePrintHint(cfg, os.Stderr)
		return exit(app.Start(nil))
	}
	if shouldCheckUpdates(cfg, args[0]) {
		snupdate.MaybePrintHint(cfg, os.Stderr)
	}
	switch args[0] {
	case "-h", "--help", "help":
		printHelp()
	case "--version", "version":
		fmt.Println(version.String())
	case "chat":
		return exit(startChat(app, args[1:]))
	case "run":
		return exit(runOnce(cfg, app.Runtime, args[1:]))
	case "aliases":
		return exit(printTools(cfg))
	case "profiles", "providers":
		return exit(printProviders(cfg.Root))
	case "config":
		return exit(runConfigCommand(cfg, args[1:]))
	case "doctor":
		return exit(runRuntimeDoctor(cfg, args[1:]))
	case "task", "turn":
		return exit(runTaskCommand(cfg, args[0], args[1:]))
	case "loop":
		return exit(runLoopCommand(cfg, args[1:]))
	case "capabilities":
		return exit(runCapabilitiesCommand(cfg, args[1:]))
	case "tools":
		return exit(runCapabilitiesCommand(cfg, append([]string{"tools"}, args[1:]...)))
	case "native":
		return exit(runNative(cfg, args[1:]))
	case "session":
		return exit(runRuntimeSession(cfg, args[1:]))
	case "command":
		return exit(runCommandCommand(cfg, args[1:]))
	case "prune":
		return exit(runPruneCommand(cfg, args[1:]))
	case "update", "upgrade":
		return exit(runUpdate(cfg, args[1:]))
	default:
		if profileConfigExists(cfg.Root, args[0]) {
			return exit(runProfile(cfg, args))
		}
		if entry, ok := cfg.Tools[args[0]]; ok {
			code, err := tool.Runner{Root: cfg.Root}.Run(args[0], entry, args[1:])
			if err != nil {
				return fail(err)
			}
			return code
		}
		return fail(fmt.Errorf("unknown command %q", args[0]))
	}
	return 0
}

func startChat(app repl.App, args []string) error {
	provider := app.Config.DefaultProvider
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--provider":
			i++
			if i >= len(args) {
				return fmt.Errorf("--provider requires value")
			}
			provider = args[i]
		default:
			return fmt.Errorf("unknown chat argument: %s", args[i])
		}
	}
	cwd, _ := os.Getwd()
	item, err := app.Store.New(provider, cwd)
	if err != nil {
		return err
	}
	return app.Start(item)
}

func newApp(cfg *config.Config) repl.App {
	return repl.App{
		Config:  cfg,
		Store:   session.Store{Root: cfg.SessionsRoot()},
		Runtime: legacyruntime.Client{Root: cfg.Root},
	}
}

func runOnce(cfg *config.Config, client legacyruntime.Client, args []string) error {
	provider := cfg.DefaultProvider
	sandbox := cfg.Runtime.DefaultSandbox
	timeout := cfg.Runtime.DefaultTimeoutSeconds
	model := ""
	var promptParts []string
	for i := 0; i < len(args); i++ {
		switch args[i] {
		case "--provider":
			i++
			if i >= len(args) {
				return fmt.Errorf("--provider requires value")
			}
			provider = args[i]
		case "--sandbox":
			i++
			if i >= len(args) {
				return fmt.Errorf("--sandbox requires value")
			}
			sandbox = args[i]
		case "--model":
			i++
			if i >= len(args) {
				return fmt.Errorf("--model requires value")
			}
			model = args[i]
		default:
			promptParts = append(promptParts, args[i])
		}
	}
	prompt := strings.TrimSpace(strings.Join(promptParts, " "))
	if prompt == "" {
		return fmt.Errorf("prompt is required")
	}
	cwd, _ := os.Getwd()
	result, err := client.Run(legacyruntime.RunOptions{
		Provider: provider,
		Prompt:   prompt,
		CWD:      cwd,
		Sandbox:  sandbox,
		Timeout:  timeout,
		Model:    model,
	})
	if err != nil {
		return err
	}
	fmt.Println(result.FinalText)
	if runDir := result.Artifacts["run_dir"]; runDir != "" {
		fmt.Fprintf(os.Stderr, "[run:%s] %s\n", result.RunID, runDir)
	}
	if result.Outcome != "succeeded" {
		return fmt.Errorf("runtime outcome=%s failure=%s blocked=%s", result.Outcome, result.FailureReason, result.BlockedReason)
	}
	return nil
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
	service := agentrun.New(cfg.Root)
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

func printTools(cfg *config.Config) error {
	names := make([]string, 0, len(cfg.Tools))
	for name := range cfg.Tools {
		names = append(names, name)
	}
	sort.Strings(names)
	tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
	fmt.Fprintln(tw, "NAME\tCOMMAND\tDESCRIPTION")
	for _, name := range names {
		entry := cfg.Tools[name]
		fmt.Fprintf(tw, "%s\t%s\t%s\n", name, entry.Command, entry.Description)
	}
	return tw.Flush()
}

func runNative(cfg *config.Config, args []string) error {
	provider := "codex"
	if len(args) >= 2 && args[0] == "--provider" {
		provider = args[1]
	} else if len(args) >= 1 {
		provider = args[0]
	}
	profile := cfg.NativeProfile(provider)
	if profile == "" {
		return fmt.Errorf("unknown native provider: %s", provider)
	}
	cwd, _ := os.Getwd()
	service := agentrun.New(cfg.Root)
	started, err := service.StartSession(context.Background(), profile, cfg.Native.Project, cwd, "", false)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "runtime session: %s project=%s\n", started.RunID, started.ProjectID)
	return service.SessionAttach(context.Background(), started.RunID)
}

func runUpdate(cfg *config.Config, args []string) error {
	checkOnly := false
	jsonOutput := false
	dryRun := false
	installDir := ""
	ref := cfg.Update.Ref
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
		case "--install-dir":
			i++
			if i >= len(args) {
				return fmt.Errorf("--install-dir requires value")
			}
			installDir = args[i]
		case "--ref":
			i++
			if i >= len(args) {
				return fmt.Errorf("--ref requires value")
			}
			ref = args[i]
		default:
			return fmt.Errorf("unknown update argument: %s", args[i])
		}
	}
	if dryRun {
		fmt.Printf("root: %s\n", cfg.Root)
		fmt.Printf("ref: %s\n", ref)
		fmt.Printf("install script: %s\n", cfg.UpdateInstallScript())
		if installDir != "" {
			fmt.Printf("install dir: %s\n", installDir)
		} else {
			fmt.Println("install dir: installer default")
		}
		return nil
	}
	status := snupdate.Check(cfg)
	if jsonOutput {
		enc := json.NewEncoder(os.Stdout)
		enc.SetIndent("", "  ")
		if err := enc.Encode(status); err != nil {
			return err
		}
	}
	if checkOnly {
		if !jsonOutput {
			fmt.Println(status.Message)
			if status.CurrentCommit != "" {
				fmt.Printf("current: %s\n", status.CurrentCommit)
			}
			if status.LatestCommit != "" {
				fmt.Printf("latest:  %s\n", status.LatestCommit)
			}
		}
		if status.Error != "" {
			return errors.New(status.Error)
		}
		return nil
	}
	if status.Error != "" {
		return errors.New(status.Error)
	}
	if !status.UpdateAvailable {
		if !jsonOutput {
			fmt.Println("sn-cli already up to date")
		}
		return nil
	}
	return snupdate.Apply(cfg, installDir, ref)
}

func printHelp() {
	fmt.Println(`sn-cli - Go Agent Runtime

Usage:
  sn-cli
  sn-cli <cnf_id> [--session-id SESSION_ID] <prompt>
  sn-cli <cnf_id> [--prompt-file FILE] [--image FILE ...]
  sn-cli cx [args...]
  sn-cli cc [args...]
  sn-cli chat
  sn-cli run [--provider PROFILE_ID] [--sandbox read-only|workspace-write] <prompt>
  sn-cli native [--provider codex|claude]
  sn-cli task run|status|logs|watch|cancel
  sn-cli turn run|status|logs|cancel
  sn-cli loop run|start|step|status|logs|cancel
  sn-cli session start|status|logs|send|interrupt|stop|attach
  sn-cli command start|status|logs|watch|interrupt|stop|attach
  sn-cli prune [--apply]
  sn-cli capabilities skills|tools|memory
  sn-cli aliases
  sn-cli profiles
  sn-cli config choices|validate
  sn-cli doctor [--json]
  sn-cli update [--check|--dry-run|--install-dir DIR|--ref REF]
  sn-cli version

Binary path: cmd/sn-cli-wrapper
Build path:  runs/global/sn-cli/storage/current/bin/sn-cli`)
}

func printUpdateHelp() {
	fmt.Println(`sn-cli update - check and upgrade sn-cli

Usage:
  sn-cli update --check
  sn-cli update --dry-run
  sn-cli update [--install-dir DIR] [--ref REF]

Options:
  --check             Check remote version without upgrading.
  --json              Print check result as JSON.
  --dry-run           Print upgrade plan without running git or installer.
  --install-dir DIR   Pass install dir to scripts/install-sn-cli.sh.
  --ref REF           Git branch or tag to upgrade from.`)
}

func shouldCheckUpdates(cfg *config.Config, command string) bool {
	if !cfg.UpdateEnabled() {
		return false
	}
	switch command {
	case "chat", "native":
		return true
	}
	_, ok := cfg.Tools[command]
	return ok
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
	profileName := strings.TrimSpace(name)
	if profileName == "" {
		return false
	}
	if filepath.Base(profileName) != profileName || strings.Contains(profileName, "..") {
		return false
	}
	profiles, err := agentrun.New(root).Profiles()
	if err != nil {
		return false
	}
	if _, exists := profiles[profileName]; exists {
		return true
	}
	for _, profile := range profiles {
		for _, alias := range profile.Aliases {
			if alias == profileName {
				return true
			}
		}
	}
	return false
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
