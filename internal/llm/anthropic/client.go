package anthropic

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"
	"time"

	"agent-arch/internal/llm"
)

const maxRetriesOn529 = 5

type Client struct {
	baseURL    string
	authToken  string
	httpClient *http.Client
}

type requestBody struct {
	Model       string           `json:"model"`
	System      string           `json:"system,omitempty"`
	Messages    []messagePayload `json:"messages"`
	Temperature float64          `json:"temperature,omitempty"`
	MaxTokens   int              `json:"max_tokens"`
}

type messagePayload struct {
	Role    string        `json:"role"`
	Content []contentPart `json:"content"`
}

type contentPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type responseBody struct {
	Content []contentPart `json:"content"`
	Usage   struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func NewClient(baseURL, authToken string, httpClient *http.Client) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		authToken:  authToken,
		httpClient: httpClient,
	}
}

func (c *Client) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	payload, err := json.Marshal(requestBody{
		Model:       req.Model,
		System:      req.System,
		Messages:    buildMessages(req.Messages),
		Temperature: req.Temperature,
		MaxTokens:   req.MaxOutputTokens,
	})
	if err != nil {
		return llm.Response{}, fmt.Errorf("marshal anthropic request: %w", err)
	}

	var raw []byte
	var statusCode int

	for attempt := 1; attempt <= maxRetriesOn529; attempt++ {
		httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/v1/messages", bytes.NewReader(payload))
		if err != nil {
			return llm.Response{}, fmt.Errorf("build anthropic request: %w", err)
		}
		httpReq.Header.Set("x-api-key", c.authToken)
		httpReq.Header.Set("Authorization", "Bearer "+c.authToken)
		httpReq.Header.Set("anthropic-version", "2023-06-01")
		httpReq.Header.Set("Content-Type", "application/json")

		httpResp, err := c.httpClient.Do(httpReq)
		if err != nil {
			return llm.Response{}, fmt.Errorf("send anthropic request: %w", err)
		}

		raw, err = io.ReadAll(httpResp.Body)
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
	for _, item := range parsed.Content {
		if item.Type == "text" {
			parts = append(parts, item.Text)
		}
	}

	return llm.Response{
		OutputText:   strings.TrimSpace(strings.Join(parts, "\n")),
		InputTokens:  parsed.Usage.InputTokens,
		OutputTokens: parsed.Usage.OutputTokens,
	}, nil
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
		out = append(out, messagePayload{
			Role: msg.Role,
			Content: []contentPart{{
				Type: "text",
				Text: msg.Content,
			}},
		})
	}
	return out
}
