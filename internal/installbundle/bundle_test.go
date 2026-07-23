package installbundle

import (
	"archive/tar"
	"compress/gzip"
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"agent-runtime/internal/provider"
)

func TestSyncMissingPreservesExistingAndCopiesRecursively(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	target := filepath.Join(t.TempDir(), "target")
	mustWrite(t, filepath.Join(source, "runtime.yaml"), "packaged")
	mustWrite(t, filepath.Join(source, "personas", "coder.yaml"), "coder")
	mustWrite(t, filepath.Join(target, "runtime.yaml"), "local")
	mustWrite(t, filepath.Join(target, "extra.yaml"), "extra")
	result, err := SyncMissing(source, target)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Copied, ",") != "personas/coder.yaml" {
		t.Fatalf("copied=%v", result.Copied)
	}
	assertContent(t, filepath.Join(target, "runtime.yaml"), "local")
	assertContent(t, filepath.Join(target, "personas", "coder.yaml"), "coder")
	assertContent(t, filepath.Join(target, "extra.yaml"), "extra")
}

func TestSyncMissingTypeConflictDoesNotPartiallyCopy(t *testing.T) {
	source := filepath.Join(t.TempDir(), "source")
	target := filepath.Join(t.TempDir(), "target")
	mustWrite(t, filepath.Join(source, "a-new.yaml"), "new")
	mustWrite(t, filepath.Join(source, "personas", "coder.yaml"), "coder")
	mustWrite(t, filepath.Join(target, "personas"), "file-conflict")
	_, err := SyncMissing(source, target)
	if err == nil || !strings.Contains(err.Error(), filepath.Join(target, "personas")) {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(target, "a-new.yaml")); !os.IsNotExist(err) {
		t.Fatalf("preflight left partial copy: %v", err)
	}
}

func TestMigrateHomeCopiesLegacyResourcesWithoutOverwritingOrDeleting(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "configs")
	resourcesDir := filepath.Join(root, "resources")
	mustWrite(t, filepath.Join(configDir, "personas", "local.yaml"), "legacy-persona")
	mustWrite(t, filepath.Join(configDir, "skills", "review", "skill.yaml"), "legacy-skill")
	mustWrite(t, filepath.Join(configDir, "tools", "local.tool.yaml"), "legacy-tool")
	mustWrite(t, filepath.Join(configDir, "schema", "profile.json"), "legacy-schema")
	mustWrite(t, filepath.Join(resourcesDir, "personas", "local.yaml"), "resource-persona")

	result, err := MigrateHome(configDir, resourcesDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result.ChangedConfigs) != 0 || strings.Join(result.CopiedResources, ",") != "schema/profile.json,skills/review/skill.yaml,tools/local.tool.yaml" {
		t.Fatalf("result=%#v", result)
	}
	assertContent(t, filepath.Join(resourcesDir, "personas", "local.yaml"), "resource-persona")
	assertContent(t, filepath.Join(resourcesDir, "skills", "review", "skill.yaml"), "legacy-skill")
	assertContent(t, filepath.Join(resourcesDir, "tools", "local.tool.yaml"), "legacy-tool")
	assertContent(t, filepath.Join(resourcesDir, "schema", "profile.json"), "legacy-schema")
	assertContent(t, filepath.Join(configDir, "personas", "local.yaml"), "legacy-persona")

	second, err := MigrateHome(configDir, resourcesDir)
	if err != nil || len(second.ChangedConfigs) != 0 || len(second.CopiedResources) != 0 {
		t.Fatalf("second=%#v err=%v", second, err)
	}
}

func TestMigrateHomePreflightsAllLegacyResourcesBeforeCopying(t *testing.T) {
	root := t.TempDir()
	configDir := filepath.Join(root, "configs")
	resourcesDir := filepath.Join(root, "resources")
	mustWrite(t, filepath.Join(configDir, "personas", "new.yaml"), "new")
	mustWrite(t, filepath.Join(configDir, "tools", "local.tool.yaml"), "tool")
	mustWrite(t, filepath.Join(resourcesDir, "tools"), "file-conflict")

	if _, err := MigrateHome(configDir, resourcesDir); err == nil || !strings.Contains(err.Error(), filepath.Join(resourcesDir, "tools")) {
		t.Fatalf("error=%v", err)
	}
	if _, err := os.Stat(filepath.Join(resourcesDir, "personas", "new.yaml")); !os.IsNotExist(err) {
		t.Fatalf("preflight left partial resource copy: %v", err)
	}
}

func TestVerifyChecksum(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "sn-cli-test.tar.gz")
	mustWrite(t, archive, "payload")
	hash := sha256.Sum256([]byte("payload"))
	checksums := []byte(fmt.Sprintf("%x  %s\n", hash, filepath.Base(archive)))
	if err := VerifyChecksum(archive, filepath.Base(archive), checksums); err != nil {
		t.Fatal(err)
	}
	if err := VerifyChecksum(archive, filepath.Base(archive), []byte(strings.Repeat("0", 64)+"  "+filepath.Base(archive)+"\n")); err == nil {
		t.Fatal("accepted checksum mismatch")
	}
}

