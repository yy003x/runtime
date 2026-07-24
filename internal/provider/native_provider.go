package provider

import (
	"context"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/yy003x/runtime/internal/capability"
	"github.com/yy003x/runtime/internal/llm"
	"github.com/yy003x/runtime/internal/llm/anthropic"
	"github.com/yy003x/runtime/internal/llm/openai"
	"github.com/yy003x/runtime/internal/persona"
	nativeengine "github.com/yy003x/runtime/internal/provider/native"
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
	tools, toolExecutor := buildNativeToolRuntime(prepared.Request)
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
		MaxRounds: maxRounds, TokenBudget: tokenBudget, LLMTimeout: llmTimeout, Tools: tools, Executor: toolExecutor,
	}, func(snapshot nativeengine.Snapshot) {
		values := map[string]any{
			"kind": TypeNative, "phase": string(snapshot.State), "native_state": string(snapshot.State),
			"round": snapshot.Round, "max_rounds": snapshot.MaxRounds, "snapshot_file": prepared.Request.SnapshotFile,
			"input_tokens": snapshot.InputTokens, "output_tokens": snapshot.OutputTokens,
			"finish_reason": snapshot.LastFinishReason,
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
		Detail: map[string]any{
			"native_state": snapshot.State, "round": snapshot.Round, "snapshot_file": prepared.Request.SnapshotFile,
			"input_tokens": snapshot.InputTokens, "output_tokens": snapshot.OutputTokens, "finish_reason": snapshot.LastFinishReason,
		},
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
	if memory := injectedMemorySection(prepared.Request.InjectedMemory); memory != "" {
		if systemPrompt != "" {
			systemPrompt += "\n\n"
		}
		systemPrompt += memory
	}
	messages := make([]nativeengine.Message, 0, len(prepared.Request.Messages)+1)
	for _, message := range prepared.Request.Messages {
		if message.Role == "user" || message.Role == "assistant" {
			messages = append(messages, nativeengine.Message{Role: message.Role, Content: message.Content})
		}
	}
	messages = append(messages, nativeengine.Message{Role: "user", Content: prepared.Request.Prompt})
	initial := nativeengine.Context{Messages: messages}
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
	key, err := resolveAPIKey(profile.ID, profile.API)
	if err != nil {
		return nil, err
	}
	client := prepared.Request.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	headers, err := resolveAPIHeaders(profile.API.Headers)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", profile.ID, err)
	}
	auth := defaultAPIAuth(profile.API.Protocol, profile.API.BaseURL)
	clientOptions := llm.HTTPOptions{Headers: headers, AuthHeader: auth.Header, AuthPrefix: auth.Prefix}
	var runtimeClient llm.Client
	switch profile.API.Protocol {
	case "openai":
		runtimeClient = openai.NewClientWithOptions(profile.API.BaseURL, key, client, clientOptions)
	case "anthropic":
		runtimeClient = anthropic.NewClientWithOptions(profile.API.BaseURL, key, client, clientOptions)
	default:
		return nil, fmt.Errorf("profile %s: unsupported native API protocol %q", profile.ID, profile.API.Protocol)
	}
	return nativeClientAdapter{client: runtimeClient, model: profile.API.Model}, nil
}

type nativeClientAdapter struct {
	client      llm.Client
	model       string
	temperature float64
	maxTokens   int
}

func (a nativeClientAdapter) Generate(ctx context.Context, request nativeengine.Request) (nativeengine.Response, error) {
	var system []string
	var messages []llm.Message
	for _, message := range request.Messages {
		if message.Role == "system" {
			system = append(system, message.Content)
			continue
		}
		converted := llm.Message{Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID}
		for _, call := range message.ToolCalls {
			converted.ToolCalls = append(converted.ToolCalls, llm.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
		}
		messages = append(messages, converted)
	}
	tools := make([]llm.Tool, 0, len(request.Tools))
	for _, tool := range request.Tools {
		tools = append(tools, llm.Tool{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	maxTokens := a.maxTokens
	if maxTokens <= 0 {
		maxTokens = 2048
	}
	response, err := a.client.Generate(ctx, llm.Request{
		Model: a.model, System: strings.Join(system, "\n\n"), Messages: messages, Tools: tools,
		Temperature: a.temperature, MaxTokens: maxTokens,
	})
	if err != nil {
		return nativeengine.Response{}, fmt.Errorf("%w: %v", nativeengine.ErrUpstream, err)
	}
	toolCalls := make([]nativeengine.ToolCall, 0, len(response.ToolCalls))
	for _, call := range response.ToolCalls {
		toolCalls = append(toolCalls, nativeengine.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	return nativeengine.Response{
		Message: nativeengine.Message{Role: "assistant", Content: response.OutputText}, ToolCalls: toolCalls,
		FinishReason: response.FinishReason, Done: response.Done,
		InputTokens: response.InputTokens, OutputTokens: response.OutputTokens,
	}, nil
}

func buildNativeToolRuntime(request Request) ([]nativeengine.Tool, nativeengine.ToolExecutor) {
	registry := capability.NewRegistry(capability.RegistryConfig{ToolsDir: request.ToolDir})
	manager := registry.Tools
	available := make(map[string]capability.Tool)
	var tools []nativeengine.Tool
	for _, tool := range manager.Schemas() {
		if tool.Kind == "external" || !nativeToolAllowed(tool, request.Allowed, request.Forbidden) {
			continue
		}
		available[tool.Name] = tool
		tools = append(tools, nativeengine.Tool{Name: tool.Name, Description: tool.Description, Parameters: tool.Schema})
	}
	if len(tools) == 0 {
		return nil, nil
	}
	executor := nativeengine.ToolExecutorFunc(func(_ context.Context, call nativeengine.ToolCall) (any, error) {
		tool, ok := available[call.Name]
		if !ok {
			return nil, fmt.Errorf("native tool is not authorized: %s", call.Name)
		}
		capabilities := append([]string(nil), request.Allowed...)
		if containsAction(request.Allowed, tool.Name) && tool.Capability != "" && !containsAction(capabilities, tool.Capability) {
			capabilities = append(capabilities, tool.Capability)
		}
		return manager.Call(call.Name, call.Arguments, capabilities, request.Forbidden)
	})
	return tools, executor
}

func nativeToolAllowed(tool capability.Tool, allowed, forbidden []string) bool {
	if containsAction(forbidden, "*") || containsAction(forbidden, tool.Name) || (tool.Capability != "" && containsAction(forbidden, tool.Capability)) {
		return false
	}
	return containsAction(allowed, "*") || containsAction(allowed, tool.Name) || (tool.Capability != "" && containsAction(allowed, tool.Capability))
}

func containsAction(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
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
	return map[string]any{"native_state": snapshot.State, "agent_state": snapshot.State, "round": snapshot.Round, "snapshot_file": snapshotFile}, nil
}

func ReadNativeSnapshot(snapshotFile string) (map[string]any, error) {
	snapshot, err := nativeengine.NewFileStore(snapshotFile).Load()
	if err != nil {
		return nil, err
	}
	return map[string]any{
		"native_state": snapshot.State, "round": snapshot.Round, "max_rounds": snapshot.MaxRounds,
		"input_tokens": snapshot.InputTokens, "output_tokens": snapshot.OutputTokens,
		"finish_reason": snapshot.LastFinishReason, "last_error": snapshot.LastError,
	}, nil
}
