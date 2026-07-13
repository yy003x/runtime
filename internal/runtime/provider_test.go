package runtime

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestCommandProviderPassesConfiguredArgsPromptImagesAndEnv(t *testing.T) {
	root := t.TempDir()
	script := filepath.Join(root, "capture.sh")
	body := `#!/usr/bin/env bash
printf 'args=%s\n' "$*"
printf 'env=%s\n' "$SN_CLI_TEST_VALUE"
printf 'prompt='
cat
`
	if err := os.WriteFile(script, []byte(body), 0o755); err != nil {
		t.Fatal(err)
	}

	result, err := (CommandProvider{}).Run(context.Background(), Profile{
		Provider: ProviderConfig{
			Command: script,
			Args:    []string{"exec", "--model", "test-model"},
			Env:     map[string]string{"SN_CLI_TEST_VALUE": "configured"},
		},
	}, RunRequest{
		Prompt: "hello from stdin",
		Images: []string{"/tmp/one.png", "/tmp/two.png"},
		CWD:    root,
	})
	if err != nil {
		t.Fatalf("Run returned error: %v", err)
	}
	for _, want := range []string{
		"args=exec --model test-model --image /tmp/one.png --image /tmp/two.png",
		"env=configured",
		"prompt=hello from stdin",
	} {
		if !strings.Contains(result.Stdout, want) {
			t.Fatalf("stdout=%q, want substring %q", result.Stdout, want)
		}
	}
}
