package command

import (
	"fmt"
	"sort"

	"github.com/yy003x/runtime/internal/domain/profileid"
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
		if err := CheckProfile(profile); err != nil {
			return nil, fmt.Errorf("command profile %q: %w", id, err)
		}
		profiles[id] = cloneProfile(profile)
	}
	return &Catalog{profiles: profiles}, nil
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
		Command: profile.Command,
		Args:    append([]string(nil), profile.Args...),
		Model:   profile.Model,
		Effort:  profile.Effort,
		Prompt:  profile.Prompt,
		CWD:     profile.CWD,
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
