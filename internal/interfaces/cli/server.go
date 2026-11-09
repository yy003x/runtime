package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/yy003x/sn-code/internal/application/activation"
	"github.com/yy003x/sn-code/internal/application/runtimebootstrap"
	"github.com/yy003x/sn-code/internal/application/serverruntime"
	"github.com/yy003x/sn-code/internal/infrastructure/buildinfo"
	"github.com/yy003x/sn-code/internal/infrastructure/layout"
	"github.com/yy003x/sn-code/internal/interfaces/cli/config"
	snupdate "github.com/yy003x/sn-code/internal/interfaces/cli/update"
	"github.com/yy003x/sn-code/pkg/contract"
)

func runServerNamespace(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	if len(args) == 0 {
		return cliValidationf(
			"usage: server info|start|status|stop",
		)
	}
	switch args[0] {
	case "info", "start", "status", "stop":
		if len(args) != 1 {
			return cliValidationf("server %s does not accept arguments", args[0])
		}
	}
	switch args[0] {
	case "info":
		core, err := runtimebootstrap.LoadProfileServices(
			paths, fixedNamespaces...,
		)
		if err != nil {
			return err
		}
		result := map[string]any{
			"schema_version":   cliOutputSchemaVersion,
			"contract_version": cliOutputContractVersion,
			"version":          buildinfo.String("sn-cli"), "runtime_home": paths.Home,
			"profiles":           core.Profiles.Entries(),
			"run_database":       paths.RunDBFile,
			"configured_address": serverAddress(),
			"namespaces":         serverNamespaces(),
			"capabilities":       serverCapabilities(),
		}
		if output.JSON() {
			return output.writeJSON(result)
		}
		if err := output.line("Runtime: %s", buildinfo.String("sn-cli")); err != nil {
			return err
		}
		if err := output.line("Runtime home: %s", paths.Home); err != nil {
			return err
		}
		if err := output.line(
			"Configured address: %s", serverAddress(),
		); err != nil {
			return err
		}
		if err := output.line(
			"Profiles: %d", len(core.Profiles.Entries()),
		); err != nil {
			return err
		}
		return output.line("Run database: %s", paths.RunDBFile)
	case "start":
		return startServer(paths, output)
	case "status":
		return serverStatus(paths, output)
	case "stop":
		return stopServer(paths, output)
	case "upgrade-activate":
		return runUpgradeActivate(paths, args[1:], output)
	default:
		return cliValidationf("unknown server action %q", args[0])
	}
}

func runUpdateNamespace(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	if len(args) > 0 && args[0] == "upgrade-check" {
		return runUpgradeCheck(paths, args[1:], output)
	}
	options, err := parseUpdateOptions(args)
	if err != nil {
		return cliValidation(err)
	}
	cfg, err := config.Load()
	if err != nil {
		return err
	}
	return executeUpdate(cfg, options, output)
}

func runUpgradeCheck(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	resources := paths.ResourcesDir
	seenResources := false
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--resources":
			if seenResources {
				return cliValidationf("--resources may only be specified once")
			}
			seenResources = true
			value, next, err := serverOptionValue(args, index, "--resources")
			if err != nil {
				return err
			}
			resources = value
			index = next
		default:
			return cliValidationf("unknown upgrade-check argument %s", args[index])
		}
	}
	if err := activation.UpgradePreflight(
		context.Background(), paths.Home, resources,
	); err != nil {
		return err
	}
	result := map[string]any{
		"ready": true, "runtime_home": paths.Home,
	}
	if output.JSON() {
		return output.writeJSON(result)
	}
	return output.line("Upgrade preflight: READY")
}

