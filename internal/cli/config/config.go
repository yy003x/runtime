package config

import (
	"fmt"
	"os"
	"path/filepath"
)

type Config struct {
	Root   string
	Update UpdateConfig
}

type UpdateConfig struct {
	Enabled            *bool
	CheckIntervalHours int
	Ref                string
	StateFile          string
	InstallScript      string
	RepoURL            string
}

func Load() (*Config, error) {
	root, err := FindRoot()
	if err != nil {
		return nil, err
	}
	enabled := true
	return &Config{Root: root, Update: UpdateConfig{
		Enabled: &enabled, CheckIntervalHours: 24, Ref: "main",
		StateFile:     "runs/global/sn-cli/state/current/update-check.json",
		InstallScript: "scripts/install-sn-cli.sh",
		RepoURL:       "https://github.com/yy003x/runtime.git",
	}}, nil
}

func FindRoot() (string, error) {
	if root := os.Getenv("SN_CLI_ROOT"); root != "" {
		return filepath.Abs(root)
	}
	current, err := os.Getwd()
	if err != nil {
		return "", err
	}
	current, err = filepath.Abs(current)
	if err != nil {
		return "", err
	}
	for {
		if fileExists(filepath.Join(current, "go.mod")) && fileExists(filepath.Join(current, "cmd", "sn-cli", "main.go")) && fileExists(filepath.Join(current, "configs", "runtime.yaml")) {
			return current, nil
		}
		next := filepath.Dir(current)
		if next == current {
			break
		}
		current = next
	}
	return "", fmt.Errorf("cannot locate agent runtime repo; set SN_CLI_ROOT")
}

func (c *Config) ResolvePath(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	return filepath.Join(c.Root, path)
}

func (c *Config) UpdateEnabled() bool {
	return c.Update.Enabled == nil || *c.Update.Enabled
}

func (c *Config) UpdateStateFile() string {
	return c.ResolvePath(c.Update.StateFile)
}

func (c *Config) UpdateInstallScript() string {
	return c.ResolvePath(c.Update.InstallScript)
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}
