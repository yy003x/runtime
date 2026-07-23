package update

import (
	"archive/tar"
	"compress/gzip"
	"context"
	"crypto/sha256"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"runtime"
	"strings"
	"testing"

	"agent-runtime/internal/cli/config"
	"agent-runtime/internal/layout"
)

func TestApplyInstallsBinaryAndOnlyMissingConfigs(t *testing.T) {
	archiveName := platformArchiveName(t)
	archive := makeArchive(t, map[string]archiveFile{
		"sn-cli":                        {mode: 0o755, content: "#!/bin/sh\n[ \"$1\" = profile ] && [ \"$2\" = list ] && exit 0\nprintf new\n"},
		"configs/runtime.yaml":          {mode: 0o644, content: "packaged\n"},
		"configs/new.json":              {mode: 0o644, content: "{\"type\":\"native\",\"native\":{\"mock\":{\"responses\":[\"ok\"],\"done_after\":1}}}\n"},
		"resources/schema/profile.json": {mode: 0o644, content: "packaged-schema\n"},
	})
	hash := sha256.Sum256(archive)
	server := releaseServer(t, "v2", archiveName, archive, fmt.Sprintf("%x  %s\n", hash, archiveName))
	defer server.Close()
	t.Setenv("SN_CLI_RELEASE_BASE_URL", server.URL+"/releases")
	cfg := testConfig(t)
	mustWriteUpdate(t, cfg.Paths.Binary, "old\n", 0o755)
	mustWriteUpdate(t, filepath.Join(cfg.Paths.ConfigDir, "runtime.yaml"), "local\n", 0o600)
	mustWriteUpdate(t, filepath.Join(cfg.Paths.ConfigDir, "cx.json"), `{"type":"cli","label":"Codex","cli":{"driver":"codex","command":{"binary":"codex","args":[],"model":"gpt"},"runtime":{"managed_args":["exec"],"result_contract":"required"}}}`+"\n", 0o600)
	mustWriteUpdate(t, filepath.Join(cfg.Paths.ConfigDir, "personas", "local.yaml"), "legacy-persona\n", 0o600)
	result, err := Apply(context.Background(), cfg, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != "v2" || len(result.CopiedConfigs) != 1 || result.CopiedConfigs[0] != "new.json" || strings.Join(result.CopiedResources, ",") != "schema/profile.json" || len(result.MigratedConfigs) != 1 || result.MigratedConfigs[0] != "cx.json" || strings.Join(result.MigratedResources, ",") != "personas/local.yaml" {
		t.Fatalf("result=%#v", result)
	}
	assertUpdateContent(t, cfg.Paths.Binary, "#!/bin/sh\n[ \"$1\" = profile ] && [ \"$2\" = list ] && exit 0\nprintf new\n")
	assertUpdateContent(t, filepath.Join(cfg.Paths.ConfigDir, "runtime.yaml"), "local\n")
	assertUpdateContent(t, filepath.Join(cfg.Paths.ConfigDir, "new.json"), "{\"type\":\"native\",\"native\":{\"mock\":{\"responses\":[\"ok\"],\"done_after\":1}}}\n")
	assertUpdateContent(t, filepath.Join(cfg.Paths.ResourcesDir, "schema", "profile.json"), "packaged-schema\n")
	assertUpdateContent(t, filepath.Join(cfg.Paths.ResourcesDir, "personas", "local.yaml"), "legacy-persona\n")
	assertUpdateContent(t, filepath.Join(cfg.Paths.ConfigDir, "personas", "local.yaml"), "legacy-persona\n")
	cxData, err := os.ReadFile(filepath.Join(cfg.Paths.ConfigDir, "cx.json"))
	if err != nil || strings.Contains(string(cxData), "result_contract") {
		t.Fatalf("cx.json=%q err=%v", cxData, err)
	}
}

func TestApplyMigratesLocalPresetsBeforeAddingPackagedProfiles(t *testing.T) {
	archiveName := platformArchiveName(t)
	archive := makeArchive(t, map[string]archiveFile{
		"sn-cli":                        {mode: 0o755, content: "#!/bin/sh\n[ \"$1\" = profile ] && [ \"$2\" = list ] && exit 0\nexit 0\n"},
		"configs/cx-fast.json":          {mode: 0o644, content: `{"type":"cli","cli":{"command":"codex","model":"packaged","effort":"high"}}` + "\n"},
		"resources/schema/profile.json": {mode: 0o644, content: "{}\n"},
	})
	hash := sha256.Sum256(archive)
	server := releaseServer(t, "v2", archiveName, archive, fmt.Sprintf("%x  %s\n", hash, archiveName))
	defer server.Close()
	t.Setenv("SN_CLI_RELEASE_BASE_URL", server.URL+"/releases")
	cfg := testConfig(t)
	mustWriteUpdate(t, cfg.Paths.Binary, "old\n", 0o755)
	mustWriteUpdate(t, filepath.Join(cfg.Paths.ConfigDir, "mcx.json"), `{
  "type":"cli",
  "cli":{"driver":"codex","command":{"binary":"codex","args":[],"model":"base"},"runtime":{"managed_args":["exec"]}},
  "presets":{"cx-fast":{"overrides":{"model":"local-custom","reasoning_effort":"medium"}}}
}`+"\n", 0o600)

	result, err := Apply(context.Background(), cfg, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if len(result.CopiedConfigs) != 0 || strings.Join(result.MigratedConfigs, ",") != "cx-fast.json,mcx.json" {
		t.Fatalf("result=%#v", result)
	}
	data, err := os.ReadFile(filepath.Join(cfg.Paths.ConfigDir, "cx-fast.json"))
	if err != nil || !strings.Contains(string(data), `"model": "local-custom"`) || strings.Contains(string(data), "packaged") {
		t.Fatalf("cx-fast=%s err=%v", data, err)
	}
}

func TestApplyChecksumFailureKeepsOldBinaryAndConfigs(t *testing.T) {
	archiveName := platformArchiveName(t)
	archive := makeArchive(t, map[string]archiveFile{
		"sn-cli":           {mode: 0o755, content: "#!/bin/sh\nexit 0\n"},
		"configs/new.json": {mode: 0o644, content: "{}\n"},
	})
	server := releaseServer(t, "v2", archiveName, archive, strings.Repeat("0", 64)+"  "+archiveName+"\n")
	defer server.Close()
	t.Setenv("SN_CLI_RELEASE_BASE_URL", server.URL+"/releases")
	cfg := testConfig(t)
	mustWriteUpdate(t, cfg.Paths.Binary, "old\n", 0o755)
	if _, err := Apply(context.Background(), cfg, "v2"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error=%v", err)
	}
	assertUpdateContent(t, cfg.Paths.Binary, "old\n")
	if _, err := os.Stat(filepath.Join(cfg.Paths.ConfigDir, "new.json")); !os.IsNotExist(err) {
		t.Fatalf("checksum failure changed configs: %v", err)
	}
}

func TestCheckUsesReleaseAPI(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		_, _ = writer.Write([]byte(`{"tag_name":"v2"}`))
	}))
	defer server.Close()
	t.Setenv("SN_CLI_RELEASE_API_URL", server.URL)
	cfg := testConfig(t)
	status := Check(context.Background(), cfg, "v1")
	if status.Error != "" || !status.UpdateAvailable || status.LatestVersion != "v2" {
		t.Fatalf("status=%#v", status)
	}
}

