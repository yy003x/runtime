package tmux

import (
	"context"
	"crypto/rand"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"time"
	"unicode/utf8"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/profileid"
)

type Service struct {
	config         Config
	home           string
	homeDigest     string
	uid            int
	tmuxPath       string
	serverEnv      []string
	helperCommand  []string
	now            func() time.Time
	random         io.Reader
	readyTimeout   time.Duration
	gateTimeout    time.Duration
	commandTimeout time.Duration
	lookupProcess  func(int) (ProcessIdentity, error)
	runner         CommandRunner
	capabilityOnce sync.Once
	capabilityErr  error
}

func NewService(config Config) (*Service, error) {
	home, err := canonicalHome(config.Home)
	if err != nil {
		return nil, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseRequest,
			"invalid Runtime home: %v", err,
		)
	}
	if strings.TrimSpace(config.LockFile) == "" ||
		strings.TrimSpace(config.ManifestDir) == "" ||
		strings.TrimSpace(config.TmuxConfigFile) == "" ||
		strings.TrimSpace(config.SocketDir) == "" ||
		strings.TrimSpace(config.SocketFile) == "" {
		return nil, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseRequest,
			"Tmux paths are incomplete",
		)
	}
	expectedLockFile := filepath.Join(home, "state", "tmux.lock")
	expectedManifestDir := filepath.Join(home, "tmp", "tmux")
	expectedConfigFile := filepath.Join(home, "resources", "tmux.conf")
	if filepath.Clean(config.LockFile) != expectedLockFile ||
		filepath.Clean(config.ManifestDir) != expectedManifestDir ||
		filepath.Clean(config.TmuxConfigFile) != expectedConfigFile {
		return nil, runtimeError(
			contract.ErrorConflict, contract.PhaseTransport,
			"Tmux active-home paths do not match Runtime home",
		)
	}
	expectedSocketDir := filepath.Join(
		"/tmp", fmt.Sprintf("sn-cli-tmux-%d", os.Getuid()),
	)
	homeDigest := digestString(home)
	expectedSocket := filepath.Join(
		expectedSocketDir, homeDigest[:16]+".sock",
	)
	if filepath.Clean(config.SocketDir) != expectedSocketDir ||
		filepath.Clean(config.SocketFile) != expectedSocket {
		return nil, runtimeError(
			contract.ErrorConflict, contract.PhaseTransport,
			"Tmux socket path does not match Runtime home",
		)
	}
	tmuxPath := strings.TrimSpace(config.TmuxBinary)
	if tmuxPath == "" {
		tmuxPath = "tmux"
	}
	helper := append([]string(nil), config.HelperCommand...)
	if len(helper) == 0 {
		self, err := os.Executable()
		if err != nil {
			return nil, fmt.Errorf("resolve sn-cli helper executable: %w", err)
		}
		helper = []string{self, helperCommandName}
	}
	if len(helper) < 2 || strings.TrimSpace(helper[0]) == "" {
		return nil, fmt.Errorf("Tmux helper command is invalid")
	}
	for _, argument := range helper {
		if !utf8.ValidString(argument) ||
			strings.ContainsRune(argument, '\x00') {
			return nil, fmt.Errorf("Tmux helper command is invalid")
		}
	}
	now := config.Now
	if now == nil {
		now = time.Now
	}
	source := config.Random
	if source == nil {
		source = rand.Reader
	}
	readyTimeout := config.ReadyTimeout
	if readyTimeout <= 0 {
		readyTimeout = defaultReadyTimeout
	}
	gateTimeout := config.GateTimeout
	if gateTimeout <= 0 {
		gateTimeout = defaultGateTimeout
	}
	if readyTimeout > time.Minute || gateTimeout > time.Minute {
		return nil, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseRequest,
			"Tmux readiness and gate timeouts must not exceed one minute",
		)
	}
	commandTimeout := config.CommandTimeout
	if commandTimeout <= 0 {
		commandTimeout = 10 * time.Second
	}
	if commandTimeout > time.Minute {
		return nil, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseRequest,
			"Tmux command timeout must not exceed one minute",
		)
	}
	lookup := config.LookupProcess
	if lookup == nil {
		lookup = lookupProcessIdentity
	}
	runner := config.RunCommand
	if runner == nil {
		runner = osCommandRunner{}
	}
	return &Service{
		config: config, home: home, homeDigest: homeDigest, uid: os.Getuid(),
		tmuxPath: tmuxPath, serverEnv: sanitizedServerEnvironment(config.ServerEnv),
		helperCommand: helper, now: now, random: source,
		readyTimeout: readyTimeout, gateTimeout: gateTimeout,
		commandTimeout: commandTimeout, lookupProcess: lookup, runner: runner,
	}, nil
}

