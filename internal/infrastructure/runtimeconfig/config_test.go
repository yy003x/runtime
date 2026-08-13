package runtimeconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"

	"github.com/yy003x/runtime/internal/testkit/reporoot"
)

func TestDefaultIsValid(t *testing.T) {
	value := Default()
	if err := value.Validate(); err != nil {
		t.Fatal(err)
	}
	want := []string{"read_file", "list_directory", "web_search", "web_fetch"}
	if len(value.Agent.Tools) != len(want) {
		t.Fatalf("default tools=%#v", value.Agent.Tools)
	}
	for index := range want {
		if value.Agent.Tools[index] != want[index] {
			t.Fatalf("default tools=%#v", value.Agent.Tools)
		}
	}
	if value.Tmux.ServerMode != TmuxServerModeDefault {
		t.Fatalf("default tmux server mode=%q", value.Tmux.ServerMode)
	}
	if value.Run.Reaper.Interval != "5m" ||
		value.Run.Reaper.PausedTTL != "30m" ||
		value.Run.Reaper.NeedsReconciliationTTL != "24h" {
		t.Fatalf("default run reaper=%#v", value.Run.Reaper)
	}
}

func TestReleaseRuntimeMatchesLoaderDefaults(t *testing.T) {
	path := filepath.Join(reporoot.Root(t), "release", "runtime.json")
	value, err := LoadRequired(path)
	if err != nil {
		t.Fatal(err)
	}
	if !reflect.DeepEqual(value, Default()) {
		t.Fatalf("release runtime=%#v, want defaults=%#v", value, Default())
	}
}

func TestValidateAgentToolsUsesManifestNameContract(t *testing.T) {
	for _, testCase := range []struct {
		name  string
		valid bool
	}{
		{name: "custom_tool", valid: true},
		{name: "WebSearch2", valid: true},
		{name: "_private"},
		{name: "web.search"},
		{name: strings.Repeat("a", 65)},
	} {
		config := Default()
		config.Agent.Tools = []string{testCase.name}
		err := config.Validate()
		if (err == nil) != testCase.valid {
			t.Fatalf("name=%q valid=%t error=%v", testCase.name, testCase.valid, err)
		}
	}
	config := Default()
	config.Agent.Tools = make([]string, maxAgentTools+1)
	for index := range config.Agent.Tools {
		config.Agent.Tools[index] = "tool" + strings.Repeat("a", index/10) +
			string(rune('a'+index%10))
	}
	if err := config.Validate(); err == nil ||
		!strings.Contains(err.Error(), "must not exceed") {
		t.Fatalf("oversized tool selection error=%v", err)
	}
}

