package config

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type Config struct {
	SchemaVersion   int                   `json:"schema_version"`
	DefaultProvider string                `json:"default_provider"`
	Runtime         RuntimeConfig         `json:"runtime"`
	Paths           PathsConfig           `json:"paths"`
	Native          NativeConfig          `json:"native"`
	Tools           map[string]ToolConfig `json:"tools"`
	Update          UpdateConfig          `json:"update"`
	Root            string                `json:"-"`
}

type RuntimeConfig struct {
	Command               string `json:"command"`
	DefaultSandbox        string `json:"default_sandbox"`
	DefaultTimeoutSeconds int    `json:"default_timeout_seconds"`
}

type PathsConfig struct {
	SessionsRoot string `json:"sessions_root"`
}

type NativeConfig struct {
	Python   string            `json:"python"`
	ConfDir  string            `json:"conf_dir"`
	RunsDir  string            `json:"runs_dir"`
	Project  string            `json:"project"`
	Profiles map[string]string `json:"profiles"`
}

type ToolConfig struct {
	Command     string            `json:"command"`
	Args        []string          `json:"args"`
	Env         map[string]string `json:"env"`
	Description string            `json:"description"`
}

type UpdateConfig struct {
	Enabled            *bool  `json:"enabled"`
	CheckIntervalHours int    `json:"check_interval_hours"`
	Ref                string `json:"ref"`
	StateFile          string `json:"state_file"`
	InstallScript      string `json:"install_script"`
	RepoURL            string `json:"repo_url"`
}

func Load() (*Config, error) {
	root, err := FindRoot()
	if err != nil {
		return nil, err
	}
	cfg := &Config{}
	if err := readJSON(filepath.Join(root, "sncli", "conf", "default.json"), cfg); err != nil {
		return nil, err
	}
	local := filepath.Join(root, "sncli", "conf", "local.json")
	if _, err := os.Stat(local); err == nil {
		var localCfg Config
		if err := readJSON(local, &localCfg); err != nil {
			return nil, err
		}
		mergeConfig(cfg, &localCfg)
	}
	cfg.Root = root
	applyDefaults(cfg)
	return cfg, cfg.Validate()
}

func FindRoot() (string, error) {
	if root := os.Getenv("SN_CLI_ROOT"); root != "" {
		return filepath.Abs(root)
	}
	if root := os.Getenv("SINAN_ROOT"); root != "" {
		if fileExists(filepath.Join(root, "apps", "sn-cli", "conf", "default.json")) {
			return filepath.Abs(root)
		}
	}
	wd, err := os.Getwd()
	if err != nil {
		return "", err
	}
	current, err := filepath.Abs(wd)
	if err != nil {
		return "", err
	}
	for {
		if fileExists(filepath.Join(current, "cmd", "sn-cli", "main.go")) &&
			fileExists(filepath.Join(current, "sncli", "conf", "default.json")) {
			return current, nil
		}
		next := filepath.Dir(current)
		if next == current {
			break
		}
		current = next
	}
	return "", fmt.Errorf("cannot locate sn-cli runtime repo; set SN_CLI_ROOT")
}

func (c *Config) SinanRoot() string {
	if root := strings.TrimSpace(os.Getenv("SINAN_ROOT")); root != "" {
		return root
	}
	return c.Root
}

func (c *Config) Validate() error {
	if c.SchemaVersion != 1 {
		return fmt.Errorf("schema_version must be 1")
	}
	if c.DefaultProvider == "" {
		return fmt.Errorf("default_provider is required")
	}
	if c.Runtime.Command == "" {
		return fmt.Errorf("runtime.command is required")
	}
	if c.Paths.SessionsRoot == "" {
		return fmt.Errorf("paths.sessions_root is required")
	}
	if c.Native.Project == "" {
		return fmt.Errorf("native.project is required")
	}
	if c.Native.ConfDir == "" {
		return fmt.Errorf("native.conf_dir is required")
	}
	if c.Native.RunsDir == "" {
		return fmt.Errorf("native.runs_dir is required")
	}
	for name, tool := range c.Tools {
		if tool.Command == "" {
			return fmt.Errorf("tools.%s.command is required", name)
		}
	}
	return nil
}

