package runtime

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

func LoadProfile(root, name string) (Profile, error) {
	profileName := strings.TrimSpace(name)
	if profileName == "" {
		return Profile{}, fmt.Errorf("profile name is required")
	}
	if filepath.Base(profileName) != profileName || strings.Contains(profileName, "..") {
		return Profile{}, fmt.Errorf("invalid profile name %q", name)
	}

	path := filepath.Join(root, "configs", profileName+".yaml")
	raw, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, fmt.Errorf("read profile %q: %w", profileName, err)
	}

	var profile Profile
	if err := yaml.Unmarshal(raw, &profile); err != nil {
		return Profile{}, fmt.Errorf("parse profile %q: %w", profileName, err)
	}
	applyProfileDefaults(&profile, profileName)
	if profile.Provider.Type == "" {
		return Profile{}, fmt.Errorf("profile %q provider.type is required", profileName)
	}
	return profile, nil
}

func applyProfileDefaults(profile *Profile, profileName string) {
	if profile.Name == "" {
		profile.Name = profileName
	}
	if profile.Runtime.TimeoutSeconds == 0 {
		profile.Runtime.TimeoutSeconds = 30
	}
	if profile.Artifacts.Root == "" {
		profile.Artifacts.Root = DefaultArtifactsRoot
	}
	if profile.Provider.Env == nil {
		profile.Provider.Env = map[string]string{}
	}
}
