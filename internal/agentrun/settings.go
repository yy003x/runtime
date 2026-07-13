package agentrun

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Settings struct {
	RunsDir           string `yaml:"runs_dir"`
	DefaultProject    string `yaml:"default_project"`
	DefaultProfile    string `yaml:"default_profile"`
	MaxConcurrency    int    `yaml:"max_concurrency"`
	ProviderConfigDir string `yaml:"provider_config_dir"`
}

func loadSettings(root string) (Settings, error) {
	settings := Settings{
		RunsDir:           "runs/global/runtime",
		DefaultProject:    "_default",
		DefaultProfile:    "cx",
		MaxConcurrency:    1,
		ProviderConfigDir: "configs",
	}
	path := filepath.Join(root, "configs", "runtime.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return Settings{}, fmt.Errorf("read runtime settings %s: %w", path, err)
	}
	if err := yaml.Unmarshal(data, &settings); err != nil {
		return Settings{}, fmt.Errorf("parse runtime settings %s: %w", path, err)
	}
	if settings.RunsDir == "" || settings.ProviderConfigDir == "" || settings.DefaultProject == "" || settings.DefaultProfile == "" {
		return Settings{}, fmt.Errorf("runtime settings requires runs_dir, provider_config_dir, default_project and default_profile")
	}
	if settings.MaxConcurrency <= 0 {
		return Settings{}, fmt.Errorf("runtime settings max_concurrency must be positive")
	}
	return settings, nil
}

func rooted(root, path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(root, path)
}
