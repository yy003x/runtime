package agentrun

import (
	"fmt"
	"os"
	"path/filepath"
	"testing"
)

func TestNewLoadsRuntimeSettings(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	assetRoot := filepath.Join(root, "project-assets")
	data := []byte(fmt.Sprintf("default_project: demo\ndefault_profile: fake\nmax_concurrency: 2\nmax_queue: 12\nqueue_timeout_seconds: 45\ndefault_deadline_seconds: 90\nassets:\n  roots:\n    project: %s\nllm:\n  mcp_servers:\n    - name: local-tools\n      command: mcp-server\n      args: [serve]\n      dir: %s\n      env:\n        API_KEY: ${TEST_MCP_KEY}\n      env_passthrough: [HTTP_PROXY]\n      timeout_seconds: 2.5\nsession:\n  default_carrier: terminal\n  terminal:\n    driver: iterm2\n", assetRoot, root))
	if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), data, 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	if service.RunsDir != filepath.Join(root, "runs") || service.ConfigDir != filepath.Join(root, "configs") {
		t.Fatalf("service paths: %#v", service)
	}
	if service.DefaultProject != "demo" || service.DefaultProfile != "fake" || service.MaxConcurrency != 2 || service.MaxQueue != 12 || service.QueueTimeout != 45 || service.DefaultDeadline != 90 {
		t.Fatalf("service settings: %#v", service)
	}
	if service.DefaultCarrier != "terminal" || service.TerminalDriver != "iterm2" {
		t.Fatalf("carrier settings: %#v", service)
	}
	if service.AssetRoots["project"] != assetRoot {
		t.Fatalf("asset roots: %#v", service.AssetRoots)
	}
	if len(service.MCPServers) != 1 ||
		service.MCPServers[0].Name != "local-tools" ||
		service.MCPServers[0].Command != "mcp-server" ||
		service.MCPServers[0].TimeoutSeconds != 2.5 ||
		service.MCPServers[0].Env["API_KEY"] != "${TEST_MCP_KEY}" ||
		len(service.MCPServers[0].EnvPassthrough) != 1 {
		t.Fatalf("MCP servers: %#v", service.MCPServers)
	}
}

func TestRuntimeSettingsRejectUnknownCarrierAndTerminalDriver(t *testing.T) {
	for name, body := range map[string]string{
		"carrier":          "session:\n  default_carrier: screen\n",
		"driver":           "session:\n  terminal:\n    driver: auto\n",
		"unknown":          "unsupported: true\n",
		"negative-timeout": "default_deadline_seconds: -1\n",
		"relative-assets":  "assets:\n  roots:\n    project: ./skills\n",
		"duplicate-mcp":    "llm:\n  mcp_servers:\n    - {name: tools, command: one}\n    - {name: tools, command: two}\n",
		"relative-mcp-dir": "llm:\n  mcp_servers:\n    - {name: tools, command: one, dir: ./tools}\n",
		"invalid-mcp-env":  "llm:\n  mcp_servers:\n    - name: tools\n      command: one\n      env:\n        BAD-NAME: value\n",
		"duplicate-env":    "llm:\n  mcp_servers:\n    - name: tools\n      command: one\n      env_passthrough: [TOKEN, TOKEN]\n",
	} {
		t.Run(name, func(t *testing.T) {
			root := t.TempDir()
			if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
				t.Fatal(err)
			}
			if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), []byte(body), 0o644); err != nil {
				t.Fatal(err)
			}
			if _, err := New(root).Profiles(); err == nil {
				t.Fatal("invalid carrier settings were accepted")
			}
		})
	}
}

func TestMalformedRuntimeSettingsBlocksProfileLoading(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), []byte("max_concurrency: 0\n"), 0o644); err != nil {
		t.Fatal(err)
	}
	if _, err := New(root).Profiles(); err == nil {
		t.Fatal("Profiles accepted malformed runtime settings")
	}
}

func TestEmptyRuntimeSettingsUseDefaults(t *testing.T) {
	root := t.TempDir()
	if err := os.MkdirAll(filepath.Join(root, "configs"), 0o755); err != nil {
		t.Fatal(err)
	}
	if err := os.WriteFile(filepath.Join(root, "configs", "runtime.yaml"), nil, 0o644); err != nil {
		t.Fatal(err)
	}
	service := New(root)
	if service.configErr != nil || service.DefaultProfile != "cx" || service.DefaultDeadline != 300 || service.DefaultCarrier != "tmux" {
		t.Fatalf("service=%#v", service)
	}
}
