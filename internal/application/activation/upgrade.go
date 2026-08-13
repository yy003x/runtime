package activation

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"github.com/yy003x/runtime/internal/infrastructure/runtimeconfig"
	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
	"github.com/yy003x/runtime/internal/infrastructure/toolbuiltin"
	"github.com/yy003x/runtime/internal/infrastructure/toolconfig"
)

const (
	maintenanceLockName = "runtime.maintenance.lock"
	lifecycleLockName   = "sn-server.lifecycle.lock"
	activationGuardName = "activation.guard.json"
	journalName         = "activation.journal.json"
	journalSchema       = 3
)

// UpgradeRequest describes a normalized release payload. The running
// executable must be the payload's sn-cli; callers cannot ask an installed
// binary to activate unrelated files.
type UpgradeRequest struct {
	TargetHome         string
	PayloadDir         string
	CandidateBinary    string
	OverwriteConfig    bool
	LocalSourceInstall bool
	// CloseNativeTUISessions closes Session-bound native TUI windows before
	// local-source activation. It is intentionally supplied by the CLI layer so
	// activation stays independent from the private PTY carrier.
	CloseNativeTUISessions func() error
	InspectServer          func() (ManagedServerProcess, error)
	StopServer             func() error
	CoordinatorPID         int
}

// ManagedServerProcess is the process identity that a local-source install is
// allowed to keep running during its read-only quiescence preflight. The
// activation lifecycle lock prevents that identity from changing before the
// subsequent StopServer call and full recheck.
type ManagedServerProcess struct {
	PID        int
	StartToken string
}

type UpgradeResult struct {
	TargetHome           string   `json:"target_home"`
	CopiedProfiles       []string `json:"copied_profiles"`
	CopiedTools          []string `json:"copied_tools"`
	CopiedRuntimeConfig  bool     `json:"copied_runtime_config"`
	ReplacedResources    bool     `json:"replaced_resources"`
	ResourceFiles        []string `json:"resource_files"`
	ActivationEpoch      int      `json:"activation_epoch"`
	ContractVersion      int      `json:"contract_version"`
	SessionSchemaVersion int      `json:"session_schema_version"`
	RunSchemaVersion     int      `json:"run_schema_version"`
	RuntimeStateReset    bool     `json:"runtime_state_reset"`
}

