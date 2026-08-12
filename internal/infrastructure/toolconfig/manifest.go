// Package toolconfig loads the provider-neutral definitions and executor
// bindings for externally hosted Agent tools.
package toolconfig

import (
	"bytes"
	"crypto/sha256"
	"encoding/json"
	"fmt"
	"net/http"
	"net/url"
	"regexp"
	"sort"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/yy003x/runtime/internal/infrastructure/envref"
	"github.com/yy003x/runtime/pkg/contract"
)

const (
	SchemaVersion = 1

	ExecutorMCP string = "mcp"

	minimumTimeout          = time.Second
	maximumTimeout          = 2 * time.Minute
	minimumMaxResponseBytes = 1024
	maximumMaxResponseBytes = 8 << 20
)

var namePattern = regexp.MustCompile(`^[A-Za-z][A-Za-z0-9_-]{0,63}$`)

type Manifest struct {
	SchemaVersion int             `json:"schema_version"`
	Name          string          `json:"name"`
	Effect        contract.Effect `json:"effect"`
	Risk          contract.Risk   `json:"risk,omitempty"`
	Description   string          `json:"description"`
	InputSchema   json.RawMessage `json:"input_schema"`
	Executor      Executor        `json:"executor"`
}

type Executor struct {
	Type             string            `json:"type"`
	Endpoint         string            `json:"endpoint"`
	RemoteTool       string            `json:"remote_tool"`
	Headers          map[string]string `json:"headers"`
	Timeout          string            `json:"timeout"`
	MaxResponseBytes int64             `json:"max_response_bytes"`
}

func ValidateName(value string) error {
	if !namePattern.MatchString(value) {
		return fmt.Errorf(
			"tool name must match %s", namePattern.String(),
		)
	}
	return nil
}

func (manifest Manifest) Validate() error {
	if manifest.SchemaVersion != SchemaVersion {
		return fmt.Errorf(
			"unsupported schema_version %d", manifest.SchemaVersion,
		)
	}
	if err := ValidateName(manifest.Name); err != nil {
		return err
	}
	switch manifest.Effect {
	case contract.EffectReadOnly, contract.EffectWriteLocal, contract.EffectWriteExternal:
	default:
		return fmt.Errorf(
			"effect must be one of %q, %q, %q",
			contract.EffectReadOnly, contract.EffectWriteLocal,
			contract.EffectWriteExternal,
		)
	}
	if manifest.Risk != "" &&
		manifest.Risk != contract.RiskLow && manifest.Risk != contract.RiskHigh {
		return fmt.Errorf("risk must be %q or %q", contract.RiskLow, contract.RiskHigh)
	}
	// 写副作用必须显式声明 risk，避免高危 tool 因缺省被当作 low 静默放行。
	if manifest.Effect != contract.EffectReadOnly && manifest.Risk == "" {
		return fmt.Errorf("risk is required for write effects")
	}
	if strings.TrimSpace(manifest.Description) == "" {
		return fmt.Errorf("description is required")
	}
	definition := manifest.Definition()
	if err := definition.Validate(); err != nil {
		return err
	}
	canonicalSchema, err := canonicalInputSchema(manifest.Name, manifest.InputSchema)
	if err != nil {
		return err
	}
	manifest.InputSchema = canonicalSchema
	if err := manifest.Executor.Validate(); err != nil {
		return fmt.Errorf("executor: %w", err)
	}
	return nil
}

func (manifest Manifest) Definition() contract.ToolSpec {
	return contract.ToolSpec{
		Name: manifest.Name, Description: manifest.Description,
		InputSchema: append(json.RawMessage(nil), manifest.InputSchema...),
	}
}

func (manifest Manifest) Clone() Manifest {
	clone := manifest
	clone.InputSchema = append(json.RawMessage(nil), manifest.InputSchema...)
	clone.Executor.Headers = cloneStrings(manifest.Executor.Headers)
	return clone
}