func runUpgradeActivate(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	payload := ""
	target := ""
	commandLink := ""
	overwrite := false
	localSourceInstall := false
	coordinatorPID := 0
	seen := make(map[string]struct{})
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--payload", "--target-home", "--command-link", "--coordinator-pid":
			name := args[index]
			if _, exists := seen[name]; exists {
				return cliValidationf("%s may only be specified once", name)
			}
			seen[name] = struct{}{}
			raw, next, err := serverOptionValue(args, index, name)
			if err != nil {
				return err
			}
			index = next
			switch name {
			case "--payload":
				payload = raw
			case "--target-home":
				target = raw
			case "--command-link":
				commandLink = raw
			case "--coordinator-pid":
				value, parseErr := strconv.Atoi(raw)
				if parseErr != nil || value <= 0 {
					return cliValidationf(
						"--coordinator-pid must be a positive integer",
					)
				}
				coordinatorPID = value
			}
		case "--overwrite-configs":
			if _, exists := seen[args[index]]; exists {
				return cliValidationf(
					"--overwrite-configs may only be specified once",
				)
			}
			seen[args[index]] = struct{}{}
			overwrite = true
		case "--local-source-install":
			if _, exists := seen[args[index]]; exists {
				return cliValidationf(
					"--local-source-install may only be specified once",
				)
			}
			seen[args[index]] = struct{}{}
			localSourceInstall = true
		default:
			return cliValidationf("unknown upgrade-activate argument %s", args[index])
		}
	}
	if payload == "" || target == "" {
		return cliValidationf(
			"upgrade-activate requires --payload and --target-home",
		)
	}
	if localSourceInstall && overwrite {
		return cliValidationf(
			"--local-source-install cannot be combined with --overwrite-configs",
		)
	}
	if localSourceInstall {
		overwrite = true
	}
	targetAbsolute, err := layout.CanonicalHome(target)
	if err != nil {
		return err
	}
	if filepath.Clean(targetAbsolute) != filepath.Clean(paths.Home) {
		return cliValidationf(
			"upgrade target %s does not match SN_CLI_HOME %s",
			targetAbsolute, paths.Home,
		)
	}
	expectedCommandTarget := filepath.Join(
		targetAbsolute, "bin", "sn-cli",
	)
	var commandReservation *activation.CommandLinkReservation
	if commandLink != "" {
		if err := validateActivationCommandLink(
			targetAbsolute, commandLink, expectedCommandTarget,
		); err != nil {
			return err
		}
		commandReservation, err = activation.ReserveCommandLink(
			commandLink, expectedCommandTarget,
		)
		if err != nil {
			return err
		}
	}
	executable, err := os.Executable()
	if err != nil {
		if commandReservation != nil {
			return errors.Join(err, commandReservation.Release())
		}
		return err
	}
	var stopServerForInstall func() error
	var closeNativeTUISessionsForInstall func() error
	var inspectServerForInstall func() (
		activation.ManagedServerProcess,
		error,
	)
	if localSourceInstall {
		inspectServerForInstall = func() (activation.ManagedServerProcess, error) {
			return serverruntime.InspectForInstall(paths)
		}
		stopServerForInstall = func() error { return serverruntime.StopForInstall(paths) }
		closeNativeTUISessionsForInstall = func() error {
			manager, managerErr := runtimebootstrap.LoadTmuxService(paths)
			if managerErr != nil {
				return managerErr
			}
			_, closeErr := closeAllSessionNativeTUILifecycles(
				context.Background(), paths, manager,
			)
			if isForeignTmuxCarrier(closeErr) {
				// A default-server sn-session owned by another Runtime home
				// cannot contain bindings for this home. It is intentionally
				// ignored by activation preflight as well.
				return nil
			}
			return closeErr
		}
	}
	result, err := activation.UpgradeActivate(
		context.Background(),
		activation.UpgradeRequest{
			TargetHome: targetAbsolute, PayloadDir: payload,
			CandidateBinary: executable, OverwriteConfig: overwrite,
			LocalSourceInstall:     localSourceInstall,
			CloseNativeTUISessions: closeNativeTUISessionsForInstall,
			InspectServer:          inspectServerForInstall,
			StopServer:             stopServerForInstall,
			CoordinatorPID:         coordinatorPID,
		},
	)
	if err != nil {
		if commandReservation != nil {
			return errors.Join(err, commandReservation.Release())
		}
		return err
	}
	if commandReservation != nil {
		if err := commandReservation.Commit(); err != nil {
			return fmt.Errorf(
				"Runtime activated in %s, but command link reservation changed: %w",
				targetAbsolute, err,
			)
		}
	}
	if output.JSON() {
		return output.writeJSON(map[string]any{
			"activated": true, "activation": result,
		})
	}
	return output.line(
		"Activated contract v%d in %s",
		result.ContractVersion, result.TargetHome,
	)
}

