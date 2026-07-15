package provider

import (
	"context"
	"fmt"
	"net/http"
	"os"
	"strings"
	"sync"
	"time"

	"agent-runtime/internal/llm"
	"agent-runtime/internal/llm/anthropic"
	"agent-runtime/internal/llm/openai"
	"agent-runtime/internal/persona"
	nativeengine "agent-runtime/internal/provider/native"
)

type nativeProvider struct{}

func (nativeProvider) Kind() string { return TypeNative }

func (nativeProvider) Prepare(_ context.Context, cfg Config, req Request) (PreparedRequest, error) {
	prepared, err := prepare(cfg, req.Prompt, req.Overrides, req.RawCLIArgs)
	if err != nil {
		return PreparedRequest{}, err
	}
	prepared.Config = cfg
	prepared.Request = req
	return prepared, nil
}

func (nativeProvider) Execute(ctx context.Context, prepared PreparedRequest, sink Sink) (Result, error) {
	if prepared.Native == nil || prepared.Config.Native == nil {
		return Result{ExitCode: 1}, fmt.Errorf("profile %s: missing prepared native request", prepared.Config.ID)
	}
	if prepared.Request.RunID == "" || prepared.Request.SnapshotFile == "" {
		return Result{ExitCode: 1}, fmt.Errorf("profile %s: native run_id and snapshot_file are required", prepared.Config.ID)
	}
	sink = ensureSink(sink)
	client, err := buildNativeClient(prepared)
	if err != nil {
		return Result{ExitCode: 1}, err
	}
	config := prepared.Config.Native
	maxRounds := intValue(prepared.Native.EffectiveOptions["max_rounds"], config.MaxRounds)
	tokenBudget := intValue(prepared.Native.EffectiveOptions["token_budget"], config.TokenBudget)
	llmTimeout := durationSeconds(prepared.Native.EffectiveOptions["llm_timeout_seconds"], config.LLMTimeoutSeconds)
	store := nativeengine.NewFileStore(prepared.Request.SnapshotFile)
	lastEvent := 0
	var sinkErr error
	var sinkErrOnce sync.Once
	recordSinkError := func(err error) {
		if err != nil {
			sinkErrOnce.Do(func() { sinkErr = err })
		}
	}
	engine := nativeengine.NewEngine(store, client, nativeengine.Config{
		MaxRounds: maxRounds, TokenBudget: tokenBudget, LLMTimeout: llmTimeout,
	}, func(snapshot nativeengine.Snapshot) {
		values := map[string]any{
			"kind": TypeNative, "phase": string(snapshot.State), "native_state": string(snapshot.State),
			"round": snapshot.Round, "max_rounds": snapshot.MaxRounds, "snapshot_file": prepared.Request.SnapshotFile,
		}
		if snapshot.LastError != "" {
			values["block_reason"] = snapshot.LastError
		}
		recordSinkError(sink.StatusPatch(StatusPatch{Message: "native " + string(snapshot.State), Values: values}))
		for lastEvent < len(snapshot.Events) {
			event := snapshot.Events[lastEvent]
			recordSinkError(sink.Event(Event{Type: "native." + event.Type, Data: map[string]any{
				"native_state": event.ToState, "from_state": event.FromState, "round": event.Round,
				"message": event.Message, "error": event.Error,
			}}))
			lastEvent++
		}
	})

	var snapshot nativeengine.Snapshot
	if prepared.Request.NativeResume {
		var patch *nativeengine.ContextPatch
		if prepared.Request.NativePatch != nil {
			converted := convertNativePatch(*prepared.Request.NativePatch)
			patch = &converted
		}
		snapshot, err = engine.Resume(ctx, patch)
	} else {
		initial, initialErr := nativeInitialContext(ctx, prepared)
		if initialErr != nil {
			return Result{ExitCode: 1}, initialErr
		}
		snapshot, err = engine.Start(ctx, prepared.Request.RunID, initial)
	}
	result := Result{
		Stdout: nativeengine.FinalText(snapshot), FinalText: nativeengine.FinalText(snapshot),
		ExitCode: 0, State: string(snapshot.State),
		Detail: map[string]any{"native_state": snapshot.State, "round": snapshot.Round, "snapshot_file": prepared.Request.SnapshotFile},
	}
	if result.Stdout != "" {
		result.Stdout += "\n"
	}
	if snapshot.State == nativeengine.StateWaitingHuman || snapshot.State == nativeengine.StateBlocked || snapshot.State == nativeengine.StateStopped {
		result.State = "blocked"
		result.BlockedReason = snapshot.LastError
		if result.BlockedReason == "" && len(snapshot.Events) > 0 {
			result.BlockedReason = snapshot.Events[len(snapshot.Events)-1].Message
		}
	}
	if snapshot.State == nativeengine.StateCancelled {
		result.State = "cancelled"
	}
	if snapshot.State == nativeengine.StateFailed {
		result.ExitCode = 1
		if err == nil {
			err = fmt.Errorf("native provider failed: %s", snapshot.LastError)
		}
	}
	if err == nil && sinkErr != nil {
		err = sinkErr
	}
	return result, err
}

