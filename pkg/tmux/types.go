// Package tmux owns Runtime's non-durable tmux window manager.
//
// It deliberately does not read Profile configuration or Session state. Its
// Start method consumes one already-resolved command invocation and every other
// operation is resolved only from the configured default or dedicated server.
package tmux

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"time"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/pkg/contract"
)

const (
	WindowSchemaVersion = 1
	serverSchemaVersion = 1
	registryVersion     = 1

	SessionName         = "sn-session"
	serverOptionName    = "@sn_runtime_server"
	sessionOptionName   = "@sn_runtime_session"
	windowRecordOption  = "@sn_runtime_record"
	windowCommitOption  = "@sn_runtime_registered"
	sentinelNamePrefix  = "__sn_sentinel_"
	windowNamePrefix    = "__sn_window_"
	helperCommandName   = "__sn_tmux_helper"
	maxManifestBytes    = 1 << 20
	maxSendBytes        = 1 << 20
	maxFramedSendBytes  = 2 << 20
	defaultReadyTimeout = 5 * time.Second
	defaultGateTimeout  = 15 * time.Second
)

type ServerMode string

const (
	ServerModeDefault   ServerMode = "default"
	ServerModeDedicated ServerMode = "dedicated"
)

type State string

const (
	StateStarting State = "starting"
	StateRunning  State = "running"
	StateExited   State = "exited"
	StateOrphaned State = "orphaned"
)

// Invocation is the final adapter output. Argv includes argv[0], Environment
// is the complete exact environment, and Path is the already-resolved
// executable.
type Invocation struct {
	ProfileID    string
	Path         string
	Argv         []string
	Environment  []string
	CWD          string
	ConfigDigest string
	Binding      *Binding
	// CooperativeReady asks the target to publish the private launch-ready fact
	// after exec. It is needed only when target and bootstrap helper share one
	// executable identity.
	CooperativeReady bool
}

// Binding is an optional, opaque ownership reference supplied by a composing
// caller. Tmux validates and preserves it, but never reads the referenced
// domain state or derives lifecycle decisions from it.
type Binding struct {
	Kind string `json:"kind"`
	ID   string `json:"id"`
}

type StartRequest struct {
	Invocation Invocation
}

type SafeError struct {
	Code    contract.ErrorCode  `json:"code"`
	Phase   contract.ErrorPhase `json:"phase"`
	Message string              `json:"message"`
}

type Window struct {
	SchemaVersion int        `json:"schema_version"`
	TmuxID        string     `json:"tmux_id"`
	State         State      `json:"state"`
	CreatedAt     time.Time  `json:"created_at"`
	WindowID      string     `json:"window_id"`
	PaneID        string     `json:"pane_id"`
	ProfileID     string     `json:"profile_id,omitempty"`
	CWD           string     `json:"cwd,omitempty"`
	ConfigDigest  string     `json:"config_digest,omitempty"`
	Binding       *Binding   `json:"binding,omitempty"`
	ExitCode      *int       `json:"exit_code,omitempty"`
	Signal        string     `json:"signal,omitempty"`
	LaunchError   *SafeError `json:"launch_error,omitempty"`
}

type StartResult struct {
	Window         Window `json:"tmux_window"`
	LaunchAccepted bool   `json:"launch_accepted"`
}

type ActionResult struct {
	TmuxID   string `json:"tmux_id"`
	Action   string `json:"action"`
	Accepted bool   `json:"accepted"`
}

type Config struct {
	Home           string
	ServerMode     ServerMode
	LockFile       string
	ManifestDir    string
	TmuxConfigFile string
	SocketDir      string
	SocketFile     string
	TmuxBinary     string
	HelperCommand  []string
	ServerEnv      []string
	Now            func() time.Time
	Random         io.Reader
	ReadyTimeout   time.Duration
	GateTimeout    time.Duration
	CommandTimeout time.Duration
	LookupProcess  func(int) (ProcessIdentity, error)
	RunCommand     CommandRunner
}

type ProcessIdentity struct {
	StartToken         string
	Executable         string
	ExecutableIdentity string
}

type CommandSpec struct {
	Path   string
	Args   []string
	Env    []string
	Dir    string
	Stdin  io.Reader
	Stdout io.Writer
	Stderr io.Writer
}

type CommandResult struct {
	Stdout   []byte
	Stderr   []byte
	ExitCode int
}

type CommandRunner interface {
	Run(context.Context, CommandSpec) (CommandResult, error)
}

type serverMarker struct {
	FullHomeDigest    string `json:"full_home_digest"`
	SchemaVersion     int    `json:"schema_version"`
	OwnerUID          int    `json:"owner_uid"`
	SentinelID        string `json:"sentinel_id"`
	ServerIncarnation string `json:"server_incarnation"`
	TmuxConfigDigest  string `json:"tmux_conf_digest"`
}

