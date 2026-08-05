package model

import (
	"encoding/json"
	"fmt"
	"reflect"
	"slices"
	"strings"
	"testing"
)

func TestValidateModelProfile(t *testing.T) {
	maxTokens := int64(1024)
	oversizedDefault := int64(32_767)
	invalidTopP := 1.1
	profile := Profile{
		Driver:   DriverOpenAI,
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
	if profile.Driver != DriverOpenAI || profile.TimeoutDuration().Minutes() != 5 {
		t.Fatalf("profile=%#v", profile)
	}
	for name, value := range map[string]Profile{
		"insecure_endpoint": {
			Driver: DriverOpenAI, Endpoint: "http://example.invalid/v1",
			Model: "x", Headers: map[string]string{"Authorization": "${KEY}"},
			Timeout: "1m",
		},
		"invalid_header_name": {
			Driver: DriverOpenAI, Endpoint: "https://example.invalid/v1",
			Model: "x", Headers: map[string]string{"Bad Header": "${KEY}"}, Timeout: "1m",
		},
		"invalid_header_value": {
			Driver: DriverOpenAI, Endpoint: "https://example.invalid/v1",
			Model: "x", Headers: map[string]string{"X-Client": "bad\nvalue"}, Timeout: "1m",
		},
		"missing_anthropic_token_limit": {
			Driver: DriverAnthropic, Endpoint: "https://example.invalid/v1",
			Model: "x", Headers: map[string]string{"x-api-key": "${KEY}"}, Timeout: "1m",
		},
		"default_context_without_input_budget": {
			Driver: DriverAnthropic, Endpoint: "https://example.invalid/v1/messages",
			Model: "x", Headers: map[string]string{"x-api-key": "${KEY}"},
			Parameters: Parameters{MaxTokens: &oversizedDefault}, Timeout: "1m",
		},
		"invalid_top_p": {
			Driver: DriverOpenAI, Endpoint: "https://example.invalid/v1/chat/completions",
			Model: "x", Headers: map[string]string{"Authorization": "${KEY}"},
			Parameters: Parameters{TopP: &invalidTopP}, Timeout: "1m",
		},
		"empty_stop_sequence": {
			Driver: DriverOpenAI, Endpoint: "https://example.invalid/v1/chat/completions",
			Model: "x", Headers: map[string]string{"Authorization": "${KEY}"},
			Parameters: Parameters{StopSequences: []string{""}}, Timeout: "1m",
		},
	} {
		if err := value.Validate(); err == nil {
			t.Fatalf("%s was accepted: %#v", name, value)
		}
	}
}

func TestResolvedEndpointSupportsExplicitEndpointAndDriverBaseURL(t *testing.T) {
	for name, testCase := range map[string]struct {
		profile Profile
		want    string
	}{
		"explicit": {
			profile: Profile{
				Driver:   DriverOpenAI,
				Endpoint: "https://example.invalid/custom/chat?region=cn",
			},
			want: "https://example.invalid/custom/chat?region=cn",
		},
		"openai_base": {
			profile: Profile{
				Driver:  DriverOpenAI,
				BaseURL: "https://example.invalid/provider/",
			},
			want: "https://example.invalid/provider/v1/chat/completions",
		},
		"anthropic_base": {
			profile: Profile{
				Driver:  DriverAnthropic,
				BaseURL: "https://example.invalid/provider",
			},
			want: "https://example.invalid/provider/v1/messages",
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
		"missing": {Driver: DriverOpenAI},
		"both": {
			Driver:   DriverOpenAI,
			Endpoint: "https://example.invalid/v1/chat/completions",
			BaseURL:  "https://example.invalid",
		},
		"base_query": {
			Driver:  DriverOpenAI,
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

func TestCatalogResolveSecret(t *testing.T) {
	catalog, err := NewCatalog(map[string]Profile{
		"fixture": {
			Driver:   DriverOpenAI,
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
			Driver:   DriverAnthropic,
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
		Driver:     DriverOpenAI,
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
		Driver:     DriverOpenAI,
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
