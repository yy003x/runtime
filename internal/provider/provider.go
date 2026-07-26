package provider

import (
	"context"
	"fmt"
	"net/http"
	"sync"

	"github.com/yy003x/runtime/internal/daemon"
)

// Provider is the single execution boundary used by agentrun.
type Provider interface {
	Kind() string
	Prepare(context.Context, Config, Request) (PreparedRequest, error)
	Execute(context.Context, PreparedRequest, Sink) (Result, error)
}

// Request contains execution context owned by agentrun but consumed by a
// provider implementation.
type Request struct {
	Prompt string
	// Messages 是由 Runtime Session 编译的跨 Provider 规范化历史；当前 Prompt 不包含在内。
	Messages            []NativeMessage
	InjectedMemory      []InjectedMemory
	RawCLIArgs          []string
	Overrides           map[string]any
	CWD                 string
	Environment         map[string]string
	HTTPClient          *http.Client
	Daemon              *daemon.Client
	Profiles            map[string]Config
	RunID               string
	RequestFile         string
	ResultFile          string
	DoneFile            string
	OutputLog           string
	SnapshotFile        string
	PersonaDir          string
	SkillDir            string
	ToolDir             string
	MemoryFile          string
	MemoryCandidateFile string
	SessionID           string
	TurnID              string
	Allowed             []string
	Forbidden           []string
	StaticContext       *StaticContextSnapshot
	NativeResume        bool
	NativePatch         *NativePatch
}

// InjectedMemory 是 Workbench/project/global memory 的只读输入快照。
// Runtime 只消费并记录 digest，不写回其 owner。
type InjectedMemory struct {
	ID      string `json:"id"`
	Type    string `json:"type,omitempty"`
	Content string `json:"content"`
	Source  string `json:"source,omitempty"`
}

type NativeMessage struct {
	Role    string `json:"role"`
	Content string `json:"content"`
	Pinned  bool   `json:"pinned,omitempty"`
}

type NativePatch struct {
	Operation          string          `json:"operation"`
	SystemInstructions []NativeMessage `json:"system_instructions,omitempty"`
	Messages           []NativeMessage `json:"messages,omitempty"`
}

type Event struct {
	Type string
	Data map[string]any
}

type StatusPatch struct {
	Message string
	Values  map[string]any
}

type Sink interface {
	Stdout([]byte) error
	Stderr([]byte) error
	Event(Event) error
	StatusPatch(StatusPatch) error
}

type nopSink struct{}

func (nopSink) Stdout([]byte) error           { return nil }
func (nopSink) Stderr([]byte) error           { return nil }
func (nopSink) Event(Event) error             { return nil }
func (nopSink) StatusPatch(StatusPatch) error { return nil }

func Select(cfg Config) (Provider, error) {
	switch cfg.Type {
	case TypeCLI:
		if cfg.CLI == nil {
			return nil, fmt.Errorf("profile %s: missing cli config", cfg.ID)
		}
		if cfg.CLI.Executor == ExecutorTmux {
			return tmuxProvider{}, nil
		}
		return cliProvider{}, nil
	case TypeAPI:
		if cfg.API == nil {
			return nil, fmt.Errorf("profile %s: missing api config", cfg.ID)
		}
		return apiProvider{}, nil
	case TypeNative:
		if cfg.Native == nil {
			return nil, fmt.Errorf("profile %s: missing native config", cfg.ID)
		}
		return nativeProvider{}, nil
	default:
		return nil, fmt.Errorf("profile %s: unsupported provider type %q", cfg.ID, cfg.Type)
	}
}

type cliProvider struct{}

func (cliProvider) Kind() string { return TypeCLI }

func (cliProvider) Prepare(ctx context.Context, cfg Config, req Request) (PreparedRequest, error) {
	if err := ValidateStaticContextSnapshot(ctx, cfg, req); err != nil {
		return PreparedRequest{}, err
	}
	prepared, err := prepare(cfg, req.Prompt, req.Overrides, req.RawCLIArgs)
	if err != nil {
		return PreparedRequest{}, err
	}
	prepared.Config = cfg
	prepared.Request = req
	return prepared, nil
}

