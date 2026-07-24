// Package runtimeclient 提供 runtimeapi.Client 的 HTTP 实现。
package runtimeclient

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"strings"

	"github.com/yy003x/runtime/runtimeapi"
)

type Options struct {
	BaseURL     string
	BearerToken string
	HTTPClient  *http.Client
}

type Client struct {
	baseURL     string
	bearerToken string
	httpClient  *http.Client
}

func New(options Options) (*Client, error) {
	baseURL := strings.TrimRight(strings.TrimSpace(options.BaseURL), "/")
	if baseURL == "" {
		return nil, fmt.Errorf("base URL is required")
	}
	parsed, err := url.Parse(baseURL)
	if err != nil || parsed.Host == "" || parsed.RawQuery != "" || parsed.Fragment != "" ||
		parsed.Scheme != "http" && parsed.Scheme != "https" {
		return nil, fmt.Errorf("base URL must be an absolute HTTP(S) URL without query or fragment")
	}
	client := options.HTTPClient
	if client == nil {
		client = http.DefaultClient
	}
	return &Client{baseURL: baseURL, bearerToken: strings.TrimSpace(options.BearerToken), httpClient: client}, nil
}

func (c *Client) Generate(ctx context.Context, request runtimeapi.Request) (runtimeapi.Response, error) {
	body, err := json.Marshal(request)
	if err != nil {
		return runtimeapi.Response{}, fmt.Errorf("marshal LLM runtime request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/llm/generate", bytes.NewReader(body))
	if err != nil {
		return runtimeapi.Response{}, fmt.Errorf("build LLM runtime request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	if c.bearerToken != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}
	httpResponse, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return runtimeapi.Response{}, fmt.Errorf("call LLM runtime: %w", err)
	}
	defer httpResponse.Body.Close()
	raw, err := io.ReadAll(io.LimitReader(httpResponse.Body, 8<<20))
	if err != nil {
		return runtimeapi.Response{}, fmt.Errorf("read LLM runtime response: %w", err)
	}
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		var failure struct {
			Error string `json:"error"`
		}
		_ = json.Unmarshal(raw, &failure)
		if failure.Error == "" {
			failure.Error = strings.TrimSpace(string(raw))
		}
		return runtimeapi.Response{}, fmt.Errorf("LLM runtime HTTP %d: %s", httpResponse.StatusCode, failure.Error)
	}
	var response runtimeapi.Response
	if err := json.Unmarshal(raw, &response); err != nil {
		return runtimeapi.Response{}, fmt.Errorf("decode LLM runtime response: %w", err)
	}
	return response, nil
}

func (c *Client) GenerateStream(ctx context.Context, request runtimeapi.Request, sink runtimeapi.EventSink) (runtimeapi.Response, error) {
	if sink == nil {
		return runtimeapi.Response{}, fmt.Errorf("event sink is required")
	}
	body, err := json.Marshal(request)
	if err != nil {
		return runtimeapi.Response{}, fmt.Errorf("marshal LLM runtime request: %w", err)
	}
	httpRequest, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/llm/generate", bytes.NewReader(body))
	if err != nil {
		return runtimeapi.Response{}, fmt.Errorf("build LLM runtime stream request: %w", err)
	}
	httpRequest.Header.Set("Content-Type", "application/json")
	httpRequest.Header.Set("Accept", "text/event-stream")
	if c.bearerToken != "" {
		httpRequest.Header.Set("Authorization", "Bearer "+c.bearerToken)
	}
	httpResponse, err := c.httpClient.Do(httpRequest)
	if err != nil {
		return runtimeapi.Response{}, fmt.Errorf("call LLM runtime stream: %w", err)
	}
	defer httpResponse.Body.Close()
	if httpResponse.StatusCode < 200 || httpResponse.StatusCode >= 300 {
		raw, readErr := io.ReadAll(io.LimitReader(httpResponse.Body, 64<<10))
		if readErr != nil {
			return runtimeapi.Response{}, fmt.Errorf("read LLM runtime stream error: %w", readErr)
		}
		return runtimeapi.Response{}, fmt.Errorf("LLM runtime HTTP %d: %s", httpResponse.StatusCode, strings.TrimSpace(string(raw)))
	}
	if !strings.HasPrefix(strings.ToLower(httpResponse.Header.Get("Content-Type")), "text/event-stream") {
		return runtimeapi.Response{}, fmt.Errorf("LLM runtime stream returned unexpected Content-Type %q", httpResponse.Header.Get("Content-Type"))
	}
	scanner := bufio.NewScanner(httpResponse.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	var response *runtimeapi.Response
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if !strings.HasPrefix(line, "data:") {
			continue
		}
		var event runtimeapi.Event
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			return runtimeapi.Response{}, fmt.Errorf("decode LLM runtime stream event: %w", err)
		}
		if err := sink(event); err != nil {
			return runtimeapi.Response{}, err
		}
		if event.Type == runtimeapi.EventError {
			return runtimeapi.Response{}, fmt.Errorf("LLM runtime stream: %s", event.Error)
		}
		if event.Type == runtimeapi.EventResponseCompleted && event.Response != nil {
			value := *event.Response
			response = &value
		}
	}
	if err := scanner.Err(); err != nil {
		return runtimeapi.Response{}, fmt.Errorf("read LLM runtime stream: %w", err)
	}
	if response == nil {
		return runtimeapi.Response{}, fmt.Errorf("LLM runtime stream ended without response.completed")
	}
	return *response, nil
}

var _ runtimeapi.Client = (*Client)(nil)
var _ runtimeapi.StreamingClient = (*Client)(nil)
