package tmux

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"time"

	"github.com/yy003x/runtime/pkg/contract"
)

func (service *Service) startWindow(
	ctx context.Context,
	invocation Invocation,
) (StartResult, error) {
	tmuxID, err := newUUIDv7(service.now(), service.random)
	if err != nil {
		return StartResult{}, err
	}
	createdAt, _ := parseUUIDv7(tmuxID)
	nonce, err := randomHex(service.random, 16)
	if err != nil {
		return StartResult{}, err
	}
	paths := service.launchPaths(nonce)
	helperExecutable, helperExecutableID, err := executableIdentity(
		service.helperCommand[0],
	)
	if err != nil {
		return StartResult{}, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseTransport,
			"identify Tmux helper executable: %v", err,
		)
	}
	executable, executableID, err := executableIdentity(invocation.Path)
	if err != nil {
		return StartResult{}, runtimeError(
			contract.ErrorInvalidRequest, contract.PhaseProfile,
			"identify command executable: %v", err,
		)
	}
	manifest := launchManifest{
		SchemaVersion: WindowSchemaVersion, OwnerUID: service.uid,
		Home: service.home, Nonce: nonce, Path: executable,
		Argv:        append([]string(nil), invocation.Argv...),
		Environment: append([]string(nil), invocation.Environment...),
		CWD:         invocation.CWD, ExecutableIdentity: executableID,
		ReadyPath: paths.ready, GoPath: paths.goFile, StatusPath: paths.status,
		GateTimeoutMS: service.gateTimeout.Milliseconds(),
	}
	if invocation.CooperativeReady {
		manifest.TargetReadyPath = paths.targetReady
	}
	manifestDigest, _, err := marshalManifest(manifest)
	if err != nil {
		return StartResult{}, err
	}
	manifest.ManifestDigest = manifestDigest
	manifestBytes, err := json.Marshal(manifest)
	if err != nil {
		return StartResult{}, err
	}
	manifestBytes = append(manifestBytes, '\n')
	if len(manifestBytes) > maxManifestBytes {
		return StartResult{}, runtimeError(
			contract.ErrorContextOverflow, contract.PhaseProfile,
			"Tmux launch manifest exceeds %d bytes", maxManifestBytes,
		)
	}
	if err := atomicWritePrivate(paths.manifest, manifestBytes, service.uid); err != nil {
		return StartResult{}, tmuxTransportError(
			contract.ErrorConflict, "write Tmux launch manifest: %v", err,
		)
	}
	cleanupFiles := true
	defer func() {
		if cleanupFiles {
			removeExact(
				paths.manifest, paths.ready, paths.goFile, paths.status,
				paths.targetReady,
			)
		}
	}()

	helperArgs := append([]string(nil), service.helperCommand[1:]...)
	helperArgs = append(helperArgs, "--manifest", paths.manifest)
	marker, windowID, paneID, err := service.createManagedTarget(
		ctx, windowName(tmuxID), invocation.CWD,
		helperExecutable, helperArgs,
	)
	if err != nil {
		return StartResult{}, err
	}
	committed := false
	defer func() {
		if !committed {
			service.cleanupProvisionalWindow(
				context.Background(), marker, windowID, paneID,
				windowName(tmuxID),
			)
		}
	}()

	ready, err := service.waitReady(
		ctx, paths.ready, nonce, manifestDigest,
	)
	if err != nil {
		return StartResult{}, err
	}
	if ready.PID <= 0 || ready.PGID != ready.PID {
		return StartResult{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux helper does not own an independent foreground process group",
		)
	}
	live, err := service.inspectWindow(ctx, windowID)
	if err != nil {
		return StartResult{}, err
	}
	if live.PaneID != paneID || live.PanePID != ready.PID ||
		live.PaneDead || live.Registered {
		return StartResult{}, tmuxTransportError(
			contract.ErrorConflict, "Tmux helper pane identity is invalid",
		)
	}
	identity, err := service.lookupProcess(ready.PID)
	if err != nil {
		return StartResult{}, tmuxTransportError(
			contract.ErrorConflict, "identify Tmux helper: %v", err,
		)
	}
	if identity.StartToken != ready.ProcessStart ||
		!processExecutableMatches(identity.Executable, helperExecutable) ||
		identity.ExecutableIdentity != helperExecutableID ||
		ready.ExecutableIdentity != helperExecutableID {
		return StartResult{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux helper process identity changed (current=%q expected=%q)",
			identity.Executable, helperExecutable,
		)
	}
	pgid, err := processGroup(ready.PID)
	if err != nil || pgid != ready.PGID {
		return StartResult{}, tmuxTransportError(
			contract.ErrorConflict, "Tmux helper process group changed",
		)
	}
	record := windowRecord{
		SchemaVersion: WindowSchemaVersion, RegistryVersion: registryVersion,
		TmuxID: tmuxID, ProfileID: invocation.ProfileID, CreatedAt: createdAt,
		CWD: invocation.CWD, ConfigDigest: invocation.ConfigDigest,
		Binding:          cloneBinding(invocation.Binding),
		CooperativeReady: invocation.CooperativeReady,
		WindowID:         windowID, PaneID: paneID, HelperPID: ready.PID,
		HelperPGID: ready.PGID, ProcessStart: ready.ProcessStart,
		HelperExecutable:         helperExecutable,
		HelperExecutableIdentity: helperExecutableID,
		ResolvedExecutable:       executable,
		ExecutableIdentity:       executableID,
		ServerIncarnation:        marker.ServerIncarnation,
	}
	recordEncoded, err := encodeWindowRecord(record)
	if err != nil {
		return StartResult{}, err
	}
	result, err := service.runTmux(
		ctx, nil,
		"set-option", "-wq", "-t", windowID,
		windowRecordOption, recordEncoded,
		";", "set-option", "-wq", "-t", windowID,
		windowCommitOption, "1",
	)
	var commitErr error
	if err != nil {
		commitErr = err
	} else if result.ExitCode != 0 {
		commitErr = tmuxTransportError(
			contract.ErrorProtocol, "commit Tmux registry record: %v",
			commandFailure("tmux set-option", result),
		)
	}
	if !service.confirmRegistryCommit(
		marker, windowID, paneID, recordEncoded,
	) {
		if commitErr != nil {
			return StartResult{}, commitErr
		}
		return StartResult{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux registry commit could not be verified",
		)
	}
	committed = true
	postCommitCtx, cancelPostCommit := context.WithTimeout(
		context.Background(),
		service.gateTimeout+3*service.commandTimeout+2*time.Second,
	)
	defer cancelPostCommit()

	goValue := goFact{
		SchemaVersion: WindowSchemaVersion, Nonce: nonce,
		ManifestDigest: manifestDigest,
	}
	if err := writeJSONPrivate(paths.goFile, goValue, service.uid); err != nil {
		safeErr := safeRuntimeError(tmuxTransportError(
			contract.ErrorConflict, "publish Tmux launch gate: %v", err,
		))
		record.LaunchError = safeErr
		currentEncoded := recordEncoded
		if updated, updateErr := service.updateRecord(
			postCommitCtx, marker, recordEncoded, record,
		); updateErr == nil {
			currentEncoded = updated
		}
		service.terminateBlockedHelper(
			postCommitCtx, marker, currentEncoded, paneID,
		)
		cleanupFiles = false
		removeExact(
			paths.manifest, paths.ready, paths.goFile, paths.status,
			paths.targetReady,
		)
		window, loadErr := service.loadCommittedWindow(
			postCommitCtx, marker, tmuxID,
		)
		if loadErr != nil {
			window = committedWindowFallback(record, false)
		}
		return StartResult{Window: window, LaunchAccepted: false}, nil
	}
	accepted, launchError := service.waitLaunchAck(
		postCommitCtx, paths, paneID, nonce, manifestDigest,
		ready.ProcessStart, helperExecutable,
		executable, executableID, invocation.CooperativeReady,
	)
	currentEncoded := recordEncoded
	if launchError == nil && accepted && invocation.CooperativeReady {
		record.TargetReady = true
		updated, updateErr := service.updateRecord(
			postCommitCtx, marker, currentEncoded, record,
		)
		if updateErr != nil {
			launchError = safeRuntimeError(tmuxTransportError(
				contract.ErrorConflict,
				"publish Tmux cooperative target ready state: %v", updateErr,
			))
			accepted = false
		} else {
			currentEncoded = updated
		}
	}
	if launchError != nil {
		record.LaunchError = launchError
		if updated, updateErr := service.updateRecord(
			postCommitCtx, marker, currentEncoded, record,
		); updateErr == nil {
			currentEncoded = updated
		}
		service.terminateBlockedHelper(
			postCommitCtx, marker, currentEncoded, paneID,
		)
	}
	cleanupFiles = false
	removeExact(
		paths.manifest, paths.ready, paths.goFile, paths.status,
		paths.targetReady,
	)
	window, err := service.loadCommittedWindow(postCommitCtx, marker, tmuxID)
	if err != nil {
		window = committedWindowFallback(record, accepted)
	}
	if !accepted {
		window.State = StateExited
		if window.LaunchError == nil {
			window.LaunchError = record.LaunchError
		}
	}
	return StartResult{Window: window, LaunchAccepted: accepted}, nil
}

