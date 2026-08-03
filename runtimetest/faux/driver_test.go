package faux

import (
	"testing"

	"github.com/yy003x/runtime/model"
)

func TestDriverExecutionIdentity(t *testing.T) {
	identity := (&Driver{}).ExecutionIdentity()
	if identity.Driver != model.DriverOpenAI ||
		identity.Implementation != executionImplementation ||
		identity.ImplementationVersion != executionImplementationVersion {
		t.Fatalf("identity=%#v", identity)
	}
	if err := identity.Validate(); err != nil {
		t.Fatal(err)
	}
}
