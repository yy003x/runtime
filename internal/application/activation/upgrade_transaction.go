package activation

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"strings"

	"github.com/yy003x/runtime/internal/infrastructure/layout"
)

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

var transactionArtifactNames = []string{
	"resources", "runtime.json", "bin", "configs", "tools",
}

var transactionBarrierNames = []string{"bin", "configs", "tools"}

func barrierFile(stageRoot, name string) string {
	return filepath.Join(stageRoot, "barriers", name)
}

func prepareBarrierFiles(stageRoot string, guard []byte) error {
	for _, name := range transactionBarrierNames {
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
	for _, name := range transactionBarrierNames {
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
		case "bin", "configs", "tools":
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
		for _, name := range transactionBarrierNames {
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
		"tools", "configs", "runtime.json", "resources",
	} {
		artifact, err := journalArtifact(&journal, name)
		if err != nil {
			return err
		}
		if err := restoreArtifact(
			journal, *artifact, name == "configs" || name == "tools",
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