// createManagedTarget makes the first managed window part of the same tmux
// client command queue that creates its owner scope and commits the marker.
func (service *Service) createManagedTarget(
	ctx context.Context,
	name string,
	cwd string,
	helperExecutable string,
	helperArgs []string,
) (serverMarker, string, string, error) {
	exists, err := service.managedSessionExists(ctx)
	if err != nil {
		return serverMarker{}, "", "", err
	}
	if exists {
		marker, loadErr := service.loadAndValidateServer(ctx)
		if loadErr == nil {
			windowID, paneID, createErr := service.createTargetOnServer(
				ctx, marker, name, cwd, helperExecutable, helperArgs,
			)
			return marker, windowID, paneID, createErr
		}
		if !service.dedicated() {
			return serverMarker{}, "", "", loadErr
		}
		missing, inspectErr := service.serverMarkerMissing(ctx)
		if inspectErr != nil || !missing {
			return serverMarker{}, "", "", loadErr
		}
		if recoverErr := service.recoverMarkerlessBootstrap(ctx); recoverErr != nil {
			return serverMarker{}, "", "", recoverErr
		}
	}
	if !service.dedicated() {
		return service.createDefaultSessionWithTarget(
			ctx, name, cwd, helperExecutable, helperArgs,
		)
	}
	return service.createServerWithTarget(
		ctx, name, cwd, helperExecutable, helperArgs,
	)
}

