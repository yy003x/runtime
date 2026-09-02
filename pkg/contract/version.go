package contract

// MachineEnvelopeSchemaVersion is the schema of CLI and HTTP machine-output
// envelopes. RuntimeContractVersion is the public Runtime API/CLI contract
// compatibility level. They are defined with the provider-neutral contract so
// every public interface and release gate reads the same canonical values.
const (
	MachineEnvelopeSchemaVersion = 1
	RuntimeContractVersion       = 8
)
