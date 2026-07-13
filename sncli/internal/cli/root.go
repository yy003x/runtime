package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"text/tabwriter"

	runtimecore "agent-arch/internal/runtime"
	"agent-arch/sncli/internal/config"
	"agent-arch/sncli/internal/doctor"
	"agent-arch/sncli/internal/native"
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
	case "tools":
		return exit(printTools(cfg))
	case "providers":
		return exit(printProviders())
	case "doctor":
		return exit(runDoctor(cfg, args[1:]))
	case "native":
		return exit(runNative(cfg, args[1:]))
	case "session":
		return exit(runSession(app, args[1:]))
	case "update", "upgrade":
		return exit(runUpdate(cfg, args[1:]))
	default:
		if entry, ok := cfg.Tools[args[0]]; ok {
			code, err := tool.Runner{Root: cfg.Root}.Run(args[0], entry, args[1:])
			if err != nil {
				return fail(err)
			}
			return code
		}
		if profileConfigExists(cfg.Root, args[0]) {
			return exit(runProfile(cfg, args))
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
		Runtime: legacyruntime.Client{Command: cfg.RuntimeCommandPath(), Root: cfg.Root},
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

func printProviders() error {
	payload := struct {
		Providers []string `json:"providers"`
		Source    string   `json:"source"`
	}{
		Providers: runtimecore.ProviderTypes(),
		Source:    "internal",
	}
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(payload)
}

func runProfile(cfg *config.Config, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("profile is required")
	}
	profile := args[0]
	sessionID := ""
	var promptParts []string
	for i := 1; i < len(args); i++ {
		switch args[i] {
		case "--session", "--session-id", "--session_id":
			i++
			if i >= len(args) {
				return fmt.Errorf("%s requires value", args[i-1])
			}
			sessionID = args[i]
		case "--":
			promptParts = append(promptParts, args[i+1:]...)
			i = len(args)
		default:
			promptParts = append(promptParts, args[i])
		}
	}

	prompt := strings.TrimSpace(strings.Join(promptParts, " "))
	if prompt == "" {
		return fmt.Errorf("prompt is required")
	}

	cwd, _ := os.Getwd()
	engine := runtimecore.NewEngine(cfg.Root)
	result, err := engine.Run(context.Background(), runtimecore.RunOptions{
		Profile:   profile,
		Prompt:    prompt,
		CWD:       cwd,
		SessionID: sessionID,
	})
	if result != nil {
		if result.FinalText != "" {
			fmt.Println(result.FinalText)
		}
		if runDir := result.Artifacts["run_dir"]; runDir != "" {
			fmt.Fprintf(os.Stderr, "[run:%s] %s\n", result.RunID, runDir)
		}
	}
	return err
}

func runDoctor(cfg *config.Config, args []string) error {
	report := doctor.Run(cfg)
	if len(args) > 0 && args[0] == "--json" {
		return doctor.PrintJSON(report)
	}
	for key, ok := range report.Checks {
		status := "ok"
		if !ok {
			status = "fail"
		}
		fmt.Printf("%s: %s\n", key, status)
	}
	if !report.OK {
		return fmt.Errorf("doctor failed")
	}
	return nil
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
	provider := cfg.DefaultProvider
	if len(args) >= 2 && args[0] == "--provider" {
		provider = args[1]
	} else if len(args) >= 1 {
		provider = args[0]
	}
	if cfg.NativeProfile(provider) == "" {
		return fmt.Errorf("unknown native provider: %s", provider)
	}
	cwd, _ := os.Getwd()
	return native.Open(cfg, provider, cwd)
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

func runSession(app repl.App, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: session list|resume <id>")
	}
	switch args[0] {
	case "list":
		items, err := app.Store.List()
		if err != nil {
			return err
		}
		tw := tabwriter.NewWriter(os.Stdout, 0, 0, 2, ' ', 0)
		fmt.Fprintln(tw, "ID\tPROVIDER\tUPDATED\tCWD")
		for _, item := range items {
			fmt.Fprintf(tw, "%s\t%s\t%s\t%s\n", item.ID, item.Provider, item.UpdatedAt.Format("2006-01-02 15:04:05"), item.CWD)
		}
		return tw.Flush()
	case "resume":
		if len(args) != 2 {
			return fmt.Errorf("usage: session resume <id>")
		}
		item, err := app.Store.Load(args[1])
		if err != nil {
			return err
		}
		return app.Start(item)
	default:
		return fmt.Errorf("unknown session command: %s", args[0])
	}
}

func printHelp() {
	fmt.Println(`sn-cli - Sinan terminal workbench

Usage:
  sn-cli
  sn-cli <profile> [--session-id SESSION_ID] <prompt>
  sn-cli cx [args...]
  sn-cli cc [args...]
  sn-cli chat
  sn-cli run [--provider codex|claude|fake] [--sandbox read-only|workspace-write] <prompt>
  sn-cli native [--provider codex|claude]
  sn-cli session list
  sn-cli session resume <id>
  sn-cli tools
  sn-cli providers
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

func debugJSON(v any) string {
	data, _ := json.MarshalIndent(v, "", "  ")
	return string(data)
}

func profileConfigExists(root, name string) bool {
	profileName := strings.TrimSpace(name)
	if profileName == "" {
		return false
	}
	if filepath.Base(profileName) != profileName || strings.Contains(profileName, "..") {
		return false
	}
	path := filepath.Join(root, "configs", profileName+".yaml")
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func rootRel(root, path string) string {
	rel, err := filepath.Rel(root, path)
	if err != nil {
		return path
	}
	return rel
}
