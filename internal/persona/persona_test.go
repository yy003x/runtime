package persona

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoaderDefaultsAndRenderSystem(t *testing.T) {
	dir := t.TempDir()
	content := `system_prompt: Be precise.
style_rules:
  - Use evidence
response_policy:
  format: markdown
  verbosity: concise
`
	if err := os.WriteFile(filepath.Join(dir, "reviewer.yaml"), []byte(content), 0o644); err != nil {
		t.Fatal(err)
	}
	loaded, err := NewLoader(dir).Load(context.Background(), "reviewer")
	if err != nil {
		t.Fatal(err)
	}
	if loaded.ID != "reviewer" || loaded.Name != "Reviewer" {
		t.Fatalf("persona=%#v", loaded)
	}
	rendered := RenderSystem(loaded)
	for _, expected := range []string{"Be precise.", "- Use evidence", "format: markdown", "verbosity: concise"} {
		if !strings.Contains(rendered, expected) {
			t.Fatalf("rendered=%q missing %q", rendered, expected)
		}
	}
	if _, err := NewLoader(dir).Load(context.Background(), "missing"); err == nil {
		t.Fatal("missing persona was accepted")
	}
}
