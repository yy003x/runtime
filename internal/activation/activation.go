// Package activation owns the contract-v3 staged-binary activation gate.
package activation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// RequireNoGuard blocks public Runtime operations while an activation
// transaction is live or still requires recovery. The journal is also a
// barrier so a crash between terminal-state persistence and guard cleanup
// cannot reopen Runtime state prematurely.
func RequireNoGuard(stateDir string) error {
	for _, name := range []string{activationGuardName, journalName} {
		path := filepath.Join(stateDir, name)
		info, err := os.Lstat(path)
		if errors.Is(err, os.ErrNotExist) {
			continue
		}
		if err != nil {
			return err
		}
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf(
				"Runtime activation barrier is invalid at %s", path,
			)
		}
		return fmt.Errorf(
			"Runtime home is undergoing activation or requires activation recovery",
		)
	}
	return nil
}

type Manifest struct {
	SchemaVersion        int    `json:"schema_version"`
	ActivationEpoch      int    `json:"activation_epoch"`
	ContractVersion      int    `json:"contract_version"`
	SessionSchemaVersion int    `json:"session_schema_version"`
	RunSchemaVersion     int    `json:"run_schema_version"`
	MinimumUpdaterEpoch  int    `json:"minimum_updater_epoch"`
	LegacySelfUpdate     string `json:"legacy_self_update"`
}

func LoadManifest(resourcesDir string) (Manifest, []byte, error) {
	path := filepath.Join(resourcesDir, "release.json")
	data, err := readRegular(path, 64<<10)
	if err != nil {
		return Manifest{}, nil, fmt.Errorf(
			"read release manifest as a no-follow regular file: %w", err,
		)
	}
	var manifest Manifest
	decoder := json.NewDecoder(strings.NewReader(string(data)))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&manifest); err != nil {
		return Manifest{}, nil, fmt.Errorf("decode release manifest: %w", err)
	}
	var trailing any
	if err := decoder.Decode(&trailing); !errors.Is(err, io.EOF) {
		return Manifest{}, nil, fmt.Errorf("release manifest has trailing JSON")
	}
	if manifest.SchemaVersion != 1 || manifest.ActivationEpoch < 1 {
		return Manifest{}, nil, fmt.Errorf("unsupported release activation manifest")
	}
	return manifest, data, nil
}

// RequireLegacyProfileListGate rejects the exact validation command used by
// the v0.1.1 updater when a contract-v3 candidate is staged outside its target
// Runtime home.
func RequireLegacyProfileListGate(
	home, binaryPath, _ string,
) error {
	staged, err := isStaged(home, binaryPath)
	if err != nil {
		return err
	}
	if !staged {
		return nil
	}
	manifest, _, err := LoadManifest(
		filepath.Join(filepath.Dir(binaryPath), "resources"),
	)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	if manifest.ActivationEpoch < 2 {
		return nil
	}
	return fmt.Errorf(
		"contract-v3 candidate cannot be activated by the legacy updater; use the current install.sh",
	)
}

func isStaged(home, binaryPath string) (bool, error) {
	expected := filepath.Join(filepath.Clean(home), "bin", "sn-cli")
	current, err := filepath.Abs(binaryPath)
	if err != nil {
		return false, err
	}
	expected, err = filepath.Abs(expected)
	if err != nil {
		return false, err
	}
	currentInfo, err := os.Stat(current)
	if err != nil {
		return false, err
	}
	expectedInfo, err := os.Stat(expected)
	if errors.Is(err, os.ErrNotExist) {
		return true, nil
	}
	if err != nil {
		return false, err
	}
	return !os.SameFile(currentInfo, expectedInfo), nil
}
