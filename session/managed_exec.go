package session

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"sync"
	"syscall"
	"time"

	runtimecommand "github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"golang.org/x/sys/unix"
)

const (
	maxCanonicalStdoutBytes     = 16 << 20
	maxDiagnosticStderrBytes    = 256 << 10
	maxInvocationManifestBytes  = 1 << 20
	execHelperArgument          = "__sn_private_session_exec_helper"
	execHelperManifestEnv       = "SN_PRIVATE_SESSION_EXEC_MANIFEST"
	execHelperManifestDirIDEnv  = "SN_PRIVATE_SESSION_EXEC_MANIFEST_DIR_ID"
	execHelperManifestFileIDEnv = "SN_PRIVATE_SESSION_EXEC_MANIFEST_FILE_ID"
)

type helperInvocationManifest struct {
	Path        string   `json:"path"`
	Argv        []string `json:"argv"`
	Environment []string `json:"environment"`
	CWD         string   `json:"cwd"`
}

type invocationManifestHandle struct {
	path              string
	name              string
	directoryIdentity safeFileIdentity
	fileIdentity      safeFileIdentity
}

type managedResult struct {
	assistant  string
	exitCode   *int
	signal     string
	stdout     StreamObservation
	stderr     StreamObservation
	outcome    ExecutionOutcome
	runtimeErr *contract.RuntimeError
}

