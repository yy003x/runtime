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

func TestMigrateProfileConfigsRemovesOnlyObsoleteField(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "cx.json")
	mustWrite(t, path, `{
  "type": "cli",
  "cli": {
    "runtime": {
      "managed_args": ["exec"],
      "result_contract": "required"
    }
  },
  "presets": {
    "strict": {
      "api": {
        "result_contract": "none",
        "temperature": 0
      }
    }
  },
  "extension": {
    "result_contract": "extension-owned"
  }
}`)
	mustWrite(t, filepath.Join(dir, "notes.yaml"), "result_contract: keep")

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
	if strings.Count(string(data), "result_contract") != 1 || !strings.Contains(string(data), `"result_contract": "extension-owned"`) || !strings.Contains(string(data), `"managed_args"`) || !strings.Contains(string(data), `"temperature": 0`) {
		t.Fatalf("migrated config=%s", data)
	}
	yamlData, err := os.ReadFile(filepath.Join(dir, "notes.yaml"))
	if err != nil || string(yamlData) != "result_contract: keep" {
		t.Fatalf("yaml=%q err=%v", yamlData, err)
	}

	second, err := MigrateProfileConfigs(dir)
	if err != nil || len(second.Changed) != 0 {
		t.Fatalf("second=%v err=%v", second.Changed, err)
	}
}

func TestMigrateProfileConfigsRejectsInvalidJSONWithoutChangingFile(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "broken.json")
	mustWrite(t, path, `{"result_contract":"required"} trailing`)
	if _, err := MigrateProfileConfigs(dir); err == nil {
		t.Fatal("invalid JSON was accepted")
	}
	assertContent(t, path, `{"result_contract":"required"} trailing`)
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
