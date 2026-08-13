package nativeconsole

import (
	"context"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"os/signal"
	"path/filepath"
	"strings"
	"syscall"
	"time"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/pkg/contract"
	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
	"github.com/yy003x/runtime/pkg/tmux"
)

type TmuxManager interface {
	Start(context.Context, tmux.StartRequest) (tmux.StartResult, error)
	List(context.Context) ([]tmux.Window, error)
	Stop(context.Context, string) (tmux.ActionResult, error)
}

type Service struct {
	paths      layout.Paths
	sessions   *session.Service
	lifecycles *runtime.NativeTUIService
	tmux       TmuxManager
	helper     string
	now        func() time.Time
}

type ServiceOptions struct {
	Paths      layout.Paths
	Sessions   *session.Service
	Lifecycles *runtime.NativeTUIService
	Tmux       TmuxManager
	Helper     string
	Now        func() time.Time
}

type OpenRequest struct {
	SessionID    string
	Retention    session.Retention
	Target       tmux.Invocation
	Input        string
	TaskID       string
	Model        string
	Effort       string
	InitialInput bool
}

type OpenResult struct {
	Session              session.Session            `json:"session"`
	Run                  runtime.Record             `json:"run"`
	Execution            runtime.NativeTUIExecution `json:"execution"`
	Window               tmux.Window                `json:"tmux_window"`
	LaunchAccepted       bool                       `json:"launch_accepted"`
	InitialInputSupplied bool                       `json:"initial_input_supplied"`
}

type CloseResult struct {
	SessionID string `json:"session_id"`
	RunID     string `json:"run_id,omitempty"`
	TmuxID    string `json:"tmux_id"`
	Action    string `json:"action"`
	Accepted  bool   `json:"accepted"`
}

func NewService(options ServiceOptions) (*Service, error) {
	if options.Sessions == nil || options.Lifecycles == nil ||
		options.Tmux == nil || options.Helper == "" {
		return nil, fmt.Errorf("native_tui console dependencies are incomplete")
	}
	if !filepath.IsAbs(options.Helper) {
		return nil, fmt.Errorf("native_tui supervisor executable must be absolute")
	}
	if options.Now == nil {
		options.Now = time.Now
	}
	return &Service{
		paths: options.Paths, sessions: options.Sessions,
		lifecycles: options.Lifecycles, tmux: options.Tmux,
		helper: options.Helper, now: options.Now,
	}, nil
}

