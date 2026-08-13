package activation

import (
	"fmt"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/session"
	"github.com/yy003x/runtime/pkg/store/sqlite"
)

// ActivationEpoch is bumped only when activation transaction semantics or
// payload mapping changes incompatibly. The remaining values are owned by the
// public contract and canonical Stores and are referenced here, not repeated.
const ActivationEpoch = 4

func CurrentManifest() Manifest {
	return Manifest{
		SchemaVersion:        1,
		ActivationEpoch:      ActivationEpoch,
		ContractVersion:      contract.RuntimeContractVersion,
		SessionSchemaVersion: session.SchemaVersion,
		RunSchemaVersion:     sqlite.SchemaVersion,
	}
}

// ValidateManifestCompatibility checks a release manifest against the exact
// compatibility contract implemented by this binary.
func ValidateManifestCompatibility(manifest Manifest) error {
	want := CurrentManifest()
	if manifest != want {
		return fmt.Errorf(
			"payload activation contract is incompatible: "+
				"epoch=%d contract=%d session_schema=%d run_schema=%d; "+
				"expected epoch=%d contract=%d session_schema=%d run_schema=%d",
			manifest.ActivationEpoch, manifest.ContractVersion,
			manifest.SessionSchemaVersion, manifest.RunSchemaVersion,
			want.ActivationEpoch, want.ContractVersion,
			want.SessionSchemaVersion, want.RunSchemaVersion,
		)
	}
	return nil
}
