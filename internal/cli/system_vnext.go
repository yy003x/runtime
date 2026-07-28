package cli

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yy003x/runtime/internal/cli/config"
	snupdate "github.com/yy003x/runtime/internal/cli/update"
	"github.com/yy003x/runtime/internal/cli/version"
	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimebootstrap"
)

func runSystemNamespaceVNext(paths layout.Paths, args []string) error {
	if len(args) == 0 {
		return fmt.Errorf("usage: system info|doctor|start|status|stop|update")
	}
	switch args[0] {
	case "info":
		core, err := runtimebootstrap.LoadVNext(paths, fixedNamespaces...)
		if err != nil {
			return err
		}
		return printJSON(map[string]any{
			"version": version.String(), "runtime_home": paths.Home,
			"profiles":        core.Profiles.Entries(),
			"run_database":    paths.RunDBFile,
			"terminal_driver": core.Config.Terminal.Driver,
		})
	case "doctor":
		return systemDoctor(paths)
	case "start":
		return startServer(paths)
	case "status":
		return serverStatus(paths)
	case "stop":
		return stopServer(paths)
	case "update":
		cfg, err := config.Load()
		if err != nil {
			return err
		}
		return runUpdateVNext(cfg, args[1:])
	default:
		return fmt.Errorf("unknown system action %q", args[0])
	}
}

func systemDoctor(paths layout.Paths) error {
	cwd, err := os.Getwd()
	if err != nil {
		return err
	}
	services, err := runtimebootstrap.LoadServices(paths, cwd, fixedNamespaces...)
	if err != nil {
		return err
	}
	defer services.Runs.Close()
	var missingBinaries []string
	var missingAuth []string
	for _, entry := range services.Profiles.Entries() {
		if entry.Command != nil {
			if _, err := exec.LookPath(entry.Command.Binary); err != nil {
				missingBinaries = append(missingBinaries, entry.ID)
			}
		}
		if entry.Model != nil {
			if value, exists := os.LookupEnv(entry.Model.Auth.FromEnv); !exists || value == "" {
				missingAuth = append(missingAuth, entry.Model.Auth.FromEnv)
			}
		}
	}
	result := map[string]any{
		"ok":                       len(missingBinaries) == 0 && len(missingAuth) == 0,
		"profile_count":            len(services.Profiles.Entries()),
		"tools":                    services.Tools.Definitions(),
		"run_store":                "sqlite_wal",
		"missing_command_binaries": missingBinaries,
		"missing_auth_environment": missingAuth,
	}
	if err := printJSON(result); err != nil {
		return err
	}
	if result["ok"] != true {
		return fmt.Errorf("Runtime doctor found unavailable dependencies")
	}
	return nil
}

func startServer(paths layout.Paths) error {
	if running, pid := serverRunning(paths); running {
		return printJSON(map[string]any{"running": true, "pid": pid})
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
	logFile, err := os.OpenFile(
		paths.ServerLogFile, os.O_CREATE|os.O_APPEND|os.O_WRONLY, 0o600,
	)
	if err != nil {
		return err
	}
	command := exec.Command(binary)
	command.Env = os.Environ()
	command.Stdout = logFile
	command.Stderr = logFile
	command.SysProcAttr = &syscall.SysProcAttr{Setsid: true}
	if err := command.Start(); err != nil {
		logFile.Close()
		return err
	}
	logFile.Close()
	if err := writePID(serverPIDFile(paths), command.Process.Pid); err != nil {
		_ = command.Process.Signal(syscall.SIGTERM)
		_ = command.Wait()
		return err
	}
	pid := command.Process.Pid
	if err := command.Process.Release(); err != nil {
		_ = os.Remove(serverPIDFile(paths))
		return fmt.Errorf("release sn-server process: %w", err)
	}
	time.Sleep(150 * time.Millisecond)
	if running, _ := serverRunning(paths); !running {
		_ = os.Remove(serverPIDFile(paths))
		return fmt.Errorf(
			"sn-server exited during startup; inspect %s", paths.ServerLogFile,
		)
	}
	return printJSON(map[string]any{
		"running": true, "pid": pid, "binary": binary,
		"log": paths.ServerLogFile,
	})
}

func serverStatus(paths layout.Paths) error {
	running, pid := serverRunning(paths)
	return printJSON(map[string]any{
		"running": running, "pid": pid, "pid_file": serverPIDFile(paths),
		"log": paths.ServerLogFile,
	})
}

func stopServer(paths layout.Paths) error {
	running, pid := serverRunning(paths)
	if !running {
		_ = os.Remove(serverPIDFile(paths))
		return printJSON(map[string]any{"running": false})
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
		if err := process.Signal(syscall.Signal(0)); err != nil {
			_ = os.Remove(serverPIDFile(paths))
			return printJSON(map[string]any{"running": false, "pid": pid})
		}
		time.Sleep(100 * time.Millisecond)
	}
	return fmt.Errorf("sn-server pid %d did not stop within 10s", pid)
}

func serverRunning(paths layout.Paths) (bool, int) {
	data, err := os.ReadFile(serverPIDFile(paths))
	if err != nil {
		return false, 0
	}
	pid, err := strconv.Atoi(strings.TrimSpace(string(data)))
	if err != nil || pid <= 0 {
		return false, 0
	}
	process, err := os.FindProcess(pid)
	if err != nil || process.Signal(syscall.Signal(0)) != nil {
		return false, pid
	}
	return true, pid
}

func serverPIDFile(paths layout.Paths) string {
	return paths.ServerPIDFile
}

func writePID(path string, pid int) error {
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
	if _, err := fmt.Fprintf(temp, "%d\n", pid); err != nil {
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

func runUpdateVNext(cfg *config.Config, args []string) error {
	checkOnly := false
	jsonOutput := false
	dryRun := false
	targetVersion := ""
	for index := 0; index < len(args); index++ {
		switch args[index] {
		case "--check":
			checkOnly = true
		case "--json":
			jsonOutput = true
		case "--dry-run":
			dryRun = true
		case "--version":
			index++
			if index >= len(args) {
				return fmt.Errorf("--version requires value")
			}
			targetVersion = args[index]
		default:
			return fmt.Errorf("unknown update argument %s", args[index])
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
		return printJSON(map[string]any{
			"home": cfg.Home, "version": versionLabel, "archive": archive,
			"archive_url": archiveURL, "checksums_url": checksumURL,
		})
	}
	var status snupdate.Status
	if checkOnly || targetVersion == "" {
		status = snupdate.Check(context.Background(), cfg, version.Version)
	}
	if jsonOutput && (checkOnly || targetVersion == "") {
		if err := json.NewEncoder(os.Stdout).Encode(status); err != nil {
			return err
		}
	}
	if checkOnly {
		if status.Error != "" {
			return errors.New(status.Error)
		}
		if !jsonOutput {
			return printJSON(status)
		}
		return nil
	}
	if targetVersion == "" {
		if status.Error != "" {
			return errors.New(status.Error)
		}
		if !status.UpdateAvailable {
			return printJSON(map[string]any{"updated": false, "status": status})
		}
		targetVersion = status.LatestVersion
	}
	result, err := snupdate.Apply(context.Background(), cfg, targetVersion)
	if err != nil {
		return err
	}
	return printJSON(map[string]any{"updated": true, "update": result})
}
