package tmux

import (
	"context"
	"encoding/base64"
	"fmt"
	"os"
	"regexp"
	"strconv"
	"strings"

	"github.com/yy003x/runtime/pkg/contract"
)

var (
	windowIDPattern     = regexp.MustCompile(`^@[0-9]+$`)
	paneIDPattern       = regexp.MustCompile(`^%[0-9]+$`)
	incarnationPattern  = regexp.MustCompile(`^[0-9a-f]{32}$`)
	configDigestPattern = regexp.MustCompile(`^[0-9a-f]{64}$`)
	launchFilePattern   = regexp.MustCompile(
		`^launch-[0-9a-f]{32}\.(json|ready|go|status)$`,
	)
)

func (service *Service) ensureServer(
	ctx context.Context,
) (serverMarker, error) {
	if !service.dedicated() {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorInvalidRequest,
			"default Tmux mode requires creating a managed target with sn-session",
		)
	}
	exists, err := service.managedSessionExists(ctx)
	if err != nil {
		return serverMarker{}, err
	}
	if exists {
		marker, err := service.loadAndValidateServer(ctx)
		if err == nil {
			return marker, nil
		}
		missing, inspectErr := service.serverMarkerMissing(ctx)
		if inspectErr != nil || !missing {
			return serverMarker{}, err
		}
		if recoverErr := service.recoverMarkerlessBootstrap(ctx); recoverErr != nil {
			return serverMarker{}, recoverErr
		}
	}
	return service.createServer(ctx)
}

func (service *Service) createServer(
	ctx context.Context,
) (serverMarker, error) {
	if !service.dedicated() {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorInvalidRequest,
			"default Tmux mode cannot create an empty managed session",
		)
	}
	configDigest, err := service.ownerConfigDigest()
	if err != nil {
		return serverMarker{}, err
	}
	incarnation, err := randomHex(service.random, 16)
	if err != nil {
		return serverMarker{}, fmt.Errorf("generate Tmux server incarnation: %w", err)
	}
	sentinelName := service.sentinelName(incarnation)
	marker := serverMarker{
		FullHomeDigest: service.homeDigest, SchemaVersion: serverSchemaVersion,
		OwnerUID: service.uid, SentinelID: "@0",
		ServerIncarnation: incarnation, TmuxConfigDigest: configDigest,
	}
	encoded := encodeServerMarker(marker)
	args := []string{
		"-S", service.config.SocketFile,
		"-f", service.config.TmuxConfigFile,
		"new-session", "-d", "-s", SessionName, "-n", sentinelName,
		"--", "/usr/bin/true",
		";", "set-option", "-gq", serverOptionName, encoded,
	}
	result, runErr := service.run(ctx, CommandSpec{
		Path: service.tmuxPath, Args: args, Env: service.serverEnv,
	})
	if runErr != nil {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorProviderUnavailable,
			"create dedicated Tmux server: %v", runErr,
		)
	}
	if result.ExitCode != 0 {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorProtocol, "create dedicated Tmux server: %v",
			commandFailure("tmux new-session", result),
		)
	}
	loaded, err := service.loadAndValidateServer(ctx)
	if err != nil {
		return serverMarker{}, err
	}
	if loaded.ServerIncarnation != incarnation {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux server incarnation changed during bootstrap",
		)
	}
	return loaded, nil
}