func (service *Service) Start(
	ctx context.Context,
	request StartRequest,
) (StartResult, error) {
	if service == nil {
		return StartResult{}, fmt.Errorf("Tmux service is required")
	}
	invocation, err := service.validateInvocation(request.Invocation)
	if err != nil {
		return StartResult{}, err
	}
	lock, err := acquireFileLock(service.config.LockFile, false)
	if err != nil {
		return StartResult{}, tmuxTransportError(
			contract.ErrorConflict, "acquire Tmux lifecycle lock: %v", err,
		)
	}
	defer lock.Close()
	if err := service.prepare(ctx, true); err != nil {
		return StartResult{}, err
	}
	return service.startWindow(ctx, invocation)
}

func (service *Service) List(ctx context.Context) ([]Window, error) {
	if service == nil {
		return nil, fmt.Errorf("Tmux service is required")
	}
	lock, err := acquireFileLock(service.config.LockFile, true)
	if err != nil {
		return nil, tmuxTransportError(
			contract.ErrorConflict, "acquire Tmux lifecycle lock: %v", err,
		)
	}
	defer lock.Close()
	if err := service.ensureCapability(ctx); err != nil {
		return nil, err
	}
	exists, err := service.socketExists()
	if err != nil {
		return nil, err
	}
	if !exists {
		return []Window{}, nil
	}
	marker, err := service.loadAndValidateServer(ctx)
	if err != nil {
		return nil, err
	}
	live, err := service.loadWindows(ctx, marker)
	if err != nil {
		return nil, err
	}
	result := make([]Window, 0, len(live))
	for _, value := range live {
		result = append(result, cloneWindow(value.Window))
	}
	sort.Slice(result, func(left, right int) bool {
		if result[left].CreatedAt.Equal(result[right].CreatedAt) {
			return result[left].TmuxID < result[right].TmuxID
		}
		return result[left].CreatedAt.Before(result[right].CreatedAt)
	})
	return result, nil
}

func (service *Service) Show(
	ctx context.Context,
	tmuxID string,
) (Window, error) {
	value, _, lock, err := service.resolve(ctx, tmuxID, true)
	if err != nil {
		return Window{}, err
	}
	defer lock.Close()
	return cloneWindow(value.Window), nil
}

func (service *Service) Send(
	ctx context.Context,
	tmuxID string,
	input string,
) (ActionResult, error) {
	if err := validateSendInput(input); err != nil {
		return ActionResult{}, err
	}
	value, marker, lock, err := service.resolve(ctx, tmuxID, false)
	if err != nil {
		return ActionResult{}, err
	}
	defer lock.Close()
	if value.State != StateRunning || value.Record == nil {
		return ActionResult{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux window %s is %s, not running", tmuxID, value.State,
		)
	}
	bufferID, err := newUUIDv7(service.now(), service.random)
	if err != nil {
		return ActionResult{}, err
	}
	bufferName := "sn-" + strings.ReplaceAll(bufferID, "-", "")
	defer service.deleteBuffer(context.Background(), bufferName)
	condition := registeredIdentityCondition(marker, value)
	trueCommand := fmt.Sprintf(
		"paste-buffer -dpr -b %s -t %s ; send-keys -t %s Enter",
		bufferName, value.PaneID, value.PaneID,
	)
	falseCommand := fmt.Sprintf(
		"delete-buffer -b %s ; run-shell \"exit 73\"", bufferName,
	)
	result, err := service.runTmux(
		ctx, strings.NewReader(input),
		"load-buffer", "-b", bufferName, "-",
		";", "if-shell", "-F", "-t", value.PaneID,
		condition, trueCommand, falseCommand,
	)
	if err != nil {
		return ActionResult{}, err
	}
	if result.ExitCode != 0 {
		return ActionResult{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux window %s identity changed during send", tmuxID,
		)
	}
	return ActionResult{TmuxID: tmuxID, Action: "send", Accepted: true}, nil
}

