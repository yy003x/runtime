package agentrun

import (
	"fmt"
	"os"
	"path/filepath"

	"gopkg.in/yaml.v3"
)

type Settings struct {
	DefaultProject string `yaml:"default_project"`
	DefaultProfile string `yaml:"default_profile"`
	MaxConcurrency int    `yaml:"max_concurrency"`
	MaxQueue       int    `yaml:"max_queue"`
	QueueTimeout   int    `yaml:"queue_timeout_seconds"`
}

func loadSettings(configDir string) (Settings, error) {
	settings := Settings{
		DefaultProject: "_default",
		DefaultProfile: "cx",
		MaxConcurrency: 1,
		MaxQueue:       64,
		QueueTimeout:   3600,
	}
	path := filepath.Join(configDir, "runtime.yaml")
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
	if settings.DefaultProject == "" || settings.DefaultProfile == "" {
		return Settings{}, fmt.Errorf("runtime settings requires default_project and default_profile")
	}
	if settings.MaxConcurrency <= 0 {
		return Settings{}, fmt.Errorf("runtime settings max_concurrency must be positive")
	}
	if settings.MaxQueue <= 0 {
		return Settings{}, fmt.Errorf("runtime settings max_queue must be positive")
	}
	if settings.QueueTimeout < 0 {
		return Settings{}, fmt.Errorf("runtime settings queue_timeout_seconds must be non-negative")
	}
	return settings, nil
}
