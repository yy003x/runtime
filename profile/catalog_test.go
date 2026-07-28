package profile

import (
	"os"
	"path/filepath"
	"reflect"
	"strings"
	"testing"

	"github.com/yy003x/runtime/command"
	"github.com/yy003x/runtime/model"
)

func TestLoadResolvesCLIAndAPIProfilesFromOneDirectory(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "configs")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(configDir, "cx.json"),
		[]byte(`{"type":"cli","binary":"codex","transport":"tty","prompt_delivery":"manual"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(configDir, "api-cx.json"),
		[]byte(`{"type":"api","driver":"openai-compatible","endpoint":"https://example.invalid/v1/chat/completions","model":"fixture","auth":{"header":"Authorization","scheme":"Bearer","from_env":"MODEL_API_KEY"},"timeout":"1m"}`),
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

func TestLoadRejectsInvalidUnifiedProfileFiles(t *testing.T) {
	for _, testCase := range []struct {
		name      string
		fileName  string
		content   string
		errorText string
	}{
		{
			name: "missing_type", fileName: "cx.json",
			content:   `{"binary":"codex","transport":"tty","prompt_delivery":"manual"}`,
			errorText: "type is required",
		},
		{
			name: "invalid_type", fileName: "cx.json",
			content:   `{"type":"other","binary":"codex","transport":"tty","prompt_delivery":"manual"}`,
			errorText: "type must be",
		},
		{
			name: "cross_domain_field", fileName: "cx.json",
			content:   `{"type":"cli","binary":"codex","transport":"tty","prompt_delivery":"manual","endpoint":"https://example.invalid/v1"}`,
			errorText: "unknown field",
		},
		{
			name: "reserved", fileName: "show.json",
			content:   `{"type":"cli","binary":"codex","transport":"tty","prompt_delivery":"manual"}`,
			errorText: "reserved profile ID",
		},
		{
			name: "legacy_openai_token_limit", fileName: "api-cx.json",
			content:   `{"type":"api","driver":"openai-compatible","endpoint":"https://example.invalid/v1/chat/completions","model":"fixture","auth":{"header":"Authorization","scheme":"Bearer","from_env":"MODEL_API_KEY"},"defaults":{"max_output_tokens":1024},"timeout":"1m"}`,
			errorText: "unknown field",
		},
		{
			name: "anthropic_token_limit_on_openai", fileName: "api-cx.json",
			content:   `{"type":"api","driver":"openai-compatible","endpoint":"https://example.invalid/v1/chat/completions","model":"fixture","auth":{"header":"Authorization","scheme":"Bearer","from_env":"MODEL_API_KEY"},"defaults":{"max_tokens":1024},"timeout":"1m"}`,
			errorText: "max_completion_tokens",
		},
		{
			name: "openai_token_limit_on_anthropic", fileName: "api-cc.json",
			content:   `{"type":"api","driver":"anthropic-compatible","endpoint":"https://example.invalid/v1/messages","model":"fixture","auth":{"header":"x-api-key","from_env":"MODEL_API_KEY"},"defaults":{"max_completion_tokens":1024},"timeout":"1m"}`,
			errorText: "max_tokens",
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

func TestLoadSubcommandsMapsOnlyDeclaredNames(t *testing.T) {
	configDir := filepath.Join(t.TempDir(), "configs")
	commandDir := filepath.Join(t.TempDir(), "commands")
	if err := os.MkdirAll(configDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(commandDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(configDir, "commit.json"),
		[]byte(`{"type":"cli","binary":"codex","transport":"tty","prompt_delivery":"argv"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(commandDir, "commit.json"),
		[]byte(`{"profile":"commit"}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	profiles, err := Load(configDir)
	if err != nil {
		t.Fatal(err)
	}
	subcommands, err := LoadSubcommands(commandDir, profiles, "profile")
	if err != nil {
		t.Fatal(err)
	}
	value, exists := subcommands.Get("commit")
	if !exists || value.Profile != "commit" {
		t.Fatalf("subcommand=%#v exists=%v", value, exists)
	}
	if _, exists := subcommands.Get("other"); exists {
		t.Fatal("undeclared profile became a top-level subcommand")
	}
}

func TestLoadSubcommandsRejectsMissingProfileAndFixedNamespace(t *testing.T) {
	commands, models := testCatalogs(t)
	profiles, err := NewCatalog(commands, models)
	if err != nil {
		t.Fatal(err)
	}
	for _, testCase := range []struct {
		name      string
		fileName  string
		content   string
		errorText string
	}{
		{
			name: "missing_profile", fileName: "missing.json",
			content: `{"profile":"missing"}`, errorText: "does not exist",
		},
		{
			name: "fixed_namespace", fileName: "profile.json",
			content: `{"profile":"cx"}`, errorText: "fixed namespace",
		},
		{
			name: "api_profile", fileName: "api.json",
			content: `{"profile":"api-cx"}`, errorText: "type=cli",
		},
		{
			name: "non_tty_profile", fileName: "detached.json",
			content: `{"profile":"detached"}`, errorText: "transport=tty",
		},
	} {
		t.Run(testCase.name, func(t *testing.T) {
			directory := t.TempDir()
			if err := os.WriteFile(
				filepath.Join(directory, testCase.fileName),
				[]byte(testCase.content),
				0o600,
			); err != nil {
				t.Fatal(err)
			}
			_, err := LoadSubcommands(directory, profiles, "profile")
			if err == nil || !strings.Contains(err.Error(), testCase.errorText) {
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
			Binary: "codex", Transport: command.TransportTTY,
			PromptDelivery: command.PromptManual,
		},
		"detached": {
			Binary: "codex", Transport: command.TransportTmux,
			PromptDelivery: command.PromptManual,
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	models, err := model.NewCatalog(map[string]model.Profile{
		"api-cx": {
			Driver:   model.DriverOpenAICompatible,
			Endpoint: "https://example.invalid/v1/chat/completions",
			Model:    "fixture",
			Auth:     model.Auth{Header: "Authorization", Scheme: "Bearer", FromEnv: "MODEL_API_KEY"},
			Timeout:  "1m",
		},
	})
	if err != nil {
		t.Fatal(err)
	}
	return commands, models
}
