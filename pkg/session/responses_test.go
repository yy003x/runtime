package session

import (
	"context"
	"encoding/json"
	"strings"
	"testing"

	"github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
	"github.com/yy003x/runtime/pkg/profile"
)

func TestResponsesSessionRejectedBeforeCreatingFacts(t *testing.T) {
	for _, value := range []model.Profile{
		{Driver: model.DriverOpenAIResponses, BaseURL: "https://example.invalid/v1", Model: "fixture", Timeout: "1m"},
		{Driver: model.DriverOpenAICodexResponses, Endpoint: model.OpenAICodexEndpoint, Model: "fixture", Timeout: "1m"},
	} {
		t.Run(string(value.Driver), func(t *testing.T) { assertResponsesSessionRejected(t, value) })
	}
}

func assertResponsesSessionRejected(t *testing.T, value model.Profile) {
	t.Helper()
	generator := &scriptedGenerator{}
	service := newTestService(t, generator, nil, nil)
	commands, err := command.NewCatalog(map[string]command.Profile{})
	if err != nil {
		t.Fatal(err)
	}
	models, err := model.NewCatalog(map[string]model.Profile{"api": value})
	if err != nil {
		t.Fatal(err)
	}
	service.profiles, err = profile.NewCatalog(commands, models)
	if err != nil {
		t.Fatal(err)
	}
	request := RunRequest{ProfileID: "api", Input: "hello"}
	_, prepareErr := service.PrepareRunRequest(request)
	_, runErr := service.Run(context.Background(), request)
	request.SessionID = "session_77777777777777777777777777777777"
	request.RunID = "run_77777777777777777777777777777777"
	_, agentErr := service.PrepareAgent(request)
	for _, runtimeErr := range []*contract.RuntimeError{prepareErr, runErr, agentErr} {
		if runtimeErr == nil || runtimeErr.Code != contract.ErrorInvalidRequest || !strings.Contains(runtimeErr.Message, "single calls only") {
			t.Fatalf("error=%v", runtimeErr)
		}
	}
	values, err := service.List(ListFilter{})
	if err != nil || len(values) != 0 || len(generator.requests) != 0 {
		t.Fatalf("sessions=%v requests=%d err=%v", values, len(generator.requests), err)
	}
}

func TestAPIConfigDigestIncludesResolvedEndpoint(t *testing.T) {
	modelProfile := model.Profile{Driver: model.DriverOpenAIChatCompletions, BaseURL: "https://example.invalid/v1", Model: "fixture", Timeout: "1m"}
	entry := profile.Entry{ID: "api", Kind: profile.KindModel, Model: &modelProfile}
	actual, err := profileConfigDigest(entry)
	if err != nil {
		t.Fatal(err)
	}
	legacy, err := json.Marshal(modelProfile)
	if err != nil {
		t.Fatal(err)
	}
	// Assert against the exact current snapshot representation and the previous
	// plain-Profile digest used by already-queued Session API requests.
	expected, err := json.Marshal(struct {
		*model.Profile
		ResolvedEndpoint string `json:"resolved_endpoint"`
	}{&modelProfile, "https://example.invalid/v1/chat/completions"})
	if err != nil {
		t.Fatal(err)
	}
	if actual != digest(expected) || actual == digest(legacy) {
		t.Fatalf("API digest did not freeze resolved URL: %s", actual)
	}
}