func (service *Service) executeManagedCLI(
	ctx context.Context,
	ids executionIDs,
	turn Turn,
	invocation runtimecommand.Invocation,
) managedResult {
	ownerStartToken, err := processStartToken(os.Getpid())
	if err != nil {
		return managedFailure(
			contract.ErrorInternal, "record Runtime owner identity",
		)
	}
	execution := Execution{
		SchemaVersion: SchemaVersion, ID: ids.execution,
		SessionID: ids.session, TurnID: ids.turn, RunID: ids.run,
		ProfileID: turn.ProfileID, ProfileKind: turn.ProfileKind,
		State: ExecutionSpawnIntent, RequestDigest: turn.RequestDigest,
		ConfigDigest:     turn.ConfigDigest,
		BasePromptDigest: turn.BasePromptDigest, CWD: turn.CWD,
		Process: &ProcessIdentity{
			OwnerPID: os.Getpid(), OwnerStartToken: ownerStartToken,
		},
		CreatedAt: turn.CreatedAt, UpdatedAt: service.now().UTC(),
	}
	if err := service.persistExecution(execution); err != nil {
		return managedFailure(contract.ErrorInternal, err.Error())
	}
	manifest, err := service.writeInvocationManifest(invocation)
	if err != nil {
		return managedFailure(contract.ErrorInternal, "prepare private invocation manifest")
	}
	defer func() {
		_ = service.store.removeInvocationManifest(manifest)
	}()
	executable, err := os.Executable()
	if err != nil {
		return managedFailure(contract.ErrorInternal, "resolve Runtime executable")
	}
	handshakeReader, handshakeWriter, err := os.Pipe()
	if err != nil {
		return managedFailure(contract.ErrorInternal, "create execution handshake")
	}
	defer handshakeWriter.Close()
	command := exec.Command(executable, execHelperArgument)
	command.Env = append(
		withoutEnvironmentKeys(
			service.environ,
			execHelperManifestEnv,
			execHelperManifestDirIDEnv,
			execHelperManifestFileIDEnv,
		),
		execHelperManifestEnv+"="+manifest.path,
		execHelperManifestDirIDEnv+"="+
			encodeSafeFileIdentity(manifest.directoryIdentity),
		execHelperManifestFileIDEnv+"="+
			encodeSafeFileIdentity(manifest.fileIdentity),
	)
	command.ExtraFiles = []*os.File{handshakeReader}
	command.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	stdin, err := os.Open(os.DevNull)
	if err != nil {
		handshakeReader.Close()
		return managedFailure(contract.ErrorInternal, "open null stdin")
	}
	defer stdin.Close()
	command.Stdin = stdin
	stdoutPipe, err := command.StdoutPipe()
	if err != nil {
		handshakeReader.Close()
		return managedFailure(contract.ErrorInternal, "open stdout pipe")
	}
	stderrPipe, err := command.StderrPipe()
	if err != nil {
		handshakeReader.Close()
		return managedFailure(contract.ErrorInternal, "open stderr pipe")
	}
	if err := command.Start(); err != nil {
		handshakeReader.Close()
		return managedFailure(contract.ErrorProviderUnavailable, "start CLI executor")
	}
	handshakeReader.Close()
	pid := command.Process.Pid
	startToken, tokenErr := processStartToken(pid)
	if tokenErr != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = command.Wait()
		return managedFailure(contract.ErrorInternal, "record CLI process identity")
	}
	pgid, pgidErr := syscall.Getpgid(pid)
	if pgidErr != nil || pgid != pid {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = command.Wait()
		return managedFailure(
			contract.ErrorInternal, "verify CLI process group identity",
		)
	}
	execution.State = ExecutionRunning
	execution.Process = &ProcessIdentity{
		OwnerPID: os.Getpid(), OwnerStartToken: ownerStartToken,
		HelperPID: pid, PGID: pgid,
		StartToken: startToken,
	}
	execution.UpdatedAt = service.now().UTC()
	if err := service.persistExecution(execution); err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = command.Wait()
		return managedFailure(contract.ErrorInternal, "persist CLI running marker")
	}
	limitSignal := make(chan struct{}, 1)
	stdoutCapture := newBoundedCapture(maxCanonicalStdoutBytes, limitSignal)
	stderrCapture := newBoundedCapture(maxDiagnosticStderrBytes, limitSignal)
	var copyGroup sync.WaitGroup
	copyGroup.Add(2)
	go func() {
		defer copyGroup.Done()
		_, _ = io.Copy(stdoutCapture, stdoutPipe)
	}()
	go func() {
		defer copyGroup.Done()
		_, _ = io.Copy(stderrCapture, stderrPipe)
	}()
	if _, err := handshakeWriter.Write([]byte{1}); err != nil {
		_ = syscall.Kill(-pid, syscall.SIGKILL)
		_ = command.Wait()
		copyGroup.Wait()
		return managedFailure(contract.ErrorInternal, "release CLI execution handshake")
	}
	_ = handshakeWriter.Close()
	waited := make(chan error, 1)
	go func() { waited <- command.Wait() }()
	trigger := ""
	var waitErr error
	select {
	case waitErr = <-waited:
	case <-ctx.Done():
		if errors.Is(ctx.Err(), context.DeadlineExceeded) {
			trigger = "deadline"
		} else {
			trigger = "cancel"
		}
		waitErr = terminateAndWait(pid, waited)
	case <-limitSignal:
		trigger = "output_limit"
		waitErr = terminateAndWait(pid, waited)
	}
	copyGroup.Wait()
	if trigger == "" && (stdoutCapture.exceededLimit() ||
		stderrCapture.exceededLimit()) {
		trigger = "output_limit"
	}
	result := managedResult{
		exitCode: exitCodePointer(command.ProcessState),
		signal:   waitSignal(command.ProcessState),
		stdout:   stdoutCapture.observation("canonical_stdout"),
		stderr:   stderrCapture.observation("diagnostic_stderr"),
	}
	switch trigger {
	case "cancel":
		result.outcome = OutcomeCancelled
		result.runtimeErr = &contract.RuntimeError{
			Code: contract.ErrorCancelled, Phase: contract.PhaseTransport,
			Message: "CLI execution was cancelled",
		}
		return result
	case "deadline":
		result.outcome = OutcomeFailed
		result.runtimeErr = &contract.RuntimeError{
			Code: contract.ErrorTimeout, Phase: contract.PhaseTransport,
			Message: "CLI execution deadline exceeded",
		}
		return result
	case "output_limit":
		result.outcome = OutcomeFailed
		result.runtimeErr = &contract.RuntimeError{
			Code: contract.ErrorContextOverflow, Phase: contract.PhaseTransport,
			Message: "CLI execution output exceeded the configured limit",
		}
		return result
	}
	if waitErr != nil {
		result.outcome = OutcomeFailed
		message := "CLI executor exited unsuccessfully"
		if result.signal != "" {
			message = "CLI executor exited after signal " + result.signal
		} else if result.exitCode != nil {
			message = fmt.Sprintf(
				"CLI executor exited with status %d", *result.exitCode,
			)
		}
		result.runtimeErr = &contract.RuntimeError{
			Code:  contract.ErrorProviderUnavailable,
			Phase: contract.PhaseTransport, Message: message,
		}
		return result
	}
	decoded, err := runtimecommand.Decode(
		invocation.Argv[0], stdoutCapture.bytes(),
	)
	if err != nil {
		result.outcome = OutcomeFailed
		var runtimeErr *contract.RuntimeError
		if errors.As(err, &runtimeErr) {
			result.runtimeErr = runtimeErr
			return result
		}
		result.runtimeErr = &contract.RuntimeError{
			Code:    contract.ErrorInvalidProviderResponse,
			Phase:   contract.PhaseTransport,
			Message: "decode CLI canonical output: " + err.Error(),
		}
		return result
	}
	result.outcome = OutcomeCompleted
	result.assistant = decoded.Assistant
	return result
}

