package model

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestValidateModelProfile(t *testing.T) {
	maxTokens := int64(1024)
	oversizedDefault := int64(32_767)
	invalidTopP := 1.1
	profile := Profile{
		Driver:   DriverOpenAICompatible,
		Endpoint: "https://example.invalid/v1/chat/completions",
		Model:    "fixture",
		Auth: Auth{
			Header: "Authorization", Scheme: "Bearer",
			FromEnv: "MODEL_API_KEY",
		},
		Headers: map[string]string{"X-Client": "runtime-test"},
		Defaults: Defaults{
			MaxTokens: &maxTokens,
		},
		Timeout: "5m",
	}
	if err := profile.Validate(); err != nil {
		t.Fatal(err)
	}
	if profile.Driver != DriverOpenAICompatible || profile.TimeoutDuration().Minutes() != 5 {
		t.Fatalf("profile=%#v", profile)
	}
	for name, value := range map[string]Profile{
		"insecure_endpoint": {
			Driver: DriverOpenAICompatible, Endpoint: "http://example.invalid/v1",
			Model: "x", Auth: Auth{
				Header: "Authorization", Scheme: "Bearer", FromEnv: "KEY",
			}, Timeout: "1m",
		},
		"reserved_header": {
			Driver: DriverOpenAICompatible, Endpoint: "https://example.invalid/v1",
			Model: "x", Auth: Auth{
				Header: "Authorization", Scheme: "Bearer", FromEnv: "KEY",
			}, Headers: map[string]string{"Authorization": "literal"}, Timeout: "1m",
		},
		"invalid_auth_env": {
			Driver: DriverOpenAICompatible, Endpoint: "https://example.invalid/v1",
			Model: "x", Auth: Auth{
				Header: "Authorization", Scheme: "Bearer", FromEnv: "${KEY}",
			}, Timeout: "1m",
		},
		"missing_anthropic_token_limit": {
			Driver: DriverAnthropicCompatible, Endpoint: "https://example.invalid/v1",
			Model: "x", Auth: Auth{
				Header: "x-api-key", FromEnv: "KEY",
			}, Timeout: "1m",
		},
		"default_context_without_input_budget": {
			Driver: DriverAnthropicCompatible, Endpoint: "https://example.invalid/v1/messages",
			Model: "x", Auth: Auth{
				Header: "x-api-key", FromEnv: "KEY",
			}, Defaults: Defaults{MaxTokens: &oversizedDefault}, Timeout: "1m",
		},
		"invalid_top_p": {
			Driver: DriverOpenAICompatible, Endpoint: "https://example.invalid/v1/chat/completions",
			Model: "x", Auth: Auth{
				Header: "Authorization", FromEnv: "KEY",
			}, Defaults: Defaults{TopP: &invalidTopP}, Timeout: "1m",
		},
		"empty_stop_sequence": {
			Driver: DriverOpenAICompatible, Endpoint: "https://example.invalid/v1/chat/completions",
			Model: "x", Auth: Auth{
				Header: "Authorization", FromEnv: "KEY",
			}, Defaults: Defaults{StopSequences: []string{""}}, Timeout: "1m",
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
				Driver:   DriverOpenAICompatible,
				Endpoint: "https://example.invalid/custom/chat?region=cn",
			},
			want: "https://example.invalid/custom/chat?region=cn",
		},
		"openai_base": {
			profile: Profile{
				Driver:  DriverOpenAICompatible,
				BaseURL: "https://example.invalid/provider/",
			},
			want: "https://example.invalid/provider/v1/chat/completions",
		},
		"anthropic_base": {
			profile: Profile{
				Driver:  DriverAnthropicCompatible,
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
		"missing": {Driver: DriverOpenAICompatible},
		"both": {
			Driver:   DriverOpenAICompatible,
			Endpoint: "https://example.invalid/v1/chat/completions",
			BaseURL:  "https://example.invalid",
		},
		"base_query": {
			Driver:  DriverOpenAICompatible,
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
			Driver:   DriverOpenAICompatible,
			Endpoint: "https://example.invalid/v1/chat/completions",
			Model:    "fixture",
			Auth: Auth{
				Header: "Authorization", Scheme: "Bearer",
				FromEnv: "MODEL_API_KEY",
			},
			Timeout: "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(catalog.IDs(), []string{"fixture"}) {
		t.Fatalf("ids=%v", catalog.IDs())
	}
	resolved, secret, err := catalog.resolve("fixture", func(name string) (string, bool) {
		return "top-secret", name == "MODEL_API_KEY"
	})
	if err != nil {
		t.Fatal(err)
	}
	if secret != "top-secret" || resolved.RequestHeaders()["Authorization"] != "Bearer top-secret" {
		t.Fatalf("resolved=%s", resolved.String())
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

func TestEffectiveContextBudgetUsesConservativeReservation(t *testing.T) {
	maxOutput := int64(12_000)
	profile := Profile{
		Driver:   DriverOpenAICompatible,
		Defaults: Defaults{MaxTokens: &maxOutput},
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
		Driver:   DriverOpenAICompatible,
		Defaults: Defaults{MaxTokens: &maxOutput},
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
