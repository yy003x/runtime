package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/internal/application/activation"
	"github.com/yy003x/runtime/internal/application/runtimebootstrap"
	"github.com/yy003x/runtime/internal/infrastructure/executionlog"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/internal/interfaces/cli/config"
	snupdate "github.com/yy003x/runtime/internal/interfaces/cli/update"
	"github.com/yy003x/runtime/internal/interfaces/cli/version"
	"github.com/yy003x/runtime/pkg/contract"
)

const serverPIDSchemaVersion = 1

var errServerLeaseReleasedWhileAlive = errors.New(
	"sn-server released its lease before process exit",
)

type processIdentity struct {
	StartToken string
	Executable string
}

type serverPIDRecord struct {
	SchemaVersion     int    `json:"schema_version"`
	PID               int    `json:"pid"`
	Binary            string `json:"binary"`
	ProcessStart      string `json:"process_start"`
	ProcessExecutable string `json:"process_executable"`
	StartedAt         string `json:"started_at"`
}

func runServerNamespace(
	paths layout.Paths,
	args []string,
	output *cliOutput,
) error {
	if len(args) == 0 {
		return cliValidationf(
			"usage: server info|start|status|stop|update|upgrade-check",
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
			"version":          version.String(), "runtime_home": paths.Home,
			"profiles":           core.Profiles.Entries(),
			"run_database":       paths.RunDBFile,
			"configured_address": serverAddress(),
			"namespaces":         serverNamespaces(),
			"capabilities":       serverCapabilities(),
		}
		if output.JSON() {
			return output.writeJSON(result)
		}
		if err := output.line("Runtime: %s", version.String()); err != nil {
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
	case "update":
		options, err := parseUpdateOptions(args[1:])
		if err != nil {
			return cliValidation(err)
		}
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return executeUpdate(cfg, options, output)
	case "upgrade-check":
		return runUpgradeCheck(paths, args[1:], output)
	case "upgrade-activate":
		return runUpgradeActivate(paths, args[1:], output)
	default:
		return cliValidationf("unknown server action %q", args[0])
	}
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
		inspectServerForInstall = func() (
			activation.ManagedServerProcess,
			error,
		) {
			running, pid, inspectErr := serverRunning(paths)
			if inspectErr != nil {
				return activation.ManagedServerProcess{}, inspectErr
			}
			if !running {
				return activation.ManagedServerProcess{}, nil
			}
			record, readErr := readServerPID(paths.ServerPIDFile)
			if readErr != nil {
				return activation.ManagedServerProcess{}, readErr
			}
			if record.PID != pid {
				return activation.ManagedServerProcess{}, fmt.Errorf(
					"sn-server process identity changed during install preflight",
				)
			}
			return activation.ManagedServerProcess{
				PID: pid, StartToken: record.ProcessStart,
			}, nil
		}
		stopServerForInstall = func() error {
			return stopServerLocked(
				paths,
				newCLIOutput(false, io.Discard, io.Discard),
			)
		}
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
	return withServerLifecycleLock(paths, func() error {
		return startServerLocked(paths, output)
	})
}

func startServerLocked(paths layout.Paths, output *cliOutput) error {
	running, pid, err := serverRunning(paths)
	if err != nil {
		return err
	}
	if running {
		if output.JSON() {
			return output.writeJSON(map[string]any{
				"running":            true,
				"pid":                pid,
				"configured_address": serverAddress(),
			})
		}
		if err := output.line(
			"sn-server is already running (pid=%d)", pid,
		); err != nil {
			return err
		}
		return output.line("Configured address: %s", serverAddress())
	}
	binary := paths.ServerBinary
	info, err := os.Lstat(binary)
	if err != nil {
		return fmt.Errorf("sn-server is not installed at %s", binary)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode()&0o111 == 0 {
		return fmt.Errorf("sn-server must be an executable regular file")
	}
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		return err
	}
	lease, acquired, err := tryAcquireFileLock(paths.ServerLeaseFile)
	if err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf(
			"sn-server lease is held without a valid process identity; retry after the previous process exits",
		)
	}
	if filepath.Clean(paths.ServerLogFile) !=
		filepath.Join(paths.LogsDir, "sn-server.log") {
		releaseFileLock(lease)
		return fmt.Errorf("sn-server log path does not match Runtime log root")
	}
	logFile, err := executionlog.OpenProcessLog(paths.LogsDir, "sn-server.log")
	if err != nil {
		releaseFileLock(lease)
		return err
	}
	command := exec.Command(binary)
	command.Env = os.Environ()
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	command.ExtraFiles = []*os.File{lease}
	if err := command.Start(); err != nil {
		logFile.Close()
		releaseFileLock(lease)
		return err
	}
	logFile.Close()
	identity, err := waitForProcessIdentity(command.Process.Pid, time.Second)
	if err != nil {
		stopStartedProcess(command)
		_ = lease.Close()
		return fmt.Errorf("identify started sn-server: %w", err)
	}
	record := serverPIDRecord{
		SchemaVersion:     serverPIDSchemaVersion,
		PID:               command.Process.Pid,
		Binary:            filepath.Clean(binary),
		ProcessStart:      identity.StartToken,
		ProcessExecutable: identity.Executable,
		StartedAt:         time.Now().UTC().Format(time.RFC3339Nano),
	}
	if err := writeServerPID(paths.ServerPIDFile, record); err != nil {
		stopStartedProcess(command)
		_ = lease.Close()
		return err
	}
	// ExtraFiles transfers the same locked open-file description to sn-server.
	// Closing only the parent descriptor keeps the lease held until that exact
	// child process exits.
	if err := lease.Close(); err != nil {
		stopStartedProcess(command)
		_ = os.Remove(paths.ServerPIDFile)
		return fmt.Errorf("close parent sn-server lease: %w", err)
	}
	pid = command.Process.Pid
	if err := command.Process.Release(); err != nil {
		_ = command.Process.Signal(syscall.SIGTERM)
		_ = os.Remove(paths.ServerPIDFile)
		return fmt.Errorf("release sn-server process: %w", err)
	}
	time.Sleep(150 * time.Millisecond)
	running, currentPID, err := serverRunning(paths)
	if err != nil {
		_ = os.Remove(paths.ServerPIDFile)
		return err
	}
	if !running || currentPID != pid {
		_ = os.Remove(paths.ServerPIDFile)
		return fmt.Errorf(
			"sn-server exited during startup; inspect %s", paths.ServerLogFile,
		)
	}
	result := map[string]any{
		"running": true, "pid": pid, "binary": binary,
		"log": paths.ServerLogFile, "configured_address": serverAddress(),
	}
	if output.JSON() {
		return output.writeJSON(result)
	}
	if err := output.line("sn-server started (pid=%d)", pid); err != nil {
		return err
	}
	if err := output.line(
		"Configured address: %s", serverAddress(),
	); err != nil {
		return err
	}
	return output.line("Log: %s", paths.ServerLogFile)
}

