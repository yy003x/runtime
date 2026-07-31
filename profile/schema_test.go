package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestPublicSchemasAreValidJSON(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	for _, name := range []string{"profile.schema.json"} {
		t.Run(name, func(t *testing.T) {
			data, err := os.ReadFile(filepath.Join(
				filepath.Dir(source), "..", "resources", "schema", name,
			))
			if err != nil {
				t.Fatal(err)
			}
			var document map[string]any
			if err := json.Unmarshal(data, &document); err != nil {
				t.Fatal(err)
			}
		})
	}
}

func TestProfileSchemaAndLoaderShareContractFixtures(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	schemaPath := filepath.Join(
		filepath.Dir(source), "..", "resources", "schema",
		"profile.schema.json",
	)
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(schemaPath)
	if err != nil {
		t.Fatal(err)
	}
	testCases := []struct {
		name         string
		document     string
		valid        bool
		semanticRule string
	}{
		{
			name:     "minimal_cli",
			document: `{"type":"cli","command":"codex"}`,
			valid:    true,
		},
		{
			name: "complete_cli",
			document: `{
				"type":"cli",
				"command":"/opt/runtime/bin/claude",
				"args":["--model","configured","--add-dir","${WORKSPACE}"],
				"env":{
					"WORKSPACE":"/tmp/work","OPTIONAL":null,
					"LITERAL":"$${VALID}","DOLLARS":"$$"
				},
				"model":"",
				"effort":"high",
				"prompt":"base",
				"exec":true,
				"cwd":"${WORKSPACE}"
			}`,
			valid: true,
		},
		{
			name: "openai_api",
			document: `{
				"type":"api",
				"driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1/chat/completions",
				"model":"fixture",
				"auth":{"header":"Authorization","scheme":"Bearer","from_env":"MODEL_API_KEY"},
				"headers":{"X-Runtime":"fixture"},
				"defaults":{"max_completion_tokens":1024,"temperature":0.2},
				"timeout":".5s",
				"context":{"window_tokens":0,"reserved_output_tokens":0,"keep_recent_turns":0}
			}`,
			valid: true,
		},
		{
			name: "anthropic_api",
			document: `{
				"type":"api",
				"driver":"anthropic-compatible",
				"endpoint":"https://example.invalid/v1/messages",
				"model":"fixture",
				"auth":{"header":"x-api-key","scheme":"","from_env":"MODEL_API_KEY"},
				"defaults":{"max_tokens":1024},
				"timeout":"5m"
			}`,
			valid: true,
		},
		{
			name: "go_duration_greek_microseconds",
			document: `{
				"type":"api",
				"driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1/chat/completions",
				"model":"fixture",
				"auth":{"header":"Authorization","from_env":"MODEL_API_KEY"},
				"timeout":"1μs"
			}`,
			valid: true,
		},
		{
			name: "nullable_api_optional_values",
			document: `{
				"type":"api",
				"driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1/chat/completions",
				"model":"fixture",
				"auth":{"header":"Authorization","from_env":"MODEL_API_KEY"},
				"defaults":{"max_completion_tokens":null,"temperature":null},
				"timeout":"1m",
				"context":{"summary_enabled":null}
			}`,
			valid: true,
		},
		{
			name:     "missing_type",
			document: `{"command":"codex"}`,
		},
		{
			name:     "unknown_field",
			document: `{"type":"cli","command":"codex","launch":"tmux"}`,
		},
		{
			name:     "null_cli_scalar",
			document: `{"type":"cli","command":"codex","model":null}`,
		},
		{
			name:     "null_cli_collection",
			document: `{"type":"cli","command":"codex","args":null}`,
		},
		{
			name:     "unsupported_command",
			document: `{"type":"cli","command":"python"}`,
		},
		{
			name:     "command_with_trailing_separator",
			document: `{"type":"cli","command":"codex/"}`,
		},
		{
			name:     "invalid_cli_env_name",
			document: `{"type":"cli","command":"codex","env":{"   ":"value"}}`,
		},
		{
			name:     "invalid_cli_argument_reference",
			document: `{"type":"cli","command":"codex","args":["${BAD-NAME}"]}`,
		},
		{
			name:     "incomplete_cli_cwd_reference",
			document: `{"type":"cli","command":"codex","cwd":"${WORKSPACE"}`,
		},
		{
			name:     "prefixed_invalid_cli_reference",
			document: `{"type":"cli","command":"codex","args":["$${BAD-NAME}"]}`,
		},
		{
			name:     "prefixed_incomplete_cli_reference",
			document: `{"type":"cli","command":"codex","args":["$${"]}`,
		},
		{
			name:     "effort_with_whitespace",
			document: `{"type":"cli","command":"codex","effort":" high "}`,
		},
		{
			name:     "invalid_effort",
			document: `{"type":"cli","command":"codex","effort":"extreme"}`,
		},
		{
			name:     "cross_domain_field",
			document: `{"type":"cli","command":"codex","endpoint":"https://example.invalid/v1"}`,
		},
		{
			name: "insecure_api_endpoint",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"http://example.invalid/v1","model":"fixture",
				"auth":{"header":"Authorization","from_env":"KEY"},"timeout":"1m"
			}`,
		},
		{
			name: "null_api_object",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1","model":"fixture",
				"auth":{"header":"Authorization","from_env":"KEY"},
				"defaults":null,"timeout":"1m"
			}`,
		},
		{
			name: "api_endpoint_without_path",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/","model":"fixture",
				"auth":{"header":"Authorization","from_env":"KEY"},"timeout":"1m"
			}`,
		},
		{
			name: "endpoint_rejected_by_net_url",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/%zz","model":"fixture",
				"auth":{"header":"Authorization","from_env":"KEY"},"timeout":"1m"
			}`,
			semanticRule: "endpoint syntax is validated by Go net/url",
		},
		{
			name: "blank_api_model",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1","model":"   ",
				"auth":{"header":"Authorization","from_env":"KEY"},"timeout":"1m"
			}`,
		},
		{
			name: "invalid_auth_header",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1","model":"fixture",
				"auth":{"header":"Bad Header","from_env":"KEY"},"timeout":"1m"
			}`,
		},
		{
			name: "invalid_literal_header_reference",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1","model":"fixture",
				"auth":{"header":"Authorization","from_env":"KEY"},
				"headers":{"X-Runtime":"${SECRET}"},"timeout":"1m"
			}`,
		},
		{
			name: "literal_secret_header",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1","model":"fixture",
				"auth":{"header":"X-Token","from_env":"KEY"},
				"headers":{"cOoKiE":"literal"},"timeout":"1m"
			}`,
		},
		{
			name: "invalid_timeout",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1","model":"fixture",
				"auth":{"header":"Authorization","from_env":"KEY"},"timeout":"forever"
			}`,
		},
		{
			name: "timeout_above_runtime_bound",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1","model":"fixture",
				"auth":{"header":"Authorization","from_env":"KEY"},"timeout":"25h"
			}`,
			semanticRule: "duration total is bounded by the Go loader",
		},
		{
			name: "adapter_selector_conflict",
			document: `{
				"type":"cli","command":"codex",
				"args":["--model","one","--model","two"]
			}`,
			semanticRule: "CLI args must satisfy the selected command adapter grammar",
		},
		{
			name: "context_without_input_budget",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1","model":"fixture",
				"auth":{"header":"Authorization","from_env":"KEY"},"timeout":"1m",
				"context":{"window_tokens":1}
			}`,
			semanticRule: "context input budget requires cross-field arithmetic",
		},
		{
			name: "literal_header_matches_dynamic_auth_header",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1","model":"fixture",
				"auth":{"header":"X-Token","from_env":"KEY"},
				"headers":{"x-token":"literal"},"timeout":"1m"
			}`,
			semanticRule: "literal header must differ from the dynamic auth header",
		},
		{
			name: "api_model_exceeds_byte_limit",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1","model":` +
				strconv.Quote(strings.Repeat("界", 500)) + `,
				"auth":{"header":"Authorization","from_env":"KEY"},"timeout":"1m"
			}`,
			semanticRule: "model limit is measured in UTF-8 bytes",
		},
		{
			name: "cli_prompt_exceeds_byte_limit",
			document: `{"type":"cli","command":"codex","prompt":` +
				strconv.Quote(strings.Repeat("界", 40_000)) + `}`,
			semanticRule: "CLI token limit is measured in UTF-8 bytes",
		},
		{
			name: "wrong_openai_token_limit",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1","model":"fixture",
				"auth":{"header":"Authorization","from_env":"KEY"},
				"defaults":{"max_tokens":1024},"timeout":"1m"
			}`,
		},
		{
			name: "openai_token_limit_over_int64",
			document: `{
				"type":"api","driver":"openai-compatible",
				"endpoint":"https://example.invalid/v1","model":"fixture",
				"auth":{"header":"Authorization","from_env":"KEY"},
				"defaults":{"max_completion_tokens":9223372036854775808},
				"timeout":"1m"
			}`,
		},
		{
			name: "missing_anthropic_token_limit",
			document: `{
				"type":"api","driver":"anthropic-compatible",
				"endpoint":"https://example.invalid/v1","model":"fixture",
				"auth":{"header":"x-api-key","from_env":"KEY"},"timeout":"1m"
			}`,
		},
	}
	for _, testCase := range testCases {
		t.Run(testCase.name, func(t *testing.T) {
			instance, err := jsonschema.UnmarshalJSON(
				strings.NewReader(testCase.document),
			)
			if err != nil {
				t.Fatal(err)
			}
			schemaErr := schema.Validate(instance)

			configDir := filepath.Join(t.TempDir(), "configs")
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				filepath.Join(configDir, "fixture.json"),
				[]byte(testCase.document), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			_, loaderErr := Load(configDir)
			wantSchema := testCase.valid || testCase.semanticRule != ""
			if (schemaErr == nil) != wantSchema ||
				(loaderErr == nil) != testCase.valid {
				t.Fatalf(
					"schema_valid=%t loader_valid=%t semantic_rule=%q schema_error=%v loader_error=%v",
					wantSchema, testCase.valid, testCase.semanticRule,
					schemaErr, loaderErr,
				)
			}
		})
	}
	sourceProfiles, err := filepath.Glob(filepath.Join(
		filepath.Dir(source), "..", "configs", "*.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	for _, path := range sourceProfiles {
		t.Run("source_"+filepath.Base(path), func(t *testing.T) {
			data, err := os.ReadFile(path)
			if err != nil {
				t.Fatal(err)
			}
			instance, err := jsonschema.UnmarshalJSON(
				strings.NewReader(string(data)),
			)
			if err != nil {
				t.Fatal(err)
			}
			if err := schema.Validate(instance); err != nil {
				t.Fatalf("loader-valid source Profile violates schema: %v", err)
			}
		})
	}
}
