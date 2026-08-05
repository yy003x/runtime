package session

import (
	"context"
	"encoding/json"
	"path/filepath"
	"testing"

	"github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/contract"
	"github.com/yy003x/runtime/model"
	"github.com/yy003x/runtime/profile"
	"github.com/yy003x/runtime/provider"
)

type originTestDriver struct{}

func (originTestDriver) ExecutionIdentity() model.DriverExecutionIdentity {
	return model.DriverExecutionIdentity{
		Driver: model.DriverOpenAI, Implementation: "session.origin-test",
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

func TestSessionModelRunMarksProviderAttemptOrigin(t *testing.T) {
	modelCatalog, err := model.NewCatalog(map[string]model.Profile{
		"api": {
			Driver:   model.DriverOpenAI,
			Endpoint: "https://example.invalid/v1/chat/completions",
			Model:    "fixture", Timeout: "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	commandCatalog, err := command.NewCatalog(nil)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewCatalog(commandCatalog, modelCatalog)
	if err != nil {
		t.Fatal(err)
	}
	var observed []model.Attempt
	models, err := model.NewService(
		modelCatalog,
		map[model.DriverName]model.Driver{model.DriverOpenAI: originTestDriver{}},
		model.ServiceOptions{AttemptObserver: func(attempt model.Attempt) {
			observed = append(observed, attempt)
		}},
	)
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := NewStore(filepath.Join(root, "sessions"), filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	service, err := NewService(ServiceOptions{
		Store: store, Profiles: profiles, Models: models,
	})
	if err != nil {
		t.Fatal(err)
	}
	result, runtimeErr := service.Run(context.Background(), RunRequest{
		ProfileID: "api", Input: "hello",
	})
	if runtimeErr != nil || result.State != TurnCompleted {
		t.Fatalf("result=%#v error=%#v", result, runtimeErr)
	}
	if len(observed) != 1 ||
		observed[0].Origin.Namespace != model.AttemptNamespaceSession ||
		observed[0].Origin.Source != "session "+result.SessionID {
		t.Fatalf("observed=%#v result=%#v", observed, result)
	}
}
