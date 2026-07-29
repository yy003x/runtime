package tmux

import (
	"context"
	"encoding/base64"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
	"time"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/internal/profileid"
)

func (service *Service) loadWindows(
	ctx context.Context,
	marker serverMarker,
) ([]liveWindow, error) {
	ids, err := service.tmuxLines(
		ctx, "list-windows", "-a", "-F", "#{window_id}",
	)
	if err != nil {
		return nil, err
	}
	result := make([]liveWindow, 0, len(ids))
	seenIDs := make(map[string]struct{})
	for _, id := range ids {
		if !windowIDPattern.MatchString(id) {
			return nil, tmuxTransportError(
				contract.ErrorProtocol, "invalid Tmux window ID %q", id,
			)
		}
		if id == marker.SentinelID {
			continue
		}
		value, err := service.inspectWindow(ctx, id)
		if err != nil {
			return nil, err
		}
		if value.Registered {
			if value.Record == nil {
				return nil, tmuxTransportError(
					contract.ErrorProtocol,
					"registered Tmux window %s has no record", id,
				)
			}
			if _, exists := seenIDs[value.TmuxID]; exists {
				return nil, tmuxTransportError(
					contract.ErrorProtocol,
					"duplicate tmux_id %s", value.TmuxID,
				)
			}
			seenIDs[value.TmuxID] = struct{}{}
			if err := service.validateRegisteredWindow(&value, marker); err != nil {
				return nil, err
			}
			if err := service.projectState(ctx, &value); err != nil {
				return nil, err
			}
			result = append(result, value)
			continue
		}
		tmuxID, ok := parseWindowName(value.WindowName)
		if !ok {
			return nil, tmuxTransportError(
				contract.ErrorProtocol,
				"unregistered Tmux window %s has an unknown name", id,
			)
		}
		createdAt, err := parseUUIDv7(tmuxID)
		if err != nil {
			return nil, tmuxTransportError(
				contract.ErrorProtocol,
				"orphan Tmux window %s has an invalid tmux_id", id,
			)
		}
		if _, exists := seenIDs[tmuxID]; exists {
			return nil, tmuxTransportError(
				contract.ErrorProtocol, "duplicate tmux_id %s", tmuxID,
			)
		}
		seenIDs[tmuxID] = struct{}{}
		value.Window = Window{
			SchemaVersion: WindowSchemaVersion,
			TmuxID:        tmuxID, State: StateOrphaned, CreatedAt: createdAt,
			WindowID: value.WindowID, PaneID: value.PaneID,
		}
		result = append(result, value)
	}
	return result, nil
}

func (service *Service) inspectWindow(
	ctx context.Context,
	windowID string,
) (liveWindow, error) {
	if !windowIDPattern.MatchString(windowID) {
		return liveWindow{}, fmt.Errorf("invalid Tmux window ID %q", windowID)
	}
	name, err := service.windowFormat(ctx, windowID, "#{window_name}")
	if err != nil {
		return liveWindow{}, err
	}
	recordEncoded, err := service.windowFormat(
		ctx, windowID, "#{"+windowRecordOption+"}",
	)
	if err != nil {
		return liveWindow{}, err
	}
	committed, err := service.windowFormat(
		ctx, windowID, "#{"+windowCommitOption+"}",
	)
	if err != nil {
		return liveWindow{}, err
	}
	sessionName, err := service.windowFormat(ctx, windowID, "#{session_name}")
	if err != nil {
		return liveWindow{}, err
	}
	linked, err := service.windowFormat(ctx, windowID, "#{window_linked}")
	if err != nil {
		return liveWindow{}, err
	}
	if sessionName != SessionName || linked == "1" {
		return liveWindow{}, tmuxTransportError(
			contract.ErrorConflict,
			"Tmux window %s is linked outside the dedicated session", windowID,
		)
	}
	panes, err := service.tmuxLines(
		ctx, "list-panes", "-t", windowID, "-F", "#{pane_id}",
	)
	if err != nil {
		return liveWindow{}, err
	}
	if len(panes) != 1 || !paneIDPattern.MatchString(panes[0]) {
		return liveWindow{}, tmuxTransportError(
			contract.ErrorProtocol,
			"Tmux window %s must contain exactly one pane", windowID,
		)
	}
	paneID := panes[0]
	paneDeadValue, err := service.paneFormat(ctx, paneID, "#{pane_dead}")
	if err != nil {
		return liveWindow{}, err
	}
	panePIDValue, err := service.paneFormat(ctx, paneID, "#{pane_pid}")
	if err != nil {
		return liveWindow{}, err
	}
	panePID, err := strconv.Atoi(panePIDValue)
	if err != nil || panePID <= 0 {
		return liveWindow{}, tmuxTransportError(
			contract.ErrorProtocol, "Tmux pane %s has an invalid PID", paneID,
		)
	}
	value := liveWindow{
		Window: Window{
			SchemaVersion: WindowSchemaVersion,
			WindowID:      windowID, PaneID: paneID,
		},
		RecordEncoded: recordEncoded, Registered: committed == "1",
		WindowName: name, PaneDead: paneDeadValue == "1", PanePID: panePID,
	}
	if committed != "" && committed != "1" {
		return liveWindow{}, tmuxTransportError(
			contract.ErrorProtocol,
			"Tmux window %s has an invalid registered marker", windowID,
		)
	}
	if recordEncoded != "" {
		var record windowRecord
		if err := decodeOption(recordEncoded, &record); err != nil {
			if committed == "1" {
				return liveWindow{}, tmuxTransportError(
					contract.ErrorProtocol,
					"decode registered Tmux record %s: %v", windowID, err,
				)
			}
		} else {
			value.Record = &record
			value.TmuxID = record.TmuxID
		}
	}
	return value, nil
}