func (service *Service) managedTargetCommand(
	name string,
	cwd string,
	helperExecutable string,
	helperArgs []string,
) []string {
	args := []string{
		"new-window", "-d", "-P", "-F", "#{window_id}|#{pane_id}",
		"-t", SessionName, "-n", name, "-c", cwd,
		"--", helperExecutable,
	}
	args = append(args, helperArgs...)
	return append(args, service.managedWindowOptionCommands(name)...)
}

func (service *Service) managedWindowOptionCommands(name string) []string {
	target := SessionName + ":" + name
	return []string{
		";", "set-window-option", "-q", "-t", target, "remain-on-exit", "on",
		";", "set-window-option", "-q", "-t", target, "automatic-rename", "off",
		";", "set-window-option", "-q", "-t", target, "allow-rename", "off",
	}
}

func (service *Service) createTargetOnServer(
	ctx context.Context,
	marker serverMarker,
	name string,
	cwd string,
	helperExecutable string,
	helperArgs []string,
) (string, string, error) {
	result, runErr := service.runTmux(
		ctx, nil,
		service.managedTargetCommand(
			name, cwd, helperExecutable, helperArgs,
		)...,
	)
	var createErr error
	if runErr != nil {
		createErr = runErr
	} else if result.ExitCode != 0 {
		createErr = tmuxTransportError(
			contract.ErrorProtocol, "create Tmux target window: %v",
			commandFailure("tmux new-window", result),
		)
	}
	windowID, paneID, parseErr := parseCreatedTarget(result.Stdout)
	if createErr == nil && parseErr != nil {
		createErr = tmuxTransportError(
			contract.ErrorProtocol, "create Tmux target window: %v", parseErr,
		)
	}
	if createErr == nil {
		return windowID, paneID, nil
	}
	windowID, paneID, recoverErr := service.recoverCreatedTarget(marker, name)
	if recoverErr == nil {
		return windowID, paneID, nil
	}
	return "", "", createErr
}

