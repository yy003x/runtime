// Package strictjson provides bounded, single-value JSON decoding for Runtime
// contracts and configuration documents.
package strictjson

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"os"
)

func Decode(reader io.Reader, maxBytes int64, target any) error {
	if reader == nil {
		return fmt.Errorf("JSON reader is required")
	}
	if maxBytes <= 0 {
		return fmt.Errorf("JSON size limit must be positive")
	}
	data, err := io.ReadAll(io.LimitReader(reader, maxBytes+1))
	if err != nil {
		return fmt.Errorf("read JSON: %w", err)
	}
	if int64(len(data)) > maxBytes {
		return fmt.Errorf("JSON exceeds %d bytes", maxBytes)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(target); err != nil {
		return fmt.Errorf("decode JSON: %w", err)
	}
	var extra any
	if err := decoder.Decode(&extra); err != io.EOF {
		if err == nil {
			return fmt.Errorf("JSON contains multiple values")
		}
		return fmt.Errorf("decode trailing JSON: %w", err)
	}
	return nil
}

func ReadRegularFile(path string, maxBytes int64, target any) error {
	info, err := os.Lstat(path)
	if err != nil {
		return fmt.Errorf("inspect %s: %w", path, err)
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return fmt.Errorf("%s must not be a symlink", path)
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s must be a regular file", path)
	}
	file, err := os.Open(path)
	if err != nil {
		return fmt.Errorf("open %s: %w", path, err)
	}
	defer file.Close()
	if err := Decode(file, maxBytes, target); err != nil {
		return fmt.Errorf("%s: %w", path, err)
	}
	return nil
}