func (service *Service) Interrupt(
	ctx context.Context,
	tmuxID string,
) (ActionResult, error) {
	value, marker, lock, err := service.resolve(ctx, tmuxID, false)
	if err != nil {
		return ActionResult{}, err
	}
	defer lock.Close()
	if value.State != StateRunning || value.Record == nil {
		return ActionResult{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux window %s is %s, not running", tmuxID, value.State,
		)
	}
	condition := registeredIdentityCondition(marker, value)
	trueCommand := fmt.Sprintf("send-keys -t %s C-c", value.PaneID)
	result, err := service.runTmux(
		ctx, nil,
		"if-shell", "-F", "-t", value.PaneID,
		condition, trueCommand, `run-shell "exit 73"`,
	)
	if err != nil {
		return ActionResult{}, err
	}
	if result.ExitCode != 0 {
		return ActionResult{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux window %s identity changed during interrupt", tmuxID,
		)
	}
	return ActionResult{
		TmuxID: tmuxID, Action: "interrupt", Accepted: true,
	}, nil
}

func (service *Service) Stop(
	ctx context.Context,
	tmuxID string,
) (ActionResult, error) {
	value, marker, lock, err := service.resolve(ctx, tmuxID, false)
	if err != nil {
		return ActionResult{}, err
	}
	defer lock.Close()
	windows, err := service.loadWindows(ctx, marker)
	if err != nil {
		return ActionResult{}, err
	}
	last := len(windows) == 1
	var stoppedSocket os.FileInfo
	if last {
		stoppedSocket, err = os.Lstat(service.config.SocketFile)
		if err != nil {
			return ActionResult{}, tmuxTransportError(
				contract.ErrorConflict, "capture Tmux socket identity: %v", err,
			)
		}
	}
	condition := orphanIdentityCondition(marker, value)
	if value.Record != nil {
		condition = registeredIdentityCondition(marker, value)
	}
	if last {
		condition = tmuxAnd(
			condition,
			tmuxEquals("#{session_windows}", "2"),
		)
	}
	trueCommand := fmt.Sprintf("kill-window -t %s", value.WindowID)
	if last {
		trueCommand = "kill-session -t " + SessionName
	}
	result, err := service.runTmux(
		ctx, nil,
		"if-shell", "-F", "-t", value.PaneID,
		condition, trueCommand, `run-shell "exit 73"`,
	)
	if err != nil {
		return ActionResult{}, err
	}
	if result.ExitCode != 0 {
		return ActionResult{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux window %s identity changed during stop", tmuxID,
		)
	}
	if last {
		if err := service.removeStoppedSocket(
			context.Background(), stoppedSocket,
		); err != nil {
			return ActionResult{}, err
		}
	}
	return ActionResult{TmuxID: tmuxID, Action: "stop", Accepted: true}, nil
}