func (c *Config) RuntimeCommandPath() string {
	return c.ResolvePathWithRuntimeFallback(c.Runtime.Command)
}

func (c *Config) SessionsRoot() string {
	return c.ResolvePath(c.Paths.SessionsRoot)
}

func (c *Config) NativePython() string {
	python := strings.TrimSpace(c.Native.Python)
	if python != "" && python != "python3" {
		return c.ResolvePath(python)
	}
	if c.Root != "" {
		venvPython := filepath.Join(c.Root, ".venv", "bin", "python3")
		if fileExists(venvPython) {
			return venvPython
		}
	}
	if python != "" {
		return python
	}
	return "python3"
}

func (c *Config) NativeConfDir() string {
	return c.ResolvePathWithRuntimeFallback(c.Native.ConfDir)
}

func (c *Config) NativeRunsDir() string {
	return c.ResolvePathWithRuntimeFallback(c.Native.RunsDir)
}

func (c *Config) NativeProfile(provider string) string {
	return c.Native.Profiles[provider]
}

func (c *Config) UpdateStateFile() string {
	return c.ResolvePath(c.Update.StateFile)
}

func (c *Config) UpdateInstallScript() string {
	return c.ResolvePath(c.Update.InstallScript)
}

func (c *Config) ResolvePathWithRuntimeFallback(path string) string {
	if filepath.IsAbs(path) {
		return path
	}
	if _, err := os.Stat(filepath.Join(c.Root, path)); err == nil {
		return filepath.Join(c.Root, path)
	}
	if sinanRoot := c.SinanRoot(); sinanRoot != "" {
		if _, err := os.Stat(filepath.Join(sinanRoot, path)); err == nil {
			return filepath.Join(sinanRoot, path)
		}
	}
	return c.ResolvePath(path)
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

func readJSON(path string, out any) error {
	data, err := os.ReadFile(path)
	if err != nil {
		return fmt.Errorf("read %s: %w", path, err)
	}
	if err := json.Unmarshal(data, out); err != nil {
		return fmt.Errorf("parse %s: %w", path, err)
	}
	return nil
}

func mergeConfig(base, overlay *Config) {
	if overlay.SchemaVersion != 0 {
		base.SchemaVersion = overlay.SchemaVersion
	}
	if overlay.DefaultProvider != "" {
		base.DefaultProvider = overlay.DefaultProvider
	}
	if overlay.Runtime.Command != "" {
		base.Runtime.Command = overlay.Runtime.Command
	}
	if overlay.Runtime.DefaultSandbox != "" {
		base.Runtime.DefaultSandbox = overlay.Runtime.DefaultSandbox
	}
	if overlay.Runtime.DefaultTimeoutSeconds != 0 {
		base.Runtime.DefaultTimeoutSeconds = overlay.Runtime.DefaultTimeoutSeconds
	}
	if overlay.Paths.SessionsRoot != "" {
		base.Paths.SessionsRoot = overlay.Paths.SessionsRoot
	}
	if overlay.Native.Python != "" {
		base.Native.Python = overlay.Native.Python
	}
	if overlay.Native.ConfDir != "" {
		base.Native.ConfDir = overlay.Native.ConfDir
	}
	if overlay.Native.RunsDir != "" {
		base.Native.RunsDir = overlay.Native.RunsDir
	}
	if overlay.Native.Project != "" {
		base.Native.Project = overlay.Native.Project
	}
	if overlay.Native.Profiles != nil {
		if base.Native.Profiles == nil {
			base.Native.Profiles = map[string]string{}
		}
		for key, value := range overlay.Native.Profiles {
			base.Native.Profiles[key] = value
		}
	}
	if overlay.Tools != nil {
		if base.Tools == nil {
			base.Tools = map[string]ToolConfig{}
		}
		for key, value := range overlay.Tools {
			base.Tools[key] = mergeToolConfig(base.Tools[key], value)
		}
	}
	mergeUpdateConfig(&base.Update, overlay.Update)
}

func applyDefaults(c *Config) {
	if c.DefaultProvider == "" {
		c.DefaultProvider = "codex"
	}
	if c.Runtime.DefaultSandbox == "" {
		c.Runtime.DefaultSandbox = "read-only"
	}
	if c.Runtime.DefaultTimeoutSeconds == 0 {
		c.Runtime.DefaultTimeoutSeconds = 1800
	}
	if c.Native.Python == "" {
		c.Native.Python = "python3"
	}
	if c.Native.ConfDir == "" {
		c.Native.ConfDir = "apps/runtime/conf"
	}
	if c.Native.RunsDir == "" {
		c.Native.RunsDir = "runs/global/runtime"
	}
	if c.Native.Project == "" {
		c.Native.Project = "agent"
	}
	if c.Native.Profiles == nil {
		c.Native.Profiles = map[string]string{}
	}
	if c.Native.Profiles["codex"] == "" {
		c.Native.Profiles["codex"] = "tmux-codex"
	}
	if c.Native.Profiles["claude"] == "" {
		c.Native.Profiles["claude"] = "tmux-claude"
	}
	if c.Tools == nil {
		c.Tools = map[string]ToolConfig{}
	}
	for name, tool := range c.Tools {
		if tool.Env == nil {
			tool.Env = map[string]string{}
		}
		for key, value := range tool.Env {
			tool.Env[key] = expandEnvValue(value)
		}
		c.Tools[name] = tool
	}
	if c.Update.Enabled == nil {
		enabled := true
		c.Update.Enabled = &enabled
	}
	if c.Update.CheckIntervalHours == 0 {
		c.Update.CheckIntervalHours = 24
	}
	if c.Update.Ref == "" {
		c.Update.Ref = "main"
	}
	if c.Update.StateFile == "" {
		c.Update.StateFile = "runs/global/sn-cli/state/current/update-check.json"
	}
	if c.Update.InstallScript == "" {
		c.Update.InstallScript = "scripts/install-sn-cli.sh"
	}
	if c.Update.RepoURL == "" {
		c.Update.RepoURL = "https://github.com/yy003x/runtime.git"
	}
}

func fileExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir()
}

