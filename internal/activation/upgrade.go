package activation

import (
	"bytes"
	"context"
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strings"
	"time"

	"github.com/santhosh-tekuri/jsonschema/v6"
	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/internal/layout"
	"github.com/yy003x/runtime/internal/runtimeconfig"
	"github.com/yy003x/runtime/internal/strictjson"
)

const (
	maintenanceLockName = "runtime.maintenance.lock"
	lifecycleLockName   = "sn-server.lifecycle.lock"
	activationGuardName = "activation.guard.json"
	journalName         = "activation.journal.json"
	journalSchema       = 2
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
	InspectServer      func() (ManagedServerProcess, error)
	StopServer         func() error
	CoordinatorPID     int
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
	CopiedRuntimeConfig  bool     `json:"copied_runtime_config"`
	ReplacedResources    bool     `json:"replaced_resources"`
	ResourceFiles        []string `json:"resource_files"`
	ActivationEpoch      int      `json:"activation_epoch"`
	ContractVersion      int      `json:"contract_version"`
	SessionSchemaVersion int      `json:"session_schema_version"`
	RunSchemaVersion     int      `json:"run_schema_version"`
	RuntimeStateReset    bool     `json:"runtime_state_reset"`
}

type transactionJournal struct {
	SchemaVersion     int                   `json:"schema_version"`
	Nonce             string                `json:"nonce"`
	OwnerPID          int                   `json:"owner_pid"`
	OwnerStartToken   string                `json:"owner_start_token"`
	GuardDigest       string                `json:"guard_digest"`
	TargetHome        string                `json:"target_home"`
	StageRoot         string                `json:"stage_root"`
	Phase             string                `json:"phase"`
	ResetRuntimeState bool                  `json:"reset_runtime_state,omitempty"`
	Artifacts         []transactionArtifact `json:"artifacts"`
}

type transactionArtifact struct {
	Name           string `json:"name"`
	Target         string `json:"target"`
	Staged         string `json:"staged"`
	Backup         string `json:"backup"`
	OriginalExists bool   `json:"original_exists"`
	OriginalDigest string `json:"original_digest,omitempty"`
	NewDigest      string `json:"new_digest"`
	BackedUp       bool   `json:"backed_up"`
	Installed      bool   `json:"installed"`
}

type fileLock struct {
	file *os.File
}

func (lock *fileLock) Close() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	_ = lock.file.Close()
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
	manifest, _, err := LoadManifest(filepath.Join(payload, "resources"))
	if err != nil {
		return UpgradeResult{}, fmt.Errorf("load payload release manifest: %w", err)
	}
	if manifest.ActivationEpoch != 2 || manifest.ContractVersion != 3 ||
		manifest.SessionSchemaVersion != 2 || manifest.RunSchemaVersion != 4 {
		return UpgradeResult{}, fmt.Errorf(
			"payload activation contract is incompatible: epoch=%d contract=%d session_schema=%d run_schema=%d",
			manifest.ActivationEpoch, manifest.ContractVersion,
			manifest.SessionSchemaVersion, manifest.RunSchemaVersion,
		)
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
	tmuxLifecycle, err := acquireUpgradeLock(
		filepath.Join(stateDir, "tmux.lock"),
	)
	if err != nil {
		return UpgradeResult{}, err
	}
	defer tmuxLifecycle.Close()

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
		"bin", "configs", "resources",
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
	for _, name := range []string{"sn-cli", "sn-server"} {
		path := filepath.Join(payload, name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 ||
			!info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
			return fmt.Errorf("payload %s must be a regular executable", name)
		}
	}
	for _, name := range []string{"configs", "resources"} {
		path := filepath.Join(payload, name)
		info, err := os.Lstat(path)
		if err != nil || info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
			return fmt.Errorf("payload %s must be a directory", name)
		}
		if err := validateTree(path); err != nil {
			return err
		}
	}
	runtimeConfig := filepath.Join(payload, "runtime.json")
	info, err := os.Lstat(runtimeConfig)
	if err != nil || info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() {
		return fmt.Errorf("payload runtime.json must be a regular file")
	}
	_ = candidate
	return nil
}

func validatePayloadContracts(
	target, payload string,
	overwrite bool,
) error {
	payloadRuntime := filepath.Join(payload, "runtime.json")
	if _, err := runtimeconfig.LoadRequired(payloadRuntime); err != nil {
		return fmt.Errorf("validate payload runtime.json: %w", err)
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
	}

	return validateRequiredResources(filepath.Join(payload, "resources"))
}