// Attach connects the calling terminal to one registered window. The
// lifecycle lock is retained until the server-side identity conditional and
// attach/switch command queue acknowledges processing, then released while
// the interactive client remains connected.
func (service *Service) Attach(
	ctx context.Context,
	tmuxID string,
	files TTYFiles,
) error {
	if err := validateTTYFiles(files); err != nil {
		return err
	}
	value, marker, lock, err := service.resolve(ctx, tmuxID, true)
	if err != nil {
		return err
	}
	if value.Record == nil || value.State == StateOrphaned {
		lock.Close()
		return tmuxTransportError(
			contract.ErrorConflict,
			"Tmux window %s is orphaned and cannot be attached", tmuxID,
		)
	}
	condition := registeredIdentityCondition(marker, value)
	currentSocket, insideTmux := currentTmuxSocket(os.Getenv("TMUX"))
	action := fmt.Sprintf(
		"attach-session -t %s:%s", SessionName, value.WindowID,
	)
	if insideTmux {
		if filepath.Clean(currentSocket) != filepath.Clean(service.config.SocketFile) {
			lock.Close()
			return tmuxTransportError(
				contract.ErrorConflict,
				"cannot attach from a different tmux server",
			)
		}
		action = fmt.Sprintf(
			"switch-client -t %s:%s", SessionName, value.WindowID,
		)
	}
	nonce, err := randomHex(service.random, 16)
	if err != nil {
		lock.Close()
		return err
	}
	ackChannel := "sn-attach-" + nonce
	trueCommand := action + " ; wait-for -S " + ackChannel
	falseCommand := "wait-for -S " + ackChannel +
		` ; run-shell "exit 73"`
	args := []string{
		"-S", service.config.SocketFile,
		"if-shell", "-F", "-t", value.PaneID,
		condition, trueCommand, falseCommand,
	}
	attachCtx, cancelAttach := context.WithCancel(ctx)
	defer cancelAttach()
	type attachOutcome struct {
		result CommandResult
		err    error
	}
	attachDone := make(chan attachOutcome, 1)
	go func() {
		result, runErr := service.runner.Run(attachCtx, CommandSpec{
			Path: service.tmuxPath, Args: args, Env: os.Environ(),
			Stdin: files.Stdin, Stdout: files.Stdout, Stderr: files.Stderr,
		})
		attachDone <- attachOutcome{result: result, err: runErr}
	}()
	ackCtx, cancelAck := context.WithTimeout(ctx, service.commandTimeout)
	ackDone := make(chan attachOutcome, 1)
	go func() {
		result, waitErr := service.runTmux(
			ackCtx, nil, "wait-for", ackChannel,
		)
		ackDone <- attachOutcome{result: result, err: waitErr}
	}()

	var outcome attachOutcome
	select {
	case outcome = <-attachDone:
		cancelAck()
		lock.Close()
	case ack := <-ackDone:
		cancelAck()
		if ack.err != nil || ack.result.ExitCode != 0 {
			cancelAttach()
			outcome = <-attachDone
			lock.Close()
			if ack.err != nil {
				return ack.err
			}
			return tmuxTransportError(
				contract.ErrorConflict,
				"Tmux window %s attach acknowledgement failed", tmuxID,
			)
		}
		lock.Close()
		outcome = <-attachDone
	case <-ctx.Done():
		cancelAck()
		cancelAttach()
		outcome = <-attachDone
		lock.Close()
		if outcome.err != nil {
			return tmuxTransportError(
				contract.ErrorProviderUnavailable,
				"attach tmux client: %v", outcome.err,
			)
		}
		return ctx.Err()
	}
	result, runErr := outcome.result, outcome.err
	if runErr != nil {
		return tmuxTransportError(
			contract.ErrorProviderUnavailable, "attach tmux client: %v", runErr,
		)
	}
	if result.ExitCode != 0 {
		return tmuxTransportError(
			contract.ErrorConflict,
			"Tmux window %s identity changed during attach", tmuxID,
		)
	}
	return nil
}

func currentTmuxSocket(value string) (string, bool) {
	if value == "" {
		return "", false
	}
	socket, _, ok := strings.Cut(value, ",")
	if !ok || !filepath.IsAbs(socket) {
		return "", true
	}
	return socket, true
}

func validateTTYFiles(files TTYFiles) error {
	if files.Stdin == nil || files.Stdout == nil ||
		!isTerminal(files.Stdin) || !isTerminal(files.Stdout) {
		return runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseRequest,
			"tmux attach requires TTY stdin and stdout",
		)
	}
	return nil
}

