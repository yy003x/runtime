// Package llmruntime 提供可嵌入业务服务的 LLM Runtime SDK。
package llmruntime

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/llm"
	"github.com/yy003x/runtime/internal/mcp"
	"github.com/yy003x/runtime/internal/provider"
	"github.com/yy003x/runtime/runtimeapi"
)

type Options struct {
	ProfileDir        string
	AssetRoots        map[string]string
	HTTPClient        *http.Client
	MaxAssetSize      int64
	AssetCacheEntries int
}

type Runtime struct {
	profileDir string
	httpClient *http.Client
	assets     *assetResolver
	registry   *registry
}

func New(options Options) (*Runtime, error) {
	if strings.TrimSpace(options.ProfileDir) == "" {
		return nil, fmt.Errorf("profile directory is required")
	}
	assets, err := newAssetResolverWithCache(options.AssetRoots, options.MaxAssetSize, options.AssetCacheEntries)
	if err != nil {
		return nil, err
	}
	return &Runtime{
		profileDir: options.ProfileDir,
		httpClient: options.HTTPClient,
		assets:     assets,
		registry:   newRegistry(),
	}, nil
}

func (r *Runtime) RegisterTool(schema runtimeapi.Tool, handler ToolHandler) error {
	return r.registry.registerTool(schema, handler)
}

func (r *Runtime) RegisterMCP(config MCPConfig) error {
	return r.registry.registerMCP(config)
}

func (r *Runtime) RegisterMemoryProvider(name string, provider MemoryProvider) error {
	return r.registry.registerMemoryProvider(name, provider)
}

func (r *Runtime) Generate(ctx context.Context, request runtimeapi.Request) (runtimeapi.Response, error) {
	return r.generate(ctx, request, nil)
}

func (r *Runtime) GenerateStream(ctx context.Context, request runtimeapi.Request, sink runtimeapi.EventSink) (runtimeapi.Response, error) {
	if sink == nil {
		return runtimeapi.Response{}, fmt.Errorf("event sink is required")
	}
	emitter := &eventEmitter{sink: sink}
	if err := emitter.emit(runtimeapi.Event{Type: runtimeapi.EventRequestStarted}); err != nil {
		return runtimeapi.Response{}, err
	}
	response, err := r.generate(ctx, request, emitter)
	if err != nil {
		_ = emitter.emit(runtimeapi.Event{Type: runtimeapi.EventError, Error: err.Error()})
		return runtimeapi.Response{}, err
	}
	if err := emitter.emit(runtimeapi.Event{Type: runtimeapi.EventResponseCompleted, Response: &response}); err != nil {
		return runtimeapi.Response{}, err
	}
	return response, nil
}

type eventEmitter struct {
	sink     runtimeapi.EventSink
	sequence int64
}

func (e *eventEmitter) emit(event runtimeapi.Event) error {
	if e == nil || e.sink == nil {
		return nil
	}
	e.sequence++
	event.Sequence = e.sequence
	event.Time = time.Now().UTC()
	return e.sink(event)
}

func (r *Runtime) generate(ctx context.Context, request runtimeapi.Request, emitter *eventEmitter) (runtimeapi.Response, error) {
	if strings.TrimSpace(request.Profile) == "" {
		return runtimeapi.Response{}, fmt.Errorf("profile is required")
	}
	if request.MaxRounds < 0 || request.MaxRounds > 64 {
		return runtimeapi.Response{}, fmt.Errorf("max_rounds must be between 0 and 64")
	}
	if request.MaxTokens < 0 {
		return runtimeapi.Response{}, fmt.Errorf("max_tokens must be non-negative")
	}
	if request.Temperature < 0 || request.Temperature > 2 {
		return runtimeapi.Response{}, fmt.Errorf("temperature must be between 0 and 2")
	}
	mode := request.ToolMode
	if mode == "" {
		mode = runtimeapi.ToolModeSchemaOnly
	}
	if mode != runtimeapi.ToolModeSchemaOnly && mode != runtimeapi.ToolModeRuntimeExecute {
		return runtimeapi.Response{}, fmt.Errorf("tool_mode must be schema_only|runtime_execute")
	}
	if mode == runtimeapi.ToolModeRuntimeExecute && len(request.Tools.Inline) > 0 {
		return runtimeapi.Response{}, fmt.Errorf("inline tools cannot be used with runtime_execute")
	}
	if mode == runtimeapi.ToolModeSchemaOnly && len(request.Tools.MCP) > 0 {
		return runtimeapi.Response{}, fmt.Errorf("registered MCP requires runtime_execute")
	}
	recalled, err := r.recallMemory(ctx, request)
	if err != nil {
		return runtimeapi.Response{}, err
	}
	system, messages, err := compileContext(r.assets, request, recalled)
	if err != nil {
		return runtimeapi.Response{}, err
	}
	if err := emitter.emit(runtimeapi.Event{Type: runtimeapi.EventContextCompiled}); err != nil {
		return runtimeapi.Response{}, err
	}
	profiles, err := provider.LoadDir(r.profileDir)
	if err != nil {
		return runtimeapi.Response{}, err
	}
	config, ok := provider.Resolve(profiles, request.Profile)
	if !ok {
		return runtimeapi.Response{}, fmt.Errorf("unknown provider profile: %s", request.Profile)
	}
	if config.Type == provider.TypeCLI {
		executionContext, cancel := profileContext(ctx, config.TimeoutSeconds)
		defer cancel()
		return r.generateCLI(executionContext, config, request, system, messages, emitter)
	}
	if config.Type != provider.TypeAPI {
		return runtimeapi.Response{}, fmt.Errorf("profile %s: structured LLM runtime supports API or CLI profiles", config.ID)
	}
	executionContext, cancel := profileContext(ctx, config.TimeoutSeconds)
	defer cancel()
	return r.generateAPI(executionContext, config, request, system, messages, mode, emitter)
}

