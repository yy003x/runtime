package provider

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"os/exec"
	"strings"
	"syscall"
	"time"
)

type Result struct {
	Stdout    string
	Stderr    string
	FinalText string
	ExitCode  int
	PID       int
	PGID      int
}

type ExecutionInfo struct {
	PID  int
	PGID int
}

func ExecuteCLI(ctx context.Context, cfg Config, request CLIRequest, cwd string, extraEnv map[string]string) (Result, error) {
	return ExecuteCLIWithObserver(ctx, cfg, request, cwd, extraEnv, nil)
}

func ExecuteCLIWithObserver(ctx context.Context, cfg Config, request CLIRequest, cwd string, extraEnv map[string]string, observer func(ExecutionInfo)) (Result, error) {
	if len(request.Argv) == 0 {
		return Result{ExitCode: 1}, fmt.Errorf("profile %s: empty argv", cfg.ID)
	}
	cmd := exec.Command(request.Argv[0], request.Argv[1:]...)
	cmd.Dir = cwd
	cmd.Env = CommandEnvironment(cfg.CLI.Command, extraEnv)
	cmd.SysProcAttr = &syscall.SysProcAttr{Setpgid: true}
	if request.Stdin != "" {
		cmd.Stdin = strings.NewReader(request.Stdin)
	}
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	if err := cmd.Start(); err != nil {
		return Result{ExitCode: 1}, fmt.Errorf("start provider command %q: %w", request.Argv[0], err)
	}
	pgid, _ := syscall.Getpgid(cmd.Process.Pid)
	if observer != nil {
		observer(ExecutionInfo{PID: cmd.Process.Pid, PGID: pgid})
	}
	done := make(chan error, 1)
	go func() { done <- cmd.Wait() }()
	var err error
	select {
	case err = <-done:
	case <-ctx.Done():
		_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGINT)
		select {
		case err = <-done:
		case <-time.After(500 * time.Millisecond):
			_ = syscall.Kill(-cmd.Process.Pid, syscall.SIGKILL)
			err = <-done
		}
		if err == nil {
			err = ctx.Err()
		}
	}
	result := Result{Stdout: stdout.String(), Stderr: stderr.String(), FinalText: strings.TrimSpace(stdout.String()), PID: cmd.Process.Pid, PGID: pgid}
	if err == nil {
		return result, nil
	}
	var exitErr *exec.ExitError
	if errors.As(err, &exitErr) {
		result.ExitCode = exitErr.ExitCode()
		return result, nil
	}
	result.ExitCode = 1
	return result, fmt.Errorf("run provider command %q: %w", request.Argv[0], err)
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
