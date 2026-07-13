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
