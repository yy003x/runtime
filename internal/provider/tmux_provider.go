package provider

import (
	"context"
	"fmt"
	"os"
	"sort"
	"strings"
	"time"

	"agent-runtime/internal/daemon"
	providertmux "agent-runtime/internal/provider/tmux"
)

type tmuxProvider struct{}

func (tmuxProvider) Kind() string { return ExecutorTmux }

func (tmuxProvider) Prepare(_ context.Context, cfg Config, req Request) (PreparedRequest, error) {
	prepared, err := Prepare(cfg, req.Prompt, req.Overrides)
	if err != nil {
		return PreparedRequest{}, err
	}
	prepared.Config = cfg
	prepared.Request = req
	return prepared, nil
}

func (tmuxProvider) Execute(ctx context.Context, prepared PreparedRequest, sink Sink) (Result, error) {
	if prepared.CLI == nil || prepared.Config.CLI == nil || prepared.Config.CLI.Tmux == nil {
		return Result{ExitCode: 1}, fmt.Errorf("profile %s: missing prepared tmux request", prepared.Config.ID)
	}
	request := prepared.Request
	if request.RunID == "" || request.CWD == "" || request.ResultFile == "" || request.DoneFile == "" {
		return Result{ExitCode: 1}, fmt.Errorf("profile %s: incomplete tmux run context", prepared.Config.ID)
	}
	sink = ensureSink(sink)
	backend, err := NewTmuxBackend(prepared.Config, request.Daemon)
	if err != nil {
		return Result{ExitCode: 1}, err
	}
	command := tmuxShellCommandArgv(prepared.Config, prepared.CLI.Argv, request.Environment)
	taskResult, err := backend.ExecuteTask(ctx, providertmux.TaskRequest{
		RunID:      request.RunID,
		CWD:        request.CWD,
		Command:    command,
		Prompt:     request.Prompt,
		ResultFile: request.ResultFile,
		DoneFile:   request.DoneFile,
		Bracketed:  prepared.Config.CLI.Tmux.PasteBracketed,
	}, func(phase string, values map[string]any) error {
		values["done_file"] = request.DoneFile
		return sink.StatusPatch(StatusPatch{Message: "tmux " + phase, Values: values})
	})
	if taskResult.Stdout != "" {
		if sinkErr := sink.Stdout([]byte(taskResult.Stdout)); err == nil && sinkErr != nil {
			err = sinkErr
		}
	}
	detail := map[string]any{
		"tmux_session":       taskResult.Session,
		"alive":              false,
		"done_file_exists":   taskResult.Done,
		"result_file_exists": taskResult.Result,
	}
	if !taskResult.Done && taskResult.ExitCode != 0 {
		detail["reason"] = "exited_without_done_file"
	}
	return Result{
		Stdout:    taskResult.Stdout,
		FinalText: strings.TrimSpace(taskResult.Stdout),
		ExitCode:  taskResult.ExitCode,
		Detail:    detail,
	}, err
}

func NewTmuxBackend(cfg Config, client *daemon.Client) (*providertmux.Backend, error) {
	if cfg.CLI == nil || cfg.CLI.Tmux == nil {
		return nil, fmt.Errorf("profile %s: tmux config is required", cfg.ID)
	}
	if client == nil {
		return nil, fmt.Errorf("profile %s: daemon client is required", cfg.ID)
	}
	execution, err := daemonExecution(cfg)
	if err != nil {
		return nil, err
	}
	tmux := cfg.CLI.Tmux
	readyTimeout := tmux.ReadyTimeoutSeconds
	if tmux.SessionReadyTimeoutSeconds > 0 {
		readyTimeout = tmux.SessionReadyTimeoutSeconds
	}
	readySettle := tmux.SessionReadySettleSeconds
	if readySettle <= 0 {
		readySettle = tmux.PromptReadySettleSeconds
	}
	return providertmux.New(providertmux.Config{
		Daemon:        client,
		SessionName:   tmux.SessionName,
		PollInterval:  seconds(tmux.PollIntervalSeconds),
		ReadyTimeout:  seconds(readyTimeout),
		ReadySettle:   seconds(readySettle),
		StableTimeout: seconds(tmux.PromptStableTimeoutSeconds),
		Depends:       daemonDependencies(cfg),
		Execution:     execution,
	}), nil
}

