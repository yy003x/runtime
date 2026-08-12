package activation

import (
	"context"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/url"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
	_ "modernc.org/sqlite"

	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/pkg/session"
)

type processTarget struct {
	Path string
	Info os.FileInfo
}

type processExclusion struct {
	StartToken string
}

type quiescenceOptions struct {
	SkipServer       bool
	SkipRuntimeState bool
}

func preflightQuiescence(
	target string,
	manifest Manifest,
	excludedPIDs map[int]processExclusion,
	processTargets []processTarget,
	options quiescenceOptions,
) error {
	if !options.SkipServer {
		if err := assertServerStopped(target); err != nil {
			return err
		}
	}
	socket := tmuxSocketForHome(target)
	if socket == "" {
		return fmt.Errorf("resolve dedicated Tmux socket for Runtime home")
	}
	if info, err := os.Lstat(socket); err == nil {
		return fmt.Errorf(
			"dedicated Tmux socket still exists at %s; stop every managed window before upgrading",
			socket,
		)
	} else if !errors.Is(err, os.ErrNotExist) {
		return fmt.Errorf("inspect dedicated Tmux socket: %w", err)
	} else if info != nil {
		return fmt.Errorf("dedicated Tmux socket state is ambiguous")
	}
	if err := assertNoTmuxServerProcess(socket); err != nil {
		return err
	}
	if err := assertNoDefaultTmuxSession(target); err != nil {
		return err
	}
	if !options.SkipRuntimeState {
		if err := preflightState(target, manifest); err != nil {
			return err
		}
	}
	if err := assertNoTargetProcesses(processTargets, excludedPIDs); err != nil {
		return err
	}
	// Re-read durable state after the process-table barrier. A short-lived
	// caller that settled before the scan is harmless; one that left a queued
	// or unknown execution must still block activation.
	if options.SkipRuntimeState {
		return nil
	}
	return preflightState(target, manifest)
}

func captureTargetProcesses(target string) ([]processTarget, error) {
	targets := make([]processTarget, 0, 2)
	for _, name := range []string{"sn-cli", "sn-server"} {
		path := filepath.Join(target, "bin", name)
		info, err := os.Stat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return nil, fmt.Errorf("inspect target binary %s: %w", path, err)
		}
		lstat, err := os.Lstat(path)
		if err != nil {
			return nil, err
		}
		if lstat.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return nil, fmt.Errorf(
				"target binary must be a regular file, not a symlink: %s", path,
			)
		}
		targets = append(targets, processTarget{
			Path: filepath.Clean(path),
			Info: info,
		})
	}
	return targets, nil
}

func findProcessTarget(
	targets []processTarget,
	base string,
) (processTarget, bool) {
	for _, target := range targets {
		if filepath.Base(target.Path) == base {
			return target, true
		}
	}
	return processTarget{}, false
}

func preflightState(target string, manifest Manifest) error {
	if manifest.SessionSchemaVersion != session.SchemaVersion {
		return fmt.Errorf(
			"candidate Session schema %d does not match runtime schema %d",
			manifest.SessionSchemaVersion, session.SchemaVersion,
		)
	}
	sessionsDir := filepath.Join(target, "sessions")
	stateDir := filepath.Join(target, "state")
	if _, err := session.NewStore(sessionsDir, stateDir); err != nil {
		return fmt.Errorf("Session schema preflight failed: %w", err)
	}
	if err := assertNoActiveSessionExecutions(sessionsDir); err != nil {
		return err
	}
	return preflightRunDatabase(
		filepath.Join(stateDir, "runtime.db"),
		manifest.RunSchemaVersion,
	)
}

