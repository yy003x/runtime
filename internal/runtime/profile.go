package runtime

import (
	"encoding/json"
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

	path, format, err := findProfilePath(root, profileName)
	if err != nil {
		return Profile{}, err
	}
	raw, err := os.ReadFile(path)
	if err != nil {
		return Profile{}, fmt.Errorf("read profile %q: %w", profileName, err)
	}

	var profile Profile
	switch format {
	case "json":
		err = json.Unmarshal(raw, &profile)
	default:
		err = yaml.Unmarshal(raw, &profile)
	}
	if err != nil {
		return Profile{}, fmt.Errorf("parse profile %q: %w", profileName, err)
	}
	applyProfileDefaults(&profile, profileName)
	if profile.Provider.Type == "" {
		return Profile{}, fmt.Errorf("profile %q provider.type is required", profileName)
	}
	if strings.TrimSpace(profile.Input.Prompt) != "" && strings.TrimSpace(profile.Input.PromptFile) != "" {
		return Profile{}, fmt.Errorf("profile %q input.prompt and input.prompt_file cannot both be set", profileName)
	}
	return profile, nil
}

func findProfilePath(root, profileName string) (string, string, error) {
	candidates := []struct {
		ext    string
		format string
	}{
		{ext: ".json", format: "json"},
		{ext: ".yaml", format: "yaml"},
		{ext: ".yml", format: "yaml"},
	}
	for _, candidate := range candidates {
		path := filepath.Join(root, "configs", profileName+candidate.ext)
		info, err := os.Stat(path)
		if err == nil && !info.IsDir() {
			return path, candidate.format, nil
		}
		if err != nil && !os.IsNotExist(err) {
			return "", "", fmt.Errorf("stat profile %q: %w", profileName, err)
		}
	}
	return "", "", fmt.Errorf("read profile %q: no configs/%s.{json,yaml,yml} found", profileName, profileName)
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
