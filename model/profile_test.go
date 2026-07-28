package model

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"
)

func TestDecodeModelProfile(t *testing.T) {
	profile, err := DecodeProfile(strings.NewReader(`{
		"driver":"openai-compatible",
		"endpoint":"https://example.invalid/v1/chat/completions",
		"model":"fixture",
		"auth":{"header":"Authorization","scheme":"Bearer","from_env":"MODEL_API_KEY"},
		"headers":{"X-Client":"runtime-test"},
		"defaults":{"max_completion_tokens":1024},
		"timeout":"5m"
	}`))
	if err != nil {
		t.Fatal(err)
	}
	if profile.Driver != DriverOpenAICompatible || profile.TimeoutDuration().Minutes() != 5 {
		t.Fatalf("profile=%#v", profile)
	}
	for _, input := range []string{
		`{"driver":"openai-compatible","endpoint":"http://example.invalid/v1","model":"x","auth":{"header":"Authorization","scheme":"Bearer","from_env":"KEY"},"timeout":"1m"}`,
		`{"driver":"openai-compatible","endpoint":"https://example.invalid/v1","model":"x","auth":{"header":"Authorization","scheme":"Bearer","from_env":"KEY"},"headers":{"Authorization":"literal"},"timeout":"1m"}`,
		`{"driver":"openai-compatible","endpoint":"https://example.invalid/v1","model":"x","auth":{"header":"Authorization","scheme":"Bearer","from_env":"KEY"},"retry":3,"timeout":"1m"}`,
		`{"driver":"openai-compatible","endpoint":"https://example.invalid/v1","model":"x","auth":{"header":"Authorization","scheme":"Bearer","from_env":"${KEY}"},"timeout":"1m"}`,
		`{"driver":"openai-compatible","endpoint":"https://example.invalid/v1","model":"x","auth":{"header":"Authorization","scheme":"Bearer","from_env":"KEY"},"defaults":{"max_tokens":1024},"timeout":"1m"}`,
		`{"driver":"openai-compatible","endpoint":"https://example.invalid/v1","model":"x","auth":{"header":"Authorization","scheme":"Bearer","from_env":"KEY"},"defaults":{"max_output_tokens":1024},"timeout":"1m"}`,
		`{"driver":"anthropic-compatible","endpoint":"https://example.invalid/v1","model":"x","auth":{"header":"x-api-key","from_env":"KEY"},"defaults":{"max_completion_tokens":1024},"timeout":"1m"}`,
	} {
		if _, err := DecodeProfile(strings.NewReader(input)); err == nil {
			t.Fatalf("DecodeProfile(%s) returned nil", input)
		}
	}
}

func TestLoadProfileDirAndResolveSecret(t *testing.T) {
	root := t.TempDir()
	document := `{
		"driver":"openai-compatible",
		"endpoint":"https://example.invalid/v1/chat/completions",
		"model":"fixture",
		"auth":{"header":"Authorization","scheme":"Bearer","from_env":"MODEL_API_KEY"},
		"timeout":"1m"
	}`
	if err := os.WriteFile(filepath.Join(root, "fixture.json"), []byte(document), 0o600); err != nil {
		t.Fatal(err)
	}
	catalog, err := LoadProfileDir(root)
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
