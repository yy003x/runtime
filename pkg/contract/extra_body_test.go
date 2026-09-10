package contract

import (
	"bytes"
	"encoding/json"
	"strings"
	"testing"
)

func TestExtraBodyPreservesExplicitValuesAndIsolation(t *testing.T) {
	defaults := ExtraBody{"metadata": json.RawMessage(`{"default":true}`), "reasoning": json.RawMessage(`{"effort":"high","summary":"auto"}`), "seed": json.RawMessage(`9007199254740993`)}
	overrides := ExtraBody{"metadata": json.RawMessage(`null`), "reasoning": json.RawMessage(`{"effort":"low"}`), "frequency_penalty": json.RawMessage(`0`), "parallel_tool_calls": json.RawMessage(`false`), "tool_choice": nil}
	merged := MergeExtraBody(defaults, overrides)
	if err := merged.Validate(); err != nil {
		t.Fatal(err)
	}
	encoded, err := json.Marshal(merged)
	if err != nil {
		t.Fatal(err)
	}
	for _, want := range []string{`"metadata":null`, `"frequency_penalty":0`, `"parallel_tool_calls":false`, `"tool_choice":null`, `"seed":9007199254740993`, `"reasoning":{"effort":"low"}`} {
		if !bytes.Contains(encoded, []byte(want)) {
			t.Fatalf("missing %s: %s", want, encoded)
		}
	}
	merged["seed"][0] = '1'
	merged["metadata"][0] = 't'
	if string(defaults["seed"]) != "9007199254740993" || string(overrides["metadata"]) != "null" {
		t.Fatal("merge aliased caller-owned RawMessage bytes")
	}
	if MergeExtraBody(nil, nil) != nil {
		t.Fatal("omission must remain omitted")
	}
}

func TestExtraBodyOnlyChecksStructureAndOwnedFields(t *testing.T) {
	for _, name := range []string{"reasoning_effort", "response_format", "tool_choice", "parallel_tool_calls", "frequency_penalty", "presence_penalty", "logprobs", "top_logprobs", "logit_bias", "seed", "metadata", "prompt_cache_key", "prompt_cache_retention", "prompt_cache_options", "reasoning", "text", "include", "max_tool_calls", "service_tier", "thinking", "output_config", "top_k", "cache_control", "provider_extension"} {
		for _, value := range []string{"null", "0", "false", `{}`, `[]`, `"provider-specific"`} {
			if err := (ExtraBody{name: json.RawMessage(value)}).Validate(); err != nil {
				t.Fatalf("%s=%s: %v", name, value, err)
			}
		}
	}
	for _, name := range []string{"model", "messages", "input", "tools", "stream", "max_tokens", "max_output_tokens", "temperature", "previous_response_id", "background", "audio"} {
		if err := (ExtraBody{name: json.RawMessage(`null`)}).Validate(); err == nil {
			t.Fatalf("owned field %s was accepted", name)
		}
	}
	for _, fields := range []ExtraBody{{"": json.RawMessage(`0`)}, {"bad\x00name": json.RawMessage(`0`)}, {strings.Repeat("x", 257): json.RawMessage(`0`)}, {"metadata": json.RawMessage(`{`)}, {"metadata": json.RawMessage(`"` + strings.Repeat("x", 1<<20) + `"`)}} {
		if err := fields.Validate(); err == nil {
			t.Fatal("invalid extra_body accepted")
		}
	}
}
