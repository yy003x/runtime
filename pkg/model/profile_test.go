package model

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestGenericContextBudgetOnlyGatesManagedExecution(t *testing.T) {
	max := int64(65536)
	profile := Profile{Driver: DriverOpenAIChatCompletions, Endpoint: "https://example.invalid", Model: "any-model", Timeout: "1m", Parameters: Parameters{MaxTokens: &max}}
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := profile.ValidateManagedExecution(); err == nil {
		t.Fatal("managed execution lost context budget guard")
	}
}

func TestGenericTransportURLBoundary(t *testing.T) {
	for _, driver := range []DriverName{DriverOpenAIChatCompletions, DriverOpenAIResponses} {
		for _, endpoint := range []string{"https://example.invalid", "https://example.invalid/", "https://example.invalid?deployment=demo", "http://localhost:1234", "http://127.0.0.1:1234/", "http://[::1]:1234/native"} {
			profile := Profile{Driver: driver, Endpoint: endpoint, Model: "any-model", Timeout: "1m"}
			if err := profile.Validate(); err != nil {
				t.Fatalf("endpoint=%s error=%v", endpoint, err)
			}
		}
		for _, endpoint := range []string{"http://example.invalid/", "https://user:pass@example.invalid/", "https://example.invalid/#fragment", "file:///tmp/request", "https:///"} {
			profile := Profile{Driver: driver, Endpoint: endpoint, Model: "any-model", Timeout: "1m"}
			if err := profile.Validate(); err == nil {
				t.Fatalf("accepted unsafe endpoint %s", endpoint)
			}
		}
	}
}

func TestValidateModelProfile(t *testing.T) {
	maxTokens := int64(1024)
	oversizedDefault := int64(32_767)
	invalidTopP := 1.1
	profile := Profile{
		Driver:   DriverOpenAIChatCompletions,
		Endpoint: "https://example.invalid/v1/chat/completions",
		Model:    "fixture",
		Headers: map[string]string{
			"Authorization": "${MODEL_API_KEY}",
			"X-Client":      "runtime-test",
		},
		Parameters: Parameters{
			MaxTokens: &maxTokens,
		},
		Timeout: "5m",
	}
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if profile.Driver != DriverOpenAIChatCompletions || profile.TimeoutDuration().Minutes() != 5 {
		t.Fatalf("profile=%#v", profile)
	}
	for name, value := range map[string]Profile{
		"insecure_endpoint": {
			Driver: DriverOpenAIChatCompletions, Endpoint: "http://example.invalid/v1",
			Model: "x", Headers: map[string]string{"Authorization": "${KEY}"},
			Timeout: "1m",
		},
		"invalid_header_name": {
			Driver: DriverOpenAIChatCompletions, Endpoint: "https://example.invalid/v1",
			Model: "x", Headers: map[string]string{"Bad Header": "${KEY}"}, Timeout: "1m",
		},
		"invalid_header_value": {
			Driver: DriverOpenAIChatCompletions, Endpoint: "https://example.invalid/v1",
			Model: "x", Headers: map[string]string{"X-Client": "bad\nvalue"}, Timeout: "1m",
		},
		"missing_anthropic_token_limit": {
			Driver: DriverAnthropicMessages, Endpoint: "https://example.invalid/v1",
			Model: "x", Headers: map[string]string{"x-api-key": "${KEY}"}, Timeout: "1m",
		},
		"default_context_without_input_budget": {
			Driver: DriverAnthropicMessages, Endpoint: "https://example.invalid/v1/messages",
			Model: "x", Headers: map[string]string{"x-api-key": "${KEY}"},
			Parameters: Parameters{MaxTokens: &oversizedDefault}, Timeout: "1m",
		},
		"invalid_top_p": {
			Driver: DriverOpenAIChatCompletions, Endpoint: "https://example.invalid/v1/chat/completions",
			Model: "x", Headers: map[string]string{"Authorization": "${KEY}"},
			Parameters: Parameters{TopP: &invalidTopP}, Timeout: "1m",
		},
		"empty_stop_sequence": {
			Driver: DriverOpenAIChatCompletions, Endpoint: "https://example.invalid/v1/chat/completions",
			Model: "x", Headers: map[string]string{"Authorization": "${KEY}"},
			Parameters: Parameters{StopSequences: []string{""}}, Timeout: "1m",
		},
	} {
		if err := value.Validate(); err == nil {
			t.Fatalf("%s was accepted: %#v", name, value)
		}
	}
}

func TestLegacyDriverNamesAreRejected(t *testing.T) {
	for _, driver := range []DriverName{"openai", "anthropic"} {
		profile := Profile{
			Driver: driver, BaseURL: "https://example.invalid/v1",
			Model: "fixture", Timeout: "1m",
		}
		if err := profile.Validate(); err == nil {
			t.Fatalf("legacy driver %q was accepted", driver)
		}
	}
}