func TestLoadOverlaysDefaultsAndRejectsUnknownConfiguration(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.json")
	if err := os.WriteFile(
		path,
		[]byte(`{"scheduler":{"workers":2}}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	value, err := Load(path)
	if err != nil {
		t.Fatal(err)
	}
	if value.Scheduler.Workers != 2 ||
		value.Agent.MaxRounds != 16 || value.Run.SettledRetention != "168h" ||
		value.Tmux.ServerMode != TmuxServerModeDefault {
		t.Fatalf("config=%#v", value)
	}
	for _, document := range []string{
		`{"unknown":true}`,
		`{"agent":{"tools":["invalid.tool"]}}`,
		`{"agent":{"workspace_roots":["/tmp","/tmp/"]}}`,
		`{"tmux":{"server_mode":"shared"}}`,
	} {
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted invalid runtime config %s", document)
		}
	}
}

func TestLoadRequiredRejectsMissingAndSymlinkRuntime(t *testing.T) {
	root := t.TempDir()
	missing := filepath.Join(root, "missing.json")
	if _, err := Load(missing); err != nil {
		t.Fatalf("optional Load rejected missing runtime: %v", err)
	}
	if _, err := LoadRequired(missing); err == nil {
		t.Fatal("LoadRequired accepted missing runtime")
	}
	target := filepath.Join(root, "target.json")
	link := filepath.Join(root, "runtime.json")
	if err := os.WriteFile(target, []byte(`{}`), 0o600); err != nil {
		t.Fatal(err)
	}
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	if _, err := LoadRequired(link); err == nil {
		t.Fatal("LoadRequired accepted a symlink")
	}
}

func TestRuntimeSchemaIsStrictJSON(t *testing.T) {
	data, err := os.ReadFile(filepath.Join(
		reporoot.Root(t), "resources", "schema",
		"runtime.schema.json",
	))
	if err != nil {
		t.Fatal(err)
	}
	var document map[string]any
	if err := json.Unmarshal(data, &document); err != nil {
		t.Fatal(err)
	}
	if document["additionalProperties"] != false ||
		!strings.Contains(string(data), `"settled_retention"`) {
		t.Fatal("runtime schema does not match the strict Runtime root")
	}
}

func TestRuntimeSchemaAndLoaderShareContractFixtures(t *testing.T) {
	repositoryRoot := reporoot.Root(t)
	schemaPath := filepath.Join(
		repositoryRoot, "resources", "schema",
		"runtime.schema.json",
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
		{name: "defaults", document: `{}`, valid: true},
		{
			name:     "partial_overlay",
			document: `{"scheduler":{"workers":2},"tmux":{"server_mode":"dedicated"}}`,
			valid:    true,
		},
		{
			name: "complete",
			document: `{
				"agent":{
					"tools":["read_file","web_search"],
					"workspace_roots":["/tmp/work"],
					"max_rounds":32,
					"max_tool_calls":128,
					"max_total_tokens":4096,
					"max_wall_time":"+1h30m"
				},
				"scheduler":{"workers":4,"poll_interval":".25s"},
				"run":{
					"settled_retention":"168h",
					"reaper":{
						"interval":"5m",
						"paused_ttl":"30m",
						"needs_reconciliation_ttl":"24h"
					}
				},
				"tmux":{"server_mode":"default"}
			}`,
			valid: true,
		},
		{
			name: "reaper_disabled",
			document: `{"run":{"reaper":{
				"interval":"0",
				"paused_ttl":"0",
				"needs_reconciliation_ttl":"0"
			}}}`,
			valid: true,
		},
		{
			name:     "go_duration_greek_microseconds",
			document: `{"agent":{"max_wall_time":"1000000μs"}}`,
			valid:    true,
		},
		{name: "unknown_root", document: `{"unknown":true}`},
		{name: "unknown_nested", document: `{"agent":{"unknown":true}}`},
		{
			name:     "unknown_reaper_property",
			document: `{"run":{"reaper":{"unknown":true}}}`,
		},
		{name: "invalid_tmux_mode", document: `{"tmux":{"server_mode":"shared"}}`},
		{name: "null_tools", document: `{"agent":{"tools":null}}`},
		{
			name:     "null_workspace_roots",
			document: `{"agent":{"workspace_roots":null}}`,
		},
		{name: "manifest_tool", document: `{"agent":{"tools":["custom_tool"]}}`, valid: true},
		{name: "tool_starts_with_underscore", document: `{"agent":{"tools":["_tool"]}}`},
		{name: "tool_contains_dot", document: `{"agent":{"tools":["web.search"]}}`},
		{name: "tool_name_too_long", document: `{"agent":{"tools":["aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"]}}`},
		{
			name:     "duplicate_tool",
			document: `{"agent":{"tools":["read_file","read_file"]}}`,
		},
		{name: "max_rounds_zero", document: `{"agent":{"max_rounds":0}}`},
		{name: "max_rounds_too_large", document: `{"agent":{"max_rounds":129}}`},
		{
			name:     "max_tool_calls_zero",
			document: `{"agent":{"max_tool_calls":0}}`,
		},
		{
			name:     "max_tool_calls_too_large",
			document: `{"agent":{"max_tool_calls":1025}}`,
		},
		{
			name:     "negative_token_budget",
			document: `{"agent":{"max_total_tokens":-1}}`,
		},
		{
			name:     "token_budget_over_int64",
			document: `{"agent":{"max_total_tokens":9223372036854775808}}`,
		},
		{
			name:     "relative_workspace_root",
			document: `{"agent":{"workspace_roots":["relative"]}}`,
		},
		{
			name:     "invalid_wall_time",
			document: `{"agent":{"max_wall_time":"forever"}}`,
		},
		{
			name:     "zero_wall_time",
			document: `{"agent":{"max_wall_time":"0s"}}`,
		},
		{
			name:         "wall_time_below_runtime_bound",
			document:     `{"agent":{"max_wall_time":"1ms"}}`,
			semanticRule: "duration total is bounded by the Go loader",
		},
		{
			name:         "wall_time_above_runtime_bound",
			document:     `{"agent":{"max_wall_time":"25h"}}`,
			semanticRule: "duration total is bounded by the Go loader",
		},
		{
			name:         "clean_duplicate_workspace_roots",
			document:     `{"agent":{"workspace_roots":["/tmp","/tmp/"]}}`,
			semanticRule: "workspace roots are unique after filepath.Clean",
		},
		{name: "workers_zero", document: `{"scheduler":{"workers":0}}`},
		{name: "workers_too_large", document: `{"scheduler":{"workers":33}}`},
		{
			name:     "invalid_poll_interval",
			document: `{"scheduler":{"poll_interval":"soon"}}`,
		},
		{
			name:         "poll_interval_below_runtime_bound",
			document:     `{"scheduler":{"poll_interval":"1ms"}}`,
			semanticRule: "duration total is bounded by the Go loader",
		},
		{
			name:         "poll_interval_above_runtime_bound",
			document:     `{"scheduler":{"poll_interval":"2m"}}`,
			semanticRule: "duration total is bounded by the Go loader",
		},
		{
			name:     "invalid_retention",
			document: `{"run":{"settled_retention":"later"}}`,
		},
		{
			name:         "retention_below_runtime_bound",
			document:     `{"run":{"settled_retention":"1m"}}`,
			semanticRule: "duration total is bounded by the Go loader",
		},
		{
			name:         "retention_above_runtime_bound",
			document:     `{"run":{"settled_retention":"8761h"}}`,
			semanticRule: "duration total is bounded by the Go loader",
		},
		{
			name:     "noncanonical_zero_reaper_interval",
			document: `{"run":{"reaper":{"interval":"0s"}}}`,
		},
		{
			name:     "invalid_reaper_interval",
			document: `{"run":{"reaper":{"interval":"later"}}}`,
		},
		{
			name:         "reaper_interval_above_runtime_bound",
			document:     `{"run":{"reaper":{"interval":"1h1ns"}}}`,
			semanticRule: "duration total is bounded by the Go loader",
		},
		{
			name:         "reaper_paused_ttl_above_runtime_bound",
			document:     `{"run":{"reaper":{"paused_ttl":"721h"}}}`,
			semanticRule: "duration total is bounded by the Go loader",
		},
		{
			name:         "reaper_reconciliation_ttl_above_runtime_bound",
			document:     `{"run":{"reaper":{"needs_reconciliation_ttl":"721h"}}}`,
			semanticRule: "duration total is bounded by the Go loader",
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

			path := filepath.Join(t.TempDir(), "runtime.json")
			if err := os.WriteFile(
				path, []byte(testCase.document), 0o600,
			); err != nil {
				t.Fatal(err)
			}
			_, loaderErr := Load(path)
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
	sourceConfig := filepath.Join(
		repositoryRoot, "release",
		"runtime.json",
	)
	data, err := os.ReadFile(sourceConfig)
	if err != nil {
		t.Fatal(err)
	}
	instance, err := jsonschema.UnmarshalJSON(strings.NewReader(string(data)))
	if err != nil {
		t.Fatal(err)
	}
	if err := schema.Validate(instance); err != nil {
		t.Fatalf("loader-valid source runtime config violates schema: %v", err)
	}
	if _, err := Load(sourceConfig); err != nil {
		t.Fatalf("source runtime config is not loader-valid: %v", err)
	}
}
