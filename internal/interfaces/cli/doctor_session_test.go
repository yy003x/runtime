package cli

import (
	"bytes"
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestRuntimeDoctorPerformsExplicitFullSessionValidation(t *testing.T) {
	paths := prepareRuntimeHome(t)
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	sessionID := "session_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	if err := os.MkdirAll(
		filepath.Join(paths.SessionsDir, "_system"), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(
		filepath.Join(paths.SessionsDir, sessionID), 0o700,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(paths.SessionsDir, "_system", "index.json"),
		[]byte(`{"schema_version":3,"sessions":[]}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(paths.SessionsDir, sessionID, "session.json"),
		[]byte(`{"corrupt":true}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	err := runtimeDoctor(
		paths,
		newCLIOutput(false, &bytes.Buffer{}, &bytes.Buffer{}),
	)
	if err == nil || !strings.Contains(err.Error(), "validate Session store") {
		t.Fatalf("doctor error=%v", err)
	}
}