func (service *Service) createServerWithTarget(
	ctx context.Context,
	name string,
	cwd string,
	helperExecutable string,
	helperArgs []string,
) (serverMarker, string, string, error) {
	configDigest, err := service.ownerConfigDigest()
	if err != nil {
		return serverMarker{}, "", "", err
	}
	incarnation, err := randomHex(service.random, 16)
	if err != nil {
		return serverMarker{}, "", "", fmt.Errorf(
			"generate Tmux server incarnation: %w", err,
		)
	}
	marker := serverMarker{
		FullHomeDigest: service.homeDigest, SchemaVersion: serverSchemaVersion,
		OwnerUID: service.uid, SentinelID: "@0",
		ServerIncarnation: incarnation, TmuxConfigDigest: configDigest,
	}
	args := []string{
		"-f", service.config.TmuxConfigFile,
		"new-session", "-d", "-s", SessionName,
		"-n", service.sentinelName(incarnation),
		"--", "/usr/bin/true",
		";", "set-option", "-gq",
		serverOptionName, encodeServerMarker(marker),
		";",
	}
	args = append(args, service.managedTargetCommand(
		name, cwd, helperExecutable, helperArgs,
	)...)
	result, runErr := service.runTmux(ctx, nil, args...)
	var createErr error
	if runErr != nil {
		createErr = runErr
	} else if result.ExitCode != 0 {
		createErr = tmuxTransportError(
			contract.ErrorProtocol,
			"create dedicated Tmux server and target window: %v",
			commandFailure("tmux new-session/new-window", result),
		)
	}
	windowID, paneID, parseErr := parseCreatedTarget(result.Stdout)
	if createErr == nil && parseErr != nil {
		createErr = tmuxTransportError(
			contract.ErrorProtocol,
			"create dedicated Tmux server and target window: %v", parseErr,
		)
	}

	exists, socketErr := service.socketExists()
	if socketErr != nil {
		if createErr != nil {
			return serverMarker{}, "", "", createErr
		}
		return serverMarker{}, "", "", socketErr
	}
	if !exists {
		if createErr != nil {
			return serverMarker{}, "", "", createErr
		}
		return serverMarker{}, "", "", tmuxTransportError(
			contract.ErrorProtocol,
			"dedicated Tmux server disappeared during bootstrap",
		)
	}
	loaded, loadErr := service.loadAndValidateServer(ctx)
	if loadErr != nil {
		missing, inspectErr := service.serverMarkerMissing(ctx)
		if inspectErr == nil && missing {
			_ = service.recoverMarkerlessBootstrap(context.Background())
		}
		if createErr != nil {
			return serverMarker{}, "", "", createErr
		}
		return serverMarker{}, "", "", loadErr
	}
	if loaded.ServerIncarnation != marker.ServerIncarnation {
		return serverMarker{}, "", "", tmuxTransportError(
			contract.ErrorConflict,
			"Tmux server incarnation changed during bootstrap",
		)
	}
	if createErr == nil {
		return loaded, windowID, paneID, nil
	}
	windowID, paneID, recoverErr := service.recoverCreatedTarget(loaded, name)
	if recoverErr == nil {
		return loaded, windowID, paneID, nil
	}
	if cleanupErr := service.cleanupEmptyServer(
		context.Background(), loaded,
	); cleanupErr != nil {
		return serverMarker{}, "", "", tmuxTransportError(
			contract.ErrorConflict,
			"bootstrap target failed and empty Tmux server cleanup was not provable: %v",
			cleanupErr,
		)
	}
	return serverMarker{}, "", "", createErr
}