func TestParametersStreamDefaultsAndCatalogIsolation(t *testing.T) {
	for _, test := range []struct {
		name   string
		stream *bool
		want   bool
	}{
		{name: "omitted", want: true},
		{name: "true", stream: boolPointer(true), want: true},
		{name: "false", stream: boolPointer(false), want: false},
	} {
		t.Run(test.name, func(t *testing.T) {
			profile := Profile{
				Driver: DriverOpenAIChatCompletions, BaseURL: "https://example.invalid", Model: "fixture",
				Parameters: Parameters{Stream: test.stream}, Timeout: "1m",
			}
			catalog, err := NewCatalog(map[string]Profile{"fixture": profile})
			if err != nil {
				t.Fatal(err)
			}
			if test.stream != nil {
				*test.stream = !test.want
			}
			first, _ := catalog.Get("fixture")
			if first.Parameters.StreamingEnabled() != test.want {
				t.Fatal("input Profile mutation affected the catalog")
			}
			data, err := json.Marshal(first.Parameters)
			if err != nil {
				t.Fatal(err)
			}
			var fields map[string]json.RawMessage
			if err := json.Unmarshal(data, &fields); err != nil {
				t.Fatal(err)
			}
			if test.stream == nil {
				if _, exists := fields["stream"]; exists {
					t.Fatal("omitted stream was serialized")
				}
			} else if string(fields["stream"]) != fmt.Sprint(test.want) {
				t.Fatalf("stream=%s want=%t", fields["stream"], test.want)
			}
			if first.Parameters.Stream != nil {
				*first.Parameters.Stream = !test.want
			}
			resolved, _, err := catalog.resolve("fixture", nil)
			if err != nil {
				t.Fatal(err)
			}
			if resolved.Parameters.StreamingEnabled() != test.want {
				t.Fatal("getter shares Stream storage")
			}
			if resolved.Parameters.Stream != nil {
				*resolved.Parameters.Stream = !test.want
			}
			second, _ := catalog.Get("fixture")
			if second.Parameters.StreamingEnabled() != test.want {
				t.Fatal("resolved model shares Stream storage")
			}
		})
	}
}

func boolPointer(value bool) *bool { return &value }

func TestResolvedEndpointSupportsExplicitEndpointAndDriverBaseURL(t *testing.T) {
	for name, testCase := range map[string]struct {
		profile Profile
		want    string
	}{
		"explicit": {
			profile: Profile{
				Driver:   DriverOpenAIChatCompletions,
				Endpoint: "https://example.invalid/custom/chat?region=cn",
			},
			want: "https://example.invalid/custom/chat?region=cn",
		},
		"openai_base": {
			profile: Profile{
				Driver:  DriverOpenAIChatCompletions,
				BaseURL: "https://example.invalid/provider/",
			},
			want: "https://example.invalid/provider/chat/completions",
		},
		"anthropic_base": {
			profile: Profile{
				Driver:  DriverAnthropicMessages,
				BaseURL: "https://example.invalid/provider",
			},
			want: "https://example.invalid/provider/messages",
		},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := testCase.profile.ResolvedEndpoint()
			if err != nil || got != testCase.want {
				t.Fatalf("endpoint=%q want=%q error=%v", got, testCase.want, err)
			}
		})
	}
	for name, profile := range map[string]Profile{
		"missing": {Driver: DriverOpenAIChatCompletions},
		"both": {
			Driver:   DriverOpenAIChatCompletions,
			Endpoint: "https://example.invalid/v1/chat/completions",
			BaseURL:  "https://example.invalid",
		},
		"base_query": {
			Driver:  DriverOpenAIChatCompletions,
			BaseURL: "https://example.invalid/provider?region=cn",
		},
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := profile.ResolvedEndpoint(); err == nil {
				t.Fatalf("profile=%#v was accepted", profile)
			}
		})
	}
}

func TestEndpointNeverInsertsOrDetectsVersion(t *testing.T) {
	for driver, suffix := range map[DriverName]string{
		DriverOpenAIChatCompletions: "/chat/completions", DriverAnthropicMessages: "/messages", DriverOpenAIResponses: "/responses",
	} {
		for _, prefix := range []string{"", "/v1", "/api/v3", "/api/paas/v4", "/compatible-mode/v1", "/vendor/unversioned"} {
			for _, trailing := range []string{"", "/"} {
				base := "https://example.invalid" + prefix
				got, err := (Profile{Driver: driver, BaseURL: base + trailing}).ResolvedEndpoint()
				if err != nil || got != base+suffix {
					t.Fatalf("driver=%s base=%s got=%s err=%v", driver, base+trailing, got, err)
				}
			}
		}
		endpoint := "https://example.invalid/custom/endpoint?api-version=fixture"
		got, err := (Profile{Driver: driver, Endpoint: endpoint}).ResolvedEndpoint()
		if err != nil || got != endpoint {
			t.Fatalf("driver=%s got=%s err=%v", driver, got, err)
		}
	}
}

func TestResponsesProfileValidation(t *testing.T) {
	profile := Profile{Driver: DriverOpenAIResponses, BaseURL: "https://example.invalid/v1", Model: "fixture", Timeout: "1m"}
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if err := profile.ValidateManagedExecution(); err == nil {
		t.Fatal("Responses managed execution accepted")
	}
	profile.Parameters.StopSequences = []string{"END"}
	if err := profile.Validate(); err == nil {
		t.Fatal("Responses stop sequences accepted")
	}
	for _, driver := range []DriverName{DriverOpenAIChatCompletions, DriverAnthropicMessages} {
		if err := (Profile{Driver: driver}).ValidateManagedExecution(); err != nil {
			t.Fatal(err)
		}
	}
}

