package activation

import (
	"path/filepath"
	"testing"

	"github.com/yy003x/runtime/internal/testkit/reporoot"
)

func TestReleaseManifestMatchesCurrentRuntimeCompatibility(t *testing.T) {
	manifest, _, err := LoadManifest(filepath.Join(reporoot.Root(t), "release"))
	if err != nil {
		t.Fatal(err)
	}
	if err := ValidateManifestCompatibility(manifest); err != nil {
		t.Fatal(err)
	}
}

func TestValidateManifestCompatibilityRejectsEveryDrift(t *testing.T) {
	current := CurrentManifest()
	tests := map[string]Manifest{
		"manifest schema":  current,
		"activation epoch": current,
		"contract":         current,
		"Session schema":   current,
		"Run schema":       current,
	}
	value := tests["manifest schema"]
	value.SchemaVersion++
	tests["manifest schema"] = value
	value = tests["activation epoch"]
	value.ActivationEpoch++
	tests["activation epoch"] = value
	value = tests["contract"]
	value.ContractVersion++
	tests["contract"] = value
	value = tests["Session schema"]
	value.SessionSchemaVersion++
	tests["Session schema"] = value
	value = tests["Run schema"]
	value.RunSchemaVersion++
	tests["Run schema"] = value

	for name, manifest := range tests {
		t.Run(name, func(t *testing.T) {
			if err := ValidateManifestCompatibility(manifest); err == nil {
				t.Fatal("compatibility drift was accepted")
			}
		})
	}
}
