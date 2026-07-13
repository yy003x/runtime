package cli

import (
	"bytes"
	"io"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-arch/sncli/internal/config"
)

func TestRunProfilePrintsFinalText(t *testing.T) {
	root := t.TempDir()
	writeCLIProfile(t, root, "fake", `name: fake
provider:
  type: fake
  echo_prefix: ""
runtime:
  timeout_seconds: 30
artifacts:
  root: runs/global/runtime
`)

	stdout := captureStdout(t, func() {
		err := runProfile(&config.Config{Root: root}, []string{"fake", "hello"})
		if err != nil {
			t.Fatalf("runProfile returned error: %v", err)
		}
	})

	if strings.TrimSpace(stdout) != "hello" {
		t.Fatalf("stdout=%q, want %q", stdout, "hello")
	}
}

func TestProfileConfigExists(t *testing.T) {
	root := t.TempDir()
	writeCLIProfile(t, root, "fake", `name: fake
provider:
  type: fake
`)

	if !profileConfigExists(root, "fake") {
		t.Fatal("profileConfigExists(fake)=false, want true")
	}
	if profileConfigExists(root, "../fake") {
		t.Fatal("profileConfigExists accepted path traversal")
	}
	if profileConfigExists(root, "missing") {
		t.Fatal("profileConfigExists(missing)=true, want false")
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "json.json"), []byte(`{"provider":{"type":"fake"}}`), 0o644); err != nil {
		t.Fatal(err)
	}
	if !profileConfigExists(root, "json") {
		t.Fatal("profileConfigExists(json)=false, want true")
	}
}

func TestParseProfileInvocationSupportsPromptFileAndImages(t *testing.T) {
	got, err := parseProfileInvocation([]string{
		"codex", "--prompt_file", "prompt.md", "--image", "one.png", "--image", "two.png", "--session-id", "s1",
	})
	if err != nil {
		t.Fatalf("parseProfileInvocation returned error: %v", err)
	}
	if got.Profile != "codex" || got.PromptFile != "prompt.md" || got.SessionID != "s1" {
		t.Fatalf("invocation=%#v", got)
	}
	if len(got.Images) != 2 || got.Images[0] != "one.png" || got.Images[1] != "two.png" {
		t.Fatalf("images=%#v", got.Images)
	}
}

func TestParseProfileInvocationSupportsInlinePrompt(t *testing.T) {
	got, err := parseProfileInvocation([]string{"codex", "review", "this", "repo"})
	if err != nil {
		t.Fatalf("parseProfileInvocation returned error: %v", err)
	}
	if got.Prompt != "review this repo" {
		t.Fatalf("prompt=%q", got.Prompt)
	}
}

func TestParseProfileInvocationKeepsLeadingDashPrompt(t *testing.T) {
	got, err := parseProfileInvocation([]string{"codex", "--not-a-cli-flag", "-x"})
	if err != nil {
		t.Fatalf("parseProfileInvocation returned error: %v", err)
	}
	if got.Prompt != "--not-a-cli-flag -x" {
		t.Fatalf("prompt=%q", got.Prompt)
	}
}

func TestParseProfileInvocationRejectsPromptConflict(t *testing.T) {
	_, err := parseProfileInvocation([]string{"codex", "inline", "--prompt-file", "prompt.md"})
	if err == nil || !strings.Contains(err.Error(), "cannot be used together") {
		t.Fatalf("error=%v", err)
	}
}

func TestPrintProvidersUsesInternalRegistry(t *testing.T) {
	stdout := captureStdout(t, func() {
		if err := printProviders(); err != nil {
			t.Fatalf("printProviders returned error: %v", err)
		}
	})
	if !strings.Contains(stdout, `"source": "internal"`) {
		t.Fatalf("stdout=%q, want internal source", stdout)
	}
	if !strings.Contains(stdout, `"fake"`) {
		t.Fatalf("stdout=%q, want fake provider", stdout)
	}
}

func captureStdout(t *testing.T, fn func()) string {
	t.Helper()
	original := os.Stdout
	reader, writer, err := os.Pipe()
	if err != nil {
		t.Fatalf("create stdout pipe: %v", err)
	}
	os.Stdout = writer
	defer func() {
		os.Stdout = original
	}()

	fn()

	if err := writer.Close(); err != nil {
		t.Fatalf("close stdout writer: %v", err)
	}
	var buf bytes.Buffer
	if _, err := io.Copy(&buf, reader); err != nil {
		t.Fatalf("read stdout pipe: %v", err)
	}
	return buf.String()
}

func writeCLIProfile(t *testing.T, root, name, body string) {
	t.Helper()
	dir := filepath.Join(root, "configs")
	if err := os.MkdirAll(dir, 0o755); err != nil {
		t.Fatalf("create configs dir: %v", err)
	}
	if err := os.WriteFile(filepath.Join(dir, name+".yaml"), []byte(body), 0o644); err != nil {
		t.Fatalf("write profile: %v", err)
	}
}