func (service *Service) createDefaultSessionWithTarget(
	ctx context.Context,
	name string,
	cwd string,
	helperExecutable string,
	helperArgs []string,
) (serverMarker, string, string, error) {
	configDigest, err := service.ownerConfigDigest()
	if err != nil {
		return serverMarker{}, "", "", err
	}
	incarnation, err := randomHex(service.random, 16)
	if err != nil {
		return serverMarker{}, "", "", fmt.Errorf(
			"generate Tmux session incarnation: %w", err,
		)
	}
	marker := serverMarker{
		FullHomeDigest: service.homeDigest, SchemaVersion: serverSchemaVersion,
		OwnerUID: service.uid, ServerIncarnation: incarnation,
		TmuxConfigDigest: configDigest,
	}
	args := []string{
		"new-session", "-d", "-P", "-F", "#{window_id}|#{pane_id}",
		"-s", SessionName, "-n", name, "-c", cwd,
		"--", helperExecutable,
	}
	args = append(args, helperArgs...)
	args = append(args,
		";", "set-option", "-q", "-t", SessionName,
		sessionOptionName, encodeServerMarker(marker),
		";", "set-option", "-q", "-t", SessionName,
		"update-environment", "",
	)
	args = append(args, service.managedWindowOptionCommands(name)...)
	result, runErr := service.runTmux(ctx, nil, args...)
	var createErr error
	if runErr != nil {
		createErr = runErr
	} else if result.ExitCode != 0 {
		createErr = tmuxTransportError(
			contract.ErrorProtocol,
			"create default Tmux session and target window: %v",
			commandFailure("tmux new-session", result),
		)
	}
	windowID, paneID, parseErr := parseCreatedTarget(result.Stdout)
	if createErr == nil && parseErr != nil {
		createErr = tmuxTransportError(
			contract.ErrorProtocol,
			"create default Tmux session and target window: %v", parseErr,
		)
	}
	loaded, loadErr := service.loadAndValidateServer(ctx)
	if loadErr != nil {
		if createErr != nil {
			return serverMarker{}, "", "", createErr
		}
		return serverMarker{}, "", "", loadErr
	}
	if loaded.ServerIncarnation != marker.ServerIncarnation {
		return serverMarker{}, "", "", tmuxTransportError(
			contract.ErrorConflict,
			"Tmux session changed during bootstrap",
		)
	}
	if createErr == nil {
		return loaded, windowID, paneID, nil
	}
	windowID, paneID, recoverErr := service.recoverCreatedTarget(loaded, name)
	if recoverErr == nil {
		return loaded, windowID, paneID, nil
	}
	return serverMarker{}, "", "", createErr
}

func (service *Service) recoverCreatedTarget(
	marker serverMarker,
	name string,
) (string, string, error) {
	ctx, cancel := context.WithTimeout(
		context.Background(), service.commandTimeout,
	)
	defer cancel()
	encodedMarker, err := service.showOwnerOption(ctx, true)
	if err != nil || encodedMarker != encodeServerMarker(marker) {
		return "", "", fmt.Errorf("Tmux server identity changed")
	}
	ids, err := service.tmuxLines(
		ctx, "list-windows", "-t", SessionName, "-F", "#{window_id}",
	)
	if err != nil {
		return "", "", err
	}
	var match *liveWindow
	for _, id := range ids {
		if id == marker.SentinelID {
			continue
		}
		value, inspectErr := service.inspectWindow(ctx, id)
		if inspectErr != nil {
			return "", "", inspectErr
		}
		if value.WindowName != name {
			continue
		}
		if match != nil || value.Registered {
			return "", "", fmt.Errorf("Tmux provisional target is ambiguous")
		}
		current := value
		match = &current
	}
	if match == nil {
		return "", "", fmt.Errorf("Tmux provisional target was not created")
	}
	return match.WindowID, match.PaneID, nil
}