func TestExtractTarGzRejectsTraversal(t *testing.T) {
	archive := filepath.Join(t.TempDir(), "bad.tar.gz")
	file, err := os.Create(archive)
	if err != nil {
		t.Fatal(err)
	}
	gzipWriter := gzip.NewWriter(file)
	tarWriter := tar.NewWriter(gzipWriter)
	if err := tarWriter.WriteHeader(&tar.Header{Name: "../escape", Mode: 0o644, Size: 1}); err != nil {
		t.Fatal(err)
	}
	_, _ = tarWriter.Write([]byte("x"))
	_ = tarWriter.Close()
	_ = gzipWriter.Close()
	_ = file.Close()
	if err := ExtractTarGz(archive, t.TempDir()); err == nil {
		t.Fatal("accepted traversal archive")
	}
}

func TestMigrateProfileConfigsCanonicalizesAndSplitsPresets(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcx.json")
	mustWrite(t, path, `{
  "type": "cli",
  "label": "Codex",
  "cli": {
    "driver": "codex",
    "executor": "command",
    "command": {
      "binary": "codex",
      "args": ["-c", "model_reasoning_effort=high"],
      "model": "gpt-base"
    },
    "runtime": {
      "prompt_delivery": "stdin",
      "managed_args": ["exec"],
      "result_contract": "required",
      "override_policy": {"allow": ["model", "reasoning_effort", "sandbox_mode", "approval_policy", "service_tier", "verbosity", "images"]}
    }
  },
  "presets": {
    "cx-fast": {"overrides": {"model": "gpt-fast", "reasoning_effort": "medium"}},
    "cx-keep": {"type": "api", "preset-must-not-win": true}
  }
}`)
	mustWrite(t, filepath.Join(dir, "cx-keep.json"), `{"type":"cli","cli":{"command":"codex","model":"standalone"}}`)
	mustWrite(t, filepath.Join(dir, "notes.yaml"), "result_contract: keep")

	result, err := MigrateProfileConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Changed, ",") != "cx-fast.json,cx-keep.json,mcx.json" {
		t.Fatalf("changed=%v", result.Changed)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "label") || strings.Contains(string(data), "presets") || strings.Contains(string(data), `"driver"`) || strings.Contains(string(data), `"binary"`) || strings.Contains(string(data), "runtime") || !strings.Contains(string(data), `"command": "codex"`) || !strings.Contains(string(data), `"effort": "high"`) {
		t.Fatalf("migrated config=%s", data)
	}
	fast, err := os.ReadFile(filepath.Join(dir, "cx-fast.json"))
	if err != nil || !strings.Contains(string(fast), `"model": "gpt-fast"`) || !strings.Contains(string(fast), `"effort": "medium"`) {
		t.Fatalf("fast=%s err=%v", fast, err)
	}
	keep, err := os.ReadFile(filepath.Join(dir, "cx-keep.json"))
	if err != nil || !strings.Contains(string(keep), `"model": "standalone"`) || strings.Contains(string(keep), "preset-must-not-win") || strings.Contains(string(keep), `"type"`) {
		t.Fatalf("keep=%s err=%v", keep, err)
	}
	yamlData, err := os.ReadFile(filepath.Join(dir, "notes.yaml"))
	if err != nil || string(yamlData) != "result_contract: keep" {
		t.Fatalf("yaml=%q err=%v", yamlData, err)
	}
	profiles, err := provider.LoadDir(dir)
	if err != nil || len(profiles) != 3 || profiles["cx-keep"].CLI.Command.Model != "standalone" {
		t.Fatalf("profiles=%v err=%v", profiles, err)
	}

	second, err := MigrateProfileConfigs(dir)
	if err != nil || len(second.Changed) != 0 {
		t.Fatalf("second=%v err=%v", second.Changed, err)
	}
}

func TestMigrateProfileConfigsRejectsInvalidJSONWithoutChangingFile(t *testing.T) {
	dir := t.TempDir()
	goodPath := filepath.Join(dir, "a-good.json")
	good := `{"type":"api","label":"Legacy","api":{"protocol":"openai","base_url":"https://example.test/v1","model":"m","api_key":"${UNSET}"}}`
	mustWrite(t, goodPath, good)
	path := filepath.Join(dir, "broken.json")
	mustWrite(t, path, `{"result_contract":"required"} trailing`)
	if _, err := MigrateProfileConfigs(dir); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	assertContent(t, goodPath, good)
	assertContent(t, path, `{"result_contract":"required"} trailing`)
}

