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
	"strings"

	"github.com/yy003x/runtime/internal/agentrun"
	"github.com/yy003x/runtime/internal/cli/config"
	snupdate "github.com/yy003x/runtime/internal/cli/update"
	"github.com/yy003x/runtime/internal/cli/version"
	"github.com/yy003x/runtime/internal/provider"
)

func Main(args []string) int {
	if len(args) == 0 {
		printHelp()
		return 0
	}
	switch args[0] {
	case "-h", "--help":
		printHelp()
		return 0
	case "--version":
		fmt.Println(version.String())
		return 0
	}
	cfg, err := config.Load()
	if err != nil {
		return fail(err)
	}
	switch args[0] {
	case "run":
		return exit(runRunNamespace(cfg, args[1:]))
	case "session":
		return exit(runSessionNamespace(cfg, args[1:]))
	case "profile":
		return exit(runProfileNamespace(cfg, args[1:]))
	case "system":
		return exit(runSystemNamespace(cfg, args[1:]))
	case "loop":
		return exit(runLoopNamespace(cfg, args[1:]))
	case "skill":
		return exit(runSkillNamespace(cfg, args[1:]))
	case "tool":
		return exit(runToolNamespace(cfg, args[1:]))
	case "memory":
		return exit(runMemoryNamespace(cfg, args[1:]))
	case "llm":
		return exit(runLLMNamespace(cfg, args[1:]))
	default:
		if profile, ok := resolveProfile(cfg.Home, args[0]); ok {
			code, runErr := runResolvedProfile(cfg, profile, args[1:])
			if runErr != nil {
				return fail(runErr)
			}
			return code
		}
		return fail(fmt.Errorf("unknown command %q", args[0]))
	}
}

func runResolvedProfile(cfg *config.Config, profile provider.Config, args []string) (int, error) {
	if profile.Type == provider.TypeCLI && profile.CLI != nil && profile.CLI.Executor == provider.ExecutorCommand {
		return runInteractiveProfile(cfg, profile, args)
	}
	if profile.Type == provider.TypeCLI {
		return 1, fmt.Errorf("profile %q has no direct command; use 'session open %s'", profile.ID, profile.ID)
	}
	return runUnrecordedProfile(cfg, profile, args)
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
	service := agentrun.New(cfg.Home)
	result, err := provider.ExecuteCLIInteractive(context.Background(), profile, request, cwd, service.DaemonClient())
	if err != nil {
		return 1, err
	}
	if result.ExitCode < 0 {
		return 1, nil
	}
	return result.ExitCode, nil
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
		})
	}
	sort.Slice(items, func(i, j int) bool { return fmt.Sprint(items[i]["id"]) < fmt.Sprint(items[j]["id"]) })
	return printJSON(map[string]any{"ok": true, "source": "configs", "profiles": items})
}

func runWithFollow(ctx context.Context, service *agentrun.Service, options agentrun.RunOptions) (agentrun.RunSummary, error) {
	submitted, err := service.Submit(ctx, options)
	if submitted.RunID == "" {
		return submitted, err
	}
	fmt.Fprintf(os.Stderr, "[run:%s] %s\n", submitted.RunID, submitted.RunDir)
	if err != nil {
		return submitted, err
	}
	followed, followErr := service.Follow(ctx, submitted.RunType, submitted.RunID, os.Stderr)
	if followed.FinalText == "" {
		followed.FinalText = submitted.FinalText
	}
	return followed, followErr
}

func stdinHasPrompt() bool {
	info, err := os.Stdin.Stat()
	return err == nil && info.Mode()&os.ModeCharDevice == 0
}

func applyStdinPrompt(prompt *string, promptFile string) error {
	stdinPrompt, err := readStdinPrompt()
	if err != nil {
		return err
	}
	if stdinPrompt == "" {
		return nil
	}
	if strings.TrimSpace(*prompt) != "" || strings.TrimSpace(promptFile) != "" {
		return fmt.Errorf("positional prompt, --prompt-file, and stdin are mutually exclusive")
	}
	*prompt = stdinPrompt
	return nil
}

func readStdinPrompt() (string, error) {
	if !stdinHasPrompt() {
		return "", nil
	}
	data, err := io.ReadAll(os.Stdin)
	if err != nil {
		return "", fmt.Errorf("read stdin prompt: %w", err)
	}
	return strings.TrimSpace(string(data)), nil
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
  sn-cli <profile> [native-cli-args...]
  sn-cli profile exec <profile> [provider-input...]
  sn-cli session run|submit [runtime-options...] <profile> [provider-input...]
  sn-cli session open [runtime-options...] <profile> [native-cli-args...]

Namespaces:
  sn-cli run list|show|logs|result|watch|cancel|reconcile
  sn-cli session run|submit|open|list|show|messages|events|logs|send|interrupt|stop|attach|configure|export|delete|gc
  sn-cli profile list|show|validate|command|exec
  sn-cli system doctor|start|status|stop|restart|update
  sn-cli loop run|list|show|logs|cancel
  sn-cli skill list|show|run
  sn-cli tool list|show|call
  sn-cli memory list|recall|add|remove|promote
  sn-cli llm generate --request-file <path|-> [--stream]

Global flags:
  -h, --help       Show this Runtime help.
  --version        Show the Git tag version and build metadata.

Execution:
  <CLI profile>      native direct execution in the current TTY; no Runtime record
  <API profile>      direct typed API request; no Runtime record
  profile exec       explicit unrecorded batch execution
  session run        synchronous recorded execution
  session submit     asynchronous recorded execution
  session open       recorded interactive carrier execution

Routing:
  CLI profile        every token after <profile> is passed to the native command
  API profile        --model/--max-tokens/--temperature/--stream and one prompt
  Session options    Runtime options must appear before <profile>
  Context input      --memory-file <json> injects read-only memory without rewriting the user message

Runtime home:      ${SN_CLI_HOME:-~/.sn}
Profile configs:   <runtime-home>/configs`)
}

func printUpdateHelp() {
	fmt.Println(`sn-cli system update - check and upgrade sn-cli

Usage:
  sn-cli system update --check
  sn-cli system update --dry-run
  sn-cli system update [--version VERSION]

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
