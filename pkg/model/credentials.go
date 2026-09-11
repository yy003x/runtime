package model

import (
	"context"
	"fmt"
	"net/http"
	"strings"

	"github.com/yy003x/runtime/pkg/contract"
)

// Credentials is an ephemeral OAuth identity. It must never be persisted in a
// Profile, execution snapshot, result, or diagnostic. The resolver owns storage
// and token refresh; a Driver only receives the resulting request headers.
type Credentials struct {
	AccessToken string
	AccountID   string
}

// CredentialResolver resolves a current credential, refreshing if necessary.
// rejectedAccessToken is nonempty only after a 401 and requests one conditional
// refresh: reuse a concurrently refreshed token instead of rotating it again.
// Errors must contain no credentials or raw authentication response bodies.
type CredentialResolver func(ctx context.Context, driver DriverName, rejectedAccessToken string) (Credentials, error)

type credentialCallKey struct{}

// Only shared within one logical call; separate calls cannot switch each
// other's refresh state or account binding.
type credentialCall struct {
	token, accountID, rejected string
	refreshed                  bool
	secrets                    []string
}

func (service *Service) resolveCredentials(ctx context.Context, resolved *ResolvedModel) ([]string, error) {
	if resolved.Driver != DriverOpenAICodexResponses {
		return nil, nil
	}
	if service.credentials == nil {
		return nil, fmt.Errorf("OpenAI Codex credentials are unavailable; run sn-cli auth login openai-codex")
	}
	state, _ := ctx.Value(credentialCallKey{}).(*credentialCall)
	rejected := ""
	if state != nil {
		rejected = state.rejected
	}
	credential, err := service.credentials(ctx, resolved.Driver, rejected)
	if err != nil {
		return nil, err
	}
	if credential.AccessToken == "" || credential.AccountID == "" ||
		!validSecretValue(credential.AccessToken) || !validSecretValue(credential.AccountID) {
		return nil, fmt.Errorf("OpenAI Codex credentials are invalid; run sn-cli auth login openai-codex")
	}
	secrets := []string{credential.AccessToken, credential.AccountID}
	if state != nil {
		if state.accountID != "" && state.accountID != credential.AccountID {
			return nil, fmt.Errorf("OpenAI Codex account changed during the call; start a new call")
		}
		state.token, state.accountID, state.rejected = credential.AccessToken, credential.AccountID, ""
		state.secrets = append(state.secrets, secrets...)
		secrets = append([]string(nil), state.secrets...)
	}
	headers := map[string]string{
		"Authorization":      "Bearer " + credential.AccessToken,
		"Chatgpt-Account-Id": credential.AccountID,
	}
	resolved.headers = func() map[string]string { return cloneHeaders(headers) }
	resolved.logHeaders = func() map[string]string {
		return map[string]string{"Authorization": "[REDACTED]", "Chatgpt-Account-Id": "[REDACTED]"}
	}
	return secrets, nil
}

func cloneHeaders(headers map[string]string) map[string]string {
	cloned := make(map[string]string, len(headers))
	for name, value := range headers {
		cloned[http.CanonicalHeaderKey(name)] = value
	}
	return cloned
}

// ValidateSingleCallInput enforces canonical input capabilities and Codex OAuth
// constraints before credential resolution can perform a token refresh.
func (profile Profile) ValidateSingleCallInput(input contract.ModelRequest) error {
	if input.HasContentParts() &&
		(profile.Driver == DriverOpenAIResponses || profile.Driver == DriverOpenAICodexResponses) {
		return fmt.Errorf("%s does not support content_parts; use openai-chat-completions or anthropic-messages", profile.Driver)
	}
	if profile.Driver == DriverOpenAICodexResponses &&
		input.Options.Stream != nil && !*input.Options.Stream {
		return fmt.Errorf("openai-codex-responses requires options.stream=true when supplied")
	}
	if profile.Driver == DriverAnthropicMessages && anthropicThinkingEnabled(contract.MergeExtraBody(profile.Parameters.ExtraBody, input.Options.ExtraBody)) {
		for _, message := range input.Messages {
			if message.Role != contract.RoleUser {
				return fmt.Errorf("anthropic-messages thinking requires native thinking-block history for assistant/tool continuation")
			}
		}
	}
	if profile.Driver != DriverOpenAICodexResponses {
		return nil
	}
	if err := validateCodexExtraBody(input.Options.ExtraBody); err != nil {
		return err
	}
	if input.Options.MaxOutputTokens != nil || input.Options.TopP != nil || len(input.Options.StopSequences) != 0 {
		return fmt.Errorf("openai-codex-responses does not support max_output_tokens, top_p, or stop_sequences")
	}
	for _, message := range input.Messages {
		if message.Role != contract.RoleUser {
			return fmt.Errorf("openai-codex-responses single calls accept user messages only")
		}
	}
	return nil
}

func validateCodexExtraBody(fields contract.ExtraBody) error {
	if value, exists := fields["store"]; exists && strings.TrimSpace(string(value)) != "false" {
		return fmt.Errorf("openai-codex-responses requires extra_body.store=false when supplied")
	}
	return nil
}
