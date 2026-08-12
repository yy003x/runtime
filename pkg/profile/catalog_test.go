package profile

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yy003x/runtime/internal/testkit/reporoot"
	"github.com/yy003x/runtime/pkg/command"
	"github.com/yy003x/runtime/pkg/model"
)

func TestLoadResolvesCLIAndAPIProfilesFromOneDirectory(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "configs")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(configDir, "cx.json"),
		[]byte(`{"type":"cli","command":"codex","model":"fixture"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(configDir, "api-cx.json"),
		[]byte(`{"type":"api","driver":"openai","base_url":"https://example.invalid/provider","model":"fixture","headers":{"Authorization":"${MODEL_API_KEY}"},"timeout":"1m"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	catalog, err := Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	entries := catalog.Entries()
	if len(entries) != 2 ||
		entries[0].ID != "api-cx" || entries[0].Kind != KindModel ||
		entries[1].ID != "cx" || entries[1].Kind != KindCommand {
		t.Fatalf("entries=%#v", entries)
	}
	commandEntry, exists := catalog.Resolve("cx")
	if !exists || commandEntry.Command == nil || commandEntry.Model != nil {
		t.Fatalf("command entry=%#v exists=%v", commandEntry, exists)
	}
	modelEntry, exists := catalog.Resolve("api-cx")
	if !exists || modelEntry.Model == nil || modelEntry.Command != nil {
		t.Fatalf("model entry=%#v exists=%v", modelEntry, exists)
	}
}

func TestLoadAllowsUnreservedProfileIDs(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "configs")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"exec", "open"} {
		if err := os.WriteFile(
			filepath.Join(configDir, id+".json"),
			[]byte(`{"type":"cli","command":"codex"}`),
			0o600,
		); err != nil {
			t.Fatal(err)
		}
	}
	catalog, err := Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	for _, id := range []string{"exec", "open"} {
		entry, exists := catalog.Resolve(id)
		if !exists || entry.Kind != KindCommand {
			t.Fatalf("profile %q entry=%#v exists=%v", id, entry, exists)
		}
	}
}