func (service *Service) resolve(
	ctx context.Context,
	tmuxID string,
	shared bool,
) (liveWindow, serverMarker, *fileLock, error) {
	if service == nil {
		return liveWindow{}, serverMarker{}, nil, fmt.Errorf("Tmux service is required")
	}
	if _, err := parseUUIDv7(tmuxID); err != nil {
		return liveWindow{}, serverMarker{}, nil, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseRequest,
			"invalid tmux_id %q", tmuxID,
		)
	}
	lock, err := acquireFileLock(service.config.LockFile, shared)
	if err != nil {
		return liveWindow{}, serverMarker{}, nil, tmuxTransportError(
			contract.ErrorConflict, "acquire Tmux lifecycle lock: %v", err,
		)
	}
	fail := func(err error) (liveWindow, serverMarker, *fileLock, error) {
		lock.Close()
		return liveWindow{}, serverMarker{}, nil, err
	}
	if err := service.ensureCapability(ctx); err != nil {
		return fail(err)
	}
	exists, err := service.socketExists()
	if err != nil {
		return fail(err)
	}
	if !exists {
		return fail(runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseRequest,
			"unknown tmux_id %q", tmuxID,
		))
	}
	marker, err := service.loadAndValidateServer(ctx)
	if err != nil {
		return fail(err)
	}
	windows, err := service.loadWindows(ctx, marker)
	if err != nil {
		return fail(err)
	}
	for _, value := range windows {
		if value.TmuxID == tmuxID {
			return value, marker, lock, nil
		}
	}
	return fail(runtimeError(
		contract.ErrorInvalidRequest, contract.PhaseRequest,
		"unknown tmux_id %q", tmuxID,
	))
}

func (service *Service) prepare(ctx context.Context, createSocketDir bool) error {
	if err := service.ensureCapability(ctx); err != nil {
		return err
	}
	for _, directory := range []string{
		service.home,
		filepath.Join(service.home, "state"),
		filepath.Join(service.home, "tmp"),
		filepath.Join(service.home, "resources"),
	} {
		if err := requirePrivateDir(directory, service.uid); err != nil {
			return tmuxTransportError(
				contract.ErrorConflict,
				"validate Runtime directory for Tmux: %v", err,
			)
		}
	}
	if err := ensurePrivateDir(service.config.ManifestDir, service.uid); err != nil {
		return tmuxTransportError(
			contract.ErrorConflict, "validate Tmux manifest directory: %v", err,
		)
	}
	if err := service.cleanupStaleLaunchFiles(); err != nil {
		return err
	}
	if createSocketDir {
		if err := ensurePrivateDir(service.config.SocketDir, service.uid); err != nil {
			return tmuxTransportError(
				contract.ErrorConflict, "validate Tmux socket directory: %v", err,
			)
		}
	}
	_, err := service.readConfigDigest()
	return err
}

func (service *Service) cleanupStaleLaunchFiles() error {
	entries, err := os.ReadDir(service.config.ManifestDir)
	if err != nil {
		return tmuxTransportError(
			contract.ErrorConflict, "scan Tmux launch directory: %v", err,
		)
	}
	maxAge := 2 * (service.readyTimeout + service.gateTimeout + service.commandTimeout)
	if maxAge < 2*time.Minute {
		maxAge = 2 * time.Minute
	}
	now := time.Now()
	for _, entry := range entries {
		name := entry.Name()
		if !launchFilePattern.MatchString(name) &&
			!strings.HasPrefix(name, ".sn-tmux-write-") {
			return tmuxTransportError(
				contract.ErrorProtocol,
				"Tmux launch directory contains unexpected entry %q", name,
			)
		}
		path := filepath.Join(service.config.ManifestDir, name)
		info, err := os.Lstat(path)
		if err != nil {
			return tmuxTransportError(
				contract.ErrorConflict,
				"inspect Tmux launch artifact %q: %v", name, err,
			)
		}
		if info.Mode()&os.ModeSymlink != 0 ||
			!info.Mode().IsRegular() ||
			info.Mode().Perm() != 0o600 {
			return tmuxTransportError(
				contract.ErrorConflict,
				"Tmux launch artifact %q is not a private regular file", name,
			)
		}
		if err := requireOwner(info, service.uid, path); err != nil {
			return tmuxTransportError(
				contract.ErrorConflict,
				"validate Tmux launch artifact %q: %v", name, err,
			)
		}
		age := now.Sub(info.ModTime())
		if age < -time.Minute {
			return tmuxTransportError(
				contract.ErrorConflict,
				"Tmux launch artifact %q has a future timestamp", name,
			)
		}
		if age <= maxAge {
			continue
		}
		if err := os.Remove(path); err != nil && !os.IsNotExist(err) {
			return tmuxTransportError(
				contract.ErrorConflict,
				"remove stale Tmux launch artifact %q: %v", name, err,
			)
		}
	}
	return nil
}