func (cliProvider) Execute(ctx context.Context, prepared PreparedRequest, sink Sink) (Result, error) {
	if prepared.CLI == nil {
		return Result{ExitCode: 1}, fmt.Errorf("profile %s: missing prepared cli request", prepared.Config.ID)
	}
	sink = ensureSink(sink)
	environment := cloneEnvironment(prepared.Request.Environment)
	if requiresDaemon(prepared.Config) {
		if prepared.Request.Daemon == nil {
			return Result{ExitCode: 1}, fmt.Errorf("profile %s: daemon client is required", prepared.Config.ID)
		}
		execution, err := daemonExecution(prepared.Config)
		if err != nil {
			return Result{ExitCode: 1}, err
		}
		processID := "cli/" + prepared.Request.RunID
		injected, err := prepared.Request.Daemon.Acquire(ctx, processID, daemonDependencies(prepared.Config), execution)
		if err != nil {
			return Result{ExitCode: 1}, err
		}
		defer func() { _ = prepared.Request.Daemon.Release(context.Background(), processID) }()
		for key, value := range injected {
			environment[key] = value
		}
	}
	var callbackErr error
	var callbackErrOnce sync.Once
	recordError := func(value error) {
		if value != nil {
			callbackErrOnce.Do(func() { callbackErr = value })
		}
	}
	result, err := executeCLIStreaming(
		ctx, prepared.Config, *prepared.CLI, prepared.Request.CWD, environment,
		func(info ExecutionInfo) {
			recordError(sink.StatusPatch(StatusPatch{Message: "cli running", Values: map[string]any{"pid": info.PID, "pgid": info.PGID}}))
		},
		func(chunk []byte) { recordError(sink.Stdout(chunk)) },
		func(chunk []byte) { recordError(sink.Stderr(chunk)) },
		func() { recordError(sink.Event(Event{Type: "provider.output.started", Data: map[string]any{}})) },
	)
	if err == nil && callbackErr != nil {
		err = callbackErr
	}
	return result, err
}

func cloneEnvironment(source map[string]string) map[string]string {
	cloned := make(map[string]string, len(source))
	for key, value := range source {
		cloned[key] = value
	}
	return cloned
}

type apiProvider struct{}

func (apiProvider) Kind() string { return TypeAPI }

func (apiProvider) Prepare(ctx context.Context, cfg Config, req Request) (PreparedRequest, error) {
	if err := ValidateStaticContextSnapshot(ctx, cfg, req); err != nil {
		return PreparedRequest{}, err
	}
	prepared, err := Prepare(cfg, req.Prompt, req.Overrides)
	if err != nil {
		return PreparedRequest{}, err
	}
	prepared.Config = cfg
	prepared.Request = req
	if prepared.API != nil && (len(req.Messages) > 0 || len(req.InjectedMemory) > 0) {
		messages := make([]any, 0, len(req.Messages)+2)
		if memory := injectedMemorySection(req.InjectedMemory); memory != "" {
			if prepared.API.Protocol == "anthropic" {
				prepared.API.Payload["system"] = memory
			} else {
				messages = append(messages, map[string]any{"role": "system", "content": memory})
			}
		}
		for _, message := range req.Messages {
			if message.Role == "user" || message.Role == "assistant" {
				messages = append(messages, map[string]any{"role": message.Role, "content": message.Content})
			}
		}
		messages = append(messages, map[string]any{"role": "user", "content": req.Prompt})
		prepared.API.Payload["messages"] = messages
	}
	return prepared, nil
}

func (apiProvider) Execute(ctx context.Context, prepared PreparedRequest, sink Sink) (Result, error) {
	if prepared.API == nil {
		return Result{ExitCode: 1}, fmt.Errorf("profile %s: missing prepared api request", prepared.Config.ID)
	}
	sink = ensureSink(sink)
	if prepared.Config.API != nil && prepared.Config.API.Runtime != nil && prepared.Config.API.Runtime.Enabled {
		return executeAPIRuntime(ctx, prepared, sink)
	}
	if err := sink.StatusPatch(StatusPatch{Message: "api running", Values: map[string]any{"protocol": prepared.API.Protocol}}); err != nil {
		return Result{ExitCode: 1}, err
	}
	result, err := ExecuteAPI(ctx, prepared.Request.HTTPClient, prepared.Config, *prepared.API)
	if result.Stdout != "" {
		if sinkErr := sink.Stdout([]byte(result.Stdout)); err == nil && sinkErr != nil {
			err = sinkErr
		}
	}
	if result.Stderr != "" {
		if sinkErr := sink.Stderr([]byte(result.Stderr)); err == nil && sinkErr != nil {
			err = sinkErr
		}
	}
	return result, err
}

func ensureSink(sink Sink) Sink {
	if sink == nil {
		return nopSink{}
	}
	return sink
}
