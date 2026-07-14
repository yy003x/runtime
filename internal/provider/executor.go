package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"

	"agent-runtime/internal/daemon"
	"agent-runtime/internal/executor"
)

type Result struct {
	Stdout        string
	Stderr        string
	FinalText     string
	ExitCode      int
	PID           int
	PGID          int
	State         string
	BlockedReason string
	Detail        map[string]any
}

type ExecutionInfo struct {
	PID  int
	PGID int
}

func ExecuteCLI(ctx context.Context, cfg Config, request CLIRequest, cwd string, extraEnv map[string]string) (Result, error) {
	return ExecuteCLIWithObserver(ctx, cfg, request, cwd, extraEnv, nil)
}

func ExecuteCLIWithObserver(ctx context.Context, cfg Config, request CLIRequest, cwd string, extraEnv map[string]string, observer func(ExecutionInfo)) (Result, error) {
	return executeCLIStreaming(ctx, cfg, request, cwd, extraEnv, observer, nil, nil, nil)
}

func ExecuteCLIInteractive(ctx context.Context, cfg Config, request CLIRequest, cwd string, client *daemon.Client) (Result, error) {
	if len(request.Argv) == 0 {
		return Result{ExitCode: 1}, fmt.Errorf("profile %s: empty argv", cfg.ID)
	}
	environment := map[string]string{}
	if requiresDaemon(cfg) {
		if client == nil {
			return Result{ExitCode: 1}, fmt.Errorf("profile %s: daemon client is required", cfg.ID)
		}
		execution, err := daemonExecution(cfg)
		if err != nil {
			return Result{ExitCode: 1}, err
		}
		processID := fmt.Sprintf("interactive/%s/%d", cfg.ID, os.Getpid())
		injected, err := client.Acquire(ctx, processID, daemonDependencies(cfg), execution)
		if err != nil {
			return Result{ExitCode: 1}, err
		}
		defer func() { _ = client.Release(context.Background(), processID) }()
		environment = injected
	}
	execution, err := executor.Run(ctx, executor.Options{
		Argv: request.Argv, Env: CommandEnvironment(cfg.CLI.Command, environment), Dir: cwd,
		Interactive: true, ForwardSignals: true,
	})
	return Result{ExitCode: execution.ExitCode, PID: execution.PID, PGID: execution.PGID}, err
}

func executeCLIStreaming(
	ctx context.Context,
	cfg Config,
	request CLIRequest,
	cwd string,
	extraEnv map[string]string,
	observer func(ExecutionInfo),
	stdout func([]byte),
	stderr func([]byte),
	firstOutput func(),
) (Result, error) {
	if len(request.Argv) == 0 {
		return Result{ExitCode: 1}, fmt.Errorf("profile %s: empty argv", cfg.ID)
	}
	var stdin io.Reader
	if request.Stdin != "" {
		stdin = strings.NewReader(request.Stdin)
	}
	execution, err := executor.Run(ctx, executor.Options{
		Argv: request.Argv, Env: CommandEnvironment(cfg.CLI.Command, extraEnv), Dir: cwd, Stdin: stdin,
		ForwardSignals: true,
		Observer: executor.Observer{
			Started: func(info executor.ProcessInfo) {
				if observer != nil {
					observer(ExecutionInfo{PID: info.PID, PGID: info.PGID})
				}
			},
			Stdout: stdout, Stderr: stderr, FirstOutput: firstOutput,
		},
	})
	return Result{
		Stdout: execution.Stdout, Stderr: execution.Stderr, FinalText: strings.TrimSpace(execution.Stdout),
		ExitCode: execution.ExitCode, PID: execution.PID, PGID: execution.PGID,
	}, err
}