func isForeignTmuxCarrier(err error) bool {
	var runtimeErr *contract.RuntimeError
	return errors.As(err, &runtimeErr) &&
		runtimeErr.Phase == contract.PhaseTransport &&
		runtimeErr.Code == contract.ErrorConflict &&
		(strings.Contains(
			runtimeErr.Message,
			"Tmux session \"sn-session\" does not match this Runtime home",
		) || strings.Contains(
			runtimeErr.Message,
			"Tmux session \"sn-session\" already exists but is not owned by this Runtime home",
		))
}

func validateActivationCommandLink(
	targetHome string,
	commandLink string,
	expectedTarget string,
) error {
	if filepath.Base(commandLink) != "sn-cli" {
		return cliValidationf("command link must be named sn-cli")
	}
	if err := activation.ValidateCommandLink(
		commandLink, expectedTarget,
	); err != nil {
		return err
	}
	resolvedParent, err := filepath.EvalSymlinks(filepath.Dir(commandLink))
	if err != nil {
		return fmt.Errorf("resolve command link directory: %w", err)
	}
	resolvedLink := filepath.Join(resolvedParent, filepath.Base(commandLink))
	relative, err := filepath.Rel(targetHome, resolvedLink)
	if err != nil {
		return fmt.Errorf("compare command link with Runtime home: %w", err)
	}
	if relative == "." || relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return cliValidationf("command link must be outside the Runtime home")
	}
	return nil
}

func serverOptionValue(
	args []string,
	index int,
	name string,
) (string, int, error) {
	next := index + 1
	if next >= len(args) || args[next] == "" ||
		strings.HasPrefix(args[next], "--") {
		return "", index, cliValidationf("%s requires value", name)
	}
	return args[next], next, nil
}

func startServer(paths layout.Paths, output *cliOutput) error {
	result, err := serverruntime.Start(paths)
	if err != nil {
		return err
	}
	if output.JSON() {
		payload := map[string]any{"running": result.Running, "pid": result.PID, "configured_address": serverAddress()}
		if result.Changed {
			payload["binary"] = paths.ServerBinary
			payload["log"] = paths.ServerLogFile
		}
		return output.writeJSON(payload)
	}
	message := "sn-server is already running (pid=%d)"
	if result.Changed {
		message = "sn-server started (pid=%d)"
	}
	if err := output.line(message, result.PID); err != nil {
		return err
	}
	if err := output.line("Configured address: %s", serverAddress()); err != nil {
		return err
	}
	if result.Changed {
		return output.line("Log: %s", paths.ServerLogFile)
	}
	return nil
}

func serverStatus(paths layout.Paths, output *cliOutput) error {
	result, err := serverruntime.Status(paths)
	if err != nil {
		return err
	}
	if output.JSON() {
		return output.writeJSON(map[string]any{
			"running": result.Running, "pid": result.PID, "pid_file": paths.ServerPIDFile,
			"lease_file": paths.ServerLeaseFile, "log": paths.ServerLogFile,
		})
	}
	if result.Running {
		if err := output.line("sn-server is running (pid=%d)", result.PID); err != nil {
			return err
		}
	} else if err := output.line("sn-server is stopped"); err != nil {
		return err
	}
	return output.line("Log: %s", paths.ServerLogFile)
}

func stopServer(paths layout.Paths, output *cliOutput) error {
	result, err := serverruntime.Stop(paths)
	if err != nil {
		return err
	}
	if output.JSON() {
		payload := map[string]any{"running": false}
		if result.Changed {
			payload["pid"] = result.PID
		}
		return output.writeJSON(payload)
	}
	if result.Changed {
		return output.line("sn-server stopped (pid=%d)", result.PID)
	}
	return output.line("sn-server is already stopped")
}

func serverNamespaces() []string {
	return append([]string(nil), fixedNamespaces...)
}

func serverCapabilities() map[string][]string {
	return map[string][]string{
		"doctor":  {"runtime", "profiles", "tools", "run_store", "logs", "tmux"},
		"profile": {"single_call", "cli", "api", "typed_override", "stream"},
		"session": {
			"history", "run", "submit", "managed_cli", "execution_query",
			"reconcile", "native_tui", "paste", "attach", "close_all",
		},
		"agent": {"api_harness", "tool_loop", "stream"},
		"job": {
			"durable_queue", "events", "watch", "cancel",
			"retry", "reconcile", "gc",
		},
		"update": {"release", "upgrade_preflight"},
		"server": {
			"http", "workers", "lifecycle", "atomic_activation",
		},
	}
}

