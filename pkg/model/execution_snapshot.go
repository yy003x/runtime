package model

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"

	"github.com/yy003x/runtime/internal/domain/profileid"
)

const ExecutionSnapshotSchemaVersion = 1

// ExecutionSnapshotter exposes the non-secret execution identity frozen by a
// durable caller. A Generator used for durable execution must implement this
// interface on the same concrete object so generation and drift checks cannot
// be routed through different model services.
type ExecutionSnapshotter interface {
	ExecutionSnapshot(string) (ExecutionSnapshot, error)
}

// DriverExecutionIdentity identifies the concrete provider adapter selected by
// a model Service. ImplementationVersion is a manually maintained semantic
// contract version, not a build, release, or source-control version.
type DriverExecutionIdentity struct {
	Driver                DriverName `json:"driver"`
	Implementation        string     `json:"implementation"`
	ImplementationVersion int        `json:"implementation_version"`
}

func (identity DriverExecutionIdentity) Validate() error {
	switch identity.Driver {
	case DriverOpenAI, DriverAnthropic:
	default:
		return fmt.Errorf(
			"driver execution identity has unsupported driver %q",
			identity.Driver,
		)
	}
	if identity.Implementation == "" ||
		identity.Implementation != strings.TrimSpace(identity.Implementation) ||
		len(identity.Implementation) > 256 ||
		!utf8.ValidString(identity.Implementation) ||
		strings.ContainsRune(identity.Implementation, '\x00') {
		return fmt.Errorf(
			"driver execution identity implementation is invalid",
		)
	}
	if identity.ImplementationVersion < 1 {
		return fmt.Errorf(
			"driver execution identity implementation_version must be positive",
		)
	}
	return nil
}

// ExecutionSnapshot is the canonical, non-secret description of the API
// Profile and concrete provider adapter selected by a model Service. Header
// ${VAR} references are preserved verbatim; no secret value is ever resolved
// into this snapshot.
type ExecutionSnapshot struct {
	SchemaVersion  int                     `json:"schema_version"`
	ProfileID      string                  `json:"profile_id"`
	Profile        Profile                 `json:"profile"`
	ProfileDigest  string                  `json:"profile_digest"`
	DriverIdentity DriverExecutionIdentity `json:"driver_identity"`
}

func (snapshot ExecutionSnapshot) Validate() error {
	_, err := canonicalExecutionSnapshot(snapshot)
	return err
}

// CanonicalJSON returns the stable representation used by durable execution
// snapshots and their digests.
func (snapshot ExecutionSnapshot) CanonicalJSON() ([]byte, error) {
	canonical, err := canonicalExecutionSnapshot(snapshot)
	if err != nil {
		return nil, err
	}
	return json.Marshal(canonical)
}

func (snapshot ExecutionSnapshot) Digest() (string, error) {
	canonical, err := snapshot.CanonicalJSON()
	if err != nil {
		return "", err
	}
	return digestExecutionSnapshotBytes(canonical), nil
}

func canonicalExecutionSnapshot(
	snapshot ExecutionSnapshot,
) (ExecutionSnapshot, error) {
	if snapshot.SchemaVersion != ExecutionSnapshotSchemaVersion {
		return ExecutionSnapshot{}, fmt.Errorf(
			"unsupported model execution snapshot schema_version %d",
			snapshot.SchemaVersion,
		)
	}
	if err := profileid.Validate(snapshot.ProfileID); err != nil {
		return ExecutionSnapshot{}, fmt.Errorf(
			"model execution snapshot profile_id: %w", err,
		)
	}
	if err := snapshot.Profile.Validate(); err != nil {
		return ExecutionSnapshot{}, fmt.Errorf(
			"model execution snapshot profile: %w", err,
		)
	}
	if err := snapshot.DriverIdentity.Validate(); err != nil {
		return ExecutionSnapshot{}, err
	}
	if snapshot.DriverIdentity.Driver != snapshot.Profile.Driver {
		return ExecutionSnapshot{}, fmt.Errorf(
			"model execution snapshot driver identity does not match Profile driver",
		)
	}
	profileJSON, err := json.Marshal(cloneProfile(snapshot.Profile))
	if err != nil {
		return ExecutionSnapshot{}, fmt.Errorf(
			"encode model execution snapshot Profile: %w", err,
		)
	}
	profileDigest := digestExecutionSnapshotBytes(profileJSON)
	if snapshot.ProfileDigest != profileDigest {
		return ExecutionSnapshot{}, fmt.Errorf(
			"model execution snapshot profile_digest does not match Profile",
		)
	}
	return ExecutionSnapshot{
		SchemaVersion:  ExecutionSnapshotSchemaVersion,
		ProfileID:      snapshot.ProfileID,
		Profile:        cloneProfile(snapshot.Profile),
		ProfileDigest:  profileDigest,
		DriverIdentity: snapshot.DriverIdentity,
	}, nil
}

func newExecutionSnapshot(
	profileID string,
	profile Profile,
	identity DriverExecutionIdentity,
) (ExecutionSnapshot, error) {
	profileJSON, err := json.Marshal(cloneProfile(profile))
	if err != nil {
		return ExecutionSnapshot{}, fmt.Errorf(
			"encode model execution snapshot Profile: %w", err,
		)
	}
	snapshot := ExecutionSnapshot{
		SchemaVersion:  ExecutionSnapshotSchemaVersion,
		ProfileID:      profileID,
		Profile:        cloneProfile(profile),
		ProfileDigest:  digestExecutionSnapshotBytes(profileJSON),
		DriverIdentity: identity,
	}
	return canonicalExecutionSnapshot(snapshot)
}

func digestExecutionSnapshotBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return "sha256:" + hex.EncodeToString(sum[:])
}