func (service *Service) persistExecution(value Execution) error {
	return service.store.withLock(value.SessionID, func() error {
		return service.store.writeExecution(value)
	})
}

func (service *Service) writeInvocationManifest(
	invocation runtimecommand.Invocation,
) (invocationManifestHandle, error) {
	if err := service.store.ensure(); err != nil {
		return invocationManifestHandle{}, err
	}
	directory, err := service.store.openPinnedDirectory(
		service.store.invocationDir,
	)
	if err != nil {
		return invocationManifestHandle{}, err
	}
	defer directory.close()
	directoryIdentity, err := directory.identity()
	if err != nil {
		return invocationManifestHandle{}, err
	}
	value := helperInvocationManifest{
		Path: invocation.Path, Argv: append([]string(nil), invocation.Argv...),
		Environment: append([]string(nil), invocation.Environment...),
		CWD:         invocation.CWD,
	}
	data, err := json.Marshal(value)
	if err != nil {
		return invocationManifestHandle{}, err
	}
	if len(data) > maxInvocationManifestBytes {
		return invocationManifestHandle{}, fmt.Errorf(
			"private invocation manifest exceeds size limit",
		)
	}
	name, file, err := createRandomRegularAt(
		directory, ".invocation-", ".json", 0o600,
	)
	if err != nil {
		return invocationManifestHandle{}, err
	}
	var initialStat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &initialStat); err != nil {
		_ = file.Close()
		// Without an inode identity there is no safe conditional unlink.
		// Leave the random private entry for bounded startup cleanup.
		return invocationManifestHandle{}, err
	}
	cleanup := true
	fileIdentity := safeFileIdentity{
		dev: uint64(initialStat.Dev),
		ino: uint64(initialStat.Ino),
	}
	defer func() {
		if file != nil {
			_ = file.Close()
		}
		if cleanup {
			_ = directory.removeRegular(name, &fileIdentity)
		}
	}()
	if _, err := file.Write(data); err != nil {
		return invocationManifestHandle{}, err
	}
	if err := file.Sync(); err != nil {
		return invocationManifestHandle{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return invocationManifestHandle{}, err
	}
	entry := safeDirectoryEntry{
		name:  name,
		mode:  uint32(stat.Mode),
		size:  stat.Size,
		dev:   uint64(stat.Dev),
		ino:   uint64(stat.Ino),
		nlink: uint64(stat.Nlink),
		mtime: statModifiedTime(stat),
	}
	if !entry.isRegular() ||
		entry.nlink != 1 ||
		entry.identity() != fileIdentity ||
		os.FileMode(entry.mode).Perm() != 0o600 ||
		entry.size > maxInvocationManifestBytes {
		return invocationManifestHandle{}, fmt.Errorf(
			"invalid private invocation manifest",
		)
	}
	visible, err := directory.statEntry(name)
	if err != nil {
		return invocationManifestHandle{}, err
	}
	if !visible.sameIdentity(fileIdentity) ||
		!visible.isRegular() ||
		visible.nlink != 1 {
		return invocationManifestHandle{}, fmt.Errorf(
			"private invocation manifest changed while creating",
		)
	}
	if err := file.Close(); err != nil {
		file = nil
		return invocationManifestHandle{}, err
	}
	file = nil
	if err := directory.sync(); err != nil {
		return invocationManifestHandle{}, err
	}
	cleanup = false
	return invocationManifestHandle{
		path:              filepath.Join(directory.path, name),
		name:              name,
		directoryIdentity: directoryIdentity,
		fileIdentity:      fileIdentity,
	}, nil
}

func (store *Store) removeInvocationManifest(
	manifest invocationManifestHandle,
) error {
	directory, err := store.openPinnedDirectory(store.invocationDir)
	if err != nil {
		return err
	}
	defer directory.close()
	identity, err := directory.identity()
	if err != nil {
		return err
	}
	if identity != manifest.directoryIdentity {
		return fmt.Errorf("private invocation directory changed identity")
	}
	return directory.removeRegular(manifest.name, &manifest.fileIdentity)
}

