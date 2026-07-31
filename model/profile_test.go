package model

import (
	"encoding/json"
	"fmt"
	"reflect"
	"strings"
	"testing"
)

func TestValidateModelProfile(t *testing.T) {
	maxCompletionTokens := int64(1024)
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
			MaxCompletionTokens: &maxCompletionTokens,
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
		"wrong_openai_token_limit": {
			Driver: DriverOpenAICompatible, Endpoint: "https://example.invalid/v1",
			Model: "x", Auth: Auth{
				Header: "Authorization", Scheme: "Bearer", FromEnv: "KEY",
			}, Defaults: Defaults{MaxTokens: &maxCompletionTokens}, Timeout: "1m",
		},
		"wrong_anthropic_token_limit": {
			Driver: DriverAnthropicCompatible, Endpoint: "https://example.invalid/v1",
			Model: "x", Auth: Auth{
				Header: "x-api-key", FromEnv: "KEY",
			}, Defaults: Defaults{
				MaxCompletionTokens: &maxCompletionTokens,
			}, Timeout: "1m",
		},
	} {
		if err := value.Validate(); err == nil {
			t.Fatalf("%s was accepted: %#v", name, value)
		}
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
		Defaults: Defaults{MaxCompletionTokens: &maxOutput},
		Context: ContextPolicy{
			WindowTokens: 32_768, ReservedOutputTokens: 4_096,
		},
	}
	window, reserved, input, configured := profile.EffectiveContextBudget()
	if !configured || window != 32_768 || reserved != 12_000 ||
		input != 20_768 {
		t.Fatalf(
			"window=%d reserved=%d input=%d configured=%t",
			window, reserved, input, configured,
		)
	}
}
