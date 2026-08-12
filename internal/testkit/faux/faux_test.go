package faux

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/yy003x/runtime/internal/testkit/scenario"
)

func TestScriptedProviderScenarioMatrix(t *testing.T) {
	set := loadFixtureSet(t)
	expectedScripts := []string{
		"text", "reasoning", "single_tool", "multiple_tools", "invalid_arguments",
		"output_then_error", "abort", "timeout", "context_budget", "terminal_failure",
		"secret_redaction",
	}
	actualScripts := make([]string, 0, len(set.Scripts))
	for _, script := range set.Scripts {
		actualScripts = append(actualScripts, script.Name)
	}
	if !reflect.DeepEqual(actualScripts, expectedScripts) {
		t.Fatalf("scripts=%v", actualScripts)
	}
	provider, err := NewProvider(set.Scripts)
	if err != nil {
		t.Fatal(err)
	}

	for _, name := range []string{"text", "reasoning", "single_tool", "multiple_tools"} {
		name := name
		t.Run(name, func(t *testing.T) {
			var events []scenario.Event
			result, runtimeError := provider.Stream(context.Background(), name, func(event scenario.Event) error {
				events = append(events, event)
				return nil
			})
			if runtimeError != nil {
				t.Fatalf("error=%#v", runtimeError)
			}
			if result.FinishReason == "" {
				t.Fatalf("result=%#v", result)
			}
			if err := scenario.ValidateEvents(events); err != nil {
				t.Fatal(err)
			}
			if provider.Attempts(name) != 1 {
				t.Fatalf("attempts=%d", provider.Attempts(name))
			}
		})
	}

	t.Run("output_delta_does_not_retry", func(t *testing.T) {
		var events []scenario.Event
		_, runtimeError := provider.Stream(context.Background(), "output_then_error", func(event scenario.Event) error {
			events = append(events, event)
			return nil
		})
		if runtimeError == nil || runtimeError.Code != "provider_unavailable" {
			t.Fatalf("error=%#v", runtimeError)
		}
		if len(events) != 2 || events[1].Type != "content.delta" {
			t.Fatalf("events=%#v", events)
		}
		if provider.Attempts("output_then_error") != 1 {
			t.Fatalf("attempts=%d", provider.Attempts("output_then_error"))
		}
	})

	t.Run("typed_error_outcomes", func(t *testing.T) {
		for name, expectedCode := range map[string]string{
			"invalid_arguments": "invalid_provider_response",
			"abort":             "cancelled",
			"context_budget":    "context_overflow",
			"terminal_failure":  "protocol_error",
		} {
			var events []scenario.Event
			_, runtimeError := provider.Stream(context.Background(), name, func(event scenario.Event) error {
				events = append(events, event)
				return nil
			})
			if runtimeError == nil || runtimeError.Code != expectedCode {
				t.Fatalf("%s error=%#v", name, runtimeError)
			}
			if err := scenario.ValidateEvents(events); err != nil {
				t.Fatalf("%s events: %v", name, err)
			}
			if provider.Attempts(name) != 1 {
				t.Fatalf("%s attempts=%d", name, provider.Attempts(name))
			}
		}
	})

	t.Run("deadline_stops_delayed_script", func(t *testing.T) {
		ctx, cancel := context.WithTimeout(context.Background(), 20*time.Millisecond)
		defer cancel()
		_, runtimeError := provider.Stream(ctx, "timeout", nil)
		if runtimeError == nil || runtimeError.Code != "timeout" {
			t.Fatalf("error=%#v", runtimeError)
		}
		if provider.Attempts("timeout") != 1 {
			t.Fatalf("attempts=%d", provider.Attempts("timeout"))
		}
	})

	t.Run("secret_is_redacted_from_events_and_error", func(t *testing.T) {
		var events []scenario.Event
		_, runtimeError := provider.Stream(context.Background(), "secret_redaction", func(event scenario.Event) error {
			events = append(events, event)
			return nil
		})
		if runtimeError == nil {
			t.Fatal("expected authentication error")
		}
		data, err := json.Marshal(struct {
			Events []scenario.Event       `json:"events"`
			Error  *scenario.RuntimeError `json:"error"`
		}{Events: events, Error: runtimeError})
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(string(data), "fixture-secret") || !strings.Contains(string(data), "[REDACTED]") {
			t.Fatalf("serialized=%s", data)
		}
	})
}

func TestFauxHTTPServerMatrix(t *testing.T) {
	set := loadFixtureSet(t)
	server, err := NewServer(set.HTTP)
	if err != nil {
		t.Fatal(err)
	}
	defer server.Close()

	for _, testCase := range []struct {
		name       string
		status     int
		retryAfter string
		body       string
	}{
		{name: "fragmented_tool_arguments", status: 200, body: "Shanghai"},
		{name: "rate_limited_429", status: 429, retryAfter: "2", body: "rate limited"},
		{name: "overloaded_529", status: 529, body: "overloaded"},
		{name: "provider_500", status: 500, body: "internal"},
		{name: "invalid_sse", status: 200, body: "not-json"},
	} {
		testCase := testCase
		t.Run(testCase.name, func(t *testing.T) {
			response, err := http.Get(server.URL(testCase.name))
			if err != nil {
				t.Fatal(err)
			}
			defer response.Body.Close()
			body, err := io.ReadAll(response.Body)
			if err != nil {
				t.Fatal(err)
			}
			if response.StatusCode != testCase.status ||
				!strings.Contains(string(body), testCase.body) ||
				response.Header.Get("Retry-After") != testCase.retryAfter {
				t.Fatalf("status=%d headers=%v body=%q", response.StatusCode, response.Header, body)
			}
			if server.Attempts(testCase.name) != 1 {
				t.Fatalf("attempts=%d", server.Attempts(testCase.name))
			}
		})
	}

	t.Run("terminal_connection_failure", func(t *testing.T) {
		response, err := http.Get(server.URL("terminal_connection_failure"))
		if err == nil {
			defer response.Body.Close()
			_, err = io.ReadAll(response.Body)
		}
		if err == nil {
			t.Fatal("expected truncated connection error")
		}
		if server.Attempts("terminal_connection_failure") != 1 {
			t.Fatalf("attempts=%d", server.Attempts("terminal_connection_failure"))
		}
	})
}

func TestFauxLoaderRejectsUnknownFields(t *testing.T) {
	_, err := Load(strings.NewReader(`{
	  "schema_version": 1,
	  "scripts": [{"name":"x","steps":[],"result":{"message":{"role":"assistant"},"finish_reason":"stop"},"unknown":true}],
	  "http": [{"name":"http","status":200}]
	}`))
	if err == nil {
		t.Fatal("unknown field accepted")
	}
}

func loadFixtureSet(t *testing.T) Set {
	t.Helper()
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	set, err := LoadFile(filepath.Join(filepath.Dir(currentFile), "testdata", "provider-scenarios.json"))
	if err != nil {
		t.Fatal(err)
	}
	return set
}