func serverAddress() string {
	if value := strings.TrimSpace(os.Getenv("HTTP_ADDR")); value != "" {
		return value
	}
	return "127.0.0.1:8080"
}

type updateOptions struct {
	checkOnly     bool
	dryRun        bool
	targetVersion string
}

func parseUpdateOptions(args []string) (updateOptions, error) {
	var options updateOptions
	seen := make(map[string]struct{})
	for index := 0; index < len(args); index++ {
		name := args[index]
		if _, exists := seen[name]; exists {
			return updateOptions{}, fmt.Errorf(
				"%s may only be specified once", name,
			)
		}
		switch name {
		case "--check":
			seen[name] = struct{}{}
			options.checkOnly = true
		case "--dry-run":
			seen[name] = struct{}{}
			options.dryRun = true
		case "--version":
			seen[name] = struct{}{}
			value, next, err := serverOptionValue(args, index, name)
			if err != nil {
				return updateOptions{}, err
			}
			options.targetVersion = value
			index = next
		default:
			return updateOptions{}, fmt.Errorf(
				"unknown update argument %s", args[index],
			)
		}
	}
	if options.checkOnly && options.dryRun {
		return updateOptions{}, fmt.Errorf(
			"--check and --dry-run are mutually exclusive",
		)
	}
	if options.checkOnly && options.targetVersion != "" {
		return updateOptions{}, fmt.Errorf(
			"--check and --version are mutually exclusive",
		)
	}
	return options, nil
}

func executeUpdate(
	cfg *config.Config,
	options updateOptions,
	output *cliOutput,
) error {
	if options.dryRun {
		versionLabel := options.targetVersion
		if versionLabel == "" {
			versionLabel = "<latest-version>"
		}
		archive, archiveURL, checksumURL, err := snupdate.Plan(cfg, versionLabel)
		if err != nil {
			return err
		}
		result := map[string]any{
			"home": cfg.Home, "version": versionLabel, "archive": archive,
			"archive_url": archiveURL, "checksums_url": checksumURL,
		}
		if output.JSON() {
			return output.writeJSON(result)
		}
		if err := output.line("Update plan: %s", versionLabel); err != nil {
			return err
		}
		if err := output.line("Archive: %s", archive); err != nil {
			return err
		}
		return output.line("URL: %s", archiveURL)
	}
	var status snupdate.Status
	if options.checkOnly || options.targetVersion == "" {
		status = snupdate.Check(context.Background(), cfg, buildinfo.Version)
	}
	if options.checkOnly {
		if status.Error != "" {
			if !output.JSON() {
				_ = renderUpdateStatus(output, status)
			}
			return errors.New(status.Error)
		}
		if output.JSON() {
			return output.writeJSON(status)
		}
		return renderUpdateStatus(output, status)
	}
	if options.targetVersion == "" {
		if status.Error != "" {
			if !output.JSON() {
				_ = renderUpdateStatus(output, status)
			}
			return errors.New(status.Error)
		}
		if !status.UpdateAvailable {
			result := map[string]any{"updated": false, "status": status}
			if output.JSON() {
				return output.writeJSON(result)
			}
			return renderUpdateStatus(output, status)
		}
		options.targetVersion = status.LatestVersion
	}
	result, err := snupdate.Apply(
		context.Background(), cfg, options.targetVersion,
	)
	if err != nil {
		return err
	}
	if output.JSON() {
		return output.writeJSON(map[string]any{
			"updated": true, "update": result,
		})
	}
	if err := output.line("Updated sn-cli to %s", result.Version); err != nil {
		return err
	}
	return output.line("Binary: %s", result.Binary)
}

func renderUpdateStatus(output *cliOutput, status snupdate.Status) error {
	if status.Error != "" {
		return output.line("Update check failed: %s", status.Error)
	}
	if !status.Enabled {
		return output.line("Update check is disabled")
	}
	if status.UpdateAvailable {
		return output.line(
			"Update available: %s -> %s",
			status.CurrentVersion, status.LatestVersion,
		)
	}
	return output.line("sn-cli is up to date (%s)", status.CurrentVersion)
}
