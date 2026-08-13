package activation

import (
	"database/sql"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/yy003x/runtime/pkg/session"
	runstore "github.com/yy003x/runtime/pkg/store/sqlite"
)

func TestPreflightStatePerformsExplicitFullSessionValidation(t *testing.T) {
	target := t.TempDir()
	sessionID := "session_aaaaaaaaaaaaaaaaaaaaaaaaaaaaaaaa"
	historyDir := filepath.Join(target, "sessions", "_system")
	sessionDir := filepath.Join(target, "sessions", sessionID)
	if err := os.MkdirAll(historyDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.MkdirAll(sessionDir, 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(historyDir, "index.json"),
		[]byte(`{"schema_version":3,"sessions":[]}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(
		filepath.Join(sessionDir, "session.json"),
		[]byte(`{"corrupt":true}`),
		0o600,
	); err != nil {
		t.Fatal(err)
	}
	err := preflightState(target, Manifest{
		SessionSchemaVersion: session.SchemaVersion,
	})
	if err == nil || !strings.Contains(err.Error(), "Session schema preflight") {
		t.Fatalf("preflight error=%v", err)
	}
}

func TestPreflightRunDatabaseRejectsMissingV5ReaperIndex(t *testing.T) {
	path := filepath.Join(t.TempDir(), "runtime.db")
	store, err := runstore.Open(path, runstore.Options{})
	if err != nil {
		t.Fatal(err)
	}
	if err := store.Close(); err != nil {
		t.Fatal(err)
	}
	if err := preflightRunDatabase(path, runstore.SchemaVersion); err != nil {
		t.Fatalf("new v5 Run database preflight error=%v", err)
	}
	database, err := sql.Open("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	if _, err := database.Exec(
		"DROP INDEX runs_reaper_state_updated",
	); err != nil {
		database.Close()
		t.Fatal(err)
	}
	if err := database.Close(); err != nil {
		t.Fatal(err)
	}
	if err := preflightRunDatabase(
		path, runstore.SchemaVersion,
	); err == nil || !strings.Contains(
		err.Error(), "runs_reaper_state_updated",
	) {
		t.Fatalf("missing reaper index preflight error=%v", err)
	}
}