func assertNoActiveSessionExecutions(sessionsDir string) error {
	info, err := os.Lstat(sessionsDir)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("sessions directory must be a directory, not a symlink")
	}
	entries, err := os.ReadDir(sessionsDir)
	if err != nil {
		return err
	}
	for _, entry := range entries {
		if entry.Name() == "_system" {
			continue
		}
		sessionPath := filepath.Join(sessionsDir, entry.Name(), "session.json")
		var sessionFact session.Session
		if err := decodeStrictRegular(sessionPath, &sessionFact); err != nil {
			return err
		}
		if sessionFact.SchemaVersion != session.SchemaVersion {
			return fmt.Errorf(
				"%s uses unsupported Session schema_version %d",
				sessionPath, sessionFact.SchemaVersion,
			)
		}
		switch sessionFact.State {
		case session.SessionIdle, session.SessionBlocked,
			session.SessionArchived:
		case session.SessionActive:
			return fmt.Errorf(
				"Session %s has an active execution; reconcile it before upgrading",
				sessionFact.ID,
			)
		default:
			return fmt.Errorf(
				"Session %s has unknown state %q; reconcile it before upgrading",
				sessionFact.ID, sessionFact.State,
			)
		}
		executionsDir := filepath.Join(
			sessionsDir, entry.Name(), "executions",
		)
		executions, err := os.ReadDir(executionsDir)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		for _, executionEntry := range executions {
			if executionEntry.IsDir() ||
				executionEntry.Type()&os.ModeSymlink != 0 ||
				filepath.Ext(executionEntry.Name()) != ".json" {
				return fmt.Errorf(
					"%s contains unsupported execution fact %q",
					executionsDir, executionEntry.Name(),
				)
			}
			executionPath := filepath.Join(
				executionsDir, executionEntry.Name(),
			)
			var executionFact session.Execution
			if err := decodeStrictRegular(
				executionPath, &executionFact,
			); err != nil {
				return err
			}
			if executionFact.SchemaVersion != session.SchemaVersion {
				return fmt.Errorf(
					"%s uses unsupported Session schema_version %d",
					executionPath, executionFact.SchemaVersion,
				)
			}
			if executionFact.State != session.ExecutionSettled {
				return fmt.Errorf(
					"Session execution %s is %s; reconcile it before upgrading",
					executionFact.ID, executionFact.State,
				)
			}
			switch executionFact.Outcome {
			case session.OutcomeCompleted, session.OutcomeFailed,
				session.OutcomeCancelled:
			default:
				return fmt.Errorf(
					"Session execution %s has unknown outcome %q; reconcile it before upgrading",
					executionFact.ID, executionFact.Outcome,
				)
			}
		}
	}
	return nil
}

func preflightRunDatabase(path string, expectedVersion int) error {
	for _, suffix := range []string{"-wal", "-shm"} {
		sidecar := path + suffix
		if info, err := os.Lstat(sidecar); err == nil {
			if info.Mode()&os.ModeSymlink != 0 ||
				!info.Mode().IsRegular() {
				return fmt.Errorf(
					"SQLite sidecar must be a regular file, not a symlink: %s",
					sidecar,
				)
			}
			if _, databaseErr := os.Lstat(path); errors.Is(
				databaseErr, os.ErrNotExist,
			) {
				return fmt.Errorf(
					"SQLite sidecar exists without runtime.db: %s", sidecar,
				)
			} else if databaseErr != nil {
				return databaseErr
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return err
		}
	}
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("Run database must be a regular file, not a symlink")
	}
	dsn := (&url.URL{Scheme: "file", Path: path}).String() + "?mode=ro"
	database, err := sql.Open("sqlite", dsn)
	if err != nil {
		return err
	}
	defer database.Close()
	var version int
	if err := database.QueryRow("PRAGMA user_version").Scan(&version); err != nil {
		return fmt.Errorf("read Run schema: %w", err)
	}
	if version != expectedVersion {
		return fmt.Errorf(
			"Run database uses unsupported schema %d; expected %d; move runtime.db and its sidecars to a recoverable backup before initializing current state",
			version, expectedVersion,
		)
	}
	var integrity string
	if err := database.QueryRow("PRAGMA quick_check").Scan(&integrity); err != nil {
		return fmt.Errorf("check Run database integrity: %w", err)
	}
	if integrity != "ok" {
		return fmt.Errorf("Run database quick_check failed: %s", integrity)
	}
	rows, err := database.Query(`
		SELECT state, COUNT(*) FROM runs GROUP BY state
	`)
	if err != nil {
		return fmt.Errorf("inspect Run states: %w", err)
	}
	defer rows.Close()
	var unsettled int
	for rows.Next() {
		var state string
		var count int
		if err := rows.Scan(&state, &count); err != nil {
			return err
		}
		switch state {
		case "completed", "failed", "cancelled":
		default:
			unsettled += count
		}
	}
	if err := rows.Err(); err != nil {
		return err
	}
	if unsettled != 0 {
		return fmt.Errorf(
			"%d durable Run(s) have nonterminal or unknown state; settle or reconcile them before upgrading",
			unsettled,
		)
	}
	var queued int
	if err := database.QueryRow("SELECT COUNT(*) FROM queue").Scan(
		&queued,
	); err != nil {
		return fmt.Errorf("inspect Run queue: %w", err)
	}
	if queued != 0 {
		return fmt.Errorf(
			"Run queue contains %d stale item(s); reconcile them before upgrading",
			queued,
		)
	}
	return nil
}

