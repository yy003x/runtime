package config

import (
	"os"
	"path/filepath"

	"github.com/yy003x/runtime/internal/layout"
)

type Config struct {
	Home   string
	Paths  layout.Paths
	Update UpdateConfig
}

type UpdateConfig struct {
	Enabled            *bool
	CheckIntervalHours int
	Repository         string
}

func Load() (*Config, error) {
	paths, err := layout.Resolve()
	if err != nil {
		return nil, err
	}
	if err := paths.Ensure(); err != nil {
		return nil, err
	}
	enabled := true
	repository := os.Getenv("SN_CLI_REPOSITORY")
	if repository == "" {
		repository = "yy003x/runtime"
	}
	return &Config{Home: paths.Home, Paths: paths, Update: UpdateConfig{
		Enabled: &enabled, CheckIntervalHours: 24, Repository: repository,
	}}, nil
}

func (c *Config) ResolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(c.Home, path)
}

func (c *Config) UpdateEnabled() bool {
	return c.Update.Enabled == nil || *c.Update.Enabled
}

func (c *Config) UpdateStateFile() string {
	if c.Paths.UpdateStateFile != "" {
		return c.Paths.UpdateStateFile
	}
	return filepath.Join(c.Home, "state", "update.json")
}
