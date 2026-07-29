package runtimeconfig

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
)

func TestDefaultIsValid(t *testing.T) {
	if err := Default().Validate(); err != nil {
		t.Fatal(err)
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
		value.Agent.MaxRounds != 16 || value.Run.SettledRetention != "168h" {
		t.Fatalf("config=%#v", value)
	}
	for _, document := range []string{
		`{"unknown":true}`,
		`{"agent":{"tools":["unknown"]}}`,
		`{"agent":{"workspace_roots":["/tmp","/tmp/"]}}`,
	} {
		if err := os.WriteFile(path, []byte(document), 0o600); err != nil {
			t.Fatal(err)
		}
		if _, err := Load(path); err == nil {
			t.Fatalf("accepted invalid runtime config %s", document)
		}
	}
}

func TestRuntimeSchemaIsStrictJSON(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	data, err := os.ReadFile(filepath.Join(
		filepath.Dir(source), "..", "..", "resources", "schema",
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
		t.Fatal("runtime schema does not match the strict vNext root")
	}
}
