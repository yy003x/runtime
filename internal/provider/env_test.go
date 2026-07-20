package provider

import (
	"strings"
	"testing"
)

func TestResolveEnvUsesOnlyBracedReferences(t *testing.T) {
	t.Setenv("SN_ENV_VALUE", "resolved")

	resolved, err := ResolveEnv("prefix-${SN_ENV_VALUE}-$SN_ENV_VALUE-SN_ENV_VALUE")
	if err != nil {
		t.Fatal(err)
	}
	if resolved != "prefix-resolved-$SN_ENV_VALUE-SN_ENV_VALUE" {
		t.Fatalf("ResolveEnv=%q", resolved)
	}
}

func TestResolveEnvRejectsMissingAndMalformedReferences(t *testing.T) {
	for name, value := range map[string]string{
		"missing":    "${SN_ENV_DOES_NOT_EXIST}",
		"unclosed":   "${SN_ENV_VALUE",
		"empty":      "${}",
		"bad-name":   "${BAD-NAME}",
		"bad-prefix": "${1BAD}",
	} {
		t.Run(name, func(t *testing.T) {
			if _, err := ResolveEnv(value); err == nil {
				t.Fatalf("ResolveEnv(%q) returned nil error", value)
			}
		})
	}
}

func TestEnvironmentReferenceNameRequiresCompleteReference(t *testing.T) {
	if name, ok := EnvironmentReferenceName("${ANTHROPIC_API_KEY}"); !ok || name != "ANTHROPIC_API_KEY" {
		t.Fatalf("name=%q ok=%v", name, ok)
	}
	for _, value := range []string{"ANTHROPIC_API_KEY", "$ANTHROPIC_API_KEY", "prefix-${ANTHROPIC_API_KEY}", "${BAD-NAME}"} {
		if _, ok := EnvironmentReferenceName(value); ok {
			t.Fatalf("EnvironmentReferenceName(%q) unexpectedly accepted", value)
		}
	}
}

func TestResolveAPIKeyRejectsProgrammaticPlaintext(t *testing.T) {
	if _, err := resolveAPIKey("plain", &APIConfig{APIKey: "secret"}); err == nil {
		t.Fatal("resolveAPIKey accepted a plaintext key")
	}
	t.Setenv("SN_API_KEY", "resolved-secret")
	key, err := resolveAPIKey("reference", &APIConfig{APIKey: "${SN_API_KEY}"})
	if err != nil {
		t.Fatal(err)
	}
	if key != "resolved-secret" {
		t.Fatalf("key was not resolved")
	}
}

func TestCommandEnvironmentAppliesUnsetOverridesAndRuntimeValues(t *testing.T) {
	t.Setenv("SN_ENV_DROP", "remove-me")
	t.Setenv("SN_ENV_SOURCE", "from-parent")
	t.Setenv("SN_ENV_OVERRIDE", "old-value")

	cfg := CommandConfig{
		Env:            map[string]string{"SN_ENV_OVERRIDE": "${SN_ENV_SOURCE}"},
		EnvPassthrough: []string{"SN_ENV_SOURCE"},
		EnvUnset:       []string{"SN_ENV_DROP"},
	}
	items, err := CommandEnvironment(cfg, map[string]string{"SN_ENV_RUNTIME": "runtime"})
	if err != nil {
		t.Fatal(err)
	}
	values := environmentMap(items)
	if _, exists := values["SN_ENV_DROP"]; exists {
		t.Fatalf("SN_ENV_DROP should be removed: %#v", values)
	}
	if values["SN_ENV_SOURCE"] != "from-parent" || values["SN_ENV_OVERRIDE"] != "from-parent" {
		t.Fatalf("configured environment not applied: %#v", values)
	}
	if values["SN_ENV_RUNTIME"] != "runtime" {
		t.Fatalf("runtime environment not applied: %#v", values)
	}
}

func environmentMap(items []string) map[string]string {
	values := make(map[string]string, len(items))
	for _, item := range items {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	return values
}