func TestCatalogResolveSecret(t *testing.T) {
	catalog, err := NewCatalog(map[string]Profile{
		"fixture": {
			Driver:   DriverOpenAIChatCompletions,
			Endpoint: "https://example.invalid/v1/chat/completions",
			Model:    "fixture",
			Headers:  map[string]string{"Authorization": "Bearer ${MODEL_API_KEY}"},
			Timeout:  "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(catalog.IDs(), []string{"fixture"}) {
		t.Fatalf("ids=%v", catalog.IDs())
	}
	resolved, secrets, err := catalog.resolve("fixture", func(name string) (string, bool) {
		return "top-secret", name == "MODEL_API_KEY"
	})
	if err != nil {
		t.Fatal(err)
	}
	if !slices.Contains(secrets, "top-secret") ||
		resolved.RequestHeaders()["Authorization"] != "Bearer top-secret" ||
		resolved.LogRequestHeaders()["Authorization"] != "Bearer ${MODEL_API_KEY}" {
		t.Fatalf("resolved=%s secrets=%v", resolved.String(), secrets)
	}
	data, err := json.Marshal(resolved)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "top-secret") ||
		strings.Contains(resolved.String(), "top-secret") ||
		strings.Contains(fmt.Sprintf("%#v", resolved), "top-secret") {
		t.Fatal("resolved model exposed the secret")
	}

	for _, control := range []byte{0x00, 0x09, 0x0a, 0x0d, 0x1f, 0x7f} {
		secretWithControl := "prefix" + string([]byte{control}) + "suffix"
		if _, _, err := catalog.resolve("fixture", func(string) (string, bool) {
			return secretWithControl, true
		}); err == nil {
			t.Fatalf("secret containing control byte 0x%02x was accepted", control)
		}
	}
}

func TestCatalogLogHeadersRedactSensitiveLiterals(t *testing.T) {
	maxTokens := int64(64)
	catalog, err := NewCatalog(map[string]Profile{
		"fixture": {
			Driver:   DriverAnthropicMessages,
			Endpoint: "https://example.invalid/v1/messages", Model: "fixture",
			Headers: map[string]string{
				"x-api-key": "literal-secret", "x-region": "cn",
			},
			Parameters: Parameters{MaxTokens: &maxTokens}, Timeout: "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	resolved, secrets, err := catalog.resolve("fixture", func(string) (string, bool) {
		return "", false
	})
	if err != nil {
		t.Fatal(err)
	}
	if got := resolved.LogRequestHeaders(); got["X-Api-Key"] != "[REDACTED]" ||
		got["X-Region"] != "cn" {
		t.Fatalf("log headers=%#v", got)
	}
	if !slices.Contains(secrets, "literal-secret") {
		t.Fatalf("secrets=%v", secrets)
	}
}

func TestEffectiveContextBudgetUsesConservativeReservation(t *testing.T) {
	maxOutput := int64(12_000)
	profile := Profile{
		Driver:     DriverOpenAIChatCompletions,
		Parameters: Parameters{MaxTokens: &maxOutput},
		Context: ContextPolicy{
			WindowTokens: 32_768, ReservedOutputTokens: 4_096,
		},
	}
	window, reserved, input, explicit := profile.EffectiveContextBudget()
	if !explicit || window != 32_768 || reserved != 12_000 ||
		input != 20_768 {
		t.Fatalf(
			"window=%d reserved=%d input=%d explicit=%t",
			window, reserved, input, explicit,
		)
	}
}

func TestEffectiveContextBudgetUsesConservativeDefaultsAndRequestLimit(t *testing.T) {
	maxOutput := int64(12_000)
	profile := Profile{
		Driver:     DriverOpenAIChatCompletions,
		Parameters: Parameters{MaxTokens: &maxOutput},
	}
	window, reserved, input, explicit := profile.EffectiveContextBudget()
	if explicit || window != 32_768 || reserved != 12_000 || input != 20_768 {
		t.Fatalf(
			"window=%d reserved=%d input=%d explicit=%t",
			window, reserved, input, explicit,
		)
	}
	lowerRequestLimit := int64(4_096)
	window, reserved, input, explicit =
		profile.EffectiveContextBudgetForRequest(&lowerRequestLimit)
	if explicit || window != 32_768 || reserved != 12_000 || input != 20_768 {
		t.Fatalf(
			"lower override: window=%d reserved=%d input=%d explicit=%t",
			window, reserved, input, explicit,
		)
	}
	higherRequestLimit := int64(16_000)
	window, reserved, input, explicit =
		profile.EffectiveContextBudgetForRequest(&higherRequestLimit)
	if explicit || window != 32_768 || reserved != 16_000 || input != 16_768 {
		t.Fatalf(
			"higher override: window=%d reserved=%d input=%d explicit=%t",
			window, reserved, input, explicit,
		)
	}
}