func (service *Service) confirmRegistryCommit(
	marker serverMarker,
	windowID string,
	paneID string,
	recordEncoded string,
) bool {
	ctx, cancel := context.WithTimeout(
		context.Background(), service.commandTimeout,
	)
	defer cancel()
	exists, err := service.managedSessionExists(ctx)
	if err != nil || !exists {
		return false
	}
	encodedMarker, err := service.showOwnerOption(ctx, true)
	if err != nil ||
		encodedMarker != encodeServerMarker(marker) {
		return false
	}
	value, err := service.inspectWindow(ctx, windowID)
	if err != nil {
		return false
	}
	return value.PaneID == paneID &&
		value.RecordEncoded == recordEncoded &&
		value.Registered
}

func committedWindowFallback(record windowRecord, accepted bool) Window {
	state := StateStarting
	if !accepted {
		state = StateExited
	}
	return Window{
		SchemaVersion: WindowSchemaVersion, TmuxID: record.TmuxID,
		State: state, CreatedAt: record.CreatedAt.UTC(),
		WindowID: record.WindowID, PaneID: record.PaneID,
		ProfileID: record.ProfileID, CWD: record.CWD,
		ConfigDigest: record.ConfigDigest, Binding: cloneBinding(record.Binding),
		LaunchError: record.LaunchError,
	}
}

type launchFilePaths struct {
	manifest    string
	ready       string
	goFile      string
	status      string
	targetReady string
}

func (service *Service) launchPaths(nonce string) launchFilePaths {
	base := filepath.Join(service.config.ManifestDir, "launch-"+nonce)
	return launchFilePaths{
		manifest:    base + ".json",
		ready:       base + ".ready",
		goFile:      base + ".go",
		status:      base + ".status",
		targetReady: base + ".target-ready",
	}
}

func marshalManifest(value launchManifest) (string, []byte, error) {
	value.ManifestDigest = ""
	data, err := json.Marshal(value)
	if err != nil {
		return "", nil, err
	}
	return digestBytes(data), data, nil
}

func parseCreatedTarget(value []byte) (string, string, error) {
	line, err := trimOneLine(value)
	if err != nil {
		return "", "", err
	}
	parts := strings.Split(line, "|")
	if len(parts) != 2 ||
		!windowIDPattern.MatchString(parts[0]) ||
		!paneIDPattern.MatchString(parts[1]) {
		return "", "", fmt.Errorf("tmux returned invalid window identity %q", line)
	}
	return parts[0], parts[1], nil
}

func (service *Service) waitReady(
	ctx context.Context,
	path string,
	nonce string,
	manifestDigest string,
) (readyFact, error) {
	deadline := time.Now().Add(service.readyTimeout)
	var lastErr error
	for time.Now().Before(deadline) {
		var value readyFact
		err := decodePrivateJSON(path, 64<<10, service.uid, &value)
		if err == nil {
			if value.SchemaVersion != WindowSchemaVersion ||
				value.ManifestDigest != manifestDigest ||
				value.Nonce != nonce || value.PID <= 0 || value.PGID <= 0 ||
				value.ProcessStart == "" || value.Executable == "" {
				return readyFact{}, tmuxTransportError(
					contract.ErrorProtocol, "Tmux helper ready fact is invalid",
				)
			}
			return value, nil
		}
		lastErr = err
		select {
		case <-ctx.Done():
			return readyFact{}, ctx.Err()
		case <-time.After(10 * time.Millisecond):
		}
	}
	return readyFact{}, tmuxTransportError(
		contract.ErrorTimeout, "wait for Tmux helper readiness: %v", lastErr,
	)
}

