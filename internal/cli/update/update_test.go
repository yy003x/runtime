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

	"github.com/yy003x/runtime/internal/activation"
	"github.com/yy003x/runtime/internal/cli/config"
	"github.com/yy003x/runtime/internal/installbundle"
	"github.com/yy003x/runtime/internal/layout"
)

func TestApplyInstallsBinariesAndOnlyMissingConfiguration(t *testing.T) {
	archiveName := platformArchiveName(t)
	archive := makeArchive(t, map[string]archiveFile{
		"sn-cli":                        {mode: 0o755, content: "#!/bin/sh\n[ \"$1\" = profile ] && [ \"$2\" = check ] && exit 0\nprintf new\n"},
		"sn-server":                     {mode: 0o755, content: "#!/bin/sh\nprintf server\n"},
		"configs/new-profile.json":      {mode: 0o644, content: `{"type":"api","driver":"openai-compatible","endpoint":"https://example.invalid/v1/chat/completions","model":"fixture","auth":{"header":"Authorization","scheme":"Bearer","from_env":"FIXTURE_KEY"},"timeout":"1m"}` + "\n"},
		"commands/new-command.json":     {mode: 0o644, content: "{\"profile\":\"new-profile\"}\n"},
		"runtime.json":                  {mode: 0o644, content: "{\"agent\":{}}\n"},
		"resources/schema/profile.json": {mode: 0o644, content: "packaged-schema\n"},
	})
	hash := sha256.Sum256(archive)
	server := releaseServer(t, "v2", archiveName, archive, fmt.Sprintf("%x  %s\n", hash, archiveName))
	defer server.Close()
	t.Setenv("SN_CLI_RELEASE_BASE_URL", server.URL+"/releases")
	cfg := testConfig(t)
	previousActivation := runCandidateActivation
	runCandidateActivation = func(
		_ context.Context,
		_ string,
		payload string,
		targetHome string,
		_ int,
	) (activation.UpgradeResult, error) {
		profiles, err := installbundle.SyncMissing(
			filepath.Join(payload, "configs"), cfg.Paths.ConfigDir,
		)
		if err != nil {
			return activation.UpgradeResult{}, err
		}
		commands, err := installbundle.SyncMissing(
			filepath.Join(payload, "commands"), cfg.Paths.CommandDir,
		)
		if err != nil {
			return activation.UpgradeResult{}, err
		}
		copiedRuntime, err := copyMissingFile(
			filepath.Join(payload, "runtime.json"),
			cfg.Paths.RuntimeConfigFile,
		)
		if err != nil {
			return activation.UpgradeResult{}, err
		}
		resources, err := installbundle.SyncMissing(
			filepath.Join(payload, "resources"), cfg.Paths.ResourcesDir,
		)
		if err != nil {
			return activation.UpgradeResult{}, err
		}
		if err := installBinary(
			filepath.Join(payload, "sn-cli"), cfg.Paths.Binary,
		); err != nil {
			return activation.UpgradeResult{}, err
		}
		if err := installBinary(
			filepath.Join(payload, "sn-server"), cfg.Paths.ServerBinary,
		); err != nil {
			return activation.UpgradeResult{}, err
		}
		return activation.UpgradeResult{
			TargetHome: targetHome, CopiedProfiles: profiles.Copied,
			CopiedCommands:      commands.Copied,
			CopiedRuntimeConfig: copiedRuntime,
			ResourceFiles:       resources.Copied,
		}, nil
	}
	defer func() { runCandidateActivation = previousActivation }()
	mustWriteUpdate(t, cfg.Paths.Binary, "old\n", 0o755)
	mustWriteUpdate(t, cfg.Paths.ServerBinary, "old-server\n", 0o755)
	mustWriteUpdate(t, filepath.Join(cfg.Paths.ConfigDir, "local.json"), `{"type":"api","driver":"openai-compatible","endpoint":"https://example.invalid/v1/chat/completions","model":"local","auth":{"header":"Authorization","scheme":"Bearer","from_env":"LOCAL_KEY"},"timeout":"1m"}`+"\n", 0o600)
	mustWriteUpdate(t, filepath.Join(cfg.Paths.CommandDir, "local.json"), "{\"profile\":\"local\"}\n", 0o600)
	mustWriteUpdate(t, cfg.Paths.RuntimeConfigFile, "{\"agent\":{}}\n", 0o600)
	result, err := Apply(context.Background(), cfg, "v2")
	if err != nil {
		t.Fatal(err)
	}
	if result.Version != "v2" ||
		strings.Join(result.CopiedProfiles, ",") != "new-profile.json" ||
		strings.Join(result.CopiedCommands, ",") != "new-command.json" ||
		result.CopiedRuntimeConfig ||
		strings.Join(result.CopiedResources, ",") != "schema/profile.json" {
		t.Fatalf("result=%#v", result)
	}
	assertUpdateContent(t, cfg.Paths.Binary, "#!/bin/sh\n[ \"$1\" = profile ] && [ \"$2\" = check ] && exit 0\nprintf new\n")
	assertUpdateContent(t, cfg.Paths.ServerBinary, "#!/bin/sh\nprintf server\n")
	assertUpdateContent(t, filepath.Join(cfg.Paths.ConfigDir, "local.json"), `{"type":"api","driver":"openai-compatible","endpoint":"https://example.invalid/v1/chat/completions","model":"local","auth":{"header":"Authorization","scheme":"Bearer","from_env":"LOCAL_KEY"},"timeout":"1m"}`+"\n")
	assertUpdateContent(t, filepath.Join(cfg.Paths.ConfigDir, "new-profile.json"), `{"type":"api","driver":"openai-compatible","endpoint":"https://example.invalid/v1/chat/completions","model":"fixture","auth":{"header":"Authorization","scheme":"Bearer","from_env":"FIXTURE_KEY"},"timeout":"1m"}`+"\n")
	assertUpdateContent(t, filepath.Join(cfg.Paths.CommandDir, "local.json"), "{\"profile\":\"local\"}\n")
	assertUpdateContent(t, filepath.Join(cfg.Paths.CommandDir, "new-command.json"), "{\"profile\":\"new-profile\"}\n")
	assertUpdateContent(t, cfg.Paths.RuntimeConfigFile, "{\"agent\":{}}\n")
	assertUpdateContent(t, filepath.Join(cfg.Paths.ResourcesDir, "schema", "profile.json"), "packaged-schema\n")
}

func TestApplyChecksumFailureKeepsOldBinariesAndConfiguration(t *testing.T) {
	archiveName := platformArchiveName(t)
	archive := makeArchive(t, map[string]archiveFile{
		"sn-cli":    {mode: 0o755, content: "#!/bin/sh\nexit 0\n"},
		"sn-server": {mode: 0o755, content: "#!/bin/sh\nexit 0\n"},
	})
	server := releaseServer(t, "v2", archiveName, archive, strings.Repeat("0", 64)+"  "+archiveName+"\n")
	defer server.Close()
	t.Setenv("SN_CLI_RELEASE_BASE_URL", server.URL+"/releases")
	cfg := testConfig(t)
	mustWriteUpdate(t, cfg.Paths.Binary, "old\n", 0o755)
	mustWriteUpdate(t, cfg.Paths.ServerBinary, "old-server\n", 0o755)
	if _, err := Apply(context.Background(), cfg, "v2"); err == nil || !strings.Contains(err.Error(), "checksum mismatch") {
		t.Fatalf("error=%v", err)
	}
	assertUpdateContent(t, cfg.Paths.Binary, "old\n")
	assertUpdateContent(t, cfg.Paths.ServerBinary, "old-server\n")
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