func (service *Service) readConfigDigest() (string, error) {
	if err := requirePrivateDir(
		filepath.Dir(service.config.TmuxConfigFile), service.uid,
	); err != nil {
		return "", tmuxTransportError(
			contract.ErrorConflict,
			"validate fixed tmux config directory: %v", err,
		)
	}
	value, err := readSourceRegular(service.config.TmuxConfigFile, 1<<20)
	if err != nil {
		return "", tmuxTransportError(
			contract.ErrorProtocol,
			"read fixed tmux config: %v", err,
		)
	}
	return digestBytes(value), nil
}

func (service *Service) ensureCapability(ctx context.Context) error {
	service.capabilityOnce.Do(func() {
		path, err := exec.LookPath(service.tmuxPath)
		if err != nil {
			service.capabilityErr = tmuxTransportError(
				contract.ErrorProviderUnavailable,
				"tmux binary is unavailable: %v", err,
			)
			return
		}
		service.tmuxPath = path
		result, err := service.run(
			ctx, CommandSpec{
				Path: path, Args: []string{"-V"}, Env: service.serverEnv,
			},
		)
		if err != nil || result.ExitCode != 0 {
			if err == nil {
				err = commandFailure("tmux -V", result)
			}
			service.capabilityErr = tmuxTransportError(
				contract.ErrorProviderUnavailable,
				"inspect tmux capability: %v", err,
			)
			return
		}
		version := strings.TrimSpace(string(result.Stdout))
		if !supportedTmuxVersion(version) {
			service.capabilityErr = tmuxTransportError(
				contract.ErrorProtocol,
				"unsupported tmux version %q; tmux 3.2 or newer is required",
				version,
			)
		}
	})
	return service.capabilityErr
}

func (service *Service) runTmux(
	ctx context.Context,
	stdin io.Reader,
	args ...string,
) (CommandResult, error) {
	full := append([]string{"-S", service.config.SocketFile}, args...)
	result, err := service.run(ctx, CommandSpec{
		Path: service.tmuxPath, Args: full, Env: service.serverEnv, Stdin: stdin,
	})
	if err != nil {
		return CommandResult{}, tmuxTransportError(
			contract.ErrorProviderUnavailable, "run tmux: %v", err,
		)
	}
	return result, nil
}

func (service *Service) run(
	ctx context.Context,
	spec CommandSpec,
) (CommandResult, error) {
	if service.commandTimeout <= 0 {
		return service.runner.Run(ctx, spec)
	}
	childCtx, cancel := context.WithTimeout(ctx, service.commandTimeout)
	defer cancel()
	return service.runner.Run(childCtx, spec)
}

func (service *Service) socketExists() (bool, error) {
	info, err := os.Lstat(service.config.SocketFile)
	if os.IsNotExist(err) {
		return false, nil
	}
	if err != nil {
		return false, tmuxTransportError(
			contract.ErrorConflict, "inspect Tmux socket: %v", err,
		)
	}
	if info.Mode()&os.ModeSymlink != 0 || info.Mode()&os.ModeSocket == 0 {
		return false, tmuxTransportError(
			contract.ErrorConflict, "Tmux socket path is not a Unix socket",
		)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return false, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux socket must not be accessible by group or other (mode=%o)",
			info.Mode().Perm(),
		)
	}
	if err := requireOwner(info, service.uid, service.config.SocketFile); err != nil {
		return false, tmuxTransportError(
			contract.ErrorConflict, "validate Tmux socket owner: %v", err,
		)
	}
	return true, nil
}

