package anthropic

import (
	"bufio"
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/yy003x/runtime/internal/llm"
)

const maxRetriesOn529 = 5

type Client struct {
	baseURL    string
	authToken  string
	httpClient *http.Client
	options    llm.HTTPOptions
}

type requestBody struct {
	Model       string           `json:"model"`
	System      string           `json:"system,omitempty"`
	Messages    []messagePayload `json:"messages"`
	Tools       []toolPayload    `json:"tools,omitempty"`
	Temperature float64          `json:"temperature,omitempty"`
	MaxTokens   int              `json:"max_tokens"`
	Stream      bool             `json:"stream,omitempty"`
}

type streamEvent struct {
	Type    string `json:"type"`
	Index   int    `json:"index"`
	Message struct {
		Usage struct {
			InputTokens int `json:"input_tokens"`
		} `json:"usage"`
	} `json:"message"`
	ContentBlock struct {
		Type  string         `json:"type"`
		Text  string         `json:"text,omitempty"`
		ID    string         `json:"id,omitempty"`
		Name  string         `json:"name,omitempty"`
		Input map[string]any `json:"input,omitempty"`
	} `json:"content_block"`
	Delta struct {
		Type        string `json:"type"`
		Text        string `json:"text,omitempty"`
		PartialJSON string `json:"partial_json,omitempty"`
		StopReason  string `json:"stop_reason,omitempty"`
	} `json:"delta"`
	Usage struct {
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

type messagePayload struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type      string         `json:"type"`
	Text      string         `json:"text,omitempty"`
	ID        string         `json:"id,omitempty"`
	Name      string         `json:"name,omitempty"`
	Input     map[string]any `json:"input,omitempty"`
	ToolUseID string         `json:"tool_use_id,omitempty"`
	Content   string         `json:"content,omitempty"`
}

type toolPayload struct {
	Name        string         `json:"name"`
	Description string         `json:"description,omitempty"`
	InputSchema map[string]any `json:"input_schema"`
}

type responseBody struct {
	Content    []contentPart `json:"content"`
	StopReason string        `json:"stop_reason"`
	Usage      struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func NewClient(baseURL, authToken string, httpClient *http.Client) *Client {
	return NewClientWithOptions(baseURL, authToken, httpClient, llm.HTTPOptions{})
}

func NewClientWithOptions(baseURL, authToken string, httpClient *http.Client, options llm.HTTPOptions) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		authToken:  authToken,
		httpClient: httpClient,
		options:    options,
	}
}

func (c *Client) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	var raw []byte
	var statusCode int

	for attempt := 1; attempt <= maxRetriesOn529; attempt++ {
		httpReq, err := c.buildRequest(ctx, req, false)
		if err != nil {
			return llm.Response{}, err
		}
		httpResp, err := c.httpClient.Do(httpReq)
		if err != nil {
			return llm.Response{}, fmt.Errorf("send anthropic request: %w", err)
		}

		limit := int64(8 << 20)
		if httpResp.StatusCode >= 300 {
			limit = 64 << 10
		}
		raw, err = readLimited(httpResp.Body, limit)
		httpResp.Body.Close()
		if err != nil {
			return llm.Response{}, fmt.Errorf("read anthropic response: %w", err)
		}

		statusCode = httpResp.StatusCode
		if statusCode != http.StatusTooManyRequests && statusCode != 529 {
			break
		}
		if attempt == maxRetriesOn529 {
			break
		}

		if err := sleepWithContext(ctx, retryBackoff(attempt)); err != nil {
			return llm.Response{}, fmt.Errorf("wait before anthropic retry: %w", err)
		}
	}

	if statusCode >= 300 {
		return llm.Response{}, fmt.Errorf("anthropic status %d: %s", statusCode, strings.TrimSpace(string(raw)))
	}

	var parsed responseBody
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return llm.Response{}, fmt.Errorf("decode anthropic response: %w", err)
	}

	var parts []string
	var toolCalls []llm.ToolCall
	for _, item := range parsed.Content {
		if item.Type == "text" {
			parts = append(parts, item.Text)
		} else if item.Type == "tool_use" {
			toolCalls = append(toolCalls, llm.ToolCall{ID: item.ID, Name: item.Name, Arguments: item.Input})
		}
	}

	return llm.Response{
		OutputText:   strings.TrimSpace(strings.Join(parts, "\n")),
		ToolCalls:    toolCalls,
		FinishReason: parsed.StopReason,
		Done:         len(toolCalls) == 0,
		InputTokens:  parsed.Usage.InputTokens,
		OutputTokens: parsed.Usage.OutputTokens,
	}, nil
}