// UpgradeActivate validates, quiesces, and atomically activates a complete
// release payload. It never migrates Session or SQLite state.
func UpgradeActivate(
	ctx context.Context,
	request UpgradeRequest,
) (UpgradeResult, error) {
	target, payload, candidate, err := normalizeUpgradeRequest(request)
	if err != nil {
		return UpgradeResult{}, err
	}
	if request.LocalSourceInstall {
		if !request.OverwriteConfig {
			return UpgradeResult{}, fmt.Errorf(
				"local source install must overwrite active configs",
			)
		}
		if request.InspectServer == nil || request.StopServer == nil {
			return UpgradeResult{}, fmt.Errorf(
				"local source install requires managed server quiescence",
			)
		}
	} else if request.InspectServer != nil || request.StopServer != nil {
		return UpgradeResult{}, fmt.Errorf(
			"server quiescence callback is only valid for local source install",
		)
	}
	payloadLayout := newReleasePayloadLayout(payload)
	manifest, _, err := LoadManifest(payloadLayout.ReleaseDir)
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("load payload release manifest: %w", err)
	}
	if err := ValidateManifestCompatibility(manifest); err != nil {
		return UpgradeResult{}, err
	}
	if err := validatePayload(payload, candidate); err != nil {
		return UpgradeResult{}, err
	}
	if err := validatePayloadContracts(
		target, payload, request.OverwriteConfig,
	); err != nil {
		return UpgradeResult{}, err
	}
	if err := validateCandidateProfileHome(
		ctx, candidate, payload,
	); err != nil {
		return UpgradeResult{}, err
	}
	if err := ensurePrivateDirectory(target); err != nil {
		return UpgradeResult{}, err
	}
	stateDir := filepath.Join(target, "state")
	for _, directory := range []string{
		stateDir, filepath.Join(target, "tmp"),
	} {
		if err := ensurePrivateDirectory(directory); err != nil {
			return UpgradeResult{}, err
		}
	}
	maintenance, err := acquireUpgradeLock(
		filepath.Join(stateDir, maintenanceLockName),
	)
	if err != nil {
		return UpgradeResult{}, err
	}
	defer maintenance.Close()
	lifecycle, err := acquireUpgradeLock(
		filepath.Join(stateDir, lifecycleLockName),
	)
	if err != nil {
		return UpgradeResult{}, err
	}
	defer lifecycle.Close()
	journalPath := filepath.Join(stateDir, journalName)
	if err := recoverUpgradeTransaction(target, journalPath); err != nil {
		return UpgradeResult{}, fmt.Errorf("recover previous activation: %w", err)
	}
	if err := validateActiveHomeShape(target); err != nil {
		return UpgradeResult{}, err
	}
	processTargets, err := captureTargetProcesses(target)
	if err != nil {
		return UpgradeResult{}, err
	}
	ownerStartToken, err := processStartToken(os.Getpid())
	if err != nil {
		return UpgradeResult{}, fmt.Errorf(
			"identify activation helper: %w", err,
		)
	}
	excluded := map[int]processExclusion{
		os.Getpid(): {StartToken: ownerStartToken},
	}
	if request.CoordinatorPID != 0 {
		if request.CoordinatorPID != os.Getppid() {
			return UpgradeResult{}, fmt.Errorf(
				"activation coordinator must be the candidate parent process",
			)
		}
		cliTarget, exists := findProcessTarget(processTargets, "sn-cli")
		if !exists {
			return UpgradeResult{}, fmt.Errorf(
				"activation coordinator was supplied but target sn-cli does not exist",
			)
		}
		if err := requireTargetCLIProcess(request.CoordinatorPID, cliTarget); err != nil {
			return UpgradeResult{}, err
		}
		coordinatorToken, tokenErr := processStartToken(
			request.CoordinatorPID,
		)
		if tokenErr != nil {
			return UpgradeResult{}, fmt.Errorf(
				"identify activation coordinator: %w", tokenErr,
			)
		}
		excluded[request.CoordinatorPID] = processExclusion{
			StartToken: coordinatorToken,
		}
	}
	if !request.LocalSourceInstall {
		tmuxLifecycle, lockErr := acquireUpgradeLock(
			filepath.Join(stateDir, "tmux.lock"),
		)
		if lockErr != nil {
			return UpgradeResult{}, lockErr
		}
		defer tmuxLifecycle.Close()
		if err := preflightQuiescence(
			target, manifest, excluded, processTargets,
			quiescenceOptions{},
		); err != nil {
			return UpgradeResult{}, err
		}
	}

	stageRoot, err := os.MkdirTemp(
		filepath.Join(target, "tmp"), "activation-",
	)
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("create activation stage: %w", err)
	}
	if err := os.Chmod(stageRoot, 0o700); err != nil {
		_ = os.RemoveAll(stageRoot)
		return UpgradeResult{}, err
	}
	cleanupStage := true
	defer func() {
		if cleanupStage {
			_ = os.RemoveAll(stageRoot)
		}
	}()

	desired := filepath.Join(stageRoot, "desired")
	result, err := buildDesiredHome(
		target, payload, desired, request.OverwriteConfig,
	)
	if err != nil {
		return UpgradeResult{}, err
	}
	if err := validateDesiredHomeContracts(desired, manifest); err != nil {
		return UpgradeResult{}, err
	}
	result.TargetHome = target
	result.ActivationEpoch = manifest.ActivationEpoch
	result.ContractVersion = manifest.ContractVersion
	result.SessionSchemaVersion = manifest.SessionSchemaVersion
	result.RunSchemaVersion = manifest.RunSchemaVersion
	result.RuntimeStateReset = request.LocalSourceInstall
	if err := validateCandidateHome(ctx, candidate, desired); err != nil {
		return UpgradeResult{}, err
	}
	if request.LocalSourceInstall {
		if err := validateRuntimeStateReset(target); err != nil {
			return UpgradeResult{}, fmt.Errorf(
				"validate Runtime state reset: %w", err,
			)
		}
		managedServer, err := request.InspectServer()
		if err != nil {
			return UpgradeResult{}, fmt.Errorf(
				"inspect sn-server for local source install: %w", err,
			)
		}
		preStopExcluded, err := managedServerExclusions(
			managedServer, excluded,
		)
		if err != nil {
			return UpgradeResult{}, err
		}
		if request.CloseNativeTUISessions != nil {
			if err := request.CloseNativeTUISessions(); err != nil {
				return UpgradeResult{}, fmt.Errorf(
					"close native_tui Sessions for local source install: %w", err,
				)
			}
		}
		tmuxLifecycle, lockErr := acquireUpgradeLock(
			filepath.Join(stateDir, "tmux.lock"),
		)
		if lockErr != nil {
			return UpgradeResult{}, lockErr
		}
		defer tmuxLifecycle.Close()
		if err := preflightQuiescence(
			target, manifest, preStopExcluded, processTargets,
			quiescenceOptions{
				SkipServer:       true,
				SkipRuntimeState: true,
			},
		); err != nil {
			return UpgradeResult{}, err
		}
		if err := request.StopServer(); err != nil {
			return UpgradeResult{}, fmt.Errorf(
				"stop sn-server for local source install: %w", err,
			)
		}
		if err := preflightQuiescence(
			target, manifest, excluded, processTargets,
			quiescenceOptions{SkipRuntimeState: true},
		); err != nil {
			return UpgradeResult{}, err
		}
	} else if err := preflightState(target, manifest); err != nil {
		return UpgradeResult{}, err
	}

	nonce, err := randomNonce()
	if err != nil {
		return UpgradeResult{}, err
	}
	guard := []byte(fmt.Sprintf(
		"{\"schema_version\":2,\"nonce\":%q,\"owner_pid\":%d,\"owner_start_token\":%q,\"created_at\":%q}\n",
		nonce, os.Getpid(), ownerStartToken,
		time.Now().UTC().Format(time.RFC3339Nano),
	))
	if err := prepareBarrierFiles(stageRoot, guard); err != nil {
		return UpgradeResult{}, err
	}
	if err := os.MkdirAll(filepath.Join(stageRoot, "backup"), 0o700); err != nil {
		return UpgradeResult{}, err
	}
	if err := syncTree(stageRoot); err != nil {
		return UpgradeResult{}, fmt.Errorf(
			"persist activation stage: %w", err,
		)
	}
	if err := syncDirectory(filepath.Dir(stageRoot)); err != nil {
		return UpgradeResult{}, fmt.Errorf(
			"persist activation stage root: %w", err,
		)
	}
	journal, err := newTransactionJournal(
		target, stageRoot, desired, nonce, ownerStartToken, guard,
		request.LocalSourceInstall,
	)
	if err != nil {
		return UpgradeResult{}, err
	}
	if err := writeJournal(journalPath, journal); err != nil {
		return UpgradeResult{}, err
	}
	cleanupStage = false
	activeGuard := filepath.Join(stateDir, activationGuardName)
	if err := writeActivationGuard(activeGuard, guard); err != nil {
		return UpgradeResult{}, rollbackAfterCommitError(
			journalPath, journal, err,
		)
	}
	if err := installActivationBarriers(journalPath, &journal); err != nil {
		return UpgradeResult{}, rollbackAfterCommitError(
			journalPath, journal, err,
		)
	}
	journal.Phase = "barriered"
	if err := writeJournal(journalPath, journal); err != nil {
		return UpgradeResult{}, rollbackAfterCommitError(
			journalPath, journal, err,
		)
	}
	if err := preflightQuiescence(
		target, manifest, excluded, processTargets,
		quiescenceOptions{
			SkipRuntimeState: request.LocalSourceInstall,
		},
	); err != nil {
		return UpgradeResult{}, rollbackAfterCommitError(
			journalPath, journal, err,
		)
	}
	if err := commitUpgradeTransaction(journalPath, &journal); err != nil {
		return UpgradeResult{}, err
	}
	journal.Phase = "committed"
	if err := writeJournal(journalPath, journal); err != nil {
		return UpgradeResult{}, err
	}
	if err := finalizeUpgradeTransaction(
		journalPath, journal, activeGuard, guard,
	); err != nil {
		return UpgradeResult{}, err
	}
	return result, nil
}