func (service *Service) loadAndValidateServer(
	ctx context.Context,
) (serverMarker, error) {
	if !service.dedicated() {
		return service.loadAndValidateDefaultSession(ctx)
	}
	configDigest, err := service.ownerConfigDigest()
	if err != nil {
		return serverMarker{}, err
	}
	if _, err := service.socketExists(); err != nil {
		return serverMarker{}, err
	}
	encoded, err := service.showGlobalOption(ctx, serverOptionName, true)
	if err != nil {
		return serverMarker{}, err
	}
	if encoded == "" {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorProtocol, "Tmux server marker is missing",
		)
	}
	var marker serverMarker
	if err := decodeOption(encoded, &marker); err != nil {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorProtocol, "decode Tmux server marker: %v", err,
		)
	}
	if marker.SchemaVersion != serverSchemaVersion ||
		marker.FullHomeDigest != service.homeDigest ||
		marker.OwnerUID != service.uid ||
		!windowIDPattern.MatchString(marker.SentinelID) ||
		!incarnationPattern.MatchString(marker.ServerIncarnation) ||
		marker.TmuxConfigDigest != configDigest {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux server marker does not match this Runtime home",
		)
	}
	sessions, err := service.tmuxLines(
		ctx, "list-sessions", "-F", "#{session_name}",
	)
	if err != nil {
		return serverMarker{}, err
	}
	if len(sessions) != 1 || sessions[0] != SessionName {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorProtocol,
			"dedicated Tmux server must contain exactly session %q",
			SessionName,
		)
	}
	sentinel, err := service.inspectWindow(ctx, marker.SentinelID)
	if err != nil {
		return serverMarker{}, err
	}
	if sentinel.WindowName != service.sentinelName(marker.ServerIncarnation) ||
		sentinel.RecordEncoded != "" || sentinel.Registered ||
		!sentinel.PaneDead {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorProtocol, "Tmux sentinel identity is invalid",
		)
	}
	return marker, nil
}

func (service *Service) loadAndValidateDefaultSession(
	ctx context.Context,
) (serverMarker, error) {
	configDigest, err := service.ownerConfigDigest()
	if err != nil {
		return serverMarker{}, err
	}
	exists, err := service.managedSessionExists(ctx)
	if err != nil {
		return serverMarker{}, err
	}
	if !exists {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorConflict, "managed Tmux session %q is missing", SessionName,
		)
	}
	encoded, err := service.showSessionOption(ctx, sessionOptionName, true)
	if err != nil {
		return serverMarker{}, err
	}
	if encoded == "" {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux session %q already exists but is not owned by this Runtime home",
			SessionName,
		)
	}
	var marker serverMarker
	if err := decodeOption(encoded, &marker); err != nil {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorProtocol, "decode Tmux session marker: %v", err,
		)
	}
	if marker.SchemaVersion != serverSchemaVersion ||
		marker.FullHomeDigest != service.homeDigest ||
		marker.OwnerUID != service.uid || marker.SentinelID != "" ||
		!incarnationPattern.MatchString(marker.ServerIncarnation) ||
		marker.TmuxConfigDigest != configDigest {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux session %q does not match this Runtime home",
			SessionName,
		)
	}
	grouped, err := service.tmuxLines(
		ctx, "display-message", "-p", "-t", SessionName,
		"#{session_grouped}",
	)
	if err != nil {
		return serverMarker{}, err
	}
	if len(grouped) != 1 || grouped[0] != "0" {
		return serverMarker{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux session %q must not be grouped", SessionName,
		)
	}
	return marker, nil
}

func (service *Service) serverMarkerMissing(ctx context.Context) (bool, error) {
	var value string
	var err error
	if service.dedicated() {
		value, err = service.showGlobalOption(ctx, serverOptionName, true)
	} else {
		value, err = service.showSessionOption(ctx, sessionOptionName, true)
	}
	if err != nil {
		return false, err
	}
	return value == "", nil
}

