package provider

import (
	"os"
	"path/filepath"
	"strings"
	"testing"
)

func TestLoadDirExpandsPresetsAndAliases(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "cx.json", `{
  "type":"cli",
  "label":"Codex",
  "timeout_seconds":300,
  "cli":{
    "driver":"codex",
    "executor":"command",
    "command":{"binary":"codex","args":["exec"],"model":"gpt-base","env_unset":["BASE_DROP"]},
    "runtime":{"prompt_delivery":"stdin","result_contract":"required","override_policy":{"allow":["model","images"]}}
  },
  "presets":{
    "cx-fast":{"aliases":["codex-default"],"overrides":{"model":"gpt-fast"}},
    "cx-image":{"cli":{"command":{"args_append":["--color","never"],"env_unset_append":["CHILD_DROP"]}}}
  }
}`)

	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(profiles) != 3 {
		t.Fatalf("profiles=%d, want 3", len(profiles))
	}
	if profiles["cx-fast"].CLI.Command.Model != "gpt-fast" {
		t.Fatalf("preset model=%q", profiles["cx-fast"].CLI.Command.Model)
	}
	if got := strings.Join(profiles["cx-image"].CLI.Command.Args, " "); got != "exec --color never" {
		t.Fatalf("preset args=%q", got)
	}
	if got := strings.Join(profiles["cx-image"].CLI.Command.EnvUnset, " "); got != "BASE_DROP CHILD_DROP" {
		t.Fatalf("preset env_unset=%q", got)
	}
	resolved, ok := Resolve(profiles, "codex-default")
	if !ok || resolved.ID != "cx-fast" {
		t.Fatalf("alias resolution=%#v ok=%v", resolved, ok)
	}
}

func TestReservedCommandsReflectCurrentCLI(t *testing.T) {
	for _, command := range []string{"clean", "update", "profiles", "providers", "upgrade", "runs"} {
		if _, ok := ReservedCommands[command]; !ok {
			t.Fatalf("%s must be reserved", command)
		}
	}
	for _, removed := range []string{"prompt", "prune"} {
		if _, ok := ReservedCommands[removed]; ok {
			t.Fatalf("removed command %q is still reserved", removed)
		}
	}
}