func managedServerExclusions(
	managed ManagedServerProcess,
	base map[int]processExclusion,
) (map[int]processExclusion, error) {
	result := make(map[int]processExclusion, len(base)+1)
	for pid, exclusion := range base {
		result[pid] = exclusion
	}
	if managed.PID == 0 {
		if managed.StartToken != "" {
			return nil, fmt.Errorf(
				"managed server start token requires a process id",
			)
		}
		return result, nil
	}
	if managed.PID <= 0 || managed.StartToken == "" {
		return nil, fmt.Errorf("managed server identity is incomplete")
	}
	if _, reserved := result[managed.PID]; reserved {
		return nil, fmt.Errorf(
			"managed server pid %d conflicts with an activation process",
			managed.PID,
		)
	}
	current, err := processStartToken(managed.PID)
	if err != nil {
		return nil, fmt.Errorf(
			"identify managed sn-server pid=%d: %w", managed.PID, err,
		)
	}
	if current != managed.StartToken {
		return nil, fmt.Errorf(
			"managed sn-server pid=%d changed identity", managed.PID,
		)
	}
	result[managed.PID] = processExclusion{StartToken: managed.StartToken}
	return result, nil
}

func validateActiveHomeShape(target string) error {
	for _, name := range []string{
		"bin", "configs", "tools", "resources", "logs",
		"sessions", "state", "tmp",
	} {
		path := filepath.Join(target, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf(
				"active %s must be a directory, not a symlink", name,
			)
		}
	}
	for _, name := range []string{"runtime.json"} {
		path := filepath.Join(target, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 ||
			!info.Mode().IsRegular() {
			return fmt.Errorf(
				"active %s must be a regular file, not a symlink", name,
			)
		}
	}
	return nil
}

func normalizeUpgradeRequest(
	request UpgradeRequest,
) (target, payload, candidate string, err error) {
	for label, value := range map[string]string{
		"target home": request.TargetHome,
		"payload":     request.PayloadDir,
		"candidate":   request.CandidateBinary,
	} {
		if strings.TrimSpace(value) == "" {
			return "", "", "", fmt.Errorf("%s is required", label)
		}
	}
	target, err = layout.CanonicalHome(request.TargetHome)
	if err != nil {
		return "", "", "", fmt.Errorf("resolve target home: %w", err)
	}
	payload, err = filepath.Abs(request.PayloadDir)
	if err != nil {
		return "", "", "", err
	}
	candidate, err = filepath.Abs(request.CandidateBinary)
	if err != nil {
		return "", "", "", err
	}
	payload = filepath.Clean(payload)
	candidate = filepath.Clean(candidate)
	if target == string(filepath.Separator) || target == "." {
		return "", "", "", fmt.Errorf("target home is unsafe")
	}
	if info, statErr := os.Lstat(target); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return "", "", "", fmt.Errorf(
				"target home must be a directory, not a symlink",
			)
		}
		if info.Mode().Perm()&0o077 != 0 {
			return "", "", "", fmt.Errorf(
				"target home must not be accessible by group or others",
			)
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return "", "", "", statErr
	}
	payloadInfo, err := os.Lstat(payload)
	if err != nil || payloadInfo.Mode()&os.ModeSymlink != 0 ||
		!payloadInfo.IsDir() {
		return "", "", "", fmt.Errorf("payload must be a directory, not a symlink")
	}
	payloadCandidate := filepath.Join(payload, "sn-cli")
	left, err := os.Stat(candidate)
	if err != nil {
		return "", "", "", err
	}
	currentExecutable, err := os.Executable()
	if err != nil {
		return "", "", "", err
	}
	currentInfo, err := os.Stat(currentExecutable)
	if err != nil {
		return "", "", "", err
	}
	if !os.SameFile(left, currentInfo) {
		return "", "", "", fmt.Errorf(
			"upgrade activation must run from the payload candidate",
		)
	}
	right, err := os.Stat(payloadCandidate)
	if err != nil {
		return "", "", "", err
	}
	if !os.SameFile(left, right) {
		return "", "", "", fmt.Errorf(
			"running candidate must be payload sn-cli",
		)
	}
	return target, payload, candidate, nil
}