func (service *Service) Open(
	ctx context.Context,
	request OpenRequest,
) (OpenResult, error) {
	if request.Target.Binding != nil {
		return OpenResult{}, fmt.Errorf("native_tui target already has a binding")
	}
	if request.SessionID == "" {
		var err error
		request.SessionID, err = session.NewID()
		if err != nil {
			return OpenResult{}, err
		}
	}
	executionID, err := identity.New("execution")
	if err != nil {
		return OpenResult{}, err
	}
	sessionValue, err := service.sessions.CreateNativeTUIWithID(
		request.SessionID, request.Retention,
	)
	if err != nil {
		return OpenResult{}, err
	}
	record, execution, runtimeErr := service.lifecycles.Begin(
		ctx, runtime.NativeTUIBeginRequest{
			SessionID: request.SessionID, ExecutionID: executionID,
			ProfileID: request.Target.ProfileID, Input: request.Input,
			TaskID: request.TaskID, CWD: request.Target.CWD,
			Model: request.Model, Effort: request.Effort,
			ConfigDigest: request.Target.ConfigDigest,
		},
	)
	if runtimeErr != nil {
		return OpenResult{Session: sessionValue}, runtimeErr
	}
	manifest := supervisorManifest{
		SchemaVersion: manifestSchema, Home: service.paths.Home,
		SessionID: request.SessionID, RunID: record.ID,
		ExecutionID: executionID,
		Target: targetInvocation{
			Path: request.Target.Path,
			Argv: append([]string(nil), request.Target.Argv...),
			Environment: append(
				[]string(nil), request.Target.Environment...,
			),
			CWD: request.Target.CWD,
		},
	}
	manifestPath, digest, err := writeSupervisorManifest(service.paths, manifest)
	if err != nil {
		return service.failOpen(
			ctx, sessionValue, record, execution, tmux.Window{}, err,
		)
	}
	defer removeSupervisorManifest(manifestPath)
	supervisor := tmux.Invocation{
		ProfileID: request.Target.ProfileID, Path: service.helper,
		Argv: []string{
			service.helper, SupervisorCommand,
			"--manifest", manifestPath, "--digest", digest,
		},
		// The resolved Provider environment stays only in the private,
		// consume-on-read manifest. The supervisor bootstrap needs the Runtime
		// home but must not duplicate Provider secrets into the tmux launch env.
		Environment: supervisorEnvironment(
			service.paths.Home, request.Target.Environment,
		),
		CWD: request.Target.CWD, ConfigDigest: request.Target.ConfigDigest,
		Binding:          &tmux.Binding{Kind: "session", ID: request.SessionID},
		CooperativeReady: true,
	}
	started, err := service.tmux.Start(
		ctx, tmux.StartRequest{Invocation: supervisor},
	)
	if err != nil {
		return service.failOpen(
			ctx, sessionValue, record, execution, tmux.Window{}, err,
		)
	}
	if !started.LaunchAccepted {
		message := "native TUI provider did not start"
		if started.Window.LaunchError != nil {
			message = started.Window.LaunchError.Message
		}
		return service.failOpen(
			ctx, sessionValue, record, execution, started.Window,
			errors.New(message),
		)
	}
	execution.TmuxID = started.Window.TmuxID
	current, getErr := service.lifecycles.Get(ctx, record.ID)
	if getErr == nil {
		record = current
		if currentExecution, decodeErr := runtime.NativeTUIExecutionFromRecord(
			current,
		); decodeErr == nil {
			execution = currentExecution
		}
	}
	return OpenResult{
		Session: sessionValue, Run: record, Execution: execution,
		Window: started.Window, LaunchAccepted: true,
		InitialInputSupplied: request.InitialInput,
	}, nil
}

func (service *Service) failOpen(
	ctx context.Context,
	sessionValue session.Session,
	record runtime.Record,
	execution runtime.NativeTUIExecution,
	window tmux.Window,
	cause error,
) (OpenResult, error) {
	runtimeErr := &contract.RuntimeError{
		Code:  contract.ErrorProviderUnavailable,
		Phase: contract.PhaseTransport, Message: cause.Error(),
	}
	settled := runtime.NewSettledNativeTUIExecution(
		record, window.TmuxID, window.ExitCode, window.Signal,
		runtime.NativeTUIOutcomeFailed, "launch_failed", runtimeErr,
		service.now(),
	)
	terminal, settleErr := service.lifecycles.Settle(
		context.WithoutCancel(ctx), settled,
	)
	if window.TmuxID != "" {
		_, _ = service.tmux.Stop(context.Background(), window.TmuxID)
	}
	result := OpenResult{
		Session: sessionValue, Run: terminal, Execution: settled,
		Window: window, LaunchAccepted: false,
	}
	if settleErr != nil {
		return result, fmt.Errorf(
			"native_tui launch failed and lifecycle settlement failed: %v; original error: %w",
			settleErr, cause,
		)
	}
	return result, cause
}

func (service *Service) CloseSession(
	ctx context.Context,
	sessionID string,
) (CloseResult, error) {
	window, err := service.findWindow(ctx, sessionID)
	if err != nil {
		return CloseResult{}, err
	}
	record, found, err := service.lifecycles.OpenForSession(ctx, sessionID)
	if err != nil {
		return CloseResult{}, err
	}
	result := CloseResult{
		SessionID: sessionID, TmuxID: window.TmuxID,
		Action: "close", Accepted: true,
	}
	if found {
		result.RunID = record.ID
		runtimeErr := &contract.RuntimeError{
			Code: contract.ErrorCancelled, Phase: contract.PhaseRun,
			Message: "native_tui Session was closed",
		}
		execution := runtime.NewSettledNativeTUIExecution(
			record, window.TmuxID, window.ExitCode, window.Signal,
			runtime.NativeTUIOutcomeCancelled, "session_closed",
			runtimeErr, service.now(),
		)
		if _, settleErr := service.lifecycles.Settle(
			context.WithoutCancel(ctx), execution,
		); settleErr != nil {
			return CloseResult{}, settleErr
		}
	}
	accepted, err := service.stopBoundWindow(
		ctx, sessionID, window.TmuxID,
	)
	if err != nil {
		return CloseResult{}, err
	}
	result.Accepted = accepted
	return result, nil
}

