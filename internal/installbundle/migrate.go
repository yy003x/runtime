package installbundle

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
)

// MigrationResult reports profile files changed by an install-time schema
// migration. Runtime config loading stays strict and performs no writes.
type MigrationResult struct {
	Changed []string `json:"changed_configs"`
}

// MigrateProfileConfigs removes profile fields that are no longer part of the
// current schema. It is intentionally called only by install/update flows.
func MigrateProfileConfigs(dir string) (MigrationResult, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return MigrationResult{}, fmt.Errorf("read profile config directory %s: %w", dir, err)
	}
	result := MigrationResult{Changed: []string{}}
	for _, entry := range entries {
		if entry.IsDir() || !strings.HasSuffix(entry.Name(), ".json") {
			continue
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return MigrationResult{}, fmt.Errorf("profile config is a symlink: %s", filepath.Join(dir, entry.Name()))
		}
		path := filepath.Join(dir, entry.Name())
		changed, err := migrateProfileConfig(path)
		if err != nil {
			return MigrationResult{}, err
		}
		if changed {
			result.Changed = append(result.Changed, entry.Name())
		}
	}
	return result, nil
}

func migrateProfileConfig(path string) (bool, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return false, fmt.Errorf("read profile config %s: %w", path, err)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.UseNumber()
	var document any
	if err := decoder.Decode(&document); err != nil {
		return false, fmt.Errorf("decode profile config %s: %w", path, err)
	}
	if err := requireJSONEOF(decoder); err != nil {
		return false, fmt.Errorf("decode profile config %s: %w", path, err)
	}
	if !removeObsoleteProfileFields(document) {
		return false, nil
	}

	var output bytes.Buffer
	encoder := json.NewEncoder(&output)
	encoder.SetEscapeHTML(false)
	encoder.SetIndent("", "  ")
	if err := encoder.Encode(document); err != nil {
		return false, fmt.Errorf("encode profile config %s: %w", path, err)
	}
	info, err := os.Stat(path)
	if err != nil {
		return false, fmt.Errorf("stat profile config %s: %w", path, err)
	}
	if err := writeFileAtomic(path, output.Bytes(), info.Mode().Perm()); err != nil {
		return false, fmt.Errorf("write profile config %s: %w", path, err)
	}
	return true, nil
}

func requireJSONEOF(decoder *json.Decoder) error {
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("multiple JSON values")
		}
		return err
	}
	return nil
}

func removeObsoleteProfileFields(value any) bool {
	document, ok := value.(map[string]any)
	if !ok {
		return false
	}
	changed := removeProfileResultContract(document)
	presets, _ := document["presets"].(map[string]any)
	for _, value := range presets {
		preset, _ := value.(map[string]any)
		if removeProfileResultContract(preset) {
			changed = true
		}
	}
	return changed
}

func removeProfileResultContract(profile map[string]any) bool {
	changed := false
	if cli, ok := profile["cli"].(map[string]any); ok {
		if runtime, ok := cli["runtime"].(map[string]any); ok {
			if _, exists := runtime["result_contract"]; exists {
				delete(runtime, "result_contract")
				changed = true
			}
		}
	}
	if api, ok := profile["api"].(map[string]any); ok {
		if _, exists := api["result_contract"]; exists {
			delete(api, "result_contract")
			changed = true
		}
	}
	return changed
}

func writeFileAtomic(path string, data []byte, mode os.FileMode) error {
	temporary, err := os.CreateTemp(filepath.Dir(path), "."+filepath.Base(path)+".migrate-")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	ok := false
	defer func() {
		_ = temporary.Close()
		if !ok {
			_ = os.Remove(temporaryPath)
		}
	}()
	if err := temporary.Chmod(mode); err != nil {
		return err
	}
	if _, err := temporary.Write(data); err != nil {
		return err
	}
	if err := temporary.Sync(); err != nil {
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	if err := os.Rename(temporaryPath, path); err != nil {
		return err
	}
	ok = true
	return nil
}