func TestSourceProfilesUseCurrentUnifiedProtocol(t *testing.T) {
	files, err := filepath.Glob(
		filepath.Join(reporoot.Root(t), "configs", "*.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	required := map[string]bool{"api-cc.json": true, "api-cx.json": true}
	for _, file := range files {
		delete(required, filepath.Base(file))
		kind, cliProfile, _, err := loadFile(file)
		if err != nil {
			t.Fatalf("%s: %v", file, err)
		}
		if kind == KindCommand {
			if err := command.CheckProfile(cliProfile); err != nil {
				t.Fatalf("%s: %v", file, err)
			}
		}
	}
	if len(required) != 0 {
		t.Fatalf("source profiles are missing required files: %v", required)
	}
}

func TestSourceAPIProfilesResolveDriverDefaultEndpoints(t *testing.T) {
	repositoryRoot := reporoot.Root(t)
	for name, want := range map[string]string{
		"api-cc.json": "https://open.bigmodel.cn/api/anthropic/v1/messages",
		"api-cx.json": "https://ws-guu9tlrmhj23g0fa.cn-beijing.maas.aliyuncs.com/compatible-mode/v1/chat/completions",
	} {
		t.Run(name, func(t *testing.T) {
			kind, _, apiProfile, err := loadFile(filepath.Join(
				repositoryRoot, "configs", name,
			))
			if err != nil {
				t.Fatal(err)
			}
			if kind != KindModel {
				t.Fatalf("kind=%q", kind)
			}
			got, err := apiProfile.ResolvedEndpoint()
			if err != nil || got != want {
				t.Fatalf("endpoint=%q want=%q error=%v", got, want, err)
			}
		})
	}
}

func TestLoadRejectsInvalidUnifiedProfileFiles(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		fileName  string
		content   string
		errorText string
	}{
		{
			name: "missing_type", fileName: "cx.json",
			content:   `{"command":"codex"}`,
			errorText: "type is required",
		},
		{
			name: "invalid_type", fileName: "cx.json",
			content:   `{"type":"other","command":"codex"}`,
			errorText: "type must be",
		},
		{
			name: "cross_domain_field", fileName: "cx.json",
			content:   `{"type":"cli","command":"codex","endpoint":"https://example.invalid/v1"}`,
			errorText: "unknown field",
		},
		{
			name: "unknown_cli_field", fileName: "cx.json",
			content:   `{"type":"cli","command":"codex","unexpected":true}`,
			errorText: "unknown field",
		},
		{
			name: "reserved_show", fileName: "show.json",
			content:   `{"type":"cli","command":"codex"}`,
			errorText: "reserved profile ID",
		},
		{
			name: "reserved_list", fileName: "list.json",
			content:   `{"type":"cli","command":"codex"}`,
			errorText: "reserved profile ID",
		},
		{
			name: "reserved_check", fileName: "check.json",
			content:   `{"type":"cli","command":"codex"}`,
			errorText: "reserved profile ID",
		},
		{
			name: "endpoint_and_base_url", fileName: "api-cx.json",
			content:   `{"type":"api","driver":"openai","endpoint":"https://example.invalid/v1/chat/completions","base_url":"https://example.invalid","model":"fixture","headers":{"Authorization":"${MODEL_API_KEY}"},"timeout":"1m"}`,
			errorText: "exactly one",
		},
		{
			name: "missing_endpoint_and_base_url", fileName: "api-cx.json",
			content:   `{"type":"api","driver":"openai","model":"fixture","headers":{"Authorization":"${MODEL_API_KEY}"},"timeout":"1m"}`,
			errorText: "exactly one",
		},
		{
			name: "default_context_without_input_budget", fileName: "api-cc.json",
			content:   `{"type":"api","driver":"anthropic","endpoint":"https://example.invalid/v1/messages","model":"fixture","headers":{"x-api-key":"${MODEL_API_KEY}"},"parameters":{"max_tokens":32767},"timeout":"1m"}`,
			errorText: "context window",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			configDir := filepath.Join(t.TempDir(), "configs")
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				filepath.Join(configDir, testCase.fileName),
				[]byte(testCase.content),
				0o600,
			); err != nil {
				t.Fatal(err)
			}
			_, err := Load(configDir)
			if err == nil || !strings.Contains(err.Error(), testCase.errorText) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestLoadRejectsCallerReservedProfileID(t *testing.T) {
	for _, id := range []string{"profile", "session", "tmux", "agent"} {
		t.Run(id, func(t *testing.T) {
			configDir := filepath.Join(t.TempDir(), "configs")
			if err := os.MkdirAll(configDir, 0o700); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(
				filepath.Join(configDir, id+".json"),
				[]byte(`{"type":"cli","command":"codex"}`),
				0o600,
			); err != nil {
				t.Fatal(err)
			}
			_, err := Load(
				configDir, "profile", "session", "tmux", "agent",
			)
			if err == nil || !strings.Contains(
				err.Error(), "reserved profile ID",
			) {
				t.Fatalf("error=%v", err)
			}
		})
	}
}

func TestEntriesReturnsDefensiveCopies(t *testing.T) {
	commands, models := testCatalogs(t)
	catalog, err := NewCatalog(commands, models)
	if err != nil {
		t.Fatal(err)
	}
	first := catalog.Entries()
	second := catalog.Entries()
	if !reflect.DeepEqual(first, second) {
		t.Fatalf("first=%#v second=%#v", first, second)
	}
	first[0].ID = "mutated"
	if catalog.Entries()[0].ID == "mutated" {
		t.Fatal("catalog entries were mutated")
	}
}

func testCatalogs(t *testing.T) (*command.Catalog, *model.Catalog) {
	t.Helper()
	commands, err := command.NewCatalog(map[string]command.Profile{
		"cx": {
			Command: "codex", Model: "fixture",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	models, err := model.NewCatalog(map[string]model.Profile{
		"api-cx": {
			Driver:   model.DriverOpenAI,
			Endpoint: "https://example.invalid/v1/chat/completions",
			Model:    "fixture",
			Headers:  map[string]string{"Authorization": "${MODEL_API_KEY}"},
			Timeout:  "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return commands, models
}