func (service *Service) recoverMarkerlessBootstrap(ctx context.Context) error {
	if !service.dedicated() {
		return tmuxTransportError(
			contract.ErrorConflict,
			"markerless default Tmux session cannot be adopted",
		)
	}
	sessions, err := service.tmuxLines(
		ctx, "list-sessions", "-F", "#{session_name}",
	)
	if err != nil {
		return err
	}
	if len(sessions) != 1 || sessions[0] != SessionName {
		return tmuxTransportError(
			contract.ErrorProtocol,
			"markerless Tmux server is not a recoverable bootstrap",
		)
	}
	ids, err := service.tmuxLines(
		ctx, "list-windows", "-a", "-F", "#{window_id}",
	)
	if err != nil {
		return err
	}
	if len(ids) != 1 || !windowIDPattern.MatchString(ids[0]) {
		return tmuxTransportError(
			contract.ErrorProtocol,
			"markerless Tmux server contains managed or unknown windows",
		)
	}
	value, err := service.inspectWindow(ctx, ids[0])
	if err != nil {
		return err
	}
	incarnation, ok := service.parseSentinelName(value.WindowName)
	if !ok || value.RecordEncoded != "" || value.Registered ||
		!value.PaneDead {
		return tmuxTransportError(
			contract.ErrorProtocol,
			"markerless Tmux server sentinel cannot be proved",
		)
	}
	if !incarnationPattern.MatchString(incarnation) {
		return tmuxTransportError(
			contract.ErrorProtocol,
			"markerless Tmux server incarnation is invalid",
		)
	}
	if err := service.requireMarkerlessUserOptionsEmpty(
		ctx, value.WindowID, value.PaneID,
	); err != nil {
		return err
	}
	socketIdentity, err := os.Lstat(service.config.SocketFile)
	if err != nil {
		return tmuxTransportError(
			contract.ErrorConflict,
			"capture markerless Tmux socket identity: %v", err,
		)
	}
	result, err := service.runTmux(ctx, nil, "kill-server")
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return tmuxTransportError(
			contract.ErrorConflict, "clean markerless Tmux server: %v",
			commandFailure("tmux kill-server", result),
		)
	}
	if err := service.removeStoppedSocket(
		context.Background(), socketIdentity,
	); err != nil {
		return err
	}
	return nil
}

func (service *Service) requireMarkerlessUserOptionsEmpty(
	ctx context.Context,
	windowID string,
	paneID string,
) error {
	queries := [][]string{
		{"show-options", "-g"},
		{"show-options", "-t", SessionName},
		{"show-options", "-w", "-t", windowID},
		{"show-options", "-p", "-t", paneID},
	}
	for _, query := range queries {
		lines, err := service.tmuxLines(ctx, query...)
		if err != nil {
			return err
		}
		for _, line := range lines {
			if strings.HasPrefix(strings.TrimSpace(line), "@") {
				return tmuxTransportError(
					contract.ErrorProtocol,
					"markerless Tmux server contains user options",
				)
			}
		}
	}
	return nil
}

func (service *Service) sentinelName(incarnation string) string {
	return sentinelNamePrefix + service.homeDigest + "_" + incarnation
}

func (service *Service) parseSentinelName(value string) (string, bool) {
	prefix := sentinelNamePrefix + service.homeDigest + "_"
	if !strings.HasPrefix(value, prefix) {
		return "", false
	}
	incarnation := strings.TrimPrefix(value, prefix)
	return incarnation, incarnationPattern.MatchString(incarnation)
}

func (service *Service) showGlobalOption(
	ctx context.Context,
	name string,
	quiet bool,
) (string, error) {
	args := []string{"show-options", "-g"}
	if quiet {
		args = append(args, "-q")
	}
	args = append(args, "-v", name)
	result, err := service.runTmux(ctx, nil, args...)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", tmuxTransportError(
			contract.ErrorProtocol, "query Tmux server option %s: %v",
			name, commandFailure("tmux show-options", result),
		)
	}
	return trimOneLine(result.Stdout)
}

func (service *Service) showSessionOption(
	ctx context.Context,
	name string,
	quiet bool,
) (string, error) {
	args := []string{"show-options"}
	if quiet {
		args = append(args, "-q")
	}
	args = append(args, "-v", "-t", SessionName, name)
	result, err := service.runTmux(ctx, nil, args...)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", tmuxTransportError(
			contract.ErrorProtocol, "query Tmux session option %s: %v",
			name, commandFailure("tmux show-options", result),
		)
	}
	return trimOneLine(result.Stdout)
}

func (service *Service) showOwnerOption(
	ctx context.Context,
	quiet bool,
) (string, error) {
	if service.dedicated() {
		return service.showGlobalOption(ctx, serverOptionName, quiet)
	}
	return service.showSessionOption(ctx, sessionOptionName, quiet)
}