type archiveFile struct {
	mode    int64
	content string
}

func makeArchive(t *testing.T, files map[string]archiveFile) []byte {
	t.Helper()
	path := filepath.Join(t.TempDir(), "archive.tar.gz")
	file, err := os.Create(path)
	if err != nil {
		t.Fatal(err)
	}
	compressed := gzip.NewWriter(file)
	writer := tar.NewWriter(compressed)
	for name, item := range files {
		if err := writer.WriteHeader(&tar.Header{Name: name, Mode: item.mode, Size: int64(len(item.content))}); err != nil {
			t.Fatal(err)
		}
		if _, err := writer.Write([]byte(item.content)); err != nil {
			t.Fatal(err)
		}
	}
	_ = writer.Close()
	_ = compressed.Close()
	_ = file.Close()
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	return data
}

func releaseServer(t *testing.T, version, archiveName string, archive []byte, checksums string) *httptest.Server {
	t.Helper()
	return httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.URL.Path {
		case "/releases/" + version + "/" + archiveName:
			_, _ = writer.Write(archive)
		case "/releases/" + version + "/checksums.txt":
			_, _ = writer.Write([]byte(checksums))
		default:
			http.NotFound(writer, request)
		}
	}))
}

func testConfig(t *testing.T) *config.Config {
	t.Helper()
	paths, err := layout.FromHome(filepath.Join(t.TempDir(), ".sn"))
	if err != nil {
		t.Fatal(err)
	}
	if err := paths.Ensure(); err != nil {
		t.Fatal(err)
	}
	return &config.Config{Home: paths.Home, Paths: paths, Update: config.UpdateConfig{Repository: "test/runtime"}}
}

func platformArchiveName(t *testing.T) string {
	t.Helper()
	if runtime.GOOS != "darwin" && runtime.GOOS != "linux" {
		t.Skip("unsupported test platform")
	}
	return fmt.Sprintf("sn-cli-%s-%s.tar.gz", runtime.GOOS, runtime.GOARCH)
}

func mustWriteUpdate(t *testing.T, path, content string, mode os.FileMode) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), mode); err != nil {
		t.Fatal(err)
	}
}

func assertUpdateContent(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != expected {
		t.Fatalf("%s=%q err=%v", path, data, err)
	}
}