func (r *Runtime) recallMemory(ctx context.Context, request runtimeapi.Request) ([]runtimeapi.MemoryItem, error) {
	var values []runtimeapi.MemoryItem
	for index, query := range request.Context.Recall {
		query.Provider = strings.TrimSpace(query.Provider)
		if query.Provider == "" {
			return nil, fmt.Errorf("context.recall[%d].provider is required", index)
		}
		if query.TopK < 0 || query.TopK > 100 {
			return nil, fmt.Errorf("context.recall[%d].top_k must be between 0 and 100", index)
		}
		if query.TopK == 0 {
			query.TopK = 5
		}
		if strings.TrimSpace(query.Query) == "" {
			query.Query = memoryQueryText(request)
		}
		if strings.TrimSpace(query.Query) == "" {
			return nil, fmt.Errorf("context.recall[%d] requires query or request prompt", index)
		}
		provider, err := r.registry.memoryProvider(query.Provider)
		if err != nil {
			return nil, err
		}
		items, err := provider.Recall(ctx, query)
		if err != nil {
			return nil, fmt.Errorf("memory provider %s: %w", query.Provider, err)
		}
		if len(items) > query.TopK {
			items = items[:query.TopK]
		}
		for itemIndex := range items {
			items[itemIndex].Content = strings.TrimSpace(items[itemIndex].Content)
			if items[itemIndex].Content == "" {
				return nil, fmt.Errorf("memory provider %s returned empty item", query.Provider)
			}
			if len(items[itemIndex].Content) > 64<<10 {
				return nil, fmt.Errorf("memory provider %s returned oversized item", query.Provider)
			}
			if items[itemIndex].Source == "" {
				items[itemIndex].Source = query.Provider
			}
		}
		values = append(values, items...)
	}
	return values, nil
}

func memoryQueryText(request runtimeapi.Request) string {
	if value := strings.TrimSpace(request.Prompt); value != "" {
		return value
	}
	for index := len(request.Messages) - 1; index >= 0; index-- {
		if request.Messages[index].Role == "user" {
			return strings.TrimSpace(request.Messages[index].Content)
		}
	}
	return ""
}

func profileContext(ctx context.Context, timeoutSeconds int) (context.Context, context.CancelFunc) {
	if timeoutSeconds <= 0 {
		return context.WithCancel(ctx)
	}
	return context.WithTimeout(ctx, time.Duration(timeoutSeconds)*time.Second)
}

func (r *Runtime) generateCLI(ctx context.Context, config provider.Config, request runtimeapi.Request, system string, messages []runtimeapi.Message, emitter *eventEmitter) (runtimeapi.Response, error) {
	if len(request.Tools.Inline)+len(request.Tools.Registered)+len(request.Tools.MCP) > 0 {
		return runtimeapi.Response{}, fmt.Errorf("CLI profile does not support structured tools")
	}
	prompt := compileCLIPrompt(system, messages)
	overrides := map[string]any{}
	if request.Model != "" {
		overrides["model"] = request.Model
	}
	prepared, err := provider.Prepare(config, prompt, overrides)
	if err != nil {
		return runtimeapi.Response{}, err
	}
	cwd, err := os.Getwd()
	if err != nil {
		return runtimeapi.Response{}, err
	}
	if err := emitter.emit(runtimeapi.Event{Type: runtimeapi.EventProviderStarted, Round: 1}); err != nil {
		return runtimeapi.Response{}, err
	}
	var result provider.Result
	if emitter == nil {
		result, err = provider.ExecuteCLI(ctx, config, *prepared.CLI, cwd, nil)
	} else {
		result, err = provider.ExecuteCLIWithStream(ctx, config, *prepared.CLI, cwd, nil, func(value []byte) error {
			return emitter.emit(runtimeapi.Event{Type: runtimeapi.EventOutputDelta, Round: 1, Delta: string(value)})
		}, nil)
	}
	if err != nil {
		return runtimeapi.Response{}, err
	}
	text := strings.TrimSpace(result.FinalText)
	return runtimeapi.Response{
		Message: runtimeapi.Message{Role: "assistant", Content: text},
		Done:    true, Rounds: 1, Metadata: cloneMetadata(request.Metadata),
	}, nil
}