func (service *Service) validateInvocation(
	value Invocation,
) (Invocation, error) {
	if err := profileid.Validate(value.ProfileID); err != nil {
		return Invocation{}, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseRequest,
			"invalid profile_id: %v", err,
		)
	}
	path, _, err := executableIdentity(value.Path)
	if err != nil {
		return Invocation{}, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseProfile,
			"invalid command executable: %v", err,
		)
	}
	if len(value.Argv) == 0 || value.Argv[0] == "" {
		return Invocation{}, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseProfile,
			"command argv is required",
		)
	}
	if !filepath.IsAbs(value.CWD) {
		return Invocation{}, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseProfile,
			"command cwd must be absolute",
		)
	}
	info, err := os.Stat(value.CWD)
	if err != nil || !info.IsDir() {
		return Invocation{}, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseProfile,
			"command cwd is not an accessible directory",
		)
	}
	if !configDigestPattern.MatchString(value.ConfigDigest) {
		return Invocation{}, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseProfile,
			"command config digest must be 64 lowercase hexadecimal characters",
		)
	}
	if err := validateExactEnvironment(value.Environment); err != nil {
		return Invocation{}, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseProfile,
			"invalid command environment: %v", err,
		)
	}
	for _, argument := range value.Argv {
		if !utf8.ValidString(argument) ||
			strings.ContainsRune(argument, '\x00') {
			return Invocation{}, runtimeError(
				contract.ErrorInvalidRequest, contract.PhaseProfile,
				"command argv must be UTF-8 without NUL",
			)
		}
	}
	result := value
	result.Path = path
	result.Argv = append([]string(nil), value.Argv...)
	result.Environment = append([]string(nil), value.Environment...)
	result.CWD = filepath.Clean(value.CWD)
	return result, nil
}

func validateSendInput(value string) error {
	if value == "" {
		return runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseRequest,
			"Tmux input must not be empty",
		)
	}
	if len(value) > maxSendBytes {
		return runtimeError(
			contract.ErrorContextOverflow, contract.PhaseRequest,
			"Tmux input exceeds %d bytes", maxSendBytes,
		)
	}
	if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
		return runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseRequest,
			"Tmux input must be UTF-8 without NUL",
		)
	}
	return nil
}

func validateExactEnvironment(values []string) error {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		if !utf8.ValidString(value) || strings.ContainsRune(value, '\x00') {
			return fmt.Errorf("environment must be UTF-8 without NUL")
		}
		index := strings.IndexByte(value, '=')
		if index <= 0 {
			return fmt.Errorf("environment entry %q is invalid", value)
		}
		name := value[:index]
		if _, exists := seen[name]; exists {
			return fmt.Errorf("environment contains duplicate %q", name)
		}
		seen[name] = struct{}{}
	}
	return nil
}

func sanitizedServerEnvironment(values []string) []string {
	if values == nil {
		values = os.Environ()
	}
	allowed := map[string]bool{
		"HOME": true, "LANG": true, "LC_ALL": true, "LC_CTYPE": true,
		"PATH": true, "SHELL": true, "TERMINFO": true, "TERMINFO_DIRS": true,
	}
	effective := make(map[string]string, len(allowed))
	for _, value := range values {
		index := strings.IndexByte(value, '=')
		if index <= 0 || !allowed[value[:index]] {
			continue
		}
		effective[value[:index]] = value[index+1:]
	}
	// Tmux uses a shell for the fixed if-shell failure branches. Do not let
	// caller-controlled PATH or SHELL change those server-side semantics.
	effective["PATH"] = "/usr/bin:/bin"
	effective["SHELL"] = "/bin/sh"
	result := make([]string, 0, len(effective))
	for name, value := range effective {
		result = append(result, name+"="+value)
	}
	sort.Strings(result)
	return result
}

func supportedTmuxVersion(value string) bool {
	fields := strings.Fields(value)
	if len(fields) < 2 || fields[0] != "tmux" {
		return false
	}
	number := strings.TrimLeft(fields[1], "v")
	parts := strings.SplitN(number, ".", 3)
	if len(parts) < 2 {
		return false
	}
	major, err := strconv.Atoi(parts[0])
	if err != nil {
		return false
	}
	minorText := parts[1]
	for index, character := range minorText {
		if character < '0' || character > '9' {
			minorText = minorText[:index]
			break
		}
	}
	minor, err := strconv.Atoi(minorText)
	if err != nil {
		return false
	}
	return major > 3 || major == 3 && minor >= 2
}