func (service *Service) validateRegisteredWindow(
	value *liveWindow,
	marker serverMarker,
) error {
	record := value.Record
	if record == nil ||
		record.SchemaVersion != WindowSchemaVersion ||
		record.RegistryVersion != registryVersion ||
		record.TmuxID == "" || record.ProfileID == "" ||
		record.CreatedAt.IsZero() || record.CWD == "" ||
		record.ConfigDigest == "" ||
		record.WindowID != value.WindowID ||
		record.PaneID != value.PaneID ||
		record.HelperPID <= 0 || record.HelperPGID <= 0 ||
		record.ProcessStart == "" || record.HelperExecutable == "" ||
		record.HelperExecutableIdentity == "" ||
		record.ResolvedExecutable == "" ||
		record.ExecutableIdentity == "" ||
		record.ServerIncarnation != marker.ServerIncarnation {
		return tmuxTransportError(
			contract.ErrorProtocol,
			"registered Tmux window %s has an incomplete record",
			value.WindowID,
		)
	}
	if err := profileid.Validate(record.ProfileID); err != nil ||
		!filepath.IsAbs(record.CWD) ||
		!configDigestPattern.MatchString(record.ConfigDigest) ||
		!windowIDPattern.MatchString(record.WindowID) ||
		!paneIDPattern.MatchString(record.PaneID) ||
		record.HelperPGID != record.HelperPID {
		return tmuxTransportError(
			contract.ErrorProtocol,
			"registered Tmux window %s has an invalid record",
			value.WindowID,
		)
	}
	if record.LaunchError != nil {
		if err := (&contract.RuntimeError{
			Code: record.LaunchError.Code, Phase: record.LaunchError.Phase,
			Message: record.LaunchError.Message,
		}).Validate(); err != nil {
			return tmuxTransportError(
				contract.ErrorProtocol,
				"registered Tmux window %s has an invalid launch error",
				value.WindowID,
			)
		}
	}
	createdAt, err := parseUUIDv7(record.TmuxID)
	if err != nil || !createdAt.Equal(record.CreatedAt.UTC().Truncate(time.Millisecond)) {
		return tmuxTransportError(
			contract.ErrorProtocol,
			"registered Tmux window %s has an invalid creation identity",
			value.WindowID,
		)
	}
	if expected := windowName(record.TmuxID); value.WindowName != expected {
		return tmuxTransportError(
			contract.ErrorConflict,
			"registered Tmux window %s name changed", value.WindowID,
		)
	}
	value.Window = Window{
		SchemaVersion: WindowSchemaVersion, TmuxID: record.TmuxID,
		CreatedAt: record.CreatedAt.UTC(), WindowID: record.WindowID,
		PaneID: record.PaneID, ProfileID: record.ProfileID,
		CWD: record.CWD, ConfigDigest: record.ConfigDigest,
		LaunchError: record.LaunchError,
	}
	return nil
}