func (r *Runtime) generateAPI(ctx context.Context, config provider.Config, request runtimeapi.Request, system string, messages []runtimeapi.Message, mode string, emitter *eventEmitter) (runtimeapi.Response, error) {
	selected, err := r.registry.selectedTools(request.Tools.Registered)
	if err != nil {
		return runtimeapi.Response{}, err
	}
	tools := append([]runtimeapi.Tool(nil), request.Tools.Inline...)
	handlers := make(map[string]ToolHandler, len(selected))
	for _, item := range selected {
		tools = append(tools, item.schema)
		handlers[item.schema.Name] = item.handler
	}
	mcpServers, err := r.registry.selectedMCP(request.Tools.MCP)
	if err != nil {
		return runtimeapi.Response{}, err
	}
	var clients []*mcp.Client
	defer func() {
		for _, client := range clients {
			client.Close()
		}
	}()
	for _, server := range mcpServers {
		client, err := mcp.Start(ctx, mcp.Config{
			Name: server.Name, Command: server.Command, Args: server.Args,
			Dir: server.Dir, Env: server.Env, Timeout: server.Timeout,
		})
		if err != nil {
			return runtimeapi.Response{}, err
		}
		clients = append(clients, client)
		discovered, err := client.ListTools(ctx)
		if err != nil {
			return runtimeapi.Response{}, err
		}
		for _, tool := range discovered {
			if _, exists := handlers[tool.Name]; exists {
				return runtimeapi.Response{}, fmt.Errorf("tool name collision: %s", tool.Name)
			}
			toolName := tool.Name
			tools = append(tools, runtimeapi.Tool{Name: tool.Name, Description: tool.Description, Parameters: tool.InputSchema})
			handlers[tool.Name] = func(ctx context.Context, arguments map[string]any) (any, error) {
				return client.CallTool(ctx, toolName, arguments)
			}
		}
	}
	if err := validateToolNames(tools); err != nil {
		return runtimeapi.Response{}, err
	}
	client, err := provider.NewLLMClient(config, r.httpClient)
	if err != nil {
		return runtimeapi.Response{}, err
	}
	model := config.API.Model
	if request.Model != "" {
		model = request.Model
	}
	maxTokens := request.MaxTokens
	if maxTokens == 0 {
		maxTokens = config.API.MaxTokens
	}
	if maxTokens == 0 {
		maxTokens = 2048
	}
	maxRounds := request.MaxRounds
	if maxRounds == 0 {
		maxRounds = 8
	}
	llmMessages := toLLMMessages(messages)
	var usage runtimeapi.Usage
	for round := 1; round <= maxRounds; round++ {
		if err := emitter.emit(runtimeapi.Event{Type: runtimeapi.EventProviderStarted, Round: round}); err != nil {
			return runtimeapi.Response{}, err
		}
		llmRequest := llm.Request{
			Model: model, System: system, Messages: llmMessages, Tools: toLLMTools(tools),
			Temperature: request.Temperature, MaxTokens: maxTokens,
		}
		response, err := generateLLM(ctx, client, llmRequest, emitter, round)
		if err != nil {
			return runtimeapi.Response{}, err
		}
		usage.InputTokens += response.InputTokens
		usage.OutputTokens += response.OutputTokens
		publicCalls := fromLLMToolCalls(response.ToolCalls)
		for index := range publicCalls {
			call := publicCalls[index]
			if err := emitter.emit(runtimeapi.Event{Type: runtimeapi.EventToolCall, Round: round, ToolCall: &call}); err != nil {
				return runtimeapi.Response{}, err
			}
		}
		publicResponse := runtimeapi.Response{
			Message: runtimeapi.Message{
				Role: "assistant", Content: response.OutputText, ToolCalls: publicCalls,
			},
			ToolCalls: publicCalls, FinishReason: response.FinishReason,
			Done: len(response.ToolCalls) == 0, Rounds: round, Usage: usage,
			Metadata: cloneMetadata(request.Metadata),
		}
		if len(response.ToolCalls) == 0 || mode == runtimeapi.ToolModeSchemaOnly {
			return publicResponse, nil
		}
		llmMessages = append(llmMessages, llm.Message{
			Role: "assistant", Content: response.OutputText, ToolCalls: response.ToolCalls,
		})
		for _, call := range response.ToolCalls {
			handler, ok := handlers[call.Name]
			if !ok {
				return runtimeapi.Response{}, fmt.Errorf("tool %s has no registered handler", call.Name)
			}
			if err := emitter.emit(runtimeapi.Event{Type: runtimeapi.EventToolStarted, Round: round, ToolName: call.Name}); err != nil {
				return runtimeapi.Response{}, err
			}
			result, err := handler(ctx, call.Arguments)
			if err != nil {
				return runtimeapi.Response{}, fmt.Errorf("tool %s: %w", call.Name, err)
			}
			encoded, err := json.Marshal(result)
			if err != nil {
				return runtimeapi.Response{}, fmt.Errorf("tool %s result: %w", call.Name, err)
			}
			if err := emitter.emit(runtimeapi.Event{
				Type: runtimeapi.EventToolCompleted, Round: round, ToolName: call.Name, ToolResult: result,
			}); err != nil {
				return runtimeapi.Response{}, err
			}
			llmMessages = append(llmMessages, llm.Message{Role: "tool", ToolCallID: call.ID, Content: string(encoded)})
		}
	}
	return runtimeapi.Response{}, fmt.Errorf("LLM runtime exceeded max_rounds=%d", maxRounds)
}

