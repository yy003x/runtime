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
)

const (
	maxCanonicalStdoutBytes    = 16 << 20
	maxDiagnosticStderrBytes   = 256 << 10
	maxInvocationManifestBytes = 1 << 20
	execHelperArgument         = "__sn_private_session_exec_helper"
	execHelperManifestEnv      = "SN_PRIVATE_SESSION_EXEC_MANIFEST"
)

type helperInvocationManifest struct {
	Path        string   `json:"path"`
	Argv        []string `json:"argv"`
	Environment []string `json:"environment"`
	CWD         string   `json:"cwd"`
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
	manifestPath, err := service.writeInvocationManifest(invocation)
	if err != nil {
		return managedFailure(contract.ErrorInternal, "prepare private invocation manifest")
	}
	defer os.Remove(manifestPath)
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
		append([]string(nil), service.environ...),
		execHelperManifestEnv+"="+manifestPath,
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
) (string, error) {
	directory := filepath.Join(service.store.stateDir, "session-invocations")
	if err := ensureDirectory(directory); err != nil {
		return "", err
	}
	temp, err := os.CreateTemp(directory, ".invocation-*.json")
	if err != nil {
		return "", err
	}
	path := temp.Name()
	if err := temp.Close(); err != nil {
		os.Remove(path)
		return "", err
	}
	os.Remove(path)
	value := helperInvocationManifest{
		Path: invocation.Path, Argv: append([]string(nil), invocation.Argv...),
		Environment: append([]string(nil), invocation.Environment...),
		CWD:         invocation.CWD,
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	if len(data) > maxInvocationManifestBytes {
		return "", fmt.Errorf("private invocation manifest exceeds size limit")
	}
	if err := atomicJSON(path, value, 0o600); err != nil {
		return "", err
	}
	return path, nil
}

func (service *Service) cleanupInvocationManifests() error {
	directory := filepath.Join(service.store.stateDir, "session-invocations")
	info, err := os.Lstat(directory)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf(
			"private invocation directory must be a directory, not a symlink",
		)
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return err
	}
	cutoff := service.now().UTC().Add(-24 * time.Hour)
	removed := 0
	for _, entry := range entries {
		if removed >= 1000 {
			break
		}
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 {
			continue
		}
		if !strings.HasPrefix(entry.Name(), ".invocation-") ||
			!strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		path := filepath.Join(directory, entry.Name())
		info, err := entry.Info()
		if err != nil {
			return err
		}
		if !info.Mode().IsRegular() || !info.ModTime().Before(cutoff) {
			continue
		}
		if err := os.Remove(path); err != nil &&
			!errors.Is(err, os.ErrNotExist) {
			return err
		}
		removed++
	}
	return nil
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
