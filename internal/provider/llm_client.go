package provider

import (
	"fmt"
	"net/http"

	"github.com/yy003x/runtime/internal/llm"
	"github.com/yy003x/runtime/internal/llm/anthropic"
	"github.com/yy003x/runtime/internal/llm/openai"
)

// NewLLMClient 为 API profile 构建规范化的结构化消息 client。
// 密钥与 header 继续沿用现有 Provider 执行路径的解析规则。
func NewLLMClient(cfg Config, httpClient *http.Client) (llm.Client, error) {
	if cfg.Type != TypeAPI || cfg.API == nil {
		return nil, fmt.Errorf("profile %s is not an API profile", cfg.ID)
	}
	if cfg.API.Mock {
		return nil, fmt.Errorf("profile %s: mock API profile is not supported by structured LLM runtime", cfg.ID)
	}
	key, err := resolveAPIKey(cfg.ID, cfg.API)
	if err != nil {
		return nil, err
	}
	headers, err := resolveAPIHeaders(cfg.API.Headers)
	if err != nil {
		return nil, fmt.Errorf("profile %s: %w", cfg.ID, err)
	}
	auth := defaultAPIAuth(cfg.API.Protocol, cfg.API.BaseURL)
	options := llm.HTTPOptions{Headers: headers, AuthHeader: auth.Header, AuthPrefix: auth.Prefix}
	switch cfg.API.Protocol {
	case "openai":
		return openai.NewClientWithOptions(cfg.API.BaseURL, key, httpClient, options), nil
	case "anthropic":
		return anthropic.NewClientWithOptions(cfg.API.BaseURL, key, httpClient, options), nil
	default:
		return nil, fmt.Errorf("profile %s: unsupported structured LLM protocol %q", cfg.ID, cfg.API.Protocol)
	}
}