func nativeInitialContext(ctx context.Context, prepared PreparedRequest) (nativeengine.Context, error) {
	systemPrompt := strings.TrimSpace(fmt.Sprint(prepared.Native.EffectiveOptions["system_prompt"]))
	personaID := strings.TrimSpace(fmt.Sprint(prepared.Native.EffectiveOptions["persona"]))
	if systemPrompt == "" && personaID != "" && prepared.Request.PersonaDir != "" {
		loaded, err := persona.NewLoader(prepared.Request.PersonaDir).Load(ctx, personaID)
		if err != nil {
			return nativeengine.Context{}, fmt.Errorf("load native persona: %w", err)
		}
		systemPrompt = persona.RenderSystem(loaded)
	}
	initial := nativeengine.Context{Messages: []nativeengine.Message{{Role: "user", Content: prepared.Request.Prompt}}}
	if systemPrompt != "" {
		initial.SystemInstructions = []nativeengine.Message{{Role: "system", Content: systemPrompt, Pinned: true}}
	}
	return initial, nil
}

func buildNativeClient(prepared PreparedRequest) (nativeengine.Client, error) {
	config := prepared.Config.Native
	if config.Mock != nil {
		return &nativeengine.MockClient{
			Latency:   time.Duration(config.Mock.LatencyMilliseconds) * time.Millisecond,
			Responses: append([]string(nil), config.Mock.Responses...), DoneAfter: config.Mock.DoneAfter,
		}, nil
	}
	profileID := strings.TrimSpace(fmt.Sprint(prepared.Native.EffectiveOptions["model_profile"]))
	profile, ok := Resolve(prepared.Request.Profiles, profileID)
	if !ok || profile.Type != TypeAPI || profile.API == nil {
		return nil, fmt.Errorf("profile %s: native model_profile %q must resolve to an API profile", prepared.Config.ID, profileID)
	}
	key := os.Getenv(profile.API.APIKeyEnv)
	if key == "" {
		return nil, fmt.Errorf("profile %s: environment %s is required", profile.ID, profile.API.APIKeyEnv)
	}
	client := prepared.Request.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	var runtimeClient llm.Client
	switch profile.API.Protocol {
	case "openai":
		runtimeClient = openai.NewClient(profile.API.BaseURL, key, client)
	case "anthropic":
		runtimeClient = anthropic.NewClient(profile.API.BaseURL, key, client)
	default:
		return nil, fmt.Errorf("profile %s: unsupported native API protocol %q", profile.ID, profile.API.Protocol)
	}
	return nativeClientAdapter{client: runtimeClient, model: profile.API.Model}, nil
}

type nativeClientAdapter struct {
	client llm.Client
	model  string
}

func (a nativeClientAdapter) Generate(ctx context.Context, request nativeengine.Request) (nativeengine.Response, error) {
	var system []string
	var messages []llm.Message
	for _, message := range request.Messages {
		if message.Role == "system" {
			system = append(system, message.Content)
			continue
		}
		messages = append(messages, llm.Message{Role: message.Role, Content: message.Content})
	}
	response, err := a.client.Generate(ctx, llm.Request{
		Model: a.model, System: strings.Join(system, "\n\n"), Messages: messages, MaxOutputTokens: 2048,
	})
	if err != nil {
		return nativeengine.Response{}, fmt.Errorf("%w: %v", nativeengine.ErrUpstream, err)
	}
	return nativeengine.Response{Message: nativeengine.Message{Role: "assistant", Content: response.OutputText}}, nil
}

func convertNativePatch(patch NativePatch) nativeengine.ContextPatch {
	converted := nativeengine.ContextPatch{Operation: nativeengine.PatchOperation(patch.Operation)}
	for _, message := range patch.SystemInstructions {
		converted.SystemInstructions = append(converted.SystemInstructions, nativeengine.Message{Role: message.Role, Content: message.Content, Pinned: message.Pinned})
	}
	for _, message := range patch.Messages {
		converted.Messages = append(converted.Messages, nativeengine.Message{Role: message.Role, Content: message.Content, Pinned: message.Pinned})
	}
	return converted
}

func durationSeconds(value any, fallback float64) time.Duration {
	number, ok := numericValue(value)
	if !ok || number <= 0 {
		number = fallback
	}
	if number <= 0 {
		number = 5
	}
	return time.Duration(number * float64(time.Second))
}

func ControlNative(snapshotFile, action, reason string) (map[string]any, error) {
	snapshot, err := nativeengine.ControlRun(nativeengine.NewFileStore(snapshotFile), action, reason)
	if err != nil {
		return nil, err
	}
	return map[string]any{"native_state": snapshot.State, "round": snapshot.Round, "snapshot_file": snapshotFile}, nil
}

func ReadNativeSnapshot(snapshotFile string) (map[string]any, error) {
	snapshot, err := nativeengine.NewFileStore(snapshotFile).Load()
	if err != nil {
		return nil, err
	}
	return map[string]any{"native_state": snapshot.State, "round": snapshot.Round, "max_rounds": snapshot.MaxRounds, "last_error": snapshot.LastError}, nil
}
