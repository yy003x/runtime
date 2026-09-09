package agent

import (
	"context"
	"errors"
	"strings"
	"testing"

	"github.com/yy003x/runtime/pkg/contract"
)

type singleCallModel struct {
	kernelTerminalModel
	checks int
}

func (generator *singleCallModel) ValidateManagedExecution(string) error {
	generator.checks++
	return errors.New("single calls only")
}

func TestKernelManagedCapabilityGateBeforeAnyEffects(t *testing.T) {
	for _, resume := range []bool{false, true} {
		generator := &singleCallModel{}
		registry, err := NewRegistry()
		if err != nil {
			t.Fatal(err)
		}
		effects := &countingEffects{}
		kernel := Kernel{Model: generator, Tools: registry, Effects: effects}
		state := LoopState{
			SchemaVersion: LoopStateSchemaVersion, RunID: "run_00000000000000000000000000000001",
			ModelProfile: "api", BaseMessageCount: 1, Messages: []contract.Message{{Role: contract.RoleUser, Content: "hello"}},
		}
		events := 0
		sink := func(contract.Event) error { events++; return nil }
		var runtimeErr *contract.RuntimeError
		if resume {
			_, _, runtimeErr = kernel.Resume(context.Background(), state, ResumeInput{}, sink)
		} else {
			_, _, runtimeErr = kernel.Run(context.Background(), state, sink)
		}
		if runtimeErr == nil || runtimeErr.Code != contract.ErrorInvalidRequest || !strings.Contains(runtimeErr.Message, "single calls only") || generator.checks != 1 || generator.requests != 0 || events != 0 || effects.prepared != 0 || effects.started != 0 {
			t.Fatalf("resume=%t error=%v model=%#v effects=%#v events=%d", resume, runtimeErr, generator, effects, events)
		}
	}
}
