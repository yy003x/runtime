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
    "command":{"binary":"codex","args":["exec"],"model":"gpt-base"},
    "runtime":{"prompt_delivery":"stdin","result_contract":"required","override_policy":{"allow":["model","images"]}}
  },
  "presets":{
    "cx-fast":{"aliases":["codex-default"],"overrides":{"model":"gpt-fast"}},
    "cx-image":{"cli":{"command":{"args_append":["--color","never"]}}}
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
	resolved, ok := Resolve(profiles, "codex-default")
	if !ok || resolved.ID != "cx-fast" {
		t.Fatalf("alias resolution=%#v ok=%v", resolved, ok)
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

func TestLoadDirRejectsUnsafeAndConflictingConfig(t *testing.T) {
	tests := map[string]string{
		"secret":   `{"type":"api","api":{"protocol":"openai","base_url":"x","model":"m","api_key_env":"K","api_key":"secret"}}`,
		"business": `{"type":"cli","purpose":"x","cli":{"executor":"command","command":{"binary":"x","args":[],"model":""},"runtime":{}}}`,
		"modelArg": `{"type":"cli","cli":{"executor":"command","command":{"binary":"codex","args":["--model","x"],"model":""},"runtime":{}}}`,
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
