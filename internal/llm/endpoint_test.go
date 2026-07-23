package llm

import "testing"

func TestResolveCompatibleEndpoint(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		resource string
		want     string
	}{
		{
			name:     "openai adds v1",
			baseURL:  "https://api.example.test",
			resource: "chat/completions",
			want:     "https://api.example.test/v1/chat/completions",
		},
		{
			name:     "openai preserves prefixed v1",
			baseURL:  "https://api.example.test/compatible-mode/v1/",
			resource: "/chat/completions",
			want:     "https://api.example.test/compatible-mode/v1/chat/completions",
		},
		{
			name:     "anthropic adds v1 after application prefix",
			baseURL:  "https://workspace.example.test/apps/anthropic",
			resource: "messages",
			want:     "https://workspace.example.test/apps/anthropic/v1/messages",
		},
		{
			name:     "anthropic preserves explicit version",
			baseURL:  "https://api.example.test/api/v2",
			resource: "messages",
			want:     "https://api.example.test/api/v2/messages",
		},
		{
			name:     "full endpoint is idempotent",
			baseURL:  "https://api.example.test/v1/chat/completions/",
			resource: "chat/completions",
			want:     "https://api.example.test/v1/chat/completions",
		},
		{
			name:     "query is preserved",
			baseURL:  "https://api.example.test/v1?api-version=2026-01-01",
			resource: "chat/completions",
			want:     "https://api.example.test/v1/chat/completions?api-version=2026-01-01",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			got, err := ResolveCompatibleEndpoint(test.baseURL, test.resource)
			if err != nil {
				t.Fatal(err)
			}
			if got != test.want {
				t.Fatalf("endpoint=%q want=%q", got, test.want)
			}
		})
	}
}

func TestResolveCompatibleEndpointRejectsInvalidInput(t *testing.T) {
	tests := []struct {
		name     string
		baseURL  string
		resource string
	}{
		{name: "relative base", baseURL: "api.example.test/v1", resource: "messages"},
		{name: "unsupported scheme", baseURL: "file:///tmp/api", resource: "messages"},
		{name: "fragment", baseURL: "https://api.example.test/v1#fragment", resource: "messages"},
		{name: "empty resource", baseURL: "https://api.example.test/v1", resource: ""},
		{name: "resource traversal", baseURL: "https://api.example.test/v1", resource: "../messages"},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			if _, err := ResolveCompatibleEndpoint(test.baseURL, test.resource); err == nil {
				t.Fatal("ResolveCompatibleEndpoint returned nil error")
			}
		})
	}
}