func generateLLM(ctx context.Context, client llm.Client, request llm.Request, emitter *eventEmitter, round int) (llm.Response, error) {
	if emitter == nil {
		return client.Generate(ctx, request)
	}
	streaming, ok := client.(llm.StreamClient)
	if !ok {
		response, err := client.Generate(ctx, request)
		if err == nil && response.OutputText != "" {
			err = emitter.emit(runtimeapi.Event{Type: runtimeapi.EventOutputDelta, Round: round, Delta: response.OutputText})
		}
		return response, err
	}
	return streaming.GenerateStream(ctx, request, func(event llm.StreamEvent) error {
		return emitter.emit(runtimeapi.Event{Type: runtimeapi.EventOutputDelta, Round: round, Delta: event.Delta})
	})
}

func validateToolNames(tools []runtimeapi.Tool) error {
	seen := make(map[string]struct{}, len(tools))
	for _, tool := range tools {
		if strings.TrimSpace(tool.Name) == "" || tool.Parameters == nil {
			return fmt.Errorf("tool name and parameters are required")
		}
		if _, exists := seen[tool.Name]; exists {
			return fmt.Errorf("tool name collision: %s", tool.Name)
		}
		seen[tool.Name] = struct{}{}
	}
	return nil
}

func compileCLIPrompt(system string, messages []runtimeapi.Message) string {
	var sections []string
	if system != "" {
		sections = append(sections, "System:\n"+system)
	}
	for _, message := range messages {
		sections = append(sections, strings.ToUpper(message.Role)+":\n"+message.Content)
	}
	return strings.Join(sections, "\n\n")
}

func toLLMMessages(messages []runtimeapi.Message) []llm.Message {
	values := make([]llm.Message, 0, len(messages))
	for _, message := range messages {
		values = append(values, llm.Message{
			Role: message.Role, Content: message.Content, ToolCallID: message.ToolCallID,
			ToolCalls: toLLMToolCalls(message.ToolCalls),
		})
	}
	return values
}

func toLLMTools(tools []runtimeapi.Tool) []llm.Tool {
	values := make([]llm.Tool, 0, len(tools))
	for _, tool := range tools {
		values = append(values, llm.Tool{Name: tool.Name, Description: tool.Description, Parameters: tool.Parameters})
	}
	return values
}

func toLLMToolCalls(calls []runtimeapi.ToolCall) []llm.ToolCall {
	values := make([]llm.ToolCall, 0, len(calls))
	for _, call := range calls {
		values = append(values, llm.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	return values
}

func fromLLMToolCalls(calls []llm.ToolCall) []runtimeapi.ToolCall {
	values := make([]runtimeapi.ToolCall, 0, len(calls))
	for _, call := range calls {
		values = append(values, runtimeapi.ToolCall{ID: call.ID, Name: call.Name, Arguments: call.Arguments})
	}
	return values
}

func cloneMetadata(values map[string]any) map[string]any {
	if len(values) == 0 {
		return nil
	}
	cloned := make(map[string]any, len(values))
	for key, value := range values {
		cloned[key] = value
	}
	return cloned
}

var _ runtimeapi.Client = (*Runtime)(nil)
var _ runtimeapi.StreamingClient = (*Runtime)(nil)
