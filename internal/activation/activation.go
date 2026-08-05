// Package activation owns the contract-v4 staged-binary activation gate.
package activation

import (
	"bytes"
	"errors"
	"fmt"
	"os"
	"path/filepath"

	"github.com/yy003x/runtime/internal/strictjson"
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
	SchemaVersion        int `json:"schema_version"`
	ActivationEpoch      int `json:"activation_epoch"`
	ContractVersion      int `json:"contract_version"`
	SessionSchemaVersion int `json:"session_schema_version"`
	RunSchemaVersion     int `json:"run_schema_version"`
}

func LoadManifest(manifestDir string) (Manifest, []byte, error) {
	path := filepath.Join(manifestDir, "release.json")
	data, err := readRegular(path, 64<<10)
	if err != nil {
		return Manifest{}, nil, fmt.Errorf(
			"read release manifest as a no-follow regular file: %w", err,
		)
	}
	var manifest Manifest
	if err := strictjson.Decode(
		bytes.NewReader(data), 64<<10, &manifest,
	); err != nil {
		return Manifest{}, nil, fmt.Errorf("decode release manifest: %w", err)
	}
	if manifest.SchemaVersion != 1 || manifest.ActivationEpoch < 1 {
		return Manifest{}, nil, fmt.Errorf("unsupported release activation manifest")
	}
	return manifest, data, nil
}
