package run_test

import (
	"context"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/runtime/pkg/agent"
	"github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/model"
	"github.com/yy003x/runtime/pkg/profile"
	runtime "github.com/yy003x/runtime/pkg/run"
	"github.com/yy003x/runtime/pkg/session"
	sqlitestore "github.com/yy003x/runtime/pkg/store/sqlite"
)

func TestResponsesManagedSubmissionRejectedBeforeRunCreation(t *testing.T) {
	commands, err := command.NewCatalog(map[string]command.Profile{})
	if err != nil {
		t.Fatal(err)
	}
	models, err := model.NewCatalog(map[string]model.Profile{"api": {
		Driver: model.DriverOpenAIResponses, BaseURL: "https://example.invalid/v1", Model: "fixture", Timeout: "1m",
	}})
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := profile.NewCatalog(commands, models)
	if err != nil {
		t.Fatal(err)
	}
	registry, err := agent.NewRegistry()
	if err != nil {
		t.Fatal(err)
	}
	root := t.TempDir()
	store, err := sqlitestore.Open(filepath.Join(root, "runtime.db"), sqlitestore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	sessionStore, err := session.NewStore(filepath.Join(root, "sessions"), filepath.Join(root, "state"))
	if err != nil {
		t.Fatal(err)
	}
	generator := &agentModel{}
	sessions, err := session.NewService(session.ServiceOptions{Store: sessionStore, Profiles: profiles, Models: generator})
	if err != nil {
		t.Fatal(err)
	}
	runs, err := runtime.NewService(runtime.ServiceOptions{Store: store, Executors: map[runtime.Kind]runtime.Executor{
		runtime.KindAgent:   &runtime.AgentExecutor{Profiles: profiles, Model: generator, Tools: registry, Store: store},
		runtime.KindSession: &runtime.SessionExecutor{Profiles: profiles, Sessions: sessions},
	}})
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { runs.Close() })
	for _, kind := range []runtime.Kind{runtime.KindAgent, runtime.KindSession} {
		request := runtime.Request{Kind: kind, ProfileID: "api", Input: "hello"}
		if kind == runtime.KindSession {
			request.SessionID = "session_77777777777777777777777777777777"
		}
		if _, runtimeErr := runs.Submit(context.Background(), request); runtimeErr == nil || runtimeErr.Code != contract.ErrorInvalidRequest || !strings.Contains(runtimeErr.Message, "single calls only") {
			t.Fatalf("kind=%s err=%v", kind, runtimeErr)
		}
	}
	values, err := runs.List(context.Background(), runtime.ListFilter{})
	if err != nil || len(values) != 0 || len(generator.requests) != 0 {
		t.Fatalf("runs=%v requests=%d err=%v", values, len(generator.requests), err)
	}
	sessionValues, err := sessions.List(session.ListFilter{})
	if err != nil || len(sessionValues) != 0 {
		t.Fatalf("sessions=%v err=%v", sessionValues, err)
	}
}