func (c *Client) GenerateStream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) (llm.Response, error) {
	var httpResp *http.Response
	for attempt := 1; attempt <= maxRetriesOn529; attempt++ {
		httpReq, err := c.buildRequest(ctx, req, true)
		if err != nil {
			return llm.Response{}, err
		}
		httpReq.Header.Set("Accept", "text/event-stream")
		httpResp, err = c.httpClient.Do(httpReq)
		if err != nil {
			return llm.Response{}, fmt.Errorf("send anthropic stream request: %w", err)
		}
		if httpResp.StatusCode != http.StatusTooManyRequests && httpResp.StatusCode != 529 {
			break
		}
		raw, readErr := readLimited(httpResp.Body, 64<<10)
		httpResp.Body.Close()
		if readErr != nil {
			return llm.Response{}, fmt.Errorf("read anthropic stream retry response: %w", readErr)
		}
		if attempt == maxRetriesOn529 {
			return llm.Response{}, fmt.Errorf("anthropic status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(raw)))
		}
		if err := sleepWithContext(ctx, retryBackoff(attempt)); err != nil {
			return llm.Response{}, fmt.Errorf("wait before anthropic stream retry: %w", err)
		}
	}
	if httpResp == nil {
		return llm.Response{}, fmt.Errorf("anthropic stream response is unavailable")
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 300 {
		raw, err := readLimited(httpResp.Body, 64<<10)
		if err != nil {
			return llm.Response{}, fmt.Errorf("read anthropic stream error: %w", err)
		}
		return llm.Response{}, fmt.Errorf("anthropic status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(raw)))
	}

	type partialToolCall struct {
		id        string
		name      string
		input     map[string]any
		arguments strings.Builder
	}
	tools := make(map[int]*partialToolCall)
	var output strings.Builder
	response := llm.Response{}
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, "event:") || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		var event streamEvent
		if err := json.Unmarshal([]byte(strings.TrimSpace(strings.TrimPrefix(line, "data:"))), &event); err != nil {
			return llm.Response{}, fmt.Errorf("decode anthropic stream event: %w", err)
		}
		switch event.Type {
		case "message_start":
			response.InputTokens = event.Message.Usage.InputTokens
		case "content_block_start":
			if event.ContentBlock.Type == "text" && event.ContentBlock.Text != "" {
				output.WriteString(event.ContentBlock.Text)
				if emit != nil {
					if err := emit(llm.StreamEvent{Delta: event.ContentBlock.Text}); err != nil {
						return llm.Response{}, err
					}
				}
			}
			if event.ContentBlock.Type == "tool_use" {
				tools[event.Index] = &partialToolCall{
					id: event.ContentBlock.ID, name: event.ContentBlock.Name, input: event.ContentBlock.Input,
				}
			}
		case "content_block_delta":
			if event.Delta.Type == "text_delta" && event.Delta.Text != "" {
				output.WriteString(event.Delta.Text)
				if emit != nil {
					if err := emit(llm.StreamEvent{Delta: event.Delta.Text}); err != nil {
						return llm.Response{}, err
					}
				}
			}
			if event.Delta.Type == "input_json_delta" {
				value := tools[event.Index]
				if value == nil {
					value = &partialToolCall{}
					tools[event.Index] = value
				}
				value.arguments.WriteString(event.Delta.PartialJSON)
			}
		case "message_delta":
			response.FinishReason = event.Delta.StopReason
			response.OutputTokens = event.Usage.OutputTokens
		}
	}
	if err := scanner.Err(); err != nil {
		return llm.Response{}, fmt.Errorf("read anthropic stream: %w", err)
	}
	response.OutputText = strings.TrimSpace(output.String())
	indexes := make([]int, 0, len(tools))
	for index := range tools {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		value := tools[index]
		arguments := value.input
		if arguments == nil {
			arguments = map[string]any{}
		}
		if raw := strings.TrimSpace(value.arguments.String()); raw != "" {
			if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
				return llm.Response{}, fmt.Errorf("decode anthropic stream tool arguments for %s: %w", value.name, err)
			}
		}
		response.ToolCalls = append(response.ToolCalls, llm.ToolCall{ID: value.id, Name: value.name, Arguments: arguments})
	}
	response.Done = len(response.ToolCalls) == 0
	return response, nil
}