func validateDesiredHomeContracts(
	desired string,
	expectedManifest Manifest,
) error {
	if _, err := runtimeconfig.LoadRequired(
		filepath.Join(desired, "runtime.json"),
	); err != nil {
		return fmt.Errorf("validate staged runtime.json: %w", err)
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

func validateRequiredResources(resources string) error {
	tmuxConfig := filepath.Join(resources, "tmux.conf")
	if _, err := readRegular(tmuxConfig, 1<<20); err != nil {
		return fmt.Errorf(
			"validate tmux.conf as a no-follow regular file: %w",
			err,
		)
	}
	for _, name := range []string{
		"profile.schema.json", "runtime.schema.json",
	} {
		path := filepath.Join(resources, "schema", name)
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
	default:
		return fmt.Errorf("unexpected Runtime JSON Schema %q", name)
	}
	return nil
}

func buildDesiredHome(
	target, payload, desired string,
	overwrite bool,
) (UpgradeResult, error) {
	for _, directory := range []string{
		desired,
		filepath.Join(desired, "configs"),
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
	resourceFiles, err := regularRelativeFiles(
		filepath.Join(payload, "resources"),
	)
	if err != nil {
		return UpgradeResult{}, err
	}
	result.ResourceFiles = resourceFiles
	if !overwrite {
		for _, name := range []string{"configs"} {
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
	if overwrite {
		result.CopiedProfiles, err = regularRelativeFiles(
			filepath.Join(payload, "configs"),
		)
		if err != nil {
			return UpgradeResult{}, err
		}
	}
	if err := copyTree(
		filepath.Join(payload, "resources"),
		filepath.Join(desired, "resources"), true,
	); err != nil {
		return UpgradeResult{}, err
	}
	activeRuntime := filepath.Join(target, "runtime.json")
	runtimeSource := filepath.Join(payload, "runtime.json")
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

var transactionArtifactNames = []string{
	"resources", "runtime.json", "bin", "configs",
}

func barrierFile(stageRoot, name string) string {
	return filepath.Join(stageRoot, "barriers", name)
}

func prepareBarrierFiles(stageRoot string, guard []byte) error {
	for _, name := range []string{"bin", "configs"} {
		if err := writeActivationGuard(
			barrierFile(stageRoot, name), guard,
		); err != nil {
			return err
		}
	}
	return nil
}

func newTransactionJournal(
	target, stageRoot, desired, nonce, ownerStartToken string,
	guard []byte,
	resetRuntimeState bool,
) (transactionJournal, error) {
	guardDigest, err := treeDigest(barrierFile(stageRoot, "bin"))
	if err != nil {
		return transactionJournal{}, err
	}
	journal := transactionJournal{
		SchemaVersion: journalSchema,
		Nonce:         nonce,
		OwnerPID:      os.Getpid(), OwnerStartToken: ownerStartToken,
		GuardDigest: guardDigest, TargetHome: target,
		StageRoot: stageRoot, Phase: "prepared",
		ResetRuntimeState: resetRuntimeState,
		Artifacts: make(
			[]transactionArtifact, 0, len(transactionArtifactNames),
		),
	}
	_ = guard
	for index, name := range transactionArtifactNames {
		targetPath := filepath.Join(target, name)
		backupPath := filepath.Join(
			stageRoot, "backup",
			fmt.Sprintf("%02d-%s", index, filepath.Base(name)),
		)
		originalExists, originalDigest, statErr := pathDigest(targetPath)
		if statErr != nil {
			return transactionJournal{}, statErr
		}
		stagedPath := filepath.Join(desired, name)
		newDigest, digestErr := treeDigest(stagedPath)
		if digestErr != nil {
			return transactionJournal{}, digestErr
		}
		journal.Artifacts = append(journal.Artifacts, transactionArtifact{
			Name: name, Target: targetPath, Staged: stagedPath,
			Backup: backupPath, OriginalExists: originalExists,
			OriginalDigest: originalDigest, NewDigest: newDigest,
		})
	}
	return journal, nil
}

func installActivationBarriers(
	journalPath string,
	journal *transactionJournal,
) error {
	for _, name := range []string{"bin", "configs"} {
		artifact, err := journalArtifact(journal, name)
		if err != nil {
			return err
		}
		if artifact.OriginalExists {
			if err := os.MkdirAll(
				filepath.Dir(artifact.Backup), 0o700,
			); err != nil {
				return err
			}
			if err := durableRename(
				artifact.Target, artifact.Backup,
			); err != nil {
				return fmt.Errorf("install %s barrier backup: %w", name, err)
			}
			artifact.BackedUp = true
			if err := syncDirectory(
				filepath.Dir(artifact.Target),
			); err != nil {
				return err
			}
			if err := writeJournal(journalPath, *journal); err != nil {
				return err
			}
		}
		if err := os.Link(
			barrierFile(journal.StageRoot, name), artifact.Target,
		); err != nil {
			return fmt.Errorf(
				"install %s no-replace barrier: %w", name, err,
			)
		}
		if err := syncDirectory(filepath.Dir(artifact.Target)); err != nil {
			return err
		}
	}
	return nil
}

func commitUpgradeTransaction(
	journalPath string,
	journal *transactionJournal,
) error {
	journal.Phase = "committing"
	if err := writeJournal(journalPath, *journal); err != nil {
		return rollbackAfterCommitError(journalPath, *journal, err)
	}
	for _, name := range transactionArtifactNames {
		artifact, err := journalArtifact(journal, name)
		if err != nil {
			return rollbackAfterCommitError(journalPath, *journal, err)
		}
		switch name {
		case "bin", "configs":
			exists, digest, inspectErr := pathDigest(artifact.Target)
			if inspectErr != nil {
				return rollbackAfterCommitError(
					journalPath, *journal, inspectErr,
				)
			}
			if !exists || digest != journal.GuardDigest {
				return rollbackAfterCommitError(
					journalPath, *journal,
					fmt.Errorf("%s activation barrier changed", name),
				)
			}
			if err := os.Remove(artifact.Target); err != nil {
				return rollbackAfterCommitError(
					journalPath, *journal, err,
				)
			}
		default:
			if artifact.OriginalExists {
				current, inspectErr := inspectPath(artifact.Target)
				if inspectErr != nil {
					return rollbackAfterCommitError(
						journalPath, *journal, inspectErr,
					)
				}
				if !current.Exists ||
					current.Digest != artifact.OriginalDigest {
					return rollbackAfterCommitError(
						journalPath, *journal,
						fmt.Errorf(
							"%s changed before activation",
							name,
						),
					)
				}
				if err := os.MkdirAll(
					filepath.Dir(artifact.Backup), 0o700,
				); err != nil {
					return rollbackAfterCommitError(
						journalPath, *journal, err,
					)
				}
				if err := durableRename(
					artifact.Target, artifact.Backup,
				); err != nil {
					return rollbackAfterCommitError(
						journalPath, *journal, err,
					)
				}
				artifact.BackedUp = true
				if err := writeJournal(
					journalPath, *journal,
				); err != nil {
					return rollbackAfterCommitError(
						journalPath, *journal, err,
					)
				}
			}
		}
		if err := durableRename(artifact.Staged, artifact.Target); err != nil {
			return rollbackAfterCommitError(journalPath, *journal, err)
		}
		artifact.Installed = true
		if err := syncDirectory(filepath.Dir(artifact.Target)); err != nil {
			return rollbackAfterCommitError(journalPath, *journal, err)
		}
		if err := writeJournal(journalPath, *journal); err != nil {
			return rollbackAfterCommitError(journalPath, *journal, err)
		}
	}
	if err := verifyTransactionState(*journal, true); err != nil {
		return rollbackAfterCommitError(journalPath, *journal, err)
	}
	journal.Phase = "all_installed"
	return writeJournal(journalPath, *journal)
}

func rollbackAfterCommitError(
	journalPath string,
	journal transactionJournal,
	cause error,
) error {
	if rollbackErr := rollbackUpgradeTransaction(
		journalPath, journal,
	); rollbackErr != nil {
		return fmt.Errorf(
			"activation failed: %v; rollback incomplete and Runtime requires activation recovery: %w",
			cause, rollbackErr,
		)
	}
	return fmt.Errorf("activation failed and was rolled back: %w", cause)
}

func recoverUpgradeTransaction(target, journalPath string) error {
	journal, err := readJournal(journalPath)
	activeGuard := filepath.Join(
		target, "state", activationGuardName,
	)
	if errors.Is(err, os.ErrNotExist) {
		if exists, _, guardErr := pathDigest(activeGuard); guardErr != nil {
			return guardErr
		} else if exists {
			return fmt.Errorf(
				"activation guard exists without a recovery journal: %s",
				activeGuard,
			)
		}
		for _, name := range []string{"bin", "configs"} {
			path := filepath.Join(target, name)
			if info, statErr := os.Lstat(path); statErr == nil &&
				info.Mode().IsRegular() {
				return fmt.Errorf(
					"activation barrier exists without a recovery journal: %s",
					path,
				)
			} else if statErr != nil &&
				!errors.Is(statErr, os.ErrNotExist) {
				return statErr
			}
		}
		return nil
	}
	if err != nil {
		return err
	}
	if journal.TargetHome != target {
		return fmt.Errorf("activation journal target is invalid")
	}
	if token, tokenErr := processStartToken(
		journal.OwnerPID,
	); tokenErr == nil && token == journal.OwnerStartToken {
		return fmt.Errorf(
			"activation journal owner pid=%d is still running",
			journal.OwnerPID,
		)
	} else if tokenErr != nil &&
		!errors.Is(tokenErr, os.ErrNotExist) {
		return fmt.Errorf(
			"revalidate activation journal owner: %w", tokenErr,
		)
	}
	guardExists, guardDigest, guardErr := pathDigest(activeGuard)
	if guardErr != nil {
		return guardErr
	}
	if guardExists && guardDigest != journal.GuardDigest {
		return fmt.Errorf("activation guard identity does not match journal")
	}
	switch journal.Phase {
	case "all_installed":
		if !guardExists {
			return fmt.Errorf(
				"activation guard is missing before commit finalization",
			)
		}
		if err := verifyTransactionState(journal, true); err != nil {
			return err
		}
		journal.Phase = "committed"
		if err := writeJournal(journalPath, journal); err != nil {
			return err
		}
		return finalizeUpgradeTransaction(
			journalPath, journal, activeGuard, nil,
		)
	case "committed":
		return finalizeUpgradeTransaction(
			journalPath, journal, activeGuard, nil,
		)
	case "rolled_back":
		return finalizeRollbackTransaction(
			journalPath, journal, activeGuard,
		)
	default:
		if !guardExists && journal.Phase != "prepared" {
			return fmt.Errorf(
				"activation guard is missing for partial transaction",
			)
		}
		return rollbackUpgradeTransaction(journalPath, journal)
	}
}

func rollbackUpgradeTransaction(
	journalPath string,
	journal transactionJournal,
) error {
	activeGuard := filepath.Join(
		journal.TargetHome, "state", activationGuardName,
	)
	mutated, err := transactionMutated(journal)
	if err != nil {
		return err
	}
	if mutated {
		if err := ensureRollbackBinBarrier(journal); err != nil {
			return err
		}
	}
	for _, name := range []string{
		"configs", "runtime.json", "resources",
	} {
		artifact, err := journalArtifact(&journal, name)
		if err != nil {
			return err
		}
		if err := restoreArtifact(
			journal, *artifact, name == "configs",
		); err != nil {
			return err
		}
	}
	bin, err := journalArtifact(&journal, "bin")
	if err != nil {
		return err
	}
	if err := restoreArtifact(journal, *bin, true); err != nil {
		return err
	}
	if err := verifyTransactionState(journal, false); err != nil {
		return err
	}
	journal.Phase = "rolled_back"
	if err := writeJournal(journalPath, journal); err != nil {
		return err
	}
	return finalizeRollbackTransaction(journalPath, journal, activeGuard)
}

func finalizeRollbackTransaction(
	journalPath string,
	journal transactionJournal,
	activeGuard string,
) error {
	if err := verifyTransactionState(journal, false); err != nil {
		return err
	}
	if err := os.RemoveAll(journal.StageRoot); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(journal.StageRoot)); err != nil {
		return err
	}
	if err := removeGuardByDigest(
		activeGuard, journal.GuardDigest, true,
	); err != nil {
		return err
	}
	if err := os.Remove(journalPath); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(journalPath))
}

func finalizeUpgradeTransaction(
	journalPath string,
	journal transactionJournal,
	activeGuard string,
	_ []byte,
) error {
	if err := verifyTransactionState(journal, true); err != nil {
		return err
	}
	if journal.ResetRuntimeState {
		if err := resetRuntimeState(journal.TargetHome); err != nil {
			return fmt.Errorf(
				"reset Runtime state after local source activation: %w", err,
			)
		}
	}
	if err := os.RemoveAll(journal.StageRoot); err != nil {
		return err
	}
	if err := syncDirectory(filepath.Dir(journal.StageRoot)); err != nil {
		return err
	}
	if err := removeGuardByDigest(
		activeGuard, journal.GuardDigest, true,
	); err != nil {
		return err
	}
	if err := os.Remove(journalPath); err != nil &&
		!errors.Is(err, os.ErrNotExist) {
		return err
	}
	return syncDirectory(filepath.Dir(journalPath))
}

type pathState struct {
	Exists bool
	Digest string
	Mode   fs.FileMode
}

func inspectPath(path string) (pathState, error) {
	info, err := os.Lstat(path)
	if errors.Is(err, os.ErrNotExist) {
		return pathState{}, nil
	}
	if err != nil {
		return pathState{}, err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return pathState{}, fmt.Errorf(
			"transaction path must not be a symlink: %s", path,
		)
	}
	digest, err := treeDigest(path)
	if err != nil {
		return pathState{}, err
	}
	return pathState{
		Exists: true, Digest: digest, Mode: info.Mode(),
	}, nil
}

func pathDigest(path string) (bool, string, error) {
	state, err := inspectPath(path)
	return state.Exists, state.Digest, err
}

func journalArtifact(
	journal *transactionJournal,
	name string,
) (*transactionArtifact, error) {
	for index := range journal.Artifacts {
		if journal.Artifacts[index].Name == name {
			return &journal.Artifacts[index], nil
		}
	}
	return nil, fmt.Errorf("activation journal has no %s artifact", name)
}

func transactionMutated(journal transactionJournal) (bool, error) {
	for _, artifact := range journal.Artifacts {
		backup, err := inspectPath(artifact.Backup)
		if err != nil {
			return false, err
		}
		if backup.Exists {
			return true, nil
		}
		target, err := inspectPath(artifact.Target)
		if err != nil {
			return false, err
		}
		if target.Exists &&
			(target.Digest == artifact.NewDigest ||
				target.Digest == journal.GuardDigest) &&
			(!artifact.OriginalExists ||
				target.Digest != artifact.OriginalDigest) {
			return true, nil
		}
		if !target.Exists && artifact.OriginalExists {
			return true, nil
		}
	}
	return false, nil
}

func ensureRollbackBinBarrier(journal transactionJournal) error {
	bin, err := journalArtifact(&journal, "bin")
	if err != nil {
		return err
	}
	source := barrierFile(journal.StageRoot, "bin")
	sourceState, err := inspectPath(source)
	if err != nil {
		return err
	}
	if !sourceState.Exists {
		guardPath := filepath.Join(
			journal.TargetHome, "state", activationGuardName,
		)
		guard, readErr := readRegular(guardPath, 64<<10)
		if readErr != nil {
			return fmt.Errorf("restore rollback barrier source: %w", readErr)
		}
		if err := writeActivationGuard(source, guard); err != nil {
			return err
		}
		sourceState, err = inspectPath(source)
		if err != nil {
			return err
		}
	}
	if sourceState.Digest != journal.GuardDigest {
		return fmt.Errorf("rollback barrier identity changed")
	}
	target, err := inspectPath(bin.Target)
	if err != nil {
		return err
	}
	backup, err := inspectPath(bin.Backup)
	if err != nil {
		return err
	}
	if target.Exists && target.Digest == journal.GuardDigest {
		return nil
	}
	switch {
	case target.Exists && target.Digest == bin.NewDigest:
		if err := removeTransactionPath(
			bin.Target, journal.TargetHome,
		); err != nil {
			return err
		}
	case target.Exists && bin.OriginalExists &&
		target.Digest == bin.OriginalDigest && !backup.Exists:
		if err := os.MkdirAll(filepath.Dir(bin.Backup), 0o700); err != nil {
			return err
		}
		if err := durableRename(bin.Target, bin.Backup); err != nil {
			return err
		}
	case target.Exists && backup.Exists && target.Mode.IsDir():
		// Another process can win the tiny rename gap by recreating bin.
		// Only an empty directory is safe to discard; os.Remove fails closed
		// when it contains anything.
		if err := os.Remove(bin.Target); err != nil {
			return fmt.Errorf(
				"cannot clear concurrently recreated bin: %w", err,
			)
		}
	case target.Exists:
		return fmt.Errorf(
			"cannot establish rollback barrier because bin changed",
		)
	}
	if err := os.Link(source, bin.Target); err != nil {
		return fmt.Errorf("restore no-replace bin barrier: %w", err)
	}
	return syncDirectory(filepath.Dir(bin.Target))
}

func restoreArtifact(
	journal transactionJournal,
	artifact transactionArtifact,
	allowBarrier bool,
) error {
	backup, err := inspectPath(artifact.Backup)
	if err != nil {
		return err
	}
	target, err := inspectPath(artifact.Target)
	if err != nil {
		return err
	}
	if backup.Exists {
		if !artifact.OriginalExists ||
			backup.Digest != artifact.OriginalDigest {
			return fmt.Errorf(
				"activation backup for %s changed", artifact.Name,
			)
		}
		if target.Exists {
			allowed := target.Digest == artifact.NewDigest ||
				(allowBarrier &&
					target.Digest == journal.GuardDigest)
			if !allowed {
				if allowBarrier && target.Mode.IsDir() {
					if err := os.Remove(artifact.Target); err == nil {
						target.Exists = false
					} else {
						return fmt.Errorf(
							"cannot clear concurrently recreated %s: %w",
							artifact.Name, err,
						)
					}
				} else {
					return fmt.Errorf(
						"activated target %s changed", artifact.Name,
					)
				}
			}
			if target.Exists {
				if target.Mode.IsRegular() {
					if err := os.Remove(artifact.Target); err != nil {
						return err
					}
				} else if err := removeTransactionPath(
					artifact.Target, journal.TargetHome,
				); err != nil {
					return err
				}
			}
		}
		if err := durableRename(artifact.Backup, artifact.Target); err != nil {
			return err
		}
	} else if artifact.OriginalExists {
		if !target.Exists || target.Digest != artifact.OriginalDigest {
			return fmt.Errorf(
				"cannot prove original %s without its backup", artifact.Name,
			)
		}
	} else if target.Exists {
		allowed := target.Digest == artifact.NewDigest ||
			(allowBarrier && target.Digest == journal.GuardDigest)
		if !allowed {
			return fmt.Errorf(
				"cannot remove unknown target %s during rollback",
				artifact.Name,
			)
		}
		if target.Mode.IsRegular() {
			if err := os.Remove(artifact.Target); err != nil {
				return err
			}
		} else if err := removeTransactionPath(
			artifact.Target, journal.TargetHome,
		); err != nil {
			return err
		}
	}
	return syncDirectory(filepath.Dir(artifact.Target))
}

func verifyTransactionState(
	journal transactionJournal,
	wantNew bool,
) error {
	for _, artifact := range journal.Artifacts {
		target, err := inspectPath(artifact.Target)
		if err != nil {
			return err
		}
		if wantNew {
			if !target.Exists || target.Digest != artifact.NewDigest {
				return fmt.Errorf(
					"activated target %s does not match the staged release",
					artifact.Name,
				)
			}
			continue
		}
		if artifact.OriginalExists {
			if !target.Exists || target.Digest != artifact.OriginalDigest {
				return fmt.Errorf(
					"rolled back target %s does not match its original",
					artifact.Name,
				)
			}
		} else if target.Exists {
			return fmt.Errorf(
				"rolled back target %s should not exist", artifact.Name,
			)
		}
		backup, err := inspectPath(artifact.Backup)
		if err != nil {
			return err
		}
		if backup.Exists {
			return fmt.Errorf(
				"rollback left a backup for %s", artifact.Name,
			)
		}
	}
	return nil
}

func removeGuardByDigest(
	path, expectedDigest string,
	allowMissing bool,
) error {
	state, err := inspectPath(path)
	if err != nil {
		return err
	}
	if !state.Exists {
		if allowMissing {
			return nil
		}
		return os.ErrNotExist
	}
	if !state.Mode.IsRegular() || state.Digest != expectedDigest {
		return fmt.Errorf("activation guard identity changed")
	}
	if err := os.Remove(path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func writeJournal(path string, value transactionJournal) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	return atomicWriteFile(path, append(data, '\n'), 0o600)
}

func readJournal(path string) (transactionJournal, error) {
	var value transactionJournal
	data, err := readRegular(path, 1<<20)
	if err != nil {
		return value, err
	}
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, err
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return value, fmt.Errorf("activation journal has trailing JSON")
	}
	if value.SchemaVersion != journalSchema || value.Nonce == "" ||
		value.OwnerPID <= 0 || value.OwnerStartToken == "" ||
		!validDigest(value.GuardDigest) ||
		value.TargetHome == "" || value.StageRoot == "" {
		return value, fmt.Errorf("activation journal is incomplete")
	}
	switch value.Phase {
	case "prepared", "barriered", "committing",
		"all_installed", "committed", "rolled_back":
	default:
		return value, fmt.Errorf(
			"activation journal has unknown phase %q", value.Phase,
		)
	}
	canonicalTarget, err := layout.CanonicalHome(value.TargetHome)
	if err != nil || canonicalTarget != value.TargetHome ||
		!validActivationStageRoot(value.TargetHome, value.StageRoot) {
		return value, fmt.Errorf("activation journal target is invalid")
	}
	if err := validateActivationStageShape(value); err != nil {
		return value, err
	}
	if len(value.Artifacts) != len(transactionArtifactNames) {
		return value, fmt.Errorf("activation journal artifact set is incomplete")
	}
	for index, artifact := range value.Artifacts {
		name := transactionArtifactNames[index]
		expectedTarget := filepath.Join(value.TargetHome, name)
		expectedStaged := filepath.Join(value.StageRoot, "desired", name)
		expectedBackup := filepath.Join(
			value.StageRoot, "backup",
			fmt.Sprintf("%02d-%s", index, filepath.Base(name)),
		)
		if artifact.Name != name ||
			artifact.Target != expectedTarget ||
			artifact.Staged != expectedStaged ||
			artifact.Backup != expectedBackup ||
			!validDigest(artifact.NewDigest) ||
			(artifact.OriginalExists &&
				!validDigest(artifact.OriginalDigest)) ||
			(!artifact.OriginalExists &&
				artifact.OriginalDigest != "") ||
			artifact.BackedUp && !artifact.OriginalExists {
			return value, fmt.Errorf("activation journal contains unsafe paths")
		}
	}
	return value, nil
}

func validActivationStageRoot(target, stageRoot string) bool {
	if !filepath.IsAbs(stageRoot) ||
		stageRoot != filepath.Clean(stageRoot) ||
		filepath.Dir(stageRoot) != filepath.Join(target, "tmp") {
		return false
	}
	name := filepath.Base(stageRoot)
	suffix, exists := strings.CutPrefix(name, "activation-")
	if !exists || len(suffix) < 6 || len(suffix) > 64 {
		return false
	}
	for _, value := range suffix {
		if (value < 'a' || value > 'z') &&
			(value < 'A' || value > 'Z') &&
			(value < '0' || value > '9') {
			return false
		}
	}
	return true
}

func validateActivationStageShape(journal transactionJournal) error {
	info, err := os.Lstat(journal.StageRoot)
	if errors.Is(err, os.ErrNotExist) {
		if journal.Phase == "committed" || journal.Phase == "rolled_back" {
			return nil
		}
		return fmt.Errorf("activation stage is missing for phase %s", journal.Phase)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("activation stage must be a directory, not a symlink")
	}
	for _, name := range []string{"desired", "backup", "barriers"} {
		path := filepath.Join(journal.StageRoot, name)
		current, statErr := os.Lstat(path)
		if errors.Is(statErr, os.ErrNotExist) &&
			(journal.Phase == "committed" ||
				journal.Phase == "rolled_back") {
			continue
		}
		if statErr != nil {
			return fmt.Errorf("inspect activation stage %s: %w", name, statErr)
		}
		if current.Mode()&os.ModeSymlink != 0 || !current.IsDir() {
			return fmt.Errorf(
				"activation stage %s must be a directory, not a symlink", name,
			)
		}
	}
	return nil
}

func validDigest(value string) bool {
	if len(value) != sha256.Size*2 {
		return false
	}
	_, err := hex.DecodeString(value)
	return err == nil
}

func acquireUpgradeLock(path string) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(
		path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open activation lock %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("activation lock must be a regular file: %s", path)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) ||
			errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf(
				"another Runtime maintenance operation is in progress",
			)
		}
		return nil, err
	}
	return &fileLock{file: file}, nil
}

func writeActivationGuard(path string, value []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(
		path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600,
	)
	if err != nil {
		return fmt.Errorf("create activation guard: %w", err)
	}
	if _, err := file.Write(value); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func copyMissingNames(source, target string) ([]string, error) {
	sourceFiles, err := regularRelativeFiles(source)
	if err != nil {
		return nil, err
	}
	copied := make([]string, 0, len(sourceFiles))
	for _, relative := range sourceFiles {
		destination := filepath.Join(target, filepath.FromSlash(relative))
		if _, err := os.Lstat(destination); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		sourcePath := filepath.Join(source, filepath.FromSlash(relative))
		info, err := os.Stat(sourcePath)
		if err != nil {
			return nil, err
		}
		if err := copyRegular(
			sourcePath, destination, info.Mode().Perm(),
		); err != nil {
			return nil, err
		}
		copied = append(copied, relative)
	}
	return copied, nil
}

func copyTree(source, target string, overwrite bool) error {
	if overwrite {
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if err := os.MkdirAll(target, 0o700); err != nil {
			return err
		}
	}
	return filepath.WalkDir(
		source,
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(source, path)
			if err != nil || relative == "." {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("source tree contains symlink: %s", path)
			}
			destination := filepath.Join(target, relative)
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.MkdirAll(destination, 0o700)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("source tree contains unsupported file: %s", path)
			}
			if !overwrite {
				if _, err := os.Lstat(destination); err == nil {
					return nil
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			return copyRegular(path, destination, info.Mode().Perm())
		},
	)
}

func copyRegular(source, target string, mode fs.FileMode) error {
	info, err := os.Lstat(source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() {
		return fmt.Errorf("copy source must be a regular file: %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	fd, err := unix.Open(
		source, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return err
	}
	input := os.NewFile(uintptr(fd), source)
	if input == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("open copy source %s", source)
	}
	defer input.Close()
	openedInfo, err := input.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() ||
		!os.SameFile(info, openedInfo) {
		return fmt.Errorf("copy source changed while opening: %s", source)
	}
	output, err := os.OpenFile(
		target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode,
	)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = output.Close()
		if !ok {
			_ = os.Remove(target)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func validateTree(root string) error {
	return filepath.WalkDir(
		root,
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("payload contains symlink: %s", path)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return fmt.Errorf("payload contains unsupported file: %s", path)
			}
			return nil
		},
	)
}

func regularRelativeFiles(root string) ([]string, error) {
	var values []string
	err := filepath.WalkDir(
		root,
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(root, path)
			if err != nil || relative == "." {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("source tree contains symlink: %s", path)
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("source tree contains unsupported file: %s", path)
			}
			values = append(values, filepath.ToSlash(relative))
			return nil
		},
	)
	sort.Strings(values)
	return values, err
}

func treeDigest(path string) (string, error) {
	hash := sha256.New()
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("cannot digest symlink: %s", path)
	}
	if info.Mode().IsRegular() {
		if err := digestRegularFile(hash, ".", path, info); err != nil {
			return "", err
		}
		return hex.EncodeToString(hash.Sum(nil)), nil
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cannot digest unsupported path: %s", path)
	}
	err = filepath.WalkDir(
		path,
		func(entryPath string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(path, entryPath)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("cannot digest symlink: %s", entryPath)
			}
			relative = filepath.ToSlash(relative)
			if info.IsDir() {
				_, _ = fmt.Fprintf(
					hash, "dir\x00%s\x00%04o\x00",
					relative, info.Mode().Perm(),
				)
				return nil
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf(
					"cannot digest unsupported path: %s", entryPath,
				)
			}
			return digestRegularFile(
				hash, relative, entryPath, info,
			)
		},
	)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func digestRegularFile(
	hash io.Writer,
	relative, path string,
	expected os.FileInfo,
) error {
	fd, err := unix.Open(
		path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("open digest file %s", path)
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil {
		return err
	}
	if !actual.Mode().IsRegular() || !os.SameFile(expected, actual) {
		return fmt.Errorf("digest file changed while reading: %s", path)
	}
	if _, err := fmt.Fprintf(
		hash, "file\x00%s\x00%04o\x00", relative, actual.Mode().Perm(),
	); err != nil {
		return err
	}
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	_, err = io.WriteString(hash, "\x00")
	return err
}

func atomicWriteFile(path string, value []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".activation-write-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(value); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func readRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() > limit {
		return nil, fmt.Errorf("%s must be a bounded regular file", path)
	}
	fd, err := unix.Open(
		path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open regular file %s", path)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() ||
		!os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("%s changed while opening", path)
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(value)) > limit {
		return nil, fmt.Errorf("%s exceeds read limit", path)
	}
	return value, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s must be a directory, not a symlink", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must not be accessible by group or others", path)
	}
	return nil
}

