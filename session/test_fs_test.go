package session

import (
	"encoding/json"
	"os"
	"path/filepath"
)

// These pathname helpers are test-fixture builders only. Production Session
// state always goes through safeDirectory.
func ensureDirectory(path string) error {
	return os.MkdirAll(path, 0o700)
}

func atomicJSON(path string, value any, mode os.FileMode) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	return os.WriteFile(path, data, mode)
}

func isAtomicTempFact(entry os.DirEntry) bool {
	return entry.Type()&os.ModeSymlink == 0 &&
		!entry.IsDir() &&
		entry.Type().IsRegular() &&
		isOwnedAtomicTempName(entry.Name())
}