type windowRecord struct {
	SchemaVersion            int        `json:"schema_version"`
	RegistryVersion          int        `json:"registry_version"`
	TmuxID                   string     `json:"tmux_id"`
	ProfileID                string     `json:"profile_id"`
	CreatedAt                time.Time  `json:"created_at"`
	CWD                      string     `json:"cwd"`
	ConfigDigest             string     `json:"config_digest"`
	Binding                  *Binding   `json:"binding,omitempty"`
	CooperativeReady         bool       `json:"cooperative_ready,omitempty"`
	TargetReady              bool       `json:"target_ready,omitempty"`
	WindowID                 string     `json:"window_id"`
	PaneID                   string     `json:"pane_id"`
	HelperPID                int        `json:"helper_pid"`
	HelperPGID               int        `json:"helper_pgid"`
	ProcessStart             string     `json:"process_start"`
	HelperExecutable         string     `json:"helper_executable"`
	HelperExecutableIdentity string     `json:"helper_executable_identity"`
	ResolvedExecutable       string     `json:"resolved_executable"`
	ExecutableIdentity       string     `json:"executable_identity"`
	ServerIncarnation        string     `json:"server_incarnation"`
	LaunchError              *SafeError `json:"launch_error,omitempty"`
}

type launchManifest struct {
	SchemaVersion      int      `json:"schema_version"`
	OwnerUID           int      `json:"owner_uid"`
	Home               string   `json:"home"`
	Nonce              string   `json:"nonce"`
	Path               string   `json:"path"`
	Argv               []string `json:"argv"`
	Environment        []string `json:"environment"`
	CWD                string   `json:"cwd"`
	ExecutableIdentity string   `json:"executable_identity"`
	ManifestDigest     string   `json:"manifest_digest,omitempty"`
	ReadyPath          string   `json:"ready_path"`
	GoPath             string   `json:"go_path"`
	StatusPath         string   `json:"status_path"`
	TargetReadyPath    string   `json:"target_ready_path,omitempty"`
	GateTimeoutMS      int64    `json:"gate_timeout_ms"`
}

type readyFact struct {
	SchemaVersion      int    `json:"schema_version"`
	Nonce              string `json:"nonce"`
	PID                int    `json:"pid"`
	PGID               int    `json:"pgid"`
	ProcessStart       string `json:"process_start"`
	Executable         string `json:"executable"`
	ExecutableIdentity string `json:"executable_identity,omitempty"`
	ManifestDigest     string `json:"manifest_digest"`
}

type goFact struct {
	SchemaVersion  int    `json:"schema_version"`
	Nonce          string `json:"nonce"`
	ManifestDigest string `json:"manifest_digest"`
}

type helperStatus struct {
	SchemaVersion  int        `json:"schema_version"`
	Nonce          string     `json:"nonce"`
	ManifestDigest string     `json:"manifest_digest"`
	Error          *SafeError `json:"error,omitempty"`
}

type targetReadyFact struct {
	SchemaVersion      int    `json:"schema_version"`
	Nonce              string `json:"nonce"`
	ManifestDigest     string `json:"manifest_digest"`
	PID                int    `json:"pid"`
	ProcessStart       string `json:"process_start"`
	Executable         string `json:"executable"`
	ExecutableIdentity string `json:"executable_identity"`
}

type liveWindow struct {
	Window
	RecordEncoded string
	Record        *windowRecord
	Registered    bool
	WindowName    string
	PaneDead      bool
	PanePID       int
	PanePGID      int
}

func runtimeError(
	code contract.ErrorCode,
	phase contract.ErrorPhase,
	format string,
	args ...any,
) error {
	return &contract.RuntimeError{
		Code: code, Phase: phase, Message: fmt.Sprintf(format, args...),
	}
}

func safeRuntimeError(err error) *SafeError {
	if err == nil {
		return nil
	}
	value := &SafeError{
		Code: contract.ErrorInternal, Phase: contract.PhaseTransport,
		Message: err.Error(),
	}
	var runtimeErr *contract.RuntimeError
	if ok := errors.As(err, &runtimeErr); ok {
		value.Code = runtimeErr.Code
		value.Phase = runtimeErr.Phase
		value.Message = runtimeErr.Message
	}
	return value
}

func cloneWindow(value Window) Window {
	result := value
	if value.Binding != nil {
		current := *value.Binding
		result.Binding = &current
	}
	if value.ExitCode != nil {
		current := *value.ExitCode
		result.ExitCode = &current
	}
	if value.LaunchError != nil {
		current := *value.LaunchError
		result.LaunchError = &current
	}
	return result
}

func cloneBinding(value *Binding) *Binding {
	if value == nil {
		return nil
	}
	result := *value
	return &result
}

func validateBinding(value *Binding) error {
	if value == nil {
		return nil
	}
	if err := identity.Validate(value.ID, value.Kind); err != nil {
		return fmt.Errorf("invalid Tmux binding: %w", err)
	}
	return nil
}

func strictDecode(data []byte, target any) error {
	decoder := json.NewDecoder(io.LimitReader(
		bytes.NewReader(data), int64(len(data))+1,
	))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return err
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		if err == nil {
			return fmt.Errorf("trailing JSON")
		}
		return err
	}
	return nil
}

// TTYFiles groups the terminal descriptors used by Attach.
type TTYFiles struct {
	Stdin  *os.File
	Stdout *os.File
	Stderr *os.File
}