func validatePayload(payload, candidate string) error {
	payloadLayout := newReleasePayloadLayout(payload)
	for _, name := range []string{"sn-cli", "sn-server"} {
		path := filepath.Join(payload, name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 ||
			!info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("payload %s must be a regular executable", name)
		}
	}
	for _, name := range []string{"configs", "resources", "release"} {
		path := filepath.Join(payload, name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("payload %s must be a directory", name)
		}
		if err := validateTree(path); err != nil {
			return err
		}
	}
	if err := validatePayloadLayoutEntries(payload); err != nil {
		return err
	}
	for _, required := range []struct {
		name string
		path string
	}{
		{name: "resources/schema", path: payloadLayout.SchemaDir},
		{name: "resources/tools", path: payloadLayout.ToolsDir},
	} {
		info, err := os.Lstat(required.path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf(
				"payload %s must be a directory", required.name,
			)
		}
	}
	info, err := os.Lstat(payloadLayout.RuntimeConfigFile)
	if err != nil || info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() {
		return fmt.Errorf(
			"payload release/runtime.json must be a regular file",
		)
	}
	_ = candidate
	return nil
}

func validatePayloadLayoutEntries(payload string) error {
	for _, directory := range []struct {
		label   string
		path    string
		allowed []string
	}{
		{
			label: "root", path: payload,
			allowed: []string{
				"sn-cli", "sn-server", "configs", "resources", "release",
			},
		},
		{
			label: "resources", path: filepath.Join(payload, "resources"),
			allowed: []string{"schema", "tools"},
		},
		{
			label: "release", path: filepath.Join(payload, "release"),
			allowed: []string{"release.json", "runtime.json", "tmux.conf"},
		},
	} {
		allowed := make(map[string]struct{}, len(directory.allowed))
		for _, name := range directory.allowed {
			allowed[name] = struct{}{}
		}
		entries, err := os.ReadDir(directory.path)
		if err != nil {
			return fmt.Errorf("read payload %s directory: %w", directory.label, err)
		}
		for _, entry := range entries {
			if _, exists := allowed[entry.Name()]; !exists {
				return fmt.Errorf(
					"payload %s contains unexpected entry %q",
					directory.label, entry.Name(),
				)
			}
		}
	}
	return nil
}