func (executor Executor) Validate() error {
	if executor.Type != ExecutorMCP {
		return fmt.Errorf("type must be %q", ExecutorMCP)
	}
	parsed, err := url.Parse(executor.Endpoint)
	if err != nil || parsed.Scheme == "" || parsed.Host == "" {
		return fmt.Errorf("endpoint must be an absolute HTTP or HTTPS URL")
	}
	if parsed.Scheme != "https" && parsed.Scheme != "http" {
		return fmt.Errorf("endpoint must use HTTP or HTTPS")
	}
	if parsed.User != nil || parsed.Fragment != "" {
		return fmt.Errorf("endpoint must not contain userinfo or a fragment")
	}
	if strings.Contains(executor.Endpoint, "${") {
		return fmt.Errorf("endpoint must not contain environment references")
	}
	for name := range parsed.Query() {
		if sensitiveQueryName(name) {
			return fmt.Errorf(
				"endpoint must not contain sensitive query parameter %q", name,
			)
		}
	}
	if err := ValidateName(executor.RemoteTool); err != nil {
		return fmt.Errorf("remote_tool: %w", err)
	}
	if executor.Headers == nil {
		return fmt.Errorf("headers must be an object")
	}
	seenHeaders := make(map[string]string, len(executor.Headers))
	for name, value := range executor.Headers {
		canonical := http.CanonicalHeaderKey(name)
		if canonical == "" || !validHeaderName(name) {
			return fmt.Errorf("headers contains invalid name %q", name)
		}
		if previous, exists := seenHeaders[strings.ToLower(canonical)]; exists {
			return fmt.Errorf(
				"headers contains duplicate names %q and %q", previous, name,
			)
		}
		seenHeaders[strings.ToLower(canonical)] = name
		if protocolOwnedHeader(canonical) {
			return fmt.Errorf("headers must not configure protocol header %q", name)
		}
		if value == "" || !utf8.ValidString(value) || strings.ContainsAny(value, "\r\n\x00") {
			return fmt.Errorf("headers[%q] contains an invalid value", name)
		}
		if _, err := envref.Expand(value, func(string) (string, bool) {
			return "secret", true
		}); err != nil {
			return fmt.Errorf("headers[%q]: %w", name, err)
		}
		references := envref.References(value)
		if len(references) == 0 {
			return fmt.Errorf(
				"headers[%q] must contain an environment reference", name,
			)
		}
		if strings.EqualFold(canonical, "Authorization") {
			if len(references) != 1 || value != "Bearer ${"+references[0]+"}" {
				return fmt.Errorf(
					"headers[%q] must use Bearer ${VAR_NAME}", name,
				)
			}
		}
	}
	timeout, err := time.ParseDuration(executor.Timeout)
	if err != nil || timeout < minimumTimeout || timeout > maximumTimeout {
		return fmt.Errorf(
			"timeout must be between %s and %s", minimumTimeout, maximumTimeout,
		)
	}
	if executor.MaxResponseBytes < minimumMaxResponseBytes ||
		executor.MaxResponseBytes > maximumMaxResponseBytes {
		return fmt.Errorf(
			"max_response_bytes must be between %d and %d",
			minimumMaxResponseBytes, maximumMaxResponseBytes,
		)
	}
	return nil
}

func sensitiveQueryName(value string) bool {
	value = strings.ToLower(strings.ReplaceAll(value, "-", "_"))
	switch value {
	case "access_token", "api_key", "apikey", "auth", "authorization",
		"key", "password", "secret", "signature", "token":
		return true
	default:
		return strings.HasSuffix(value, "_api_key") ||
			strings.HasSuffix(value, "_secret") ||
			strings.HasSuffix(value, "_token")
	}
}

func (executor Executor) Duration() time.Duration {
	value, _ := time.ParseDuration(executor.Timeout)
	return value
}

func canonicalInputSchema(
	name string,
	value json.RawMessage,
) (json.RawMessage, error) {
	var document any
	decoder := json.NewDecoder(bytes.NewReader(value))
	decoder.UseNumber()
	if err := decoder.Decode(&document); err != nil {
		return nil, fmt.Errorf("input_schema: %w", err)
	}
	object, ok := document.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("input_schema must be a JSON object")
	}
	if kind, ok := object["type"].(string); !ok || kind != "object" {
		return nil, fmt.Errorf("input_schema type must be object")
	}
	canonical, err := json.Marshal(object)
	if err != nil {
		return nil, fmt.Errorf("canonicalize input_schema: %w", err)
	}
	parsed, err := jsonschema.UnmarshalJSON(bytes.NewReader(canonical))
	if err != nil {
		return nil, fmt.Errorf("input_schema: %w", err)
	}
	sum := sha256.Sum256(append(append([]byte(name), 0), canonical...))
	resource := fmt.Sprintf("urn:sn-runtime:configured-tool:%x", sum[:])
	compiler := jsonschema.NewCompiler()
	if err := compiler.AddResource(resource, parsed); err != nil {
		return nil, fmt.Errorf("input_schema: %w", err)
	}
	if _, err := compiler.Compile(resource); err != nil {
		return nil, fmt.Errorf("input_schema: %w", err)
	}
	return canonical, nil
}

func canonicalManifest(manifest Manifest) Manifest {
	canonical := manifest.Clone()
	canonical.InputSchema, _ = canonicalInputSchema(
		canonical.Name, canonical.InputSchema,
	)
	if len(canonical.Executor.Headers) > 0 {
		headers := make(map[string]string, len(canonical.Executor.Headers))
		names := make([]string, 0, len(canonical.Executor.Headers))
		for name := range canonical.Executor.Headers {
			names = append(names, name)
		}
		sort.Slice(names, func(left, right int) bool {
			return strings.ToLower(names[left]) < strings.ToLower(names[right])
		})
		for _, name := range names {
			headers[http.CanonicalHeaderKey(name)] = canonical.Executor.Headers[name]
		}
		canonical.Executor.Headers = headers
	}
	return canonical
}

func cloneStrings(values map[string]string) map[string]string {
	if values == nil {
		return nil
	}
	clone := make(map[string]string, len(values))
	for key, value := range values {
		clone[key] = value
	}
	return clone
}

func validHeaderName(value string) bool {
	if value == "" {
		return false
	}
	for index := 0; index < len(value); index++ {
		current := value[index]
		if current >= 'a' && current <= 'z' ||
			current >= 'A' && current <= 'Z' ||
			current >= '0' && current <= '9' ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", rune(current)) {
			continue
		}
		return false
	}
	return true
}

func protocolOwnedHeader(value string) bool {
	switch strings.ToLower(value) {
	case "accept", "content-type", "content-length", "connection",
		"host", "mcp-protocol-version", "mcp-session-id", "transfer-encoding":
		return true
	default:
		return false
	}
}
