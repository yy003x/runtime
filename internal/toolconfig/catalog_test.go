package toolconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"github.com/santhosh-tekuri/jsonschema/v6"
)

func TestLoadSourceManifests(t *testing.T) {
	root := repositoryRoot(t)
	catalog, err := LoadDirectory(
		filepath.Join(root, "resources", "tools"),
	)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(catalog.Names(), ","); got != "web_fetch,web_search" {
		t.Fatalf("unexpected tools: %s", got)
	}
	tests := []struct {
		name, endpoint, remote string
	}{
		{
			"web_search",
			"https://open.bigmodel.cn/api/mcp/web_search_prime/mcp",
			"web_search_prime",
		},
		{
			"web_fetch",
			"https://open.bigmodel.cn/api/mcp/web_reader/mcp",
			"webReader",
		},
	}
	for _, test := range tests {
		manifest, exists := catalog.Get(test.name)
		if !exists {
			t.Fatalf("missing %s", test.name)
		}
		if manifest.Executor.Endpoint != test.endpoint ||
			manifest.Executor.RemoteTool != test.remote ||
			manifest.Executor.Headers["Authorization"] !=
				"Bearer ${Z_AI_API_KEY}" {
			t.Fatalf("unexpected manifest: %#v", manifest)
		}
	}

	configuration, err := CanonicalJSON(mustSelect(t, catalog, catalog.Names()))
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(configuration), "actual-secret") ||
		!strings.Contains(string(configuration), "${Z_AI_API_KEY}") {
		t.Fatalf("unexpected configuration: %s", configuration)
	}
}

func TestToolSchemaAcceptsSourceManifests(t *testing.T) {
	root := repositoryRoot(t)
	compiler := jsonschema.NewCompiler()
	schema, err := compiler.Compile(
		filepath.Join(root, "resources", "schema", "tool.schema.json"),
	)
	if err != nil {
		t.Fatal(err)
	}
	for _, name := range []string{"web_search.json", "web_fetch.json"} {
		path := filepath.Join(root, "resources", "tools", name)
		file, err := os.Open(path)
		if err != nil {
			t.Fatal(err)
		}
		document, err := jsonschema.UnmarshalJSON(file)
		_ = file.Close()
		if err != nil {
			t.Fatal(err)
		}
		if err := schema.Validate(document); err != nil {
			t.Fatalf("%s: %v", name, err)
		}
	}
}

func TestLoadDirectoryRejectsInvalidEntriesAndDocuments(t *testing.T) {
	valid := manifestDocument("tool")
	tests := []struct {
		name    string
		prepare func(t *testing.T, directory string)
		want    string
	}{
		{
			name: "unsupported entry",
			prepare: func(t *testing.T, directory string) {
				writeFile(t, filepath.Join(directory, "README.md"), "text")
			},
			want: "unsupported entry",
		},
		{
			name: "file symlink",
			prepare: func(t *testing.T, directory string) {
				target := filepath.Join(t.TempDir(), "outside.json")
				writeFile(t, target, valid)
				if err := os.Symlink(target, filepath.Join(directory, "tool.json")); err != nil {
					t.Fatal(err)
				}
			},
			want: "unsupported entry",
		},
		{
			name: "basename mismatch",
			prepare: func(t *testing.T, directory string) {
				writeFile(t, filepath.Join(directory, "other.json"), valid)
			},
			want: "must match file basename",
		},
		{
			name: "unknown field",
			prepare: func(t *testing.T, directory string) {
				writeFile(t, filepath.Join(directory, "tool.json"),
					strings.Replace(valid, `"effect":`, `"unknown":true,"effect":`, 1))
			},
			want: "unknown field",
		},
		{
			name: "duplicate field",
			prepare: func(t *testing.T, directory string) {
				writeFile(t, filepath.Join(directory, "tool.json"),
					strings.Replace(valid, `"name":"tool"`, `"name":"tool","name":"tool"`, 1))
			},
			want: "duplicate field",
		},
		{
			name: "null",
			prepare: func(t *testing.T, directory string) {
				writeFile(t, filepath.Join(directory, "tool.json"),
					strings.Replace(valid, `"description":"test"`, `"description":null`, 1))
			},
			want: "must not be null",
		},
		{
			name: "unsupported effect",
			prepare: func(t *testing.T, directory string) {
				writeFile(t, filepath.Join(directory, "tool.json"),
					strings.Replace(valid, `"effect":"read_only"`, `"effect":"write"`, 1))
			},
			want: "effect must be",
		},
	}
	for _, test := range tests {
		t.Run(test.name, func(t *testing.T) {
			directory := t.TempDir()
			test.prepare(t, directory)
			_, err := LoadDirectory(directory)
			if err == nil || !strings.Contains(err.Error(), test.want) {
				t.Fatalf("error = %v, want %q", err, test.want)
			}
		})
	}
}