func (service *Service) waitLaunchAck(
	ctx context.Context,
	paths launchFilePaths,
	paneID string,
	nonce string,
	manifestDigest string,
	processStart string,
	helperExecutable string,
	targetExecutable string,
	targetExecutableIdentity string,
	cooperativeReady bool,
) (bool, *SafeError) {
	deadline := time.Now().Add(service.gateTimeout)
	for time.Now().Before(deadline) {
		if cooperativeReady {
			var ready targetReadyFact
			readyErr := decodePrivateJSON(
				paths.targetReady, 64<<10, service.uid, &ready,
			)
			if readyErr == nil {
				panePID, identity, identityErr := service.lookupPaneProcess(ctx, paneID)
				if ready.SchemaVersion != WindowSchemaVersion ||
					ready.Nonce != nonce ||
					ready.ManifestDigest != manifestDigest ||
					ready.PID <= 0 || ready.PID != panePID ||
					ready.ProcessStart != processStart ||
					identityErr != nil || identity.StartToken != processStart ||
					ready.ExecutableIdentity != targetExecutableIdentity ||
					identity.ExecutableIdentity != targetExecutableIdentity ||
					!processExecutableMatches(ready.Executable, targetExecutable) ||
					!processExecutableMatches(identity.Executable, targetExecutable) {
					return false, safeRuntimeError(tmuxTransportError(
						contract.ErrorConflict,
						"Tmux cooperative target ready identity is invalid",
					))
				}
				return true, nil
			}
			if !isNotExist(readyErr) {
				return false, safeRuntimeError(tmuxTransportError(
					contract.ErrorProtocol,
					"read Tmux cooperative target ready fact: %v", readyErr,
				))
			}
		}
		var status helperStatus
		statusErr := decodePrivateJSON(
			paths.status, 64<<10, service.uid, &status,
		)
		if statusErr == nil {
			if status.SchemaVersion != WindowSchemaVersion ||
				status.Nonce != nonce ||
				status.ManifestDigest != manifestDigest {
				return false, safeRuntimeError(tmuxTransportError(
					contract.ErrorProtocol,
					"Tmux helper status identity is invalid",
				))
			}
			if status.Error == nil {
				status.Error = &SafeError{
					Code: contract.ErrorInternal, Phase: contract.PhaseTransport,
					Message: "Tmux helper failed before target execution",
				}
			}
			return false, status.Error
		}
		if !isNotExist(statusErr) {
			return false, safeRuntimeError(tmuxTransportError(
				contract.ErrorProtocol,
				"read Tmux helper status: %v", statusErr,
			))
		}
		dead, err := service.paneFormat(ctx, paneID, "#{pane_dead}")
		if err == nil && dead == "1" {
			if cooperativeReady {
				return false, safeRuntimeError(tmuxTransportError(
					contract.ErrorConflict,
					"Tmux cooperative target exited before readiness acknowledgement",
				))
			}
			return true, nil
		}
		identity, identityErr := service.lookupProcessFromPane(
			ctx, paneID,
		)
		if identityErr == nil && identity.StartToken == processStart {
			switch {
			case processExecutableMatches(
				identity.Executable, helperExecutable,
			):
				// The helper has not completed exec yet.
			case processExecutableMatches(
				identity.Executable, targetExecutable,
			) && identity.ExecutableIdentity == targetExecutableIdentity:
				if !cooperativeReady {
					return true, nil
				}
				// The cooperative target has exec'd but has not yet published
				// its one-shot readiness fact.
			default:
				return false, safeRuntimeError(tmuxTransportError(
					contract.ErrorConflict,
					"Tmux target executable identity changed before launch acknowledgement",
				))
			}
		}
		select {
		case <-ctx.Done():
			if cooperativeReady {
				return false, safeRuntimeError(tmuxTransportError(
					contract.ErrorTimeout,
					"wait for Tmux cooperative target readiness: %v", ctx.Err(),
				))
			}
			return true, nil
		case <-time.After(10 * time.Millisecond):
		}
	}
	if cooperativeReady {
		return false, safeRuntimeError(tmuxTransportError(
			contract.ErrorTimeout,
			"wait for Tmux cooperative target readiness timed out",
		))
	}
	return true, nil
}

func (service *Service) lookupPaneProcess(
	ctx context.Context,
	paneID string,
) (int, ProcessIdentity, error) {
	pidValue, err := service.paneFormat(ctx, paneID, "#{pane_pid}")
	if err != nil {
		return 0, ProcessIdentity{}, err
	}
	pid, err := strconv.Atoi(pidValue)
	if err != nil || pid <= 0 {
		return 0, ProcessIdentity{}, fmt.Errorf("Tmux pane PID is invalid")
	}
	identity, err := service.lookupProcess(pid)
	return pid, identity, err
}

func (service *Service) lookupProcessFromPane(
	ctx context.Context,
	paneID string,
) (ProcessIdentity, error) {
	_, identity, err := service.lookupPaneProcess(ctx, paneID)
	return identity, err
}