func ExecuteAPI(ctx context.Context, client *http.Client, cfg Config, request APIRequest) (Result, error) {
	if client == nil {
		client = http.DefaultClient
	}
	api := cfg.API
	if api.Mock {
		model := fmt.Sprint(request.EffectiveOptions["model"])
		promptLength := 0
		if request.Protocol == "anthropic" {
			if messages, ok := request.Payload["messages"].([]any); ok && len(messages) > 0 {
				if message, ok := messages[0].(map[string]any); ok {
					promptLength = len(fmt.Sprint(message["content"]))
				}
			}
		} else if messages, ok := request.Payload["messages"].([]any); ok && len(messages) > 0 {
			if message, ok := messages[0].(map[string]any); ok {
				promptLength = len(fmt.Sprint(message["content"]))
			}
		}
		text := fmt.Sprintf("[mock %s:%s] %d chars", request.Protocol, model, promptLength)
		return Result{Stdout: text + "\n", FinalText: text}, nil
	}
	key := os.Getenv(api.APIKeyEnv)
	if key == "" {
		return Result{ExitCode: 1}, fmt.Errorf("profile %s: environment %s is required", cfg.ID, api.APIKeyEnv)
	}
	payload, err := json.Marshal(request.Payload)
	if err != nil {
		return Result{ExitCode: 1}, fmt.Errorf("marshal API request: %w", err)
	}
	endpoint, err := joinURL(api.BaseURL, request.Endpoint)
	if err != nil {
		return Result{ExitCode: 1}, err
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return Result{ExitCode: 1}, fmt.Errorf("create API request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	for name, value := range api.Headers {
		expanded, ok := expandHeader(value)
		if ok {
			httpRequest.Header.Set(name, expanded)
		}
	}
	if api.Auth.Header != "" {
		httpRequest.Header.Set(api.Auth.Header, api.Auth.Prefix+key)
	} else if request.Protocol == "anthropic" {
		httpRequest.Header.Set("x-api-key", key)
		httpRequest.Header.Set("anthropic-version", "2023-06-01")
	} else {
		httpRequest.Header.Set("Authorization", "Bearer "+key)
	}
	response, err := client.Do(httpRequest)
	if err != nil {
		return Result{ExitCode: 1}, fmt.Errorf("call provider API: %w", err)
	}
	defer response.Body.Close()
	if response.StatusCode < 200 || response.StatusCode >= 300 {
		body, _ := io.ReadAll(io.LimitReader(response.Body, 1<<20))
		return Result{Stderr: string(body), ExitCode: 1}, fmt.Errorf("provider API returned HTTP %d", response.StatusCode)
	}
	if request.Stream {
		text, raw, err := readStream(response.Body, request.Protocol)
		if err != nil {
			return Result{Stdout: raw, ExitCode: 1}, err
		}
		return Result{Stdout: raw, FinalText: text}, nil
	}
	body, err := io.ReadAll(response.Body)
	if err != nil {
		return Result{ExitCode: 1}, fmt.Errorf("read provider API response: %w", err)
	}
	text, err := parseAPIText(body, request.Protocol)
	if err != nil {
		return Result{Stdout: string(body), ExitCode: 1}, err
	}
	return Result{Stdout: string(body), FinalText: text}, nil
}

func joinURL(base, endpoint string) (string, error) {
	parsed, err := url.Parse(strings.TrimRight(base, "/") + "/")
	if err != nil {
		return "", fmt.Errorf("invalid API base_url: %w", err)
	}
	parsed.Path = strings.TrimRight(parsed.Path, "/") + "/" + strings.TrimLeft(endpoint, "/")
	return parsed.String(), nil
}

func parseAPIText(body []byte, protocol string) (string, error) {
	var raw map[string]any
	if err := json.Unmarshal(body, &raw); err != nil {
		return "", fmt.Errorf("decode provider API response: %w", err)
	}
	if text, ok := raw["output_text"].(string); ok {
		return text, nil
	}
	if protocol == "anthropic" {
		return collectText(raw["content"]), nil
	}
	if choices, ok := raw["choices"].([]any); ok && len(choices) > 0 {
		if choice, ok := choices[0].(map[string]any); ok {
			if message, ok := choice["message"].(map[string]any); ok {
				if text, ok := message["content"].(string); ok {
					return text, nil
				}
			}
		}
	}
	return "", fmt.Errorf("provider API response contains no text output")
}

func collectText(value any) string {
	items, _ := value.([]any)
	var out strings.Builder
	for _, item := range items {
		object, _ := item.(map[string]any)
		if text, ok := object["text"].(string); ok {
			out.WriteString(text)
		}
	}
	return out.String()
}

func readStream(reader io.Reader, protocol string) (string, string, error) {
	scanner := bufio.NewScanner(reader)
	scanner.Buffer(make([]byte, 64*1024), 4*1024*1024)
	var text strings.Builder
	var raw strings.Builder
	for scanner.Scan() {
		line := scanner.Text()
		raw.WriteString(line)
		raw.WriteByte('\n')
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "" || data == "[DONE]" {
			continue
		}
		var event map[string]any
		if json.Unmarshal([]byte(data), &event) != nil {
			continue
		}
		if delta, ok := event["delta"].(map[string]any); ok {
			if value, ok := delta["text"].(string); ok {
				text.WriteString(value)
			}
		}
		if protocol == "openai" {
			if choices, ok := event["choices"].([]any); ok && len(choices) > 0 {
				if choice, ok := choices[0].(map[string]any); ok {
					if delta, ok := choice["delta"].(map[string]any); ok {
						if value, ok := delta["content"].(string); ok {
							text.WriteString(value)
						}
					}
				}
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return "", raw.String(), fmt.Errorf("read provider stream: %w", err)
	}
	return text.String(), raw.String(), nil
}

func expandHeader(value string) (string, bool) {
	missing := false
	expanded := os.Expand(value, func(key string) string {
		resolved, ok := os.LookupEnv(key)
		if !ok {
			missing = true
		}
		return resolved
	})
	return expanded, !missing && expanded != ""
}
