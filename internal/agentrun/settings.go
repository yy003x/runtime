package agentrun

import (
	"bytes"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"gopkg.in/yaml.v3"
)

type Settings struct {
	DefaultProject  string          `yaml:"default_project"`
	DefaultProfile  string          `yaml:"default_profile"`
	MaxConcurrency  int             `yaml:"max_concurrency"`
	MaxQueue        int             `yaml:"max_queue"`
	QueueTimeout    int             `yaml:"queue_timeout_seconds"`
	DefaultDeadline int             `yaml:"default_deadline_seconds"`
	Session         SessionSettings `yaml:"session"`
	Assets          AssetSettings   `yaml:"assets"`
	LLM             LLMSettings     `yaml:"llm"`
}

type AssetSettings struct {
	Roots map[string]string `yaml:"roots"`
}

type LLMSettings struct {
	MCPServers []MCPServerSettings `yaml:"mcp_servers"`
}

type MCPServerSettings struct {
	Name           string            `yaml:"name"`
	Command        string            `yaml:"command"`
	Args           []string          `yaml:"args"`
	Dir            string            `yaml:"dir"`
	Env            map[string]string `yaml:"env"`
	EnvPassthrough []string          `yaml:"env_passthrough"`
	TimeoutSeconds float64           `yaml:"timeout_seconds"`
}

type SessionSettings struct {
	DefaultCarrier string                  `yaml:"default_carrier"`
	Terminal       TerminalCarrierSettings `yaml:"terminal"`
}

type TerminalCarrierSettings struct {
	Driver string `yaml:"driver"`
}

func loadSettings(configDir string) (Settings, error) {
	settings := Settings{
		DefaultProject:  "_default",
		DefaultProfile:  "cx",
		MaxConcurrency:  1,
		MaxQueue:        64,
		QueueTimeout:    3600,
		DefaultDeadline: 300,
		Session: SessionSettings{
			DefaultCarrier: "tmux",
		},
	}
	path := filepath.Join(configDir, "runtime.yaml")
	data, err := os.ReadFile(path)
	if err != nil {
		if os.IsNotExist(err) {
			return settings, nil
		}
		return Settings{}, fmt.Errorf("read runtime settings %s: %w", path, err)
	}
	decoder := yaml.NewDecoder(bytes.NewReader(data))
	decoder.KnownFields(true)
	decodeErr := decoder.Decode(&settings)
	if decodeErr != nil && decodeErr != io.EOF {
		return Settings{}, fmt.Errorf("parse runtime settings %s: %w", path, decodeErr)
	}
	if decodeErr == nil {
		var extra any
		if err := decoder.Decode(&extra); err != io.EOF {
			if err == nil {
				return Settings{}, fmt.Errorf("parse runtime settings %s: multiple YAML documents", path)
			}
			return Settings{}, fmt.Errorf("parse runtime settings %s: %w", path, err)
		}
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
	if settings.DefaultDeadline < 0 {
		return Settings{}, fmt.Errorf("runtime settings default_deadline_seconds must be non-negative")
	}
	if settings.Session.DefaultCarrier == "" {
		settings.Session.DefaultCarrier = "tmux"
	}
	if settings.Session.DefaultCarrier != "tmux" && settings.Session.DefaultCarrier != "terminal" {
		return Settings{}, fmt.Errorf("runtime settings session.default_carrier must be tmux|terminal")
	}
	if driver := settings.Session.Terminal.Driver; driver != "" && driver != "ghostty" && driver != "iterm2" {
		return Settings{}, fmt.Errorf("runtime settings session.terminal.driver must be ghostty|iterm2")
	}
	for name, root := range settings.Assets.Roots {
		if !validAssetRootName(name) {
			return Settings{}, fmt.Errorf("runtime settings assets.roots contains invalid name %q", name)
		}
		if !filepath.IsAbs(root) {
			return Settings{}, fmt.Errorf("runtime settings assets.roots.%s must be an absolute path", name)
		}
	}
	seenMCP := make(map[string]struct{}, len(settings.LLM.MCPServers))
	if len(settings.LLM.MCPServers) > 32 {
		return Settings{}, fmt.Errorf("runtime settings llm.mcp_servers supports at most 32 entries")
	}
	for index, server := range settings.LLM.MCPServers {
		server.Name = strings.TrimSpace(server.Name)
		server.Command = strings.TrimSpace(server.Command)
		server.Dir = strings.TrimSpace(server.Dir)
		if !validAssetRootName(server.Name) || server.Command == "" ||
			strings.ContainsRune(server.Command, '\x00') {
			return Settings{}, fmt.Errorf("runtime settings llm.mcp_servers[%d] requires valid name and command", index)
		}
		if _, exists := seenMCP[server.Name]; exists {
			return Settings{}, fmt.Errorf("runtime settings contains duplicate MCP server %q", server.Name)
		}
		seenMCP[server.Name] = struct{}{}
		if strings.ContainsRune(server.Dir, '\x00') ||
			server.Dir != "" && !filepath.IsAbs(server.Dir) {
			return Settings{}, fmt.Errorf("runtime settings llm.mcp_servers[%d].dir must be absolute", index)
		}
		if server.TimeoutSeconds < 0 || server.TimeoutSeconds > 3600 {
			return Settings{}, fmt.Errorf("runtime settings llm.mcp_servers[%d].timeout_seconds must be between 0 and 3600", index)
		}
		if len(server.Args) > 256 || len(server.EnvPassthrough) > 256 || len(server.Env) > 256 {
			return Settings{}, fmt.Errorf("runtime settings llm.mcp_servers[%d] exceeds argument or environment limit", index)
		}
		for _, value := range server.Args {
			if strings.ContainsRune(value, '\x00') {
				return Settings{}, fmt.Errorf("runtime settings llm.mcp_servers[%d] contains NUL", index)
			}
		}
		seenPassthrough := make(map[string]struct{}, len(server.EnvPassthrough))
		for _, name := range server.EnvPassthrough {
			if !ValidEnvironmentName(name) {
				return Settings{}, fmt.Errorf("runtime settings llm.mcp_servers[%d] contains invalid env_passthrough name %q", index, name)
			}
			if _, exists := seenPassthrough[name]; exists {
				return Settings{}, fmt.Errorf("runtime settings llm.mcp_servers[%d] contains duplicate env_passthrough name %q", index, name)
			}
			seenPassthrough[name] = struct{}{}
		}
		for name, value := range server.Env {
			if !ValidEnvironmentName(name) || strings.ContainsRune(value, '\x00') {
				return Settings{}, fmt.Errorf("runtime settings llm.mcp_servers[%d] contains invalid env entry %q", index, name)
			}
		}
		settings.LLM.MCPServers[index] = server
	}
	return settings, nil
}

func ValidEnvironmentName(name string) bool {
	if name == "" {
		return false
	}
	for index, value := range name {
		if value == '_' || value >= 'A' && value <= 'Z' || value >= 'a' && value <= 'z' ||
			index > 0 && value >= '0' && value <= '9' {
			continue
		}
		return false
	}
	return !strings.ContainsRune(name, '=')
}

func validAssetRootName(name string) bool {
	if name == "" {
		return false
	}
	for _, value := range name {
		if value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' ||
			value >= '0' && value <= '9' || value == '.' || value == '_' || value == '-' {
			continue
		}
		return false
	}
	return true
}