func mergeToolConfig(base, overlay ToolConfig) ToolConfig {
	if overlay.Command != "" {
		base.Command = overlay.Command
	}
	if overlay.Args != nil {
		base.Args = overlay.Args
	}
	if overlay.Description != "" {
		base.Description = overlay.Description
	}
	if overlay.Env != nil {
		if base.Env == nil {
			base.Env = map[string]string{}
		}
		for key, value := range overlay.Env {
			base.Env[key] = value
		}
	}
	return base
}

func mergeUpdateConfig(base *UpdateConfig, overlay UpdateConfig) {
	if overlay.Enabled != nil {
		base.Enabled = overlay.Enabled
	}
	if overlay.CheckIntervalHours != 0 {
		base.CheckIntervalHours = overlay.CheckIntervalHours
	}
	if overlay.Ref != "" {
		base.Ref = overlay.Ref
	}
	if overlay.StateFile != "" {
		base.StateFile = overlay.StateFile
	}
	if overlay.InstallScript != "" {
		base.InstallScript = overlay.InstallScript
	}
	if overlay.RepoURL != "" {
		base.RepoURL = overlay.RepoURL
	}
}

func expandEnvValue(value string) string {
	expanded := os.ExpandEnv(value)
	if expanded == "~" {
		if home, err := os.UserHomeDir(); err == nil {
			return home
		}
	}
	if strings.HasPrefix(expanded, "~/") {
		if home, err := os.UserHomeDir(); err == nil {
			return filepath.Join(home, expanded[2:])
		}
	}
	return expanded
}