func assertServerStopped(target string) error {
	stateDir := filepath.Join(target, "state")
	pidPath := filepath.Join(stateDir, "sn-server.pid")
	if _, err := os.Lstat(pidPath); err == nil {
		return fmt.Errorf(
			"sn-server PID record still exists at %s; stop the server before upgrading",
			pidPath,
		)
	} else if !errors.Is(err, os.ErrNotExist) {
		return err
	}
	leasePath := filepath.Join(stateDir, "sn-server.lease.lock")
	info, err := os.Lstat(leasePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return fmt.Errorf("sn-server lease lock is invalid")
	}
	fd, err := unix.Open(
		leasePath, unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return err
	}
	defer unix.Close(fd) //nolint:errcheck
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) ||
			errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("sn-server is running; stop it before upgrading")
		}
		return err
	}
	return unix.Flock(fd, unix.LOCK_UN)
}

func assertNoTmuxServerProcess(socket string) error {
	command := exec.Command("/bin/ps", "-ww", "-axo", "pid=,command=")
	output, err := command.Output()
	if err != nil {
		return fmt.Errorf("inspect Tmux process table: %w", err)
	}
	for _, line := range strings.Split(string(output), "\n") {
		if strings.Contains(line, socket) {
			return fmt.Errorf(
				"dedicated Tmux server still references %s; stop every managed window before upgrading",
				socket,
			)
		}
	}
	return nil
}

func assertNoDefaultTmuxSession(target string) error {
	path, err := exec.LookPath("tmux")
	if err != nil {
		return nil
	}
	environment := tmuxPreflightEnvironment(os.Environ())
	hasSession := exec.Command(
		path, "-L", "default", "has-session", "-t", "=sn-session",
	)
	hasSession.Env = environment
	if err := hasSession.Run(); err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("inspect default Tmux session: %w", err)
	}
	command := exec.Command(
		path, "-L", "default", "show-options", "-q", "-v",
		"-t", "sn-session", "@sn_runtime_session",
	)
	command.Env = environment
	output, err := command.Output()
	if err != nil {
		var exitErr *exec.ExitError
		if errors.As(err, &exitErr) && exitErr.ExitCode() == 1 {
			return nil
		}
		return fmt.Errorf("inspect default Tmux session: %w", err)
	}
	encoded := strings.TrimSpace(string(output))
	if encoded == "" {
		return nil
	}
	data, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return nil
	}
	var marker struct {
		FullHomeDigest string `json:"full_home_digest"`
	}
	if err := json.Unmarshal(data, &marker); err != nil {
		return nil
	}
	expected := fmt.Sprintf("%x", sha256.Sum256([]byte(filepath.Clean(target))))
	if marker.FullHomeDigest != expected {
		return nil
	}
	return fmt.Errorf(
		"default Tmux session sn-session still belongs to %s; stop every managed window before upgrading",
		target,
	)
}

func tmuxPreflightEnvironment(values []string) []string {
	result := make([]string, 0, len(values))
	for _, value := range values {
		name, _, ok := strings.Cut(value, "=")
		if !ok || name == "TMUX" || name == "TMUX_PANE" {
			continue
		}
		result = append(result, value)
	}
	return result
}

func tmuxSocketForHome(home string) string {
	paths, err := layout.FromHome(home)
	if err != nil {
		return ""
	}
	return paths.TmuxSocketFile
}

func decodeStrictRegular(path string, target any) error {
	data, err := readRegular(path, 16<<20)
	if err != nil {
		return err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode %s: %w", path, err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return fmt.Errorf("%s contains trailing JSON", path)
	}
	return nil
}

// UpgradePreflight exposes the same read-only state check used by activation.
// It is useful to diagnose a blocked upgrade without staging mutations.
func UpgradePreflight(
	_ context.Context,
	target string,
	resourcesDir string,
) error {
	manifest, _, err := LoadManifest(resourcesDir)
	if err != nil {
		return err
	}
	target, err = layout.CanonicalHome(target)
	if err != nil {
		return err
	}
	targets, err := captureTargetProcesses(target)
	if err != nil {
		return err
	}
	token, err := processStartToken(os.Getpid())
	if err != nil {
		return fmt.Errorf("identify upgrade-check process: %w", err)
	}
	return preflightQuiescence(
		target, manifest,
		map[int]processExclusion{
			os.Getpid(): {StartToken: token},
		},
		targets,
		quiescenceOptions{},
	)
}
