package cli

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/infrastructure/envref"
	"github.com/yy003x/runtime/internal/infrastructure/executionlog"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/internal/interfaces/cli/version"
	runtimecommand "github.com/yy003x/runtime/pkg/command"
	runtimetmux "github.com/yy003x/runtime/pkg/tmux"
)

type runtimeDoctorFailures struct {
	missingBinaries []string
	invalidCommands []string
	missingAuth     []string
	tmuxError       string
	auditLogError   string
}

func (failures runtimeDoctorFailures) ok() bool {
	return len(failures.missingBinaries) == 0 &&
		len(failures.invalidCommands) == 0 &&
		len(failures.missingAuth) == 0 &&
		failures.tmuxError == "" && failures.auditLogError == ""
}

func runtimeDoctor(paths layout.Paths, output *cliOutput) error {
	if err := runtimebootstrap.ValidateSessionStore(paths); err != nil {
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
	var failures runtimeDoctorFailures
	commandErrors := make(map[string]string)
	for _, entry := range services.Profiles.Entries() {
		if entry.Command != nil {
			if _, resolveErr := runtimecommand.ResolveExecutable(
				*entry.Command, cwd, os.Environ(),
			); resolveErr != nil {
				commandErrors[entry.ID] = resolveErr.Error()
				if errors.Is(resolveErr, exec.ErrNotFound) {
					failures.missingBinaries = append(
						failures.missingBinaries, entry.ID,
					)
				} else {
					failures.invalidCommands = append(
						failures.invalidCommands, entry.ID,
					)
				}
			}
		}
		if entry.Model != nil {
			for _, rawValue := range entry.Model.Headers {
				for _, name := range envref.References(rawValue) {
					if value, exists := os.LookupEnv(name); exists && value != "" {
						continue
					}
					failures.missingAuth = appendUniqueString(
						failures.missingAuth, name,
					)
				}
			}
		}
	}
	for _, name := range services.ToolEnvironmentReferences {
		if value, exists := os.LookupEnv(name); exists && value != "" {
			continue
		}
		failures.missingAuth = appendUniqueString(failures.missingAuth, name)
	}
	tmuxWindowCount := 0
	tmuxManager, tmuxErr := runtimebootstrap.LoadTmuxService(paths)
	if tmuxErr == nil {
		var windows []runtimetmux.Window
		windows, tmuxErr = tmuxManager.List(context.Background())
		tmuxWindowCount = len(windows)
	}
	if tmuxErr != nil {
		failures.tmuxError = tmuxErr.Error()
	}
	doctorTime := time.Now()
	auditErr := executionlog.AppendAudit(paths.LogsDir, executionlog.AuditRecord{
		Time: doctorTime, Source: "sn-cli", Namespace: "doctor",
		Action: "audit-log-check", Outcome: "succeeded",
	})
	if auditErr != nil {
		failures.auditLogError = auditErr.Error()
	}
	result := map[string]any{
		"schema_version":   cliOutputSchemaVersion,
		"contract_version": cliOutputContractVersion,
		"ok":               failures.ok(),
		"version":          version.String(),
		"runtime_home":     paths.Home,
		"namespaces":       serverNamespaces(),
		"capabilities":     serverCapabilities(),
		"profile_count":    len(services.Profiles.Entries()),
		"tools":            services.Tools.Definitions(),
		"run_store":        "sqlite_wal",
		"log_root":         paths.LogsDir,
		"audit_log": filepath.Join(
			paths.LogsDir, doctorTime.Format("060102"), "audit.jsonl",
		),
		"tmux_window_count":        tmuxWindowCount,
		"tmux_error":               failures.tmuxError,
		"audit_log_error":          failures.auditLogError,
		"missing_command_binaries": failures.missingBinaries,
		"invalid_command_profiles": failures.invalidCommands,
		"command_profile_errors":   commandErrors,
		"missing_auth_environment": failures.missingAuth,
	}
	if result["ok"] != true {
		if !output.JSON() {
			if err := renderRuntimeDoctor(
				output, false, len(services.Profiles.Entries()),
				len(services.Tools.Definitions()), tmuxWindowCount,
				failures, paths.Home, paths.LogsDir,
			); err != nil {
				return err
			}
		}
		return runtimeDoctorDependencyError(failures)
	}
	if output.JSON() {
		return output.writeJSON(result)
	}
	return renderRuntimeDoctor(
		output, true, len(services.Profiles.Entries()),
		len(services.Tools.Definitions()), tmuxWindowCount,
		failures, paths.Home, paths.LogsDir,
	)
}

func appendUniqueString(values []string, value string) []string {
	for _, existing := range values {
		if existing == value {
			return values
		}
	}
	return append(values, value)
}

func renderRuntimeDoctor(
	output *cliOutput,
	ok bool,
	profileCount int,
	toolCount int,
	tmuxWindowCount int,
	failures runtimeDoctorFailures,
	runtimeHome string,
	logsDir string,
) error {
	state := "FAILED"
	if ok {
		state = "OK"
	}
	if err := output.line("Runtime doctor: %s", state); err != nil {
		return err
	}
	if err := output.line(
		"Profiles: %d, tools: %d, run store: sqlite_wal",
		profileCount, toolCount,
	); err != nil {
		return err
	}
	if err := output.line("Runtime home: %s", runtimeHome); err != nil {
		return err
	}
	if err := output.line("Logs: %s", logsDir); err != nil {
		return err
	}
	if err := output.line("Tmux windows: %d", tmuxWindowCount); err != nil {
		return err
	}
	if len(failures.missingBinaries) > 0 {
		if err := output.line(
			"Missing command profiles: %s",
			strings.Join(failures.missingBinaries, ", "),
		); err != nil {
			return err
		}
	}
	if len(failures.invalidCommands) > 0 {
		if err := output.line(
			"Invalid command profiles: %s",
			strings.Join(failures.invalidCommands, ", "),
		); err != nil {
			return err
		}
	}
	if len(failures.missingAuth) > 0 {
		if err := output.line(
			"Missing auth environment: %s",
			strings.Join(failures.missingAuth, ", "),
		); err != nil {
			return err
		}
	}
	if failures.tmuxError != "" {
		if err := output.line("Tmux error: %s", failures.tmuxError); err != nil {
			return err
		}
	}
	if failures.auditLogError != "" {
		if err := output.line(
			"Audit log error: %s", failures.auditLogError,
		); err != nil {
			return err
		}
	}
	return nil
}

func runtimeDoctorDependencyError(failures runtimeDoctorFailures) error {
	details := make([]string, 0, 5)
	if len(failures.missingBinaries) > 0 {
		details = append(
			details,
			"missing command profiles: "+
				strings.Join(failures.missingBinaries, ", "),
		)
	}
	if len(failures.invalidCommands) > 0 {
		details = append(
			details,
			"invalid command profiles: "+
				strings.Join(failures.invalidCommands, ", "),
		)
	}
	if len(failures.missingAuth) > 0 {
		details = append(
			details,
			"missing auth environment: "+
				strings.Join(failures.missingAuth, ", "),
		)
	}
	if failures.tmuxError != "" {
		details = append(details, "tmux: "+failures.tmuxError)
	}
	if failures.auditLogError != "" {
		details = append(details, "audit log: "+failures.auditLogError)
	}
	return fmt.Errorf(
		"Runtime doctor found unavailable dependencies: %s",
		strings.Join(details, "; "),
	)
}