func TestMigrateProfileConfigsRemovesLegacyRuntimeSettings(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "runtime.yaml")
	mustWrite(t, path, "# runtime policy\nruns_dir: runs/global/runtime\ndefault_profile: cx\nprovider_config_dir: configs\nmax_concurrency: 2\n")
	result, err := MigrateProfileConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Changed, ",") != "runtime.yaml" {
		t.Fatalf("changed=%v", result.Changed)
	}
	data, err := os.ReadFile(path)
	if err != nil || strings.Contains(string(data), "runs_dir") || strings.Contains(string(data), "provider_config_dir") || !strings.Contains(string(data), "default_profile: cx") || !strings.Contains(string(data), "max_concurrency: 2") {
		t.Fatalf("runtime=%s err=%v", data, err)
	}
	second, err := MigrateProfileConfigs(dir)
	if err != nil || len(second.Changed) != 0 {
		t.Fatalf("second=%v err=%v", second.Changed, err)
	}
	legacy, err := ScanProfileMigrations(dir)
	if err != nil || len(legacy) != 0 {
		t.Fatalf("legacy=%v err=%v", legacy, err)
	}
}

func TestScanProfileMigrationsReportsLegacyRuntimeSettings(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "runtime.yaml"), "runs_dir: runs\nprovider_config_dir: configs\n")
	result, err := ScanProfileMigrations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 1 || result[0].File != "runtime.yaml" || strings.Join(result[0].Fields, ",") != "provider_config_dir,runs_dir" {
		t.Fatalf("result=%v", result)
	}
}

func TestScanProfileMigrationsReportsLegacyFields(t *testing.T) {
	dir := t.TempDir()
	mustWrite(t, filepath.Join(dir, "cx.json"), `{
  "type":"cli",
  "label":"Codex",
  "cli":{"driver":"codex","command":{"binary":"codex","args":[],"model":"m"},"runtime":{"managed_args":["exec"]}},
  "presets":{
    "fast":{"label":"Fast","cli":{"tmux":{"ready_timeout_seconds":10}}}
  }
}`)
	mustWrite(t, filepath.Join(dir, "native.json"), `{"type":"native","native":{"mock":{"responses":["ok"],"done_after":1}}}`)
	result, err := ScanProfileMigrations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(result) != 2 || result[0].File != "cx.json" || result[1].File != "native.json" {
		t.Fatalf("result=%v", result)
	}
	if got, expected := strings.Join(result[0].Fields, "|"), "cli|cli.command|cli.driver|cli.runtime|label|presets|presets.fast.cli|presets.fast.cli.tmux|presets.fast.cli.tmux.ready_timeout_seconds|presets.fast.label|type"; got != expected {
		t.Fatalf("fields=%q want=%q", got, expected)
	}
	if got, expected := strings.Join(result[1].Fields, "|"), "native|type"; got != expected {
		t.Fatalf("native fields=%q want=%q", got, expected)
	}

	result, err = ScanProfileMigrations(t.TempDir())
	if err != nil || len(result) != 0 {
		t.Fatalf("empty=%v err=%v", result, err)
	}
}

func TestScanAndMigrateFlatManagedArgs(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cx.json")
	mustWrite(t, path, `{"type":"cli","cli":{"command":"codex","model":"m","managed_args":["exec"]}}`)

	legacy, err := ScanProfileMigrations(dir)
	if err != nil {
		t.Fatal(err)
	}
	if len(legacy) != 1 || legacy[0].File != "cx.json" || strings.Join(legacy[0].Fields, ",") != "cli,cli.managed_args,type" {
		t.Fatalf("legacy=%v", legacy)
	}

	result, err := MigrateProfileConfigs(dir)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Join(result.Changed, ",") != "cx.json" {
		t.Fatalf("changed=%v", result.Changed)
	}
	data, err := os.ReadFile(path)
	if err != nil {
		t.Fatal(err)
	}
	if strings.Contains(string(data), "managed_args") {
		t.Fatalf("migrated config=%s", data)
	}
	if strings.Contains(string(data), `"type"`) || strings.Contains(string(data), `"cli"`) {
		t.Fatalf("migrated config is not flat: %s", data)
	}
}

func TestMigrateProfileConfigsRejectsNonDefaultManagedArgsWithoutWriting(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "custom.json")
	original := `{"type":"cli","cli":{"command":"codex","model":"","managed_args":["exec","--custom"]}}`
	mustWrite(t, path, original)

	_, err := MigrateProfileConfigs(dir)
	if err == nil || !strings.Contains(err.Error(), "无法自动迁移非默认 cli.managed_args") {
		t.Fatalf("err=%v", err)
	}
	data, readErr := os.ReadFile(path)
	if readErr != nil {
		t.Fatal(readErr)
	}
	if string(data) != original {
		t.Fatalf("config changed after failed migration: %s", data)
	}
}

func mustWrite(t *testing.T, path, content string) {
	t.Helper()
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(path, []byte(content), 0o600); err != nil {
		t.Fatal(err)
	}
}

func assertContent(t *testing.T, path, expected string) {
	t.Helper()
	data, err := os.ReadFile(path)
	if err != nil || string(data) != expected {
		t.Fatalf("%s=%q err=%v", path, data, err)
	}
}