func (service *Service) cleanupInvocationManifests() error {
	if err := service.store.ensure(); err != nil {
		return err
	}
	directory, err := service.store.openPinnedDirectory(
		service.store.invocationDir,
	)
	if err != nil {
		return err
	}
	defer directory.close()
	entries, err := directory.entries()
	if err != nil {
		return err
	}
	cutoff := service.now().UTC().Add(-24 * time.Hour)
	removed := 0
	for _, entry := range entries {
		if removed >= 1000 {
			break
		}
		if !entry.isRegular() || entry.nlink != 1 {
			continue
		}
		if !strings.HasPrefix(entry.name, ".invocation-") ||
			!strings.HasSuffix(entry.name, ".json") ||
			os.FileMode(entry.mode).Perm() != 0o600 ||
			entry.size > maxInvocationManifestBytes ||
			!entry.mtime.Before(cutoff) {
			continue
		}
		data, opened, err := directory.readRegularFact(
			entry.name, maxInvocationManifestBytes,
		)
		if err != nil {
			return err
		}
		if opened.identity() != entry.identity() {
			return fmt.Errorf("private invocation manifest changed during cleanup")
		}
		var manifest helperInvocationManifest
		if err := decodeStrict(data, &manifest); err != nil ||
			manifest.Path == "" ||
			len(manifest.Argv) == 0 ||
			manifest.CWD == "" {
			continue
		}
		identity := opened.identity()
		if err := directory.removeRegular(entry.name, &identity); err != nil {
			return err
		}
		removed++
	}
	return nil
}

func withoutEnvironmentKeys(
	environment []string,
	keys ...string,
) []string {
	blocked := make(map[string]struct{}, len(keys))
	for _, key := range keys {
		blocked[key] = struct{}{}
	}
	result := make([]string, 0, len(environment))
	for _, value := range environment {
		key, _, found := strings.Cut(value, "=")
		if found {
			if _, reserved := blocked[key]; reserved {
				continue
			}
		}
		result = append(result, value)
	}
	return result
}

func managedFailure(code contract.ErrorCode, message string) managedResult {
	return managedResult{
		outcome: OutcomeFailed,
		runtimeErr: &contract.RuntimeError{
			Code: code, Phase: contract.PhaseTransport, Message: message,
		},
	}
}

func terminateAndWait(pgid int, waited <-chan error) error {
	_ = syscall.Kill(-pgid, syscall.SIGTERM)
	timer := time.NewTimer(2 * time.Second)
	defer timer.Stop()
	select {
	case err := <-waited:
		return err
	case <-timer.C:
	}
	_ = syscall.Kill(-pgid, syscall.SIGKILL)
	return <-waited
}

func exitCodePointer(state *os.ProcessState) *int {
	if state == nil {
		return nil
	}
	value := state.ExitCode()
	if value < 0 {
		return nil
	}
	return &value
}

func waitSignal(state *os.ProcessState) string {
	if state == nil {
		return ""
	}
	status, ok := state.Sys().(syscall.WaitStatus)
	if !ok || !status.Signaled() {
		return ""
	}
	return status.Signal().String()
}

type boundedCapture struct {
	mu       sync.Mutex
	limit    int
	observed int64
	prefix   []byte
	exceeded bool
	notify   chan<- struct{}
}

func newBoundedCapture(limit int, notify chan<- struct{}) *boundedCapture {
	return &boundedCapture{limit: limit, notify: notify}
}

func (capture *boundedCapture) Write(value []byte) (int, error) {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	capture.observed += int64(len(value))
	remaining := capture.limit - len(capture.prefix)
	if remaining > 0 {
		if remaining > len(value) {
			remaining = len(value)
		}
		capture.prefix = append(capture.prefix, value[:remaining]...)
	}
	if capture.observed > int64(capture.limit) && !capture.exceeded {
		capture.exceeded = true
		select {
		case capture.notify <- struct{}{}:
		default:
		}
	}
	return len(value), nil
}

func (capture *boundedCapture) bytes() []byte {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return append([]byte(nil), capture.prefix...)
}

func (capture *boundedCapture) exceededLimit() bool {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	return capture.exceeded
}

func (capture *boundedCapture) observation(summary string) StreamObservation {
	capture.mu.Lock()
	defer capture.mu.Unlock()
	observation := StreamObservation{
		ObservedBytes: capture.observed, Truncated: capture.exceeded,
		LimitExceeded: capture.exceeded, Summary: summary,
	}
	if len(capture.prefix) > 0 {
		observation.PrefixDigest = digest(capture.prefix)
	}
	return observation
}