func tmuxTransportError(
	code contract.ErrorCode,
	format string,
	args ...any,
) error {
	return runtimeError(code, contract.PhaseTransport, format, args...)
}

func encodeServerMarker(value serverMarker) string {
	data, _ := json.Marshal(value)
	return base64.RawURLEncoding.EncodeToString(data)
}

func identityCondition(serverEncoded, recordEncoded string) string {
	return tmuxAnd(
		tmuxEquals("#{"+serverOptionName+"}", serverEncoded),
		tmuxEquals("#{"+windowRecordOption+"}", recordEncoded),
		tmuxEquals("#{"+windowCommitOption+"}", "1"),
	)
}

func registeredIdentityCondition(
	marker serverMarker,
	value liveWindow,
) string {
	return strictWindowIdentityCondition(
		identityCondition(
			encodeServerMarker(marker), value.RecordEncoded,
		),
		value.WindowID, value.PaneID, value.WindowName,
	)
}

func orphanIdentityCondition(
	marker serverMarker,
	value liveWindow,
) string {
	return strictWindowIdentityCondition(
		tmuxAnd(
			tmuxEquals(
				"#{"+serverOptionName+"}", encodeServerMarker(marker),
			),
			tmuxEquals(
				"#{"+windowRecordOption+"}", value.RecordEncoded,
			),
			tmuxEquals("#{"+windowCommitOption+"}", ""),
		),
		value.WindowID, value.PaneID, value.WindowName,
	)
}

func strictWindowIdentityCondition(
	base string,
	windowID string,
	paneID string,
	windowName string,
) string {
	return tmuxAnd(
		base,
		tmuxEquals("#{window_id}", windowID),
		tmuxEquals("#{pane_id}", paneID),
		tmuxEquals("#{window_name}", windowName),
		tmuxEquals("#{session_name}", SessionName),
		tmuxEquals("#{window_linked}", "0"),
		tmuxEquals("#{window_panes}", "1"),
	)
}

func tmuxEquals(left string, right string) string {
	return fmt.Sprintf("#{==:%s,%s}", left, right)
}

func tmuxAnd(values ...string) string {
	if len(values) == 0 {
		return "1"
	}
	result := values[len(values)-1]
	for index := len(values) - 2; index >= 0; index-- {
		result = fmt.Sprintf("#{&&:%s,%s}", values[index], result)
	}
	return result
}

func (service *Service) deleteBuffer(ctx context.Context, name string) {
	_, _ = service.runTmux(ctx, nil, "delete-buffer", "-b", name)
}

func (service *Service) waitSocketGone(timeout time.Duration) {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if _, err := os.Lstat(service.config.SocketFile); os.IsNotExist(err) {
			return
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func (service *Service) removeStoppedSocket(
	ctx context.Context,
	expected os.FileInfo,
) error {
	result, err := service.runTmux(ctx, nil, "list-sessions")
	if err != nil {
		return err
	}
	if result.ExitCode == 0 {
		return tmuxTransportError(
			contract.ErrorConflict,
			"Tmux server remained live after its final managed window stopped",
		)
	}
	current, err := os.Lstat(service.config.SocketFile)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return tmuxTransportError(
			contract.ErrorConflict, "inspect stopped Tmux socket: %v", err,
		)
	}
	if expected == nil || !os.SameFile(expected, current) ||
		current.Mode()&os.ModeSymlink != 0 ||
		current.Mode()&os.ModeSocket == 0 ||
		current.Mode().Perm()&0o077 != 0 {
		return tmuxTransportError(
			contract.ErrorConflict,
			"stopped Tmux socket identity changed",
		)
	}
	if err := requireOwner(current, service.uid, service.config.SocketFile); err != nil {
		return tmuxTransportError(
			contract.ErrorConflict, "validate stopped Tmux socket owner: %v", err,
		)
	}
	if err := os.Remove(service.config.SocketFile); err != nil &&
		!os.IsNotExist(err) {
		return tmuxTransportError(
			contract.ErrorConflict, "remove stopped Tmux socket: %v", err,
		)
	}
	return nil
}
