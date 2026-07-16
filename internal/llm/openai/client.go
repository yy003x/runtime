package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agent-runtime/internal/llm"
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
	Model           string           `json:"model"`
	Messages        []messagePayload `json:"messages"`
	Tools           []toolPayload    `json:"tools,omitempty"`
	Temperature     float64          `json:"temperature,omitempty"`
	MaxOutputTokens int              `json:"max_tokens,omitempty"`
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
	payload, err := json.Marshal(requestBody{
		Model:           req.Model,
		Messages:        buildMessages(req),
		Tools:           buildTools(req.Tools),
		Temperature:     req.Temperature,
		MaxOutputTokens: req.MaxOutputTokens,
	})
	if err != nil {
		return llm.Response{}, fmt.Errorf("marshal openai request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/chat/completions", bytes.NewReader(payload))
	if err != nil {
		return llm.Response{}, fmt.Errorf("build openai request: %w", err)
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
