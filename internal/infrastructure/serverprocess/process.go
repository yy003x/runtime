// Package serverprocess 管理本机服务的进程、身份记录和文件锁。
package serverprocess

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yy003x/sn-code/internal/infrastructure/executionlog"
	"github.com/yy003x/sn-code/internal/infrastructure/layout"
)

const serverPIDSchemaVersion = 1

var ErrLeaseReleasedWhileAlive = errors.New(
	"sn-server released its lease before process exit",
)

type processIdentity struct {
	StartToken string
	Executable string
}

type PIDRecord struct {
	SchemaVersion     int    `json:"schema_version"`
	PID               int    `json:"pid"`
	Binary            string `json:"binary"`
	ProcessStart      string `json:"process_start"`
	ProcessExecutable string `json:"process_executable"`
	StartedAt         string `json:"started_at"`
}

func Running(paths layout.Paths) (bool, int, error) {
	held, err := fileLockHeld(paths.ServerLeaseFile)
	if err != nil {
		return false, 0, err
	}
	if !held {
		record, readErr := ReadPID(paths.ServerPIDFile)
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
				record.PID, ErrLeaseReleasedWhileAlive,
			)
		}
		return false, record.PID, nil
	}
	record, err := ReadPID(paths.ServerPIDFile)
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

func writeServerPID(path string, record PIDRecord) error {
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

func ReadPID(path string) (PIDRecord, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return PIDRecord{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
		return PIDRecord{}, fmt.Errorf("pid file must be a regular file")
	}
	file, err := os.Open(path)
	if err != nil {
		return PIDRecord{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 64<<10))
	decoder.DisallowUnknownFields()
	var record PIDRecord
	if err := decoder.Decode(&record); err != nil {
		return PIDRecord{}, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return PIDRecord{}, fmt.Errorf("pid file contains trailing JSON")
		}
		return PIDRecord{}, err
	}
	if err := validateServerPIDRecord(record); err != nil {
		return PIDRecord{}, err
	}
	return record, nil
}

func validateServerPIDRecord(record PIDRecord) error {
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

func WithLifecycleLock(paths layout.Paths, fn func() error) error {
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

// Launch 启动并交接 lease；调用方必须已持有生命周期锁并确认服务未运行。
func Launch(paths layout.Paths) (int, error) {
	binary := paths.ServerBinary
	info, err := os.Lstat(binary)
	if err != nil {
		return 0, fmt.Errorf("sn-server is not installed at %s", binary)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Mode()&0o111 == 0 {
		return 0, fmt.Errorf("sn-server must be an executable regular file")
	}
	if err := os.MkdirAll(paths.StateDir, 0o700); err != nil {
		return 0, err
	}
	lease, acquired, err := tryAcquireFileLock(paths.ServerLeaseFile)
	if err != nil {
		return 0, err
	}
	if !acquired {
		return 0, fmt.Errorf(
			"sn-server lease is held without a valid process identity; retry after the previous process exits",
		)
	}
	if filepath.Clean(paths.ServerLogFile) !=
		filepath.Join(paths.LogsDir, "sn-server.log") {
		releaseFileLock(lease)
		return 0, fmt.Errorf("sn-server log path does not match Runtime log root")
	}
	logFile, err := executionlog.OpenProcessLog(paths.LogsDir, "sn-server.log")
	if err != nil {
		releaseFileLock(lease)
		return 0, err
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
		return 0, err
	}
	logFile.Close()
	identity, err := waitForProcessIdentity(command.Process.Pid, time.Second)
	if err != nil {
		stopStartedProcess(command)
		_ = lease.Close()
		return 0, fmt.Errorf("identify started sn-server: %w", err)
	}
	record := PIDRecord{
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
		return 0, err
	}
	// ExtraFiles transfers the same locked open-file description to sn-server.
	// Closing only the parent descriptor keeps the lease held until that exact
	// child process exits.
	if err := lease.Close(); err != nil {
		stopStartedProcess(command)
		_ = os.Remove(paths.ServerPIDFile)
		return 0, fmt.Errorf("close parent sn-server lease: %w", err)
	}
	pid := command.Process.Pid
	if err := command.Process.Release(); err != nil {
		_ = command.Process.Signal(syscall.SIGTERM)
		_ = os.Remove(paths.ServerPIDFile)
		return 0, fmt.Errorf("release sn-server process: %w", err)
	}
	time.Sleep(150 * time.Millisecond)
	running, currentPID, err := Running(paths)
	if err != nil {
		_ = os.Remove(paths.ServerPIDFile)
		return 0, err
	}
	if !running || currentPID != pid {
		_ = os.Remove(paths.ServerPIDFile)
		return 0, fmt.Errorf(
			"sn-server exited during startup; inspect %s", paths.ServerLogFile,
		)
	}
	return pid, nil
}

// Terminate 只发送终止信号，身份检查和等待由生命周期用例负责。
func Terminate(pid int) error {
	process, err := os.FindProcess(pid)
	if err != nil {
		return err
	}
	return process.Signal(syscall.SIGTERM)
}

// RemovePID 清理已结束进程的身份记录。
func RemovePID(paths layout.Paths) { _ = os.Remove(paths.ServerPIDFile) }