func (service *Service) waitPaneDead(
	ctx context.Context,
	paneID string,
	timeout time.Duration,
) bool {
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		dead, err := service.paneFormat(ctx, paneID, "#{pane_dead}")
		if err == nil && dead == "1" {
			return true
		}
		time.Sleep(10 * time.Millisecond)
	}
	return false
}

func (service *Service) updateRecord(
	ctx context.Context,
	marker serverMarker,
	previousEncoded string,
	record windowRecord,
) (string, error) {
	encoded, err := encodeWindowRecord(record)
	if err != nil {
		return "", err
	}
	condition := strictWindowIdentityCondition(
		service.identityCondition(
			encodeServerMarker(marker), previousEncoded,
		),
		record.WindowID, record.PaneID, windowName(record.TmuxID),
	)
	trueCommand := fmt.Sprintf(
		"set-option -wq -t %s %s %s",
		record.WindowID, windowRecordOption, encoded,
	)
	result, err := service.runTmux(
		ctx, nil,
		"if-shell", "-F", "-t", record.PaneID,
		condition, trueCommand, `run-shell "exit 73"`,
	)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", tmuxTransportError(
			contract.ErrorConflict, "update Tmux registry record: %v",
			commandFailure("tmux set-option", result),
		)
	}
	return encoded, nil
}

func (service *Service) terminateBlockedHelper(
	ctx context.Context,
	marker serverMarker,
	recordEncoded string,
	paneID string,
) {
	var record windowRecord
	if err := decodeOption(recordEncoded, &record); err != nil {
		return
	}
	condition := strictWindowIdentityCondition(
		service.identityCondition(
			encodeServerMarker(marker), recordEncoded,
		),
		record.WindowID, record.PaneID, windowName(record.TmuxID),
	)
	_, _ = service.runTmux(
		ctx, nil,
		"if-shell", "-F", "-t", paneID,
		condition, fmt.Sprintf("send-keys -t %s C-c", paneID),
		`run-shell "exit 73"`,
	)
	if service.waitPaneDead(ctx, paneID, 500*time.Millisecond) {
		return
	}
	_, _ = service.runTmux(
		ctx, nil,
		"if-shell", "-F", "-t", paneID,
		condition,
		fmt.Sprintf(
			"respawn-pane -k -t %s -- /usr/bin/false", paneID,
		),
		`run-shell "exit 73"`,
	)
	service.waitPaneDead(ctx, paneID, 500*time.Millisecond)
}

func (service *Service) cleanupProvisionalWindow(
	ctx context.Context,
	marker serverMarker,
	windowID string,
	paneID string,
	name string,
) {
	condition := strictWindowIdentityCondition(
		tmuxAnd(
			tmuxEquals(
				"#{"+service.ownerOptionName()+"}", encodeServerMarker(marker),
			),
			tmuxEquals("#{"+windowRecordOption+"}", ""),
			tmuxEquals("#{"+windowCommitOption+"}", ""),
		),
		windowID, paneID, name,
	)
	_, _ = service.runTmux(
		ctx, nil,
		"if-shell", "-F", "-t", paneID,
		condition, fmt.Sprintf("kill-window -t %s", windowID),
		`run-shell "exit 73"`,
	)
}

func (service *Service) loadCommittedWindow(
	ctx context.Context,
	marker serverMarker,
	tmuxID string,
) (Window, error) {
	values, err := service.loadWindows(ctx, marker)
	if err != nil {
		return Window{}, err
	}
	for _, value := range values {
		if value.TmuxID == tmuxID {
			return cloneWindow(value.Window), nil
		}
	}
	return Window{}, tmuxTransportError(
		contract.ErrorProtocol,
		"committed Tmux window %s disappeared", tmuxID,
	)
}

func (service *Service) validateManifestDigest(
	manifest launchManifest,
) error {
	expected := manifest.ManifestDigest
	actual, _, err := marshalManifest(manifest)
	if err != nil {
		return err
	}
	if expected == "" || expected != actual {
		return fmt.Errorf("manifest digest mismatch")
	}
	return nil
}
