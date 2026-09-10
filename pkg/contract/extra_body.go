package contract

import (
	"encoding/json"
	"fmt"
	"strings"
	"unicode/utf8"
)

// ExtraBody carries native Provider request options. Values remain raw JSON so
// omission, null, zero, false, and large integers retain their original meaning.
// It extends generation options, not canonical messages or execution identity.
type ExtraBody map[string]json.RawMessage

func (fields ExtraBody) Validate() error {
	for name, value := range fields {
		if name == "" || len(name) > 256 || !utf8.ValidString(name) || strings.ContainsRune(name, '\x00') {
			return fmt.Errorf("extra_body field names must be non-empty UTF-8 without NUL and at most 256 bytes")
		}
		switch name {
		case "model", "messages", "input", "system", "instructions", "tools",
			"stream", "max_tokens", "max_completion_tokens", "max_output_tokens",
			"temperature", "top_p", "stop", "stop_sequences":
			return fmt.Errorf("extra_body.%s is owned by the existing Profile or canonical request field", name)
		case "background", "conversation", "previous_response_id", "context_management",
			"container", "mcp_servers", "modalities", "audio", "n", "functions", "function_call":
			return fmt.Errorf("extra_body.%s requires native conversation, tool, or output capabilities outside generation-option extensions", name)
		}
		if len(value) != 0 && !json.Valid(value) {
			return fmt.Errorf("extra_body.%s must contain valid JSON", name)
		}
	}
	encoded, err := json.Marshal(fields)
	if err != nil {
		return fmt.Errorf("extra_body: %w", err)
	}
	if len(encoded) > 1<<20 {
		return fmt.Errorf("extra_body exceeds 1048576 bytes")
	}
	return nil
}

// MergeExtraBody returns an independent, shallow merge. An explicitly supplied
// key replaces the complete default value, including when that value is null.
func MergeExtraBody(defaults, overrides ExtraBody) ExtraBody {
	if defaults == nil && overrides == nil {
		return nil
	}
	merged := make(ExtraBody, len(defaults)+len(overrides))
	for _, fields := range []ExtraBody{defaults, overrides} {
		for name, value := range fields {
			merged[name] = append(json.RawMessage(nil), value...)
		}
	}
	return merged
}