func (service *Service) findWindow(
	ctx context.Context,
	sessionID string,
) (tmux.Window, error) {
	windows, err := service.tmux.List(ctx)
	if err != nil {
		return tmux.Window{}, err
	}
	var found *tmux.Window
	for index := range windows {
		binding := windows[index].Binding
		if binding == nil || binding.Kind != "session" || binding.ID != sessionID {
			continue
		}
		if found != nil {
			return tmux.Window{}, fmt.Errorf(
				"Session %s has multiple native TUI bindings", sessionID,
			)
		}
		current := windows[index]
		found = &current
	}
	if found == nil {
		return tmux.Window{}, &contract.RuntimeError{
			Code: contract.ErrorNotFound, Phase: contract.PhaseRequest,
			Message: fmt.Sprintf(
				"Session %s has no native TUI binding", sessionID,
			),
		}
	}
	return *found, nil
}

func (service *Service) Supervise(
	ctx context.Context,
	manifestPath string,
	digest string,
) error {
	manifest, paths, err := readSupervisorManifest(manifestPath, digest)
	if err != nil {
		return err
	}
	if paths.Home != service.paths.Home {
		return fmt.Errorf("native_tui supervisor Runtime home changed")
	}
	command := exec.CommandContext(
		ctx, manifest.Target.Path, manifest.Target.Argv[1:]...,
	)
	command.Args = append([]string(nil), manifest.Target.Argv...)
	command.Env = targetEnvironment(
		manifest.Target.Environment, os.Environ(),
	)
	command.Dir = manifest.Target.CWD
	command.Stdin = os.Stdin
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	signals := make(chan os.Signal, 8)
	signal.Notify(signals, os.Interrupt, syscall.SIGHUP, syscall.SIGTERM)
	defer signal.Stop(signals)
	go func() {
		for range signals {
			// Provider and supervisor share the pane foreground process group,
			// so the Provider receives terminal signals directly. Consuming the
			// supervisor copy keeps it alive long enough to publish lifecycle facts.
		}
	}()
	if err := command.Start(); err != nil {
		return service.settleSupervisorLaunchFailure(manifest, err)
	}
	if err := tmux.AcknowledgeTargetReady(); err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return service.settleSupervisorLaunchFailure(manifest, err)
	}
	window, err := service.waitForWindow(ctx, manifest.SessionID)
	if err != nil {
		_ = command.Process.Kill()
		_ = command.Wait()
		return service.settleSupervisorLaunchFailure(manifest, err)
	}
	waitErr := command.Wait()
	exitCode, exitSignal := processOutcome(command.ProcessState)
	record, getErr := service.lifecycles.Get(
		context.WithoutCancel(ctx), manifest.RunID,
	)
	if getErr != nil {
		return getErr
	}
	if !record.State.Terminal() {
		outcome := runtime.NativeTUIOutcomeCompleted
		var runtimeErr *contract.RuntimeError
		if waitErr != nil || exitCode == nil || *exitCode != 0 || exitSignal != "" {
			outcome = runtime.NativeTUIOutcomeFailed
			message := "native_tui provider exited unsuccessfully"
			if waitErr != nil {
				message = waitErr.Error()
			}
			runtimeErr = &contract.RuntimeError{
				Code:  contract.ErrorProviderUnavailable,
				Phase: contract.PhaseRun, Message: message,
			}
		}
		execution := runtime.NewSettledNativeTUIExecution(
			record, window.TmuxID, exitCode, exitSignal, outcome,
			"process_exited", runtimeErr, service.now(),
		)
		if _, settleErr := service.lifecycles.Settle(
			context.WithoutCancel(ctx), execution,
		); settleErr != nil {
			// Keep remain-on-exit evidence when durable settlement failed.
			return settleErr
		}
	}
	_, stopErr := service.stopBoundWindow(
		context.Background(), manifest.SessionID, window.TmuxID,
	)
	return stopErr
}