func TestLoadDirRejectsUnknownKeysAtEveryLevel(t *testing.T) {
	tests := map[string]string{
		"root":    `{"type":"api","typo":true,"api":{"protocol":"openai","base_url":"https://example.test","model":"m","api_key_env":"K"}}`,
		"api":     `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test","model":"m","api_key_env":"K","modle":"typo"}}`,
		"command": `{"type":"cli","cli":{"executor":"command","command":{"binary":"x","args":[],"model":"","unknown":1},"runtime":{}}}`,
		"preset":  `{"type":"cli","cli":{"executor":"command","command":{"binary":"x","args":[],"model":""},"runtime":{}},"presets":{"child":{"cli":{"runtime":{"unknown":true}}}}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, "bad.json", body)
			if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestLoadDirAcceptsAPIAndTmuxAndNativeProfiles(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "api.json", `{
  "type":"api",
  "api":{"protocol":"openai","base_url":"https://example.test/v1","model":"gpt-test","api_key_env":"TEST_API_KEY"}
}`)
	writeConfig(t, dir, "tmux.json", `{
  "type":"cli",
  "cli":{
    "driver":"claude",
    "executor":"tmux",
    "command":{"binary":"claude","args":[],"model":""},
    "tmux":{"session_name":"agent"},
    "runtime":{"prompt_delivery":"paste","result_contract":"optional"}
  }
	}`)
	writeConfig(t, dir, "native.json", `{
  "type":"native",
  "native":{"persona":"default","max_rounds":2,"mock":{"responses":["ok"]}}
}`)

	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if profiles["api"].Transport() != "api" {
		t.Fatalf("api transport=%q", profiles["api"].Transport())
	}
	if profiles["tmux"].Transport() != "tmux" {
		t.Fatalf("tmux transport=%q", profiles["tmux"].Transport())
	}
	if profiles["native"].Transport() != "native" || profiles["native"].ResultContract() != "none" {
		t.Fatalf("native=%#v", profiles["native"])
	}
}

func TestLoadDirAcceptsAPIAgentRuntime(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "api-agent.json", `{
  "type":"api",
  "api":{
    "protocol":"anthropic","base_url":"https://example.test","model":"claude-test","api_key_env":"TEST_API_KEY",
    "runtime":{
      "enabled":true,"system_prompt":"system","max_rounds":4,"token_budget":32000,"llm_timeout_seconds":10,
      "auto_route_skills":true,"skills":["review"],"memory":{"enabled":true,"top_k":3,"type":"fact"},
      "mcp_servers":[{"name":"local","command":"fixture","args":["serve"],"env_passthrough":["MCP_FIXTURE"]}]
    }
  }
}`)
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	runtime := profiles["api-agent"].API.Runtime
	if runtime == nil || !runtime.Enabled || runtime.MCPServers[0].Transport != "stdio" || runtime.Memory.TopK != 3 {
		t.Fatalf("runtime=%#v", runtime)
	}
}

func TestLoadDirRejectsInvalidAPIAgentRuntime(t *testing.T) {
	tests := map[string]string{
		"stream":          `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test","model":"m","api_key_env":"K","stream":true,"runtime":{"enabled":true}}}`,
		"transport":       `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test","model":"m","api_key_env":"K","runtime":{"enabled":true,"mcp_servers":[{"name":"one","transport":"http","command":"x"}]}}}`,
		"server-name":     `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test","model":"m","api_key_env":"K","runtime":{"enabled":true,"mcp_servers":[{"name":"bad name","command":"x"}]}}}`,
		"memory-top-k":    `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test","model":"m","api_key_env":"K","runtime":{"enabled":true,"memory":{"enabled":true,"top_k":-1}}}}`,
		"duplicate-skill": `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test","model":"m","api_key_env":"K","runtime":{"enabled":true,"skills":["x","x"]}}}`,
		"unknown":         `{"type":"api","api":{"protocol":"openai","base_url":"https://example.test","model":"m","api_key_env":"K","runtime":{"enabled":true,"unknown":1}}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, "bad.json", body)
			if _, err := LoadDir(dir); err == nil {
				t.Fatal("LoadDir returned nil error")
			}
		})
	}
}

func TestLoadDirRejectsUnsafeAndConflictingConfig(t *testing.T) {
	tests := map[string]string{
		"secret":          `{"type":"api","api":{"protocol":"openai","base_url":"x","model":"m","api_key_env":"K","api_key":"secret"}}`,
		"business":        `{"type":"cli","purpose":"x","cli":{"executor":"command","command":{"binary":"x","args":[],"model":""},"runtime":{}}}`,
		"modelArg":        `{"type":"cli","cli":{"executor":"command","command":{"binary":"codex","args":["--model","x"],"model":""},"runtime":{}}}`,
		"envConflict":     `{"type":"cli","cli":{"executor":"command","command":{"binary":"x","args":[],"model":"","env":{"TOKEN":"x"},"env_unset":["TOKEN"]},"runtime":{}}}`,
		"envPassConflict": `{"type":"cli","cli":{"executor":"command","command":{"binary":"x","args":[],"model":"","env_passthrough":["TOKEN"],"env_unset":["TOKEN"]},"runtime":{}}}`,
		"invalidEnvName":  `{"type":"cli","cli":{"executor":"command","command":{"binary":"x","args":[],"model":"","env_unset":["BAD=NAME"]},"runtime":{}}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, "bad.json", body)
			if _, err := LoadDir(dir); err == nil {
				t.Fatal("LoadDir returned nil error")
			}
		})
	}

	t.Run("reserved", func(t *testing.T) {
		dir := t.TempDir()
		writeConfig(t, dir, "run.json", `{"type":"cli","cli":{"executor":"command","command":{"binary":"x","args":[],"model":""},"runtime":{}}}`)
		if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "内置命令冲突") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestLoadDirAcceptsDaemonExecutionAndDependencies(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "managed.json", `{
  "type":"cli",
  "depends":[{"command":"helper --serve","wait_tcp":"127.0.0.1:4141","restart":true,"optional":false}],
  "execution":{"audit_proxy":true,"upstream_proxy_env":["TEST_UPSTREAM_PROXY"],"bypass":["localhost"],"shim":true,"dylib":"${TEST_DYLIB}"},
  "cli":{"executor":"command","command":{"binary":"agent","args":[],"model":""},"runtime":{}}
}`)
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	profile := profiles["managed"]
	if len(profile.Depends) != 1 || !profile.Depends[0].Restart || !profile.Execution.AuditProxy || !profile.Execution.Shim {
		t.Fatalf("profile=%#v", profile)
	}
}

func TestLoadDirRejectsInvalidDaemonExecution(t *testing.T) {
	tests := map[string]string{
		"api":       `{"type":"api","execution":{"audit_proxy":true},"api":{"protocol":"openai","base_url":"https://example.test","model":"m","api_key_env":"K"}}`,
		"proxy-env": `{"type":"cli","execution":{"upstream_proxy_env":["PROXY"]},"cli":{"executor":"command","command":{"binary":"x","args":[],"model":""},"runtime":{}}}`,
		"wait-both": `{"type":"cli","depends":[{"command":"x","wait_tcp":"127.0.0.1:1","wait_http":"http://127.0.0.1:1"}],"cli":{"executor":"command","command":{"binary":"x","args":[],"model":""},"runtime":{}}}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, "bad.json", body)
			if _, err := LoadDir(dir); err == nil {
				t.Fatal("LoadDir returned nil error")
			}
		})
	}
}

func writeConfig(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