func (service *Service) projectState(
	ctx context.Context,
	value *liveWindow,
) error {
	record := value.Record
	if value.PaneDead {
		value.State = StateExited
		status, err := service.paneFormat(
			ctx, value.PaneID, "#{pane_dead_status}",
		)
		if err == nil && status != "" {
			if code, parseErr := strconv.Atoi(status); parseErr == nil {
				value.ExitCode = &code
			}
		}
		signal, err := service.paneFormat(
			ctx, value.PaneID, "#{pane_dead_signal}",
		)
		if err == nil {
			value.Signal = signal
		}
		return nil
	}
	if value.PanePID != record.HelperPID {
		return tmuxTransportError(
			contract.ErrorConflict,
			"Tmux pane %s PID changed", value.PaneID,
		)
	}
	identity, err := service.lookupProcess(value.PanePID)
	if err != nil {
		return tmuxTransportError(
			contract.ErrorConflict,
			"identify Tmux pane %s process: %v", value.PaneID, err,
		)
	}
	if identity.StartToken != record.ProcessStart {
		return tmuxTransportError(
			contract.ErrorConflict,
			"Tmux pane %s process identity changed", value.PaneID,
		)
	}
	pgid, err := processGroup(value.PanePID)
	if err != nil || pgid != record.HelperPGID {
		return tmuxTransportError(
			contract.ErrorConflict,
			"Tmux pane %s process group changed", value.PaneID,
		)
	}
	current := filepath.Clean(identity.Executable)
	helper := filepath.Clean(record.HelperExecutable)
	target := filepath.Clean(record.ResolvedExecutable)
	if processExecutableMatches(current, helper) {
		if identity.ExecutableIdentity != record.HelperExecutableIdentity {
			return tmuxTransportError(
				contract.ErrorConflict,
				"Tmux pane %s helper executable identity changed", value.PaneID,
			)
		}
		value.State = StateStarting
		return nil
	}
	if !processExecutableMatches(current, target) {
		return tmuxTransportError(
			contract.ErrorConflict,
			"Tmux pane %s executable changed", value.PaneID,
		)
	}
	if identity.ExecutableIdentity != record.ExecutableIdentity {
		return tmuxTransportError(
			contract.ErrorConflict,
			"Tmux pane %s executable file identity changed", value.PaneID,
		)
	}
	value.State = StateRunning
	return nil
}

func processExecutableMatches(current, expected string) bool {
	return filepath.Clean(current) == filepath.Clean(expected)
}

func (service *Service) windowFormat(
	ctx context.Context,
	windowID string,
	format string,
) (string, error) {
	result, err := service.runTmux(
		ctx, nil, "display-message", "-p", "-t", windowID, format,
	)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", tmuxTransportError(
			contract.ErrorProtocol, "inspect Tmux window %s: %v",
			windowID, commandFailure("tmux display-message", result),
		)
	}
	value, err := trimOneLine(result.Stdout)
	if err != nil {
		return "", tmuxTransportError(
			contract.ErrorProtocol, "inspect Tmux window %s: %v",
			windowID, err,
		)
	}
	return value, nil
}

func (service *Service) paneFormat(
	ctx context.Context,
	paneID string,
	format string,
) (string, error) {
	result, err := service.runTmux(
		ctx, nil, "display-message", "-p", "-t", paneID, format,
	)
	if err != nil {
		return "", err
	}
	if result.ExitCode != 0 {
		return "", tmuxTransportError(
			contract.ErrorProtocol, "inspect Tmux pane %s: %v",
			paneID, commandFailure("tmux display-message", result),
		)
	}
	return trimOneLine(result.Stdout)
}

func windowName(tmuxID string) string {
	return windowNamePrefix + tmuxID
}

func parseWindowName(value string) (string, bool) {
	if !strings.HasPrefix(value, windowNamePrefix) {
		return "", false
	}
	tmuxID := strings.TrimPrefix(value, windowNamePrefix)
	if _, err := parseUUIDv7(tmuxID); err != nil {
		return "", false
	}
	return tmuxID, true
}

func encodeWindowRecord(value windowRecord) (string, error) {
	data, err := json.Marshal(value)
	if err != nil {
		return "", err
	}
	return base64.RawURLEncoding.EncodeToString(data), nil
}

func processGroup(pid int) (int, error) {
	return syscall.Getpgid(pid)
}