type releasePayloadLayout struct {
	SchemaDir         string
	ToolsDir          string
	ReleaseDir        string
	RuntimeConfigFile string
	TmuxConfigFile    string
}

func newReleasePayloadLayout(root string) releasePayloadLayout {
	resources := filepath.Join(root, "resources")
	release := filepath.Join(root, "release")
	return releasePayloadLayout{
		SchemaDir:         filepath.Join(resources, "schema"),
		ToolsDir:          filepath.Join(resources, "tools"),
		ReleaseDir:        release,
		RuntimeConfigFile: filepath.Join(release, "runtime.json"),
		TmuxConfigFile:    filepath.Join(release, "tmux.conf"),
	}
}

func validatePayloadContracts(
	target, payload string,
	overwrite bool,
) error {
	payloadLayout := newReleasePayloadLayout(payload)
	payloadConfig, err := runtimeconfig.LoadRequired(
		payloadLayout.RuntimeConfigFile,
	)
	if err != nil {
		return fmt.Errorf("validate payload release/runtime.json: %w", err)
	}
	payloadTools, err := toolconfig.LoadDirectory(
		payloadLayout.ToolsDir,
	)
	if err != nil {
		return fmt.Errorf("validate payload resources/tools: %w", err)
	}
	if err := validateToolSelection(payloadConfig, payloadTools); err != nil {
		return fmt.Errorf("validate payload Agent tools: %w", err)
	}
	if !overwrite {
		activeRuntime := filepath.Join(target, "runtime.json")
		if _, err := os.Lstat(activeRuntime); err == nil {
			if _, err := runtimeconfig.LoadRequired(activeRuntime); err != nil {
				return fmt.Errorf("validate active runtime.json: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect active runtime.json: %w", err)
		}
		activeTools := filepath.Join(target, "tools")
		if _, err := os.Lstat(activeTools); err == nil {
			if _, err := toolconfig.LoadDirectory(activeTools); err != nil {
				return fmt.Errorf("validate active tools: %w", err)
			}
		} else if !errors.Is(err, os.ErrNotExist) {
			return fmt.Errorf("inspect active tools: %w", err)
		}
	}

	return validatePayloadManagedFiles(payloadLayout)
}

func validateDesiredHomeContracts(
	desired string,
	expectedManifest Manifest,
) error {
	stagedConfig, err := runtimeconfig.LoadRequired(
		filepath.Join(desired, "runtime.json"),
	)
	if err != nil {
		return fmt.Errorf("validate staged runtime.json: %w", err)
	}
	stagedTools, err := toolconfig.LoadDirectory(
		filepath.Join(desired, "tools"),
	)
	if err != nil {
		return fmt.Errorf("validate staged tools: %w", err)
	}
	if err := validateToolSelection(stagedConfig, stagedTools); err != nil {
		return fmt.Errorf("validate staged Agent tools: %w", err)
	}
	resources := filepath.Join(desired, "resources")
	manifest, _, err := LoadManifest(resources)
	if err != nil {
		return fmt.Errorf("validate staged release manifest: %w", err)
	}
	if manifest != expectedManifest {
		return fmt.Errorf("staged release manifest changed during activation")
	}
	if err := validateRequiredResources(resources); err != nil {
		return fmt.Errorf("validate staged resources: %w", err)
	}
	return nil
}

func validateToolSelection(
	config runtimeconfig.Config,
	catalog *toolconfig.Catalog,
) error {
	for _, name := range catalog.Names() {
		if toolbuiltin.IsBuiltin(name) {
			return fmt.Errorf(
				"tool manifest %q conflicts with a built-in tool", name,
			)
		}
	}
	for _, name := range config.Agent.Tools {
		if toolbuiltin.IsBuiltin(name) {
			continue
		}
		if _, exists := catalog.Get(name); !exists {
			return fmt.Errorf("configured tool %q is unavailable", name)
		}
	}
	return nil
}

func validateRequiredResources(resources string) error {
	return validateManagedFiles(
		filepath.Join(resources, "schema"),
		filepath.Join(resources, "tmux.conf"),
	)
}

func validatePayloadManagedFiles(layout releasePayloadLayout) error {
	return validateManagedFiles(layout.SchemaDir, layout.TmuxConfigFile)
}

func validateManagedFiles(schemaDir, tmuxConfig string) error {
	if _, err := readRegular(tmuxConfig, 1<<20); err != nil {
		return fmt.Errorf(
			"validate tmux.conf as a no-follow regular file: %w",
			err,
		)
	}
	for _, name := range []string{
		"profile.schema.json", "runtime.schema.json", "tool.schema.json",
	} {
		path := filepath.Join(schemaDir, name)
		if err := validateJSONSchema(path); err != nil {
			return fmt.Errorf(
				"validate schema/%s: %w", name, err,
			)
		}
	}
	return nil
}

func validateJSONSchema(path string) error {
	data, err := readRegular(path, 1<<20)
	if err != nil {
		return fmt.Errorf("read as a no-follow regular file: %w", err)
	}
	var raw json.RawMessage
	if err := strictjson.Decode(
		bytes.NewReader(data), 1<<20, &raw,
	); err != nil {
		return fmt.Errorf("decode strict JSON: %w", err)
	}
	if err := validateSchemaIdentity(filepath.Base(path), raw); err != nil {
		return err
	}
	document, err := jsonschema.UnmarshalJSON(bytes.NewReader(raw))
	if err != nil {
		return fmt.Errorf("decode JSON Schema: %w", err)
	}
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(path, document); err != nil {
		return fmt.Errorf("register JSON Schema: %w", err)
	}
	if _, err := compiler.Compile(path); err != nil {
		return fmt.Errorf("compile JSON Schema: %w", err)
	}
	return nil
}

func validateSchemaIdentity(name string, raw json.RawMessage) error {
	var identity struct {
		Schema               string                     `json:"$schema"`
		ID                   string                     `json:"$id"`
		Title                string                     `json:"title"`
		Type                 string                     `json:"type"`
		AdditionalProperties *bool                      `json:"additionalProperties"`
		Properties           map[string]json.RawMessage `json:"properties"`
		OneOf                []json.RawMessage          `json:"oneOf"`
	}
	if err := json.Unmarshal(raw, &identity); err != nil {
		return fmt.Errorf("decode JSON Schema identity: %w", err)
	}
	const draft = "https://json-schema.org/draft/2020-12/schema"
	if identity.Schema != draft {
		return fmt.Errorf("JSON Schema has unexpected $schema %q", identity.Schema)
	}
	switch name {
	case "profile.schema.json":
		const id = "https://github.com/yy003x/runtime/resources/schema/profile.schema.json"
		if identity.ID != id ||
			identity.Title != "Runtime Profile" ||
			len(identity.OneOf) != 2 {
			return fmt.Errorf("profile JSON Schema identity or root shape is invalid")
		}
	case "runtime.schema.json":
		const id = "https://github.com/yy003x/runtime/resources/schema/runtime.schema.json"
		if identity.ID != id ||
			identity.Title != "SN Runtime Configuration" ||
			identity.Type != "object" ||
			identity.AdditionalProperties == nil ||
			*identity.AdditionalProperties ||
			identity.Properties == nil {
			return fmt.Errorf("runtime JSON Schema identity or root shape is invalid")
		}
		for _, property := range []string{"agent", "scheduler", "run"} {
			if _, exists := identity.Properties[property]; !exists {
				return fmt.Errorf(
					"runtime JSON Schema is missing root property %q",
					property,
				)
			}
		}
	case "tool.schema.json":
		const id = "https://github.com/yy003x/runtime/resources/schema/tool.schema.json"
		if identity.ID != id ||
			identity.Title != "Runtime Tool" ||
			identity.Type != "object" ||
			identity.AdditionalProperties == nil ||
			*identity.AdditionalProperties {
			return fmt.Errorf("tool JSON Schema identity or root shape is invalid")
		}
	default:
		return fmt.Errorf("unexpected Runtime JSON Schema %q", name)
	}
	return nil
}

func buildDesiredHome(
	target, payload, desired string,
	overwrite bool,
) (UpgradeResult, error) {
	payloadLayout := newReleasePayloadLayout(payload)
	for _, directory := range []string{
		desired,
		filepath.Join(desired, "configs"),
		filepath.Join(desired, "tools"),
		filepath.Join(desired, "resources"),
		filepath.Join(desired, "bin"),
	} {
		if err := os.MkdirAll(directory, 0o700); err != nil {
			return UpgradeResult{}, err
		}
	}
	result := UpgradeResult{ReplacedResources: true}
	activeBin := filepath.Join(target, "bin")
	if info, statErr := os.Lstat(activeBin); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return UpgradeResult{}, fmt.Errorf(
				"active bin must be a directory, not a symlink",
			)
		}
		if err := copyTree(
			activeBin, filepath.Join(desired, "bin"), false,
		); err != nil {
			return UpgradeResult{}, err
		}
	} else if !errors.Is(statErr, os.ErrNotExist) {
		return UpgradeResult{}, statErr
	}
	if !overwrite {
		for _, name := range []string{"configs", "tools"} {
			source := filepath.Join(target, name)
			if info, err := os.Lstat(source); err == nil {
				if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
					return UpgradeResult{}, fmt.Errorf(
						"active %s must be a directory, not a symlink", name,
					)
				}
				if err := copyTree(source, filepath.Join(desired, name), false); err != nil {
					return UpgradeResult{}, err
				}
			} else if !errors.Is(err, os.ErrNotExist) {
				return UpgradeResult{}, err
			}
		}
	}
	profiles, err := copyMissingNames(
		filepath.Join(payload, "configs"),
		filepath.Join(desired, "configs"),
	)
	if err != nil {
		return UpgradeResult{}, err
	}
	result.CopiedProfiles = profiles
	tools, err := copyMissingNames(
		payloadLayout.ToolsDir,
		filepath.Join(desired, "tools"),
	)
	if err != nil {
		return UpgradeResult{}, err
	}
	result.CopiedTools = tools
	if overwrite {
		result.CopiedProfiles, err = regularRelativeFiles(
			filepath.Join(payload, "configs"),
		)
		if err != nil {
			return UpgradeResult{}, err
		}
		result.CopiedTools, err = regularRelativeFiles(
			payloadLayout.ToolsDir,
		)
		if err != nil {
			return UpgradeResult{}, err
		}
	}
	if err := copyTree(
		payloadLayout.SchemaDir,
		filepath.Join(desired, "resources", "schema"), true,
	); err != nil {
		return UpgradeResult{}, err
	}
	for _, file := range []struct {
		name   string
		source string
	}{
		{
			name: "release.json",
			source: filepath.Join(
				payloadLayout.ReleaseDir, "release.json",
			),
		},
		{name: "tmux.conf", source: payloadLayout.TmuxConfigFile},
	} {
		if err := copyRegular(
			file.source,
			filepath.Join(desired, "resources", file.name), 0o600,
		); err != nil {
			return UpgradeResult{}, err
		}
	}
	resourceFiles, err := regularRelativeFiles(
		filepath.Join(desired, "resources"),
	)
	if err != nil {
		return UpgradeResult{}, err
	}
	result.ResourceFiles = resourceFiles
	activeRuntime := filepath.Join(target, "runtime.json")
	runtimeSource := payloadLayout.RuntimeConfigFile
	if !overwrite {
		if info, statErr := os.Lstat(activeRuntime); statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
				return UpgradeResult{}, fmt.Errorf(
					"active runtime.json must be a regular file",
				)
			}
			runtimeSource = activeRuntime
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return UpgradeResult{}, statErr
		} else {
			result.CopiedRuntimeConfig = true
		}
	} else {
		result.CopiedRuntimeConfig = true
	}
	if err := copyRegular(
		runtimeSource, filepath.Join(desired, "runtime.json"), 0o600,
	); err != nil {
		return UpgradeResult{}, err
	}
	for _, name := range []string{"sn-cli", "sn-server"} {
		stagedBinary := filepath.Join(desired, "bin", name)
		if info, statErr := os.Lstat(stagedBinary); statErr == nil {
			if info.Mode()&os.ModeSymlink != 0 ||
				!info.Mode().IsRegular() {
				return UpgradeResult{}, fmt.Errorf(
					"active bin entry %s must be a regular file", name,
				)
			}
			if err := os.Remove(stagedBinary); err != nil {
				return UpgradeResult{}, err
			}
		} else if !errors.Is(statErr, os.ErrNotExist) {
			return UpgradeResult{}, statErr
		}
		if err := copyRegular(
			filepath.Join(payload, name),
			stagedBinary, 0o755,
		); err != nil {
			return UpgradeResult{}, err
		}
	}
	return result, nil
}