func (c *Client) buildRequest(ctx context.Context, req llm.Request, stream bool) (*http.Request, error) {
	payload, err := json.Marshal(requestBody{
		Model:       req.Model,
		System:      req.System,
		Messages:    buildMessages(req.Messages),
		Tools:       buildTools(req.Tools),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      stream,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal anthropic request: %w", err)
	}
	endpoint, err := llm.ResolveCompatibleEndpoint(c.baseURL, "messages")
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build anthropic request: %w", err)
	}
	httpReq.Header.Set("anthropic-version", "2023-06-01")
	httpReq.Header.Set("Content-Type", "application/json")
	for name, value := range c.options.Headers {
		httpReq.Header.Set(name, value)
	}
	if c.options.AuthHeader != "" {
		httpReq.Header.Set(c.options.AuthHeader, c.options.AuthPrefix+c.authToken)
	} else {
		httpReq.Header.Set("x-api-key", c.authToken)
	}
	return httpReq, nil
}

func retryBackoff(attempt int) time.Duration {
	return time.Duration(attempt) * 200 * time.Millisecond
}

func sleepWithContext(ctx context.Context, wait time.Duration) error {
	timer := time.NewTimer(wait)
	defer timer.Stop()

	select {
	case <-ctx.Done():
		return ctx.Err()
	case <-timer.C:
		return nil
	}
}

func buildMessages(messages []llm.Message) []messagePayload {
	out := make([]messagePayload, 0, len(messages))
	for _, msg := range messages {
		role := msg.Role
		parts := make([]contentPart, 0, 1+len(msg.ToolCalls))
		if role == "tool" {
			role = "user"
			parts = append(parts, contentPart{Type: "tool_result", ToolUseID: msg.ToolCallID, Content: msg.Content})
		} else {
			if msg.Content != "" {
				parts = append(parts, contentPart{Type: "text", Text: msg.Content})
			}
			for _, call := range msg.ToolCalls {
				parts = append(parts, contentPart{Type: "tool_use", ID: call.ID, Name: call.Name, Input: call.Arguments})
			}
		}
		if len(out) > 0 && role == "user" && out[len(out)-1].Role == "user" && msg.Role == "tool" {
			out[len(out)-1].Content = append(out[len(out)-1].Content, parts...)
			continue
		}
		out = append(out, messagePayload{Role: role, Content: parts})
	}
	return out
}

func buildTools(tools []llm.Tool) []toolPayload {
	out := make([]toolPayload, 0, len(tools))
	for _, tool := range tools {
		out = append(out, toolPayload{Name: tool.Name, Description: tool.Description, InputSchema: tool.Parameters})
	}
	return out
}

func readLimited(reader io.Reader, limit int64) ([]byte, error) {
	raw, err := io.ReadAll(io.LimitReader(reader, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(raw)) > limit {
		return nil, fmt.Errorf("response body exceeds %d bytes", limit)
	}
	return raw, nil
}
