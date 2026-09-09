package model

import "testing"

func TestCodexProfileDestinationAndCapabilities(t *testing.T) {
	for _, endpoint := range []string{OpenAICodexEndpoint, "https://chatgpt.com/backend-api/codex/responses?key=secret", "https://chatgpt.com:444/backend-api/codex/responses", "https://chatgpt.com.untrusted.invalid/backend-api/codex/responses", "https://api.openai.com/v1/responses", "http://chatgpt.com/backend-api/codex/responses"} {
		profile := Profile{Driver: DriverOpenAICodexResponses, Endpoint: endpoint, Model: "fixture", Timeout: "1m"}
		if err := profile.Validate(); (err == nil) != (endpoint == OpenAICodexEndpoint) {
			t.Fatalf("endpoint=%s err=%v", endpoint, err)
		}
	}
	profile := Profile{Driver: DriverOpenAICodexResponses, BaseURL: "https://chatgpt.com/backend-api", Model: "fixture", Timeout: "1m"}
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := profile.ValidateManagedExecution(); err == nil {
		t.Fatal("managed execution accepted")
	}
	for _, option := range []string{"headers", "stream", "max_tokens", "top_p", "stop"} {
		modified := profile
		switch option {
		case "headers":
			modified.Headers = map[string]string{"Authorization": "${API_KEY}"}
		case "stream":
			value := false
			modified.Parameters.Stream = &value
		case "max_tokens":
			value := int64(100)
			modified.Parameters.MaxTokens = &value
		case "top_p":
			value := 0.5
			modified.Parameters.TopP = &value
		case "stop":
			modified.Parameters.StopSequences = []string{"END"}
		}
		if err := modified.Validate(); err == nil {
			t.Fatalf("accepted %s", option)
		}
	}
}