func requiresDaemon(cfg Config) bool {
	return len(cfg.Depends) > 0 || executionConfigured(cfg.Execution)
}

func daemonDependencies(cfg Config) []daemon.Dependency {
	dependencies := make([]daemon.Dependency, 0, len(cfg.Depends))
	for _, dependency := range cfg.Depends {
		dependencies = append(dependencies, daemon.Dependency{
			Command: dependency.Command, WaitTCP: dependency.WaitTCP, WaitHTTP: dependency.WaitHTTP,
			Restart: dependency.Restart, Silent: dependency.Silent, Optional: dependency.Optional,
		})
	}
	return dependencies
}

func daemonExecution(cfg Config) (daemon.ExecutionEnvironment, error) {
	execution := daemon.ExecutionEnvironment{
		AuditProxy: cfg.Execution.AuditProxy, Bypass: append([]string(nil), cfg.Execution.Bypass...),
		Shim: cfg.Execution.Shim, Dylib: ExpandEnv(cfg.Execution.Dylib),
	}
	for _, name := range cfg.Execution.UpstreamProxyEnv {
		value := strings.TrimSpace(os.Getenv(name))
		if value == "" {
			return daemon.ExecutionEnvironment{}, fmt.Errorf("profile %s: upstream proxy environment variable is not set: %s", cfg.ID, name)
		}
		execution.Upstreams = append(execution.Upstreams, value)
	}
	return execution, nil
}

func TmuxShellCommand(cfg Config, extra map[string]string) string {
	command := cfg.CLI.Command
	argv := []string{command.Binary}
	for _, arg := range command.Args {
		argv = append(argv, ExpandEnv(arg))
	}
	if command.Model != "" {
		argv = append(argv, "--model", command.Model)
	}
	return tmuxShellCommandArgv(cfg, argv, extra)
}

func TmuxCommandEnv(cfg Config, command string) string {
	values := tmuxEnvironment(cfg, nil)
	if len(values) == 0 {
		return command
	}
	return strings.Join(append(environmentParts(values), command), " ")
}

func ShellQuote(value string) string {
	if value == "" {
		return "''"
	}
	return "'" + strings.ReplaceAll(value, "'", "'\"'\"'") + "'"
}

func tmuxShellCommandArgv(cfg Config, argv []string, extra map[string]string) string {
	parts := []string{"exec"}
	values := tmuxEnvironment(cfg, extra)
	if len(values) > 0 {
		parts = append(parts, environmentParts(values)...)
	}
	for _, arg := range argv {
		parts = append(parts, ShellQuote(arg))
	}
	return strings.Join(parts, " ")
}

func tmuxEnvironment(cfg Config, extra map[string]string) map[string]string {
	command := cfg.CLI.Command
	values := make(map[string]string, len(command.Env)+len(command.EnvPassthrough)+len(extra))
	for key, value := range command.Env {
		values[key] = ExpandEnv(value)
	}
	for _, key := range command.EnvPassthrough {
		if _, exists := values[key]; !exists {
			values[key] = os.Getenv(key)
		}
	}
	for key, value := range extra {
		values[key] = value
	}
	return values
}

func environmentParts(values map[string]string) []string {
	keys := make([]string, 0, len(values))
	for key := range values {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	parts := []string{"env"}
	for _, key := range keys {
		parts = append(parts, ShellQuote(key+"="+values[key]))
	}
	return parts
}

func seconds(value float64) time.Duration {
	if value <= 0 {
		return 0
	}
	return time.Duration(value * float64(time.Second))
}