func serverStatus(paths layout.Paths, output *cliOutput) error {
	return withServerLifecycleLock(paths, func() error {
		running, pid, err := serverRunning(paths)
		if err != nil {
			return err
		}
		result := map[string]any{
			"running": running, "pid": pid, "pid_file": paths.ServerPIDFile,
			"lease_file": paths.ServerLeaseFile, "log": paths.ServerLogFile,
		}
		if output.JSON() {
			return output.writeJSON(result)
		}
		if running {
			if err := output.line(
				"sn-server is running (pid=%d)", pid,
			); err != nil {
				return err
			}
		} else if err := output.line("sn-server is stopped"); err != nil {
			return err
		}
		return output.line("Log: %s", paths.ServerLogFile)
	})
}

func stopServer(paths layout.Paths, output *cliOutput) error {
	return withServerLifecycleLock(paths, func() error {
		return stopServerLocked(paths, output)
	})
}

func stopServerLocked(paths layout.Paths, output *cliOutput) error {
	running, pid, err := serverRunning(paths)
	if err != nil {
		return err
	}
	if !running {
		_ = os.Remove(paths.ServerPIDFile)
		if output.JSON() {
			return output.writeJSON(map[string]any{"running": false})
		}
		return output.line("sn-server is already stopped")
	}
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	if err := process.Signal(syscall.SIGTERM); err != nil {
		return err
	}
	deadline := time.Now().Add(10 * time.Second)
	for time.Now().Before(deadline) {
		running, currentPID, stateErr := serverRunning(paths)
		if stateErr == nil && (!running || currentPID != pid) {
			_ = os.Remove(paths.ServerPIDFile)
			if output.JSON() {
				return output.writeJSON(map[string]any{
					"running": false, "pid": pid,
				})
			}
			return output.line("sn-server stopped (pid=%d)", pid)
		}
		if stateErr != nil {
			if errors.Is(
				stateErr, errServerLeaseReleasedWhileAlive,
			) && currentPID == pid {
				time.Sleep(100 * time.Millisecond)
				continue
			}
			return stateErr
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("sn-server pid %d did not stop within 10s", pid)
}

func serverRunning(paths layout.Paths) (bool, int, error) {
	held, err := fileLockHeld(paths.ServerLeaseFile)
	if err != nil {
		return false, 0, err
	}
	if !held {
		record, readErr := readServerPID(paths.ServerPIDFile)
		if errors.Is(readErr, os.ErrNotExist) {
			return false, 0, nil
		}
		if readErr != nil {
			return false, 0, fmt.Errorf(
				"unsupported or corrupt sn-server pid record at %s; "+
					"ensure any previous server is stopped, then remove the stale file: %w",
				paths.ServerPIDFile, readErr,
			)
		}
		identity, identityErr := processIdentityForPID(record.PID)
		if identityErr == nil &&
			identity.StartToken == record.ProcessStart &&
			identity.Executable == record.ProcessExecutable {
			return false, record.PID, fmt.Errorf(
				"sn-server pid %d is alive but does not hold its lease; refusing unsafe lifecycle operations: %w",
				record.PID, errServerLeaseReleasedWhileAlive,
			)
		}
		return false, record.PID, nil
	}
	record, err := readServerPID(paths.ServerPIDFile)
	if err != nil {
		return false, 0, fmt.Errorf(
			"sn-server lease is held but process identity is unavailable: %w", err,
		)
	}
	if filepath.Clean(record.Binary) != filepath.Clean(paths.ServerBinary) {
		return false, record.PID, fmt.Errorf(
			"sn-server pid record belongs to binary %s, expected %s",
			record.Binary, paths.ServerBinary,
		)
	}
	// On macOS, kern.proc.pid can transiently return EIO while the process
	// table is changing under concurrent lifecycle probes. Keep the identity
	// check fail-closed, but retry briefly before declaring the lease holder
	// unverifiable.
	identity, err := waitForProcessIdentity(record.PID, 250*time.Millisecond)
	if err != nil {
		return false, record.PID, fmt.Errorf(
			"sn-server lease holder pid %d cannot be identified: %w", record.PID, err,
		)
	}
	if identity.StartToken != record.ProcessStart ||
		identity.Executable != record.ProcessExecutable {
		return false, record.PID, fmt.Errorf(
			"sn-server pid %d identity changed; refusing to signal it", record.PID,
		)
	}
	return true, record.PID, nil
}

func writeServerPID(path string, record serverPIDRecord) error {
	if err := validateServerPIDRecord(record); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil && info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("pid file must not be a symlink")
	} else if err != nil && !os.IsNotExist(err) {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".sn-server-pid-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(0o600); err != nil {
		temp.Close()
		return err
	}
	encoder := json.NewEncoder(temp)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(record); err != nil {
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

func readServerPID(path string) (serverPIDRecord, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return serverPIDRecord{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return serverPIDRecord{}, fmt.Errorf("pid file must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return serverPIDRecord{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	var record serverPIDRecord
	if err := decoder.Decode(&record); err != nil {
		return serverPIDRecord{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return serverPIDRecord{}, fmt.Errorf("pid file contains trailing JSON")
		}
		return serverPIDRecord{}, err
	}
	if err := validateServerPIDRecord(record); err != nil {
		return serverPIDRecord{}, err
	}
	return record, nil
}

func validateServerPIDRecord(record serverPIDRecord) error {
	if record.SchemaVersion != serverPIDSchemaVersion {
		return fmt.Errorf("unsupported pid schema_version %d", record.SchemaVersion)
	}
	if record.PID <= 0 || strings.TrimSpace(record.Binary) == "" ||
		strings.TrimSpace(record.ProcessStart) == "" ||
		strings.TrimSpace(record.ProcessExecutable) == "" {
		return fmt.Errorf("incomplete sn-server pid identity")
	}
	if _, err := time.Parse(time.RFC3339Nano, record.StartedAt); err != nil {
		return fmt.Errorf("invalid sn-server started_at: %w", err)
	}
	return nil
}

func waitForProcessIdentity(pid int, timeout time.Duration) (processIdentity, error) {
	deadline := time.Now().Add(timeout)
	var lastErr error
	for {
		identity, err := processIdentityForPID(pid)
		if err == nil {
			return identity, nil
		}
		lastErr = err
		if time.Now().After(deadline) {
			return processIdentity{}, lastErr
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func stopStartedProcess(command *exec.Cmd) {
	_ = command.Process.Signal(syscall.SIGTERM)
	_ = command.Process.Kill()
	_ = command.Wait()
}

func withServerLifecycleLock(paths layout.Paths, fn func() error) error {
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		return err
	}
	lock, acquired, err := tryAcquireFileLock(paths.ServerLockFile)
	if err != nil {
		return err
	}
	if !acquired {
		return fmt.Errorf("another sn-server lifecycle operation is in progress")
	}
	defer releaseFileLock(lock)
	return fn()
}

func fileLockHeld(path string) (bool, error) {
	lock, acquired, err := tryAcquireFileLock(path)
	if err != nil {
		return false, err
	}
	if acquired {
		releaseFileLock(lock)
		return false, nil
	}
	return true, nil
}

func tryAcquireFileLock(path string) (*os.File, bool, error) {
	fd, err := unix.Open(
		path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600,
	)
	if err != nil {
		return nil, false, fmt.Errorf("open lock %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, false, err
	}
	if !info.Mode().IsRegular() {
		file.Close()
		return nil, false, fmt.Errorf("lock path %s must be a regular file", path)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) || errors.Is(err, unix.EAGAIN) {
			return nil, false, nil
		}
		return nil, false, fmt.Errorf("lock %s: %w", path, err)
	}
	return file, true, nil
}

func releaseFileLock(file *os.File) {
	if file == nil {
		return
	}
	_ = unix.Flock(int(file.Fd()), unix.LOCK_UN)
	_ = file.Close()
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
		"run": {
			"durable_queue", "events", "watch", "cancel",
			"retry", "reconcile", "gc",
		},
		"server": {
			"http", "workers", "lifecycle", "update", "upgrade_preflight",
			"atomic_activation",
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
		status = snupdate.Check(context.Background(), cfg, version.Version)
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
