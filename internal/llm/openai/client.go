package openai

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"strings"

	"agent-arch/internal/llm"
)

type Client struct {
	baseURL    string
	apiKey     string
	httpClient *http.Client
}

type inputPart struct {
	Type string `json:"type"`
	Text string `json:"text"`
}

type inputItem struct {
	Role    string      `json:"role"`
	Content []inputPart `json:"content"`
}

type requestBody struct {
	Model           string      `json:"model"`
	Input           []inputItem `json:"input"`
	Temperature     float64     `json:"temperature,omitempty"`
	MaxOutputTokens int         `json:"max_output_tokens,omitempty"`
}

type responseBody struct {
	OutputText string `json:"output_text"`
	Output     []struct {
		Content []struct {
			Type string `json:"type"`
			Text string `json:"text"`
		} `json:"content"`
	} `json:"output"`
	Usage struct {
		InputTokens  int `json:"input_tokens"`
		OutputTokens int `json:"output_tokens"`
	} `json:"usage"`
}

func NewClient(baseURL, apiKey string, httpClient *http.Client) *Client {
	return &Client{
		baseURL:    strings.TrimRight(baseURL, "/"),
		apiKey:     apiKey,
		httpClient: httpClient,
	}
}

func (c *Client) Generate(ctx context.Context, req llm.Request) (llm.Response, error) {
	payload, err := json.Marshal(requestBody{
		Model:           req.Model,
		Input:           buildInput(req),
		Temperature:     req.Temperature,
		MaxOutputTokens: req.MaxOutputTokens,
	})
	if err != nil {
		return llm.Response{}, fmt.Errorf("marshal openai request: %w", err)
	}

	httpReq, err := http.NewRequestWithContext(ctx, http.MethodPost, c.baseURL+"/responses", bytes.NewReader(payload))
	if err != nil {
		return llm.Response{}, fmt.Errorf("build openai request: %w", err)
	}
	httpReq.Header.Set("Authorization", "Bearer "+c.apiKey)
	httpReq.Header.Set("Content-Type", "application/json")

	httpResp, err := c.httpClient.Do(httpReq)
	if err != nil {
		return llm.Response{}, fmt.Errorf("send openai request: %w", err)
	}
	defer httpResp.Body.Close()

	raw, err := io.ReadAll(httpResp.Body)
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

	text := strings.TrimSpace(parsed.OutputText)
	if text == "" {
		var parts []string
		for _, item := range parsed.Output {
			for _, content := range item.Content {
				if content.Type == "output_text" || content.Type == "text" {
					parts = append(parts, content.Text)
				}
			}
		}
		text = strings.TrimSpace(strings.Join(parts, "\n"))
	}

	return llm.Response{
		OutputText:   text,
		InputTokens:  parsed.Usage.InputTokens,
		OutputTokens: parsed.Usage.OutputTokens,
	}, nil
}

func buildInput(req llm.Request) []inputItem {
	input := make([]inputItem, 0, len(req.Messages)+1)
	if strings.TrimSpace(req.System) != "" {
		input = append(input, inputItem{
			Role: "system",
			Content: []inputPart{{
				Type: "input_text",
				Text: req.System,
			}},
		})
	}

	for _, msg := range req.Messages {
		input = append(input, inputItem{
			Role: msg.Role,
			Content: []inputPart{{
				Type: "input_text",
				Text: msg.Content,
			}},
		})
	}

	return input
}
