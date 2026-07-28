package profile

import (
	"encoding/json"
	"os"
	"path/filepath"
	"runtime"
	"testing"
)

func TestPublicSchemasAreValidJSON(t *testing.T) {
	_, source, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	for _, name := range []string{"profile.schema.json", "subcommand.schema.json"} {
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
			if name == "subcommand.schema.json" &&
				document["additionalProperties"] != false {
				t.Fatal("subcommand schema must reject unknown fields")
			}
		})
	}
}
