// Package activation owns the staged-binary activation gate for the current
// Runtime contract.
package activation

import (
	"bytes"
	"fmt"
	"path/filepath"

	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
)

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
