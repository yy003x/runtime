package agent

import (
	"context"
	"encoding/json"
	"testing"

	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/provider"
)

type originTestDriver struct{}

func (originTestDriver) ExecutionIdentity() model.DriverExecutionIdentity {
	return model.DriverExecutionIdentity{
		Driver: model.DriverOpenAI, Implementation: "agent.origin-test",
		ImplementationVersion: 1,
	}
}

func (originTestDriver) Validate(model.Profile) error { return nil }

func (originTestDriver) Stream(
	_ context.Context,
	_ model.ResolvedModel,
	_ contract.ModelRequest,
	sink contract.EventSink,
) (contract.ModelResult, provider.Attempt, *contract.RuntimeError) {
	if err := sink(contract.Event{Sequence: 1, Type: contract.EventModelStarted}); err != nil {
		return contract.ModelResult{}, provider.Attempt{}, &contract.RuntimeError{
			Code: contract.ErrorCancelled, Phase: contract.PhaseConsumer,
			Message: err.Error(),
		}
	}
	return contract.ModelResult{
			Message:      contract.Message{Role: contract.RoleAssistant, Content: "done"},
			FinishReason: contract.FinishStop,
		}, provider.Attempt{
			Started: true,
			Request: provider.Request{Method: "POST", Body: json.RawMessage(`{}`)},
		}, nil
}

func TestKernelMarksEveryModelCallAsAgentAttempt(t *testing.T) {
	catalog, err := model.NewCatalog(map[string]model.Profile{
		"api": {
			Driver:   model.DriverOpenAI,
			Endpoint: "https://example.invalid/v1/chat/completions",
			Model:    "fixture", Timeout: "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	var observed []model.Attempt
	models, err := model.NewService(
		catalog,
		map[model.DriverName]model.Driver{model.DriverOpenAI: originTestDriver{}},
		model.ServiceOptions{AttemptObserver: func(attempt model.Attempt) {
			observed = append(observed, attempt)
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	tools, err := NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	runID := "run_11111111111111111111111111111111"
	kernel := &Kernel{
		Model: models, Tools: tools, Effects: NewMemoryEffects(),
	}
	_, outcome, runtimeErr := kernel.Run(context.Background(), LoopState{
		SchemaVersion: LoopStateSchemaVersion,
		RunID:         runID, ModelProfile: "api",
		Messages:         []contract.Message{{Role: contract.RoleUser, Content: "start"}},
		BaseMessageCount: 1,
	}, nil)
	if runtimeErr != nil || outcome.State != StateCompleted {
		t.Fatalf("outcome=%#v error=%#v", outcome, runtimeErr)
	}
	if len(observed) != 1 || observed[0].Origin.Namespace != model.AttemptNamespaceAgent ||
		observed[0].Origin.Source != "agent "+runID {
		t.Fatalf("observed=%#v", observed)
	}
}