func (service *Service) tmuxLines(
	ctx context.Context,
	args ...string,
) ([]string, error) {
	result, err := service.runTmux(ctx, nil, args...)
	if err != nil {
		return nil, err
	}
	if result.ExitCode != 0 {
		return nil, tmuxTransportError(
			contract.ErrorProtocol, "query Tmux server: %v",
			commandFailure("tmux "+strings.Join(args, " "), result),
		)
	}
	value := strings.TrimSuffix(string(result.Stdout), "\n")
	if value == "" {
		return nil, nil
	}
	lines := strings.Split(value, "\n")
	for _, line := range lines {
		if strings.ContainsRune(line, '\r') {
			return nil, tmuxTransportError(
				contract.ErrorProtocol, "Tmux returned malformed output",
			)
		}
	}
	return lines, nil
}

func trimOneLine(value []byte) (string, error) {
	result := strings.TrimSuffix(string(value), "\n")
	if strings.ContainsAny(result, "\r\n") {
		return "", fmt.Errorf("Tmux returned multiline metadata")
	}
	return result, nil
}

func decodeOption(encoded string, target any) error {
	raw, err := base64.RawURLEncoding.DecodeString(encoded)
	if err != nil {
		return err
	}
	if len(raw) > maxManifestBytes {
		return fmt.Errorf("Tmux option exceeds size limit")
	}
	return strictDecode(raw, target)
}

func parseNumericID(value string, prefix byte) (int, error) {
	if len(value) < 2 || value[0] != prefix {
		return 0, fmt.Errorf("invalid Tmux ID %q", value)
	}
	number, err := strconv.Atoi(value[1:])
	if err != nil || number < 0 {
		return 0, fmt.Errorf("invalid Tmux ID %q", value)
	}
	return number, nil
}

func (service *Service) forceKillServer(ctx context.Context) {
	if !service.dedicated() {
		if exists, _ := service.managedSessionExists(ctx); !exists {
			return
		}
		_, _ = service.runTmux(ctx, nil, "kill-session", "-t", "="+SessionName)
		return
	}
	if exists, _ := service.socketExists(); !exists {
		return
	}
	socketIdentity, _ := os.Lstat(service.config.SocketFile)
	_, _ = service.runTmux(ctx, nil, "kill-server")
	_ = service.removeStoppedSocket(ctx, socketIdentity)
}

func (service *Service) cleanupEmptyServer(
	ctx context.Context,
	expected serverMarker,
) error {
	current, err := service.loadAndValidateServer(ctx)
	if err != nil {
		return err
	}
	if encodeServerMarker(current) != encodeServerMarker(expected) {
		return tmuxTransportError(
			contract.ErrorConflict,
			"Tmux server identity changed before empty-server cleanup",
		)
	}
	windows, err := service.loadWindows(ctx, current)
	if err != nil {
		return err
	}
	if len(windows) != 0 {
		return tmuxTransportError(
			contract.ErrorConflict,
			"Tmux server is not empty after failed bootstrap",
		)
	}
	socketIdentity, err := os.Lstat(service.config.SocketFile)
	if err != nil {
		return tmuxTransportError(
			contract.ErrorConflict,
			"capture empty Tmux server socket identity: %v", err,
		)
	}
	condition := fmt.Sprintf(
		"#{&&:#{==:#{%s},%s},#{&&:#{==:#{session_windows},1},#{&&:#{==:#{window_id},%s},#{==:#{window_name},%s}}}}",
		serverOptionName, encodeServerMarker(current),
		current.SentinelID, service.sentinelName(current.ServerIncarnation),
	)
	result, err := service.runTmux(
		ctx, nil,
		"if-shell", "-F", "-t", current.SentinelID,
		condition, "kill-server", `run-shell "exit 73"`,
	)
	if err != nil {
		return err
	}
	if result.ExitCode != 0 {
		return tmuxTransportError(
			contract.ErrorConflict,
			"Tmux server changed during empty-server cleanup",
		)
	}
	return service.removeStoppedSocket(context.Background(), socketIdentity)
}