func removeTransactionPath(path, root string) error {
	if !pathWithin(path, root) || filepath.Clean(path) == filepath.Clean(root) {
		return fmt.Errorf("refusing unsafe transaction removal: %s", path)
	}
	return os.RemoveAll(path)
}

func pathWithin(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != "" &&
		relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(relative)
}

func randomNonce() (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func durableRename(source, target string) error {
	return durableRenameWith(source, target, os.Rename, syncDirectory)
}

func durableRenameWith(
	source, target string,
	rename func(string, string) error,
	syncDir func(string) error,
) error {
	if err := rename(source, target); err != nil {
		return err
	}
	sourceDir := filepath.Dir(source)
	targetDir := filepath.Dir(target)
	if err := syncDir(targetDir); err != nil {
		return fmt.Errorf("persist rename target directory: %w", err)
	}
	if sourceDir != targetDir {
		if err := syncDir(sourceDir); err != nil {
			return fmt.Errorf("persist rename source directory: %w", err)
		}
	}
	return nil
}

func syncTree(root string) error {
	var directories []string
	err := filepath.WalkDir(
		root,
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("activation stage contains symlink: %s", path)
			}
			if entry.IsDir() {
				directories = append(directories, path)
			}
			return nil
		},
	)
	if err != nil {
		return err
	}
	sort.Slice(directories, func(left, right int) bool {
		return len(directories[left]) > len(directories[right])
	})
	for _, directory := range directories {
		if err := syncDirectory(directory); err != nil {
			return err
		}
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