func TestLoadDirectoryRejectsMissingAndSymlinkDirectory(t *testing.T) {
	_, err := LoadDirectory(filepath.Join(t.TempDir(), "missing"))
	if err == nil {
		t.Fatal("expected missing directory error")
	}
	target := t.TempDir()
	link := filepath.Join(t.TempDir(), "tools")
	if err := os.Symlink(target, link); err != nil {
		t.Fatal(err)
	}
	_, err = LoadDirectory(link)
	if err == nil || !strings.Contains(err.Error(), "not a symlink") {
		t.Fatalf("unexpected error: %v", err)
	}
}

func TestCatalogReturnsCopiesAndSelectRejectsDuplicates(t *testing.T) {
	directory := t.TempDir()
	writeFile(t, filepath.Join(directory, "tool.json"), manifestDocument("tool"))
	catalog, err := LoadDirectory(directory)
	if err != nil {
		t.Fatal(err)
	}
	first, _ := catalog.Get("tool")
	first.Executor.Headers["Authorization"] = "changed"
	second, _ := catalog.Get("tool")
	if second.Executor.Headers["Authorization"] != "Bearer ${TOKEN}" {
		t.Fatal("catalog shared mutable headers")
	}
	if _, err := catalog.Select([]string{"tool", "tool"}); err == nil {
		t.Fatal("expected duplicate selection error")
	}
	if _, err := catalog.Select([]string{"missing"}); err == nil {
		t.Fatal("expected unknown selection error")
	}
}

func TestManifestRejectsLiteralHeadersAndSensitiveQuery(t *testing.T) {
	var manifest Manifest
	if err := json.Unmarshal([]byte(manifestDocument("tool")), &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Executor.Headers["Authorization"] = "Bearer plaintext"
	if err := manifest.Validate(); err == nil ||
		!strings.Contains(err.Error(), "environment reference") {
		t.Fatalf("unexpected literal header error: %v", err)
	}
	if err := json.Unmarshal([]byte(manifestDocument("tool")), &manifest); err != nil {
		t.Fatal(err)
	}
	manifest.Executor.Endpoint = "https://example.com/mcp?api_key=plaintext"
	if err := manifest.Validate(); err == nil ||
		!strings.Contains(err.Error(), "sensitive query parameter") {
		t.Fatalf("unexpected sensitive query error: %v", err)
	}
}

func manifestDocument(name string) string {
	return `{"schema_version":1,"name":"` + name +
		`","effect":"read_only","description":"test",` +
		`"input_schema":{"type":"object","properties":{},"additionalProperties":false},` +
		`"executor":{"type":"mcp","endpoint":"https://example.com/mcp",` +
		`"remote_tool":"remote","headers":{"Authorization":"Bearer ${TOKEN}"},` +
		`"timeout":"30s","max_response_bytes":1048576}}`
}

func mustSelect(
	t *testing.T,
	catalog *Catalog,
	names []string,
) []Manifest {
	t.Helper()
	values, err := catalog.Select(names)
	if err != nil {
		t.Fatal(err)
	}
	return values
}

func writeFile(t *testing.T, path, content string) {
	t.Helper()
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func repositoryRoot(t *testing.T) string {
	t.Helper()
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("resolve source path")
	}
	return filepath.Clean(filepath.Join(filepath.Dir(source), "..", ".."))
}

func TestCanonicalJSONDoesNotResolveEnvironment(t *testing.T) {
	manifest := Manifest{}
	if err := json.Unmarshal([]byte(manifestDocument("tool")), &manifest); err != nil {
		t.Fatal(err)
	}
	if err := os.Setenv("TOKEN", "actual-secret"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = os.Unsetenv("TOKEN") })
	data, err := CanonicalJSON([]Manifest{manifest})
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "actual-secret") ||
		!strings.Contains(string(data), "${TOKEN}") {
		t.Fatalf("snapshot leaked or lost reference: %s", data)
	}
}
