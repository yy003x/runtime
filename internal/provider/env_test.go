package provider

import (
	"strings"
	"testing"
)

func TestCommandEnvironmentAppliesUnsetOverridesAndRuntimeValues(t *testing.T) {
	t.Setenv("SN_ENV_DROP", "remove-me")
	t.Setenv("SN_ENV_SOURCE", "from-parent")
	t.Setenv("SN_ENV_OVERRIDE", "old-value")

	cfg := CommandConfig{
		Env:            map[string]string{"SN_ENV_OVERRIDE": "${SN_ENV_SOURCE}"},
		EnvPassthrough: []string{"SN_ENV_SOURCE"},
		EnvUnset:       []string{"SN_ENV_DROP"},
	}
	values := environmentMap(CommandEnvironment(cfg, map[string]string{"SN_ENV_RUNTIME": "runtime"}))
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