func validateCandidateHome(
	ctx context.Context,
	candidate, desired string,
) error {
	for _, args := range [][]string{
		{"profile", "check"},
		{"server", "info"},
	} {
		if err := validateCandidateCommand(
			ctx, candidate, desired, args,
		); err != nil {
			return err
		}
	}
	return nil
}

func validateCandidateProfileHome(
	ctx context.Context,
	candidate, home string,
) error {
	return validateCandidateCommand(
		ctx, candidate, home, []string{"profile", "check"},
	)
}

func validateCandidateCommand(
	ctx context.Context,
	candidate, home string,
	args []string,
) error {
	command := exec.CommandContext(ctx, candidate, args...)
	command.Env = replaceEnvironment(
		os.Environ(),
		map[string]string{
			"SN_CLI_HOME":                  home,
			"SN_CLI_ACTIVATION_VALIDATION": "1",
		},
	)
	output, err := command.CombinedOutput()
	if err != nil {
		return fmt.Errorf(
			"candidate %s failed: %w: %s",
			strings.Join(args, " "), err,
			strings.TrimSpace(string(output)),
		)
	}
	return nil
}

func replaceEnvironment(
	environment []string,
	values map[string]string,
) []string {
	result := make([]string, 0, len(environment)+len(values))
	for _, item := range environment {
		name, _, _ := strings.Cut(item, "=")
		if _, replace := values[name]; !replace {
			result = append(result, item)
		}
	}
	for name, value := range values {
		result = append(result, name+"="+value)
	}
	sort.Strings(result)
	return result
}
