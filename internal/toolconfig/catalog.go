package toolconfig

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yy003x/runtime/internal/strictjson"
)

const maxManifestBytes int64 = 1 << 20

type Catalog struct {
	values map[string]Manifest
}

func LoadDirectory(directory string) (*Catalog, error) {
	info, err := os.Lstat(directory)
	if err != nil {
		return nil, fmt.Errorf("inspect tool directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("tool path must be a directory, not a symlink")
	}
	entries, err := os.ReadDir(directory)
	if err != nil {
		return nil, fmt.Errorf("read tool directory: %w", err)
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].Name() < entries[right].Name()
	})
	values := make(map[string]Manifest, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 ||
			filepath.Ext(name) != ".json" {
			return nil, fmt.Errorf(
				"tool directory contains unsupported entry %q", name,
			)
		}
		id := strings.TrimSuffix(name, ".json")
		if err := ValidateName(id); err != nil {
			return nil, fmt.Errorf("tool file %q: %w", name, err)
		}
		path := filepath.Join(directory, name)
		manifest, err := loadManifest(path)
		if err != nil {
			return nil, err
		}
		if manifest.Name != id {
			return nil, fmt.Errorf(
				"%s: name %q must match file basename %q",
				path, manifest.Name, id,
			)
		}
		if _, exists := values[manifest.Name]; exists {
			return nil, fmt.Errorf("duplicate tool %q", manifest.Name)
		}
		values[manifest.Name] = canonicalManifest(manifest)
	}
	return &Catalog{values: values}, nil
}

func loadManifest(path string) (Manifest, error) {
	data, err := strictjson.ReadRegularFileBytes(path, maxManifestBytes)
	if err != nil {
		return Manifest{}, err
	}
	if err := strictjson.RejectNulls(data, nil); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	var manifest Manifest
	if err := strictjson.DecodeObjectNoNulls(
		bytes.NewReader(data), int64(len(data)), &manifest,
	); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	if err := manifest.Validate(); err != nil {
		return Manifest{}, fmt.Errorf("%s: %w", path, err)
	}
	return manifest, nil
}

func (catalog *Catalog) Names() []string {
	if catalog == nil {
		return nil
	}
	names := make([]string, 0, len(catalog.values))
	for name := range catalog.values {
		names = append(names, name)
	}
	sort.Strings(names)
	return names
}

func (catalog *Catalog) Get(name string) (Manifest, bool) {
	if catalog == nil {
		return Manifest{}, false
	}
	manifest, exists := catalog.values[name]
	return manifest.Clone(), exists
}

func (catalog *Catalog) Select(names []string) ([]Manifest, error) {
	if catalog == nil {
		return nil, fmt.Errorf("tool catalog is required")
	}
	selected := make([]Manifest, 0, len(names))
	seen := make(map[string]struct{}, len(names))
	for _, name := range names {
		if err := ValidateName(name); err != nil {
			return nil, err
		}
		if _, exists := seen[name]; exists {
			return nil, fmt.Errorf("duplicate configured tool %q", name)
		}
		seen[name] = struct{}{}
		manifest, exists := catalog.values[name]
		if !exists {
			return nil, fmt.Errorf("unknown configured tool %q", name)
		}
		selected = append(selected, manifest.Clone())
	}
	return selected, nil
}

// CanonicalJSON returns a stable, non-secret snapshot of the selected
// manifests. Environment references remain unresolved.
func CanonicalJSON(manifests []Manifest) (json.RawMessage, error) {
	values := make([]Manifest, len(manifests))
	seen := make(map[string]struct{}, len(manifests))
	for index, manifest := range manifests {
		if err := manifest.Validate(); err != nil {
			return nil, fmt.Errorf("tools[%d]: %w", index, err)
		}
		if _, exists := seen[manifest.Name]; exists {
			return nil, fmt.Errorf("duplicate tool %q", manifest.Name)
		}
		seen[manifest.Name] = struct{}{}
		values[index] = canonicalManifest(manifest)
	}
	sort.Slice(values, func(left, right int) bool {
		return values[left].Name < values[right].Name
	})
	data, err := json.Marshal(struct {
		SchemaVersion int        `json:"schema_version"`
		Tools         []Manifest `json:"tools"`
	}{SchemaVersion: SchemaVersion, Tools: values})
	if err != nil {
		return nil, fmt.Errorf("encode tool configuration: %w", err)
	}
	return data, nil
}