func (service *Service) stopBoundWindow(
	ctx context.Context,
	sessionID string,
	tmuxID string,
) (bool, error) {
	accepted, err := service.tmux.Stop(ctx, tmuxID)
	if err == nil {
		return accepted.Accepted, nil
	}
	_, findErr := service.findWindow(context.WithoutCancel(ctx), sessionID)
	var runtimeErr *contract.RuntimeError
	if errors.As(findErr, &runtimeErr) &&
		runtimeErr.Code == contract.ErrorNotFound {
		// Provider exit and an explicit close can race after terminal Run
		// publication. An already-absent exact binding satisfies both owners.
		return true, nil
	}
	return false, err
}

func (service *Service) settleSupervisorLaunchFailure(
	manifest supervisorManifest,
	cause error,
) error {
	record, err := service.lifecycles.Get(
		context.Background(), manifest.RunID,
	)
	if err != nil {
		return errors.Join(cause, err)
	}
	runtimeErr := &contract.RuntimeError{
		Code:  contract.ErrorProviderUnavailable,
		Phase: contract.PhaseTransport, Message: cause.Error(),
	}
	execution := runtime.NewSettledNativeTUIExecution(
		record, "", nil, "", runtime.NativeTUIOutcomeFailed,
		"launch_failed", runtimeErr, service.now(),
	)
	_, settleErr := service.lifecycles.Settle(
		context.Background(), execution,
	)
	return errors.Join(cause, settleErr)
}

func (service *Service) waitForWindow(
	ctx context.Context,
	sessionID string,
) (tmux.Window, error) {
	deadline := time.Now().Add(15 * time.Second)
	for {
		window, err := service.findWindow(ctx, sessionID)
		if err == nil {
			return window, nil
		}
		if time.Now().After(deadline) {
			return tmux.Window{}, err
		}
		timer := time.NewTimer(20 * time.Millisecond)
		select {
		case <-ctx.Done():
			timer.Stop()
			return tmux.Window{}, ctx.Err()
		case <-timer.C:
		}
	}
}

func targetEnvironment(configured, current []string) []string {
	reserved := map[string]string{}
	for _, value := range current {
		name, currentValue, ok := strings.Cut(value, "=")
		if ok && (name == "TERM" || name == "TMUX" || name == "TMUX_PANE") {
			reserved[name] = currentValue
		}
	}
	result := make([]string, 0, len(configured)+3)
	for _, value := range configured {
		name, _, ok := strings.Cut(value, "=")
		if !ok || name == "TERM" || name == "TMUX" || name == "TMUX_PANE" {
			continue
		}
		result = append(result, value)
	}
	for _, name := range []string{"TERM", "TMUX", "TMUX_PANE"} {
		if value, exists := reserved[name]; exists {
			result = append(result, name+"="+value)
		}
	}
	return result
}

func supervisorEnvironment(home string, target []string) []string {
	allowed := map[string]struct{}{
		"HOME": {}, "LANG": {}, "LC_ALL": {}, "LC_CTYPE": {},
		"LOGNAME": {}, "PATH": {}, "SHELL": {}, "TMPDIR": {},
		"TMUX_TMPDIR": {}, "USER": {},
	}
	result := []string{layout.HomeEnv + "=" + home}
	seen := map[string]struct{}{layout.HomeEnv: {}}
	for _, value := range target {
		name, _, ok := strings.Cut(value, "=")
		if !ok {
			continue
		}
		if _, exists := allowed[name]; !exists {
			continue
		}
		if _, exists := seen[name]; exists {
			continue
		}
		seen[name] = struct{}{}
		result = append(result, value)
	}
	return result
}

func processOutcome(state *os.ProcessState) (*int, string) {
	if state == nil {
		return nil, ""
	}
	code := state.ExitCode()
	var signalName string
	if status, ok := state.Sys().(syscall.WaitStatus); ok && status.Signaled() {
		signalName = status.Signal().String()
	}
	return &code, signalName
}

func (service *Service) Close() error {
	return service.lifecycles.Close()
}
