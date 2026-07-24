package openai

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

	"github.com/yy003x/runtime/internal/llm"
)

type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
	options    llm.HTTPOptions
}

type messagePayload struct {
	Role       string            `json:"role"`
	Content    string            `json:"content,omitempty"`
	ToolCallID string            `json:"tool_call_id,omitempty"`
	ToolCalls  []toolCallPayload `json:"tool_calls,omitempty"`
}

type toolCallPayload struct {
	ID       string          `json:"id,omitempty"`
	Type     string          `json:"type"`
	Function functionPayload `json:"function"`
}

type functionPayload struct {
	Name      string `json:"name"`
	Arguments string `json:"arguments"`
}

type toolPayload struct {
	Type     string `json:"type"`
	Function struct {
		Name        string         `json:"name"`
		Description string         `json:"description,omitempty"`
		Parameters  map[string]any `json:"parameters"`
	} `json:"function"`
}

type requestBody struct {
	Model       string           `json:"model"`
	Messages    []messagePayload `json:"messages"`
	Tools       []toolPayload    `json:"tools,omitempty"`
	Temperature float64          `json:"temperature,omitempty"`
	MaxTokens   int              `json:"max_tokens,omitempty"`
	Stream      bool             `json:"stream,omitempty"`
}

type streamChunk struct {
	Choices []struct {
		Delta struct {
			Content   string `json:"content"`
			ToolCalls []struct {
				Index    int    `json:"index"`
				ID       string `json:"id,omitempty"`
				Function struct {
					Name      string `json:"name,omitempty"`
					Arguments string `json:"arguments,omitempty"`
				} `json:"function"`
			} `json:"tool_calls,omitempty"`
		} `json:"delta"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		InputTokens  int `json:"prompt_tokens"`
		OutputTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

type responseBody struct {
	Choices []struct {
		Message struct {
			Content   string            `json:"content"`
			ToolCalls []toolCallPayload `json:"tool_calls"`
		} `json:"message"`
		FinishReason string `json:"finish_reason"`
	} `json:"choices"`
	Usage struct {
		InputTokens  int `json:"prompt_tokens"`
		OutputTokens int `json:"completion_tokens"`
	} `json:"usage"`
}

func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	return NewClientWithOptions(baseURL, apiKey, httpClient, llm.HTTPOptions{})
}

func NewClientWithOptions(baseURL, apiKey string, httpClient *http.Client, options llm.HTTPOptions) *Client {
	if httpClient == nil {
		httpClient = http.DefaultClient
	}
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: httpClient,
		options:    options,
	}
}

func (c *Client) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	httpReq, err := c.buildRequest(ctx, req, false)
	if err != nil {
		return llm.Response{}, err
	}
	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return llm.Response{}, fmt.Errorf("send openai request: %w", err)
	}
	defer httpResp.Body.Close()

	limit := int64(8 << 20)
	if httpResp.StatusCode >= 300 {
		limit = 64 << 10
	}
	raw, err := readLimited(httpResp.Body, limit)
	if err != nil {
		return llm.Response{}, fmt.Errorf("read openai response: %w", err)
	}
	if httpResp.StatusCode >= 300 {
		return llm.Response{}, fmt.Errorf("openai status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(raw)))
	}

	var parsed responseBody
	if err := json.Unmarshal(raw, &parsed); err != nil {
		return llm.Response{}, fmt.Errorf("decode openai response: %w", err)
	}

	if len(parsed.Choices) == 0 {
		return llm.Response{}, fmt.Errorf("decode openai response: choices is empty")
	}
	choice := parsed.Choices[0]
	toolCalls, err := parseToolCalls(choice.Message.ToolCalls)
	if err != nil {
		return llm.Response{}, err
	}

	return llm.Response{
		OutputText:   strings.TrimSpace(choice.Message.Content),
		ToolCalls:    toolCalls,
		FinishReason: choice.FinishReason,
		Done:         len(toolCalls) == 0,
		InputTokens:  parsed.Usage.InputTokens,
		OutputTokens: parsed.Usage.OutputTokens,
	}, nil
}

func (c *Client) GenerateStream(ctx context.Context, req llm.Request, emit func(llm.StreamEvent) error) (llm.Response, error) {
	httpReq, err := c.buildRequest(ctx, req, true)
	if err != nil {
		return llm.Response{}, err
	}
	httpReq.Header.Set("Accept", "text/event-stream")
	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return llm.Response{}, fmt.Errorf("send openai stream request: %w", err)
	}
	defer httpResp.Body.Close()
	if httpResp.StatusCode >= 300 {
		raw, readErr := readLimited(httpResp.Body, 64<<10)
		if readErr != nil {
			return llm.Response{}, fmt.Errorf("read openai stream error: %w", readErr)
		}
		return llm.Response{}, fmt.Errorf("openai status %d: %s", httpResp.StatusCode, strings.TrimSpace(string(raw)))
	}

	type partialToolCall struct {
		id        string
		name      string
		arguments strings.Builder
	}
	partial := make(map[int]*partialToolCall)
	var output strings.Builder
	response := llm.Response{}
	scanner := bufio.NewScanner(httpResp.Body)
	scanner.Buffer(make([]byte, 64<<10), 8<<20)
	for scanner.Scan() {
		line := strings.TrimSpace(scanner.Text())
		if line == "" || strings.HasPrefix(line, ":") || !strings.HasPrefix(line, "data:") {
			continue
		}
		data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
		if data == "[DONE]" {
			break
		}
		var chunk streamChunk
		if err := json.Unmarshal([]byte(data), &chunk); err != nil {
			return llm.Response{}, fmt.Errorf("decode openai stream event: %w", err)
		}
		response.InputTokens = chunk.Usage.InputTokens
		response.OutputTokens = chunk.Usage.OutputTokens
		for _, choice := range chunk.Choices {
			if choice.Delta.Content != "" {
				output.WriteString(choice.Delta.Content)
				if emit != nil {
					if err := emit(llm.StreamEvent{Delta: choice.Delta.Content}); err != nil {
						return llm.Response{}, err
					}
				}
			}
			for _, call := range choice.Delta.ToolCalls {
				value := partial[call.Index]
				if value == nil {
					value = &partialToolCall{}
					partial[call.Index] = value
				}
				if call.ID != "" {
					value.id = call.ID
				}
				if call.Function.Name != "" {
					value.name += call.Function.Name
				}
				value.arguments.WriteString(call.Function.Arguments)
			}
			if choice.FinishReason != "" {
				response.FinishReason = choice.FinishReason
			}
		}
	}
	if err := scanner.Err(); err != nil {
		return llm.Response{}, fmt.Errorf("read openai stream: %w", err)
	}
	response.OutputText = strings.TrimSpace(output.String())
	indexes := make([]int, 0, len(partial))
	for index := range partial {
		indexes = append(indexes, index)
	}
	sort.Ints(indexes)
	for _, index := range indexes {
		value := partial[index]
		arguments := map[string]any{}
		if raw := strings.TrimSpace(value.arguments.String()); raw != "" {
			if err := json.Unmarshal([]byte(raw), &arguments); err != nil {
				return llm.Response{}, fmt.Errorf("decode openai stream tool arguments for %s: %w", value.name, err)
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
		Messages:    buildMessages(req),
		Tools:       buildTools(req.Tools),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxTokens,
		Stream:      stream,
	})
	if err != nil {
		return nil, fmt.Errorf("marshal openai request: %w", err)
	}

	endpoint, err := llm.ResolveCompatibleEndpoint(c.baseURL, "chat/completions")
	if err != nil {
		return nil, err
	}
	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, endpoint, bytes.NewReader(payload))
	if err != nil {
		return nil, fmt.Errorf("build openai request: %w", err)
	}
	httpReq.Header.Set("Content-Type", "application/json")
	for name, value := range c.options.Headers {
		httpReq.Header.Set(name, value)
	}
	authHeader := c.options.AuthHeader
	if authHeader == "" {
		authHeader = "Authorization"
	}
	authPrefix := c.options.AuthPrefix
	if c.options.AuthHeader == "" && authPrefix == "" {
		authPrefix = "Bearer "
	}
	httpReq.Header.Set(authHeader, authPrefix+c.apiKey)
	return httpReq, nil
}

func buildMessages(req llm.Request) []messagePayload {
	messages := make([]messagePayload, 0, len(req.Messages)+1)
	if strings.TrimSpace(req.System) != "" {
		messages = append(messages, messagePayload{Role: "system", Content: req.System})
	}

	for _, msg := range req.Messages {
		item := messagePayload{Role: msg.Role, Content: msg.Content, ToolCallID: msg.ToolCallID}
		for _, call := range msg.ToolCalls {
			arguments, _ := json.Marshal(call.Arguments)
			item.ToolCalls = append(item.ToolCalls, toolCallPayload{
				ID: call.ID, Type: "function", Function: functionPayload{Name: call.Name, Arguments: string(arguments)},
			})
		}
		messages = append(messages, item)
	}

	return messages
}

func buildTools(tools []llm.Tool) []toolPayload {
	result := make([]toolPayload, 0, len(tools))
	for _, tool := range tools {
		item := toolPayload{Type: "function"}
		item.Function.Name = tool.Name
		item.Function.Description = tool.Description
		item.Function.Parameters = tool.Parameters
		result = append(result, item)
	}
	return result
}

func parseToolCalls(calls []toolCallPayload) ([]llm.ToolCall, error) {
	result := make([]llm.ToolCall, 0, len(calls))
	for _, call := range calls {
		arguments := map[string]any{}
		if strings.TrimSpace(call.Function.Arguments) != "" {
			if err := json.Unmarshal([]byte(call.Function.Arguments), &arguments); err != nil {
				return nil, fmt.Errorf("decode openai tool arguments for %s: %w", call.Function.Name, err)
			}
		}
		result = append(result, llm.ToolCall{ID: call.ID, Name: call.Function.Name, Arguments: arguments})
	}
	return result, nil
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
