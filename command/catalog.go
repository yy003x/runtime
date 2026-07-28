package command

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"github.com/yy003x/runtime/internal/profileid"
)

type Catalog struct {
	profiles map[string]Profile
}

func NewCatalog(values map[string]Profile, reservedIDs ...string) (*Catalog, error) {
	reserved := make(map[string]struct{}, len(reservedIDs))
	for _, id := range reservedIDs {
		reserved[id] = struct{}{}
	}
	profiles := make(map[string]Profile, len(values))
	for id, profile := range values {
		if err := profileid.Validate(id); err != nil {
			return nil, fmt.Errorf("command profile %q: %w", id, err)
		}
		if _, exists := reserved[id]; exists {
			return nil, fmt.Errorf("command profile %q conflicts with a fixed namespace", id)
		}
		if err := profile.Validate(); err != nil {
			return nil, fmt.Errorf("command profile %q: %w", id, err)
		}
		profiles[id] = cloneProfile(profile)
	}
	return &Catalog{profiles: profiles}, nil
}

func LoadDir(path string, reservedIDs ...string) (*Catalog, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, fmt.Errorf("inspect command profile directory: %w", err)
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return nil, fmt.Errorf("command profile path must be a directory, not a symlink")
	}
	entries, err := os.ReadDir(path)
	if err != nil {
		return nil, fmt.Errorf("read command profile directory: %w", err)
	}
	sort.Slice(entries, func(left, right int) bool {
		return entries[left].Name() < entries[right].Name()
	})
	values := make(map[string]Profile, len(entries))
	for _, entry := range entries {
		name := entry.Name()
		if entry.IsDir() || entry.Type()&os.ModeSymlink != 0 || filepath.Ext(name) != ".json" {
			return nil, fmt.Errorf("command profile directory contains unsupported entry %q", name)
		}
		id := strings.TrimSuffix(name, ".json")
		if err := profileid.Validate(id); err != nil {
			return nil, fmt.Errorf("command profile file %q: %w", name, err)
		}
		profile, err := LoadFile(filepath.Join(path, name))
		if err != nil {
			return nil, err
		}
		values[id] = profile
	}
	return NewCatalog(values, reservedIDs...)
}

func (catalog *Catalog) Get(id string) (Profile, bool) {
	if catalog == nil {
		return Profile{}, false
	}
	profile, exists := catalog.profiles[id]
	if !exists {
		return Profile{}, false
	}
	return cloneProfile(profile), true
}

func (catalog *Catalog) IDs() []string {
	if catalog == nil {
		return nil
	}
	values := make([]string, 0, len(catalog.profiles))
	for id := range catalog.profiles {
		values = append(values, id)
	}
	sort.Strings(values)
	return values
}

func cloneProfile(profile Profile) Profile {
	result := Profile{
		Binary:         profile.Binary,
		Args:           append([]string(nil), profile.Args...),
		Transport:      profile.Transport,
		PromptDelivery: profile.PromptDelivery,
		EffortAdapter:  profile.EffortAdapter,
	}
	if profile.Env != nil {
		result.Env = make(map[string]*string, len(profile.Env))
		for name, value := range profile.Env {
			if value == nil {
				result.Env[name] = nil
				continue
			}
			current := *value
			result.Env[name] = &current
		}
	}
	return result
}
