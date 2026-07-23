package provider

import (
	"encoding/json"
	"os"
	"path/filepath"
	"reflect"
	"runtime"
	"strings"
	"testing"
)

func TestLoadDirNormalizesMinimalCLIProfiles(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "cx.json", `{
  "command":"codex","model":"gpt-base","effort":"high"
}`)
	writeConfig(t, dir, "cc.json", `{
	  "command":"claude"
	}`)

	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if len(profiles) != 2 {
		t.Fatalf("profiles=%d, want 2", len(profiles))
	}
	cx := profiles["cx"]
	if cx.CLI.Driver != "codex" || cx.CLI.Command.Binary != "codex" || cx.CLI.Command.Model != "gpt-base" || cx.CLI.Effort != "high" {
		t.Fatalf("cx=%#v", cx)
	}
	if cx.CLI.Runtime.PromptDelivery != "stdin" {
		t.Fatalf("cx runtime=%#v", cx.CLI.Runtime)
	}
	cxRequest, err := Prepare(cx, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cxRequest.CLI.Argv, " "); got != "codex -c model_reasoning_effort=high --model gpt-base exec" {
		t.Fatalf("cx argv=%q", got)
	}
	cxOverride, err := Prepare(cx, "hello", map[string]any{"reasoning_effort": "low"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cxOverride.CLI.Argv, " "); got != "codex -c model_reasoning_effort=low --model gpt-base exec" {
		t.Fatalf("cx override argv=%q", got)
	}
	cc := profiles["cc"]
	if cc.CLI.Command.Binary != "claude" || cc.CLI.Command.Model != "" || cc.CLI.Effort != "" {
		t.Fatalf("cc=%#v", cc)
	}
	ccRequest, err := Prepare(cc, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ccRequest.CLI.Argv, " "); got != "claude -p" {
		t.Fatalf("cc argv=%q", got)
	}
	ccOverride, err := Prepare(cc, "hello", map[string]any{"effort": "low"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(ccOverride.CLI.Argv, " "); got != "claude --effort low -p" {
		t.Fatalf("cc override argv=%q", got)
	}
	resolved, ok := Resolve(profiles, "cx")
	if !ok || resolved.ID != "cx" {
		t.Fatalf("resolution=%#v ok=%v", resolved, ok)
	}
}

func TestLoadDirUsesGenericAdapterForUnrecognizedCommand(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "custom.json", `{"command":"/opt/bin/claude-wrapper"}`)
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	cli := profiles["custom"].CLI
	if cli.Driver != "generic" || cli.Command.Binary != "/opt/bin/claude-wrapper" {
		t.Fatalf("cli=%#v", cli)
	}
}

func TestRepositoryProfileTemplatesLoadAsStandaloneProfiles(t *testing.T) {
	_, currentFile, _, ok := runtime.Caller(0)
	if !ok {
		t.Fatal("runtime.Caller failed")
	}
	configDir := filepath.Clean(filepath.Join(filepath.Dir(currentFile), "..", "..", "configs"))
	profiles, err := LoadDir(configDir)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 11 {
		t.Fatalf("profiles=%d, want 11", len(profiles))
	}
	for _, id := range []string{"api-cc", "api-cx", "cc", "cc-bai", "cc-glm", "commit", "cx", "cx-image", "cx-spark", "mcc", "mcx"} {
		if _, exists := profiles[id]; !exists {
			t.Fatalf("missing required repository profile %q", id)
		}
	}
	originalImagePath, imagePathWasSet := os.LookupEnv("WB_RUNTIME_IMAGE_PATH")
	if err := os.Unsetenv("WB_RUNTIME_IMAGE_PATH"); err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() {
		if imagePathWasSet {
			_ = os.Setenv("WB_RUNTIME_IMAGE_PATH", originalImagePath)
		} else {
			_ = os.Unsetenv("WB_RUNTIME_IMAGE_PATH")
		}
	})
	if _, err := Prepare(profiles["cx-image"], "describe", nil); err == nil || !strings.Contains(err.Error(), "环境变量未设置: WB_RUNTIME_IMAGE_PATH") {
		t.Fatalf("cx-image missing path error=%v", err)
	}
	t.Setenv("HOME", "/tmp/runtime-home")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "must-not-reach-cc-glm")
	t.Setenv("BAILIAN_API_KEY", "test-bailian-key")
	t.Setenv("OPENROUTER_API_KEY", "test-openrouter-key")
	t.Setenv("WB_RUNTIME_IMAGE_PATH", "/tmp/image.png")
	cc := profiles["cc"]
	if cc.CLI.Command.Model != "" || cc.CLI.Effort != "" || len(cc.CLI.Command.Env) != 4 || cc.CLI.Command.Env["CLAUDE_CONFIG_DIR"] != "${HOME}/.claude.aip" {
		t.Fatalf("cc=%#v", cc)
	}
	ccEnvironment, err := CommandEnvironment(cc.CLI.Command, nil)
	if err != nil {
		t.Fatal(err)
	}
	if !contains(ccEnvironment, "CLAUDE_CONFIG_DIR=/tmp/runtime-home/.claude.aip") {
		t.Fatalf("cc environment missing resolved CLAUDE_CONFIG_DIR")
	}
	bai := profiles["cc-bai"]
	if bai.CLI.Command.Model != "qwen3.5-plus-2026-04-20" || bai.CLI.Effort != "high" || strings.Join(bai.CLI.Command.EnvUnset, ",") != "ANTHROPIC_AUTH_TOKEN" {
		t.Fatalf("cc-bai=%#v", bai)
	}
	baiEnvironment, err := CommandEnvironment(bai.CLI.Command, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"ANTHROPIC_API_KEY=test-bailian-key", "ANTHROPIC_BASE_URL=https://ws-guu9tlrmhj23g0fa.cn-beijing.maas.aliyuncs.com/apps/anthropic", "ANTHROPIC_MODEL=qwen3.5-plus-2026-04-20", "CLAUDE_CONFIG_DIR=/tmp/runtime-home/.claude.aip"} {
		if !contains(baiEnvironment, value) {
			t.Fatalf("cc-bai environment missing %q", value)
		}
	}
	if contains(baiEnvironment, "ANTHROPIC_AUTH_TOKEN=must-not-reach-cc-glm") {
		t.Fatal("cc-bai environment retained ANTHROPIC_AUTH_TOKEN")
	}
	for _, id := range []string{"commit", "cx", "cx-image"} {
		profile := profiles[id]
		if profile.CLI == nil || profile.CLI.Command.Env["CODEX_HOME"] != "${HOME}/.codex-aip" {
			t.Fatalf("%s CODEX_HOME=%#v", id, profile.CLI)
		}
		environment, environmentErr := CommandEnvironment(profile.CLI.Command, nil)
		if environmentErr != nil {
			t.Fatalf("%s environment: %v", id, environmentErr)
		}
		if !contains(environment, "CODEX_HOME=/tmp/runtime-home/.codex-aip") {
			t.Fatalf("%s environment missing resolved CODEX_HOME", id)
		}
	}
	sparkProfile := profiles["cx-spark"]
	if sparkProfile.CLI == nil || sparkProfile.CLI.Command.Env["CODEX_HOME"] != "${HOME}/.codex-ait" {
		t.Fatalf("cx-spark CODEX_HOME=%#v", sparkProfile.CLI)
	}
	sparkEnvironment, err := CommandEnvironment(sparkProfile.CLI.Command, nil)
	if err != nil {
		t.Fatalf("cx-spark environment: %v", err)
	}
	if !contains(sparkEnvironment, "CODEX_HOME=/tmp/runtime-home/.codex-ait") {
		t.Fatal("cx-spark environment missing resolved CODEX_HOME")
	}
	cxProfile := profiles["cx"]
	if cxProfile.CLI.Command.Model != "gpt-5.6-sol" || cxProfile.CLI.Effort != "" || len(cxProfile.CLI.Command.Args) != 0 {
		t.Fatalf("cx=%#v", cxProfile)
	}
	cxRequest, err := Prepare(cxProfile, "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cxRequest.CLI.Argv, " "); got != `codex --model gpt-5.6-sol exec` {
		t.Fatalf("cx argv=%q", got)
	}
	cxDirect, err := PrepareInteractiveCLI(cxProfile, []string{"review"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cxDirect.Argv, " "); got != `codex --model gpt-5.6-sol review` {
		t.Fatalf("cx direct argv=%q", got)
	}
	image := profiles["cx-image"]
	if image.TimeoutSeconds != 180 || image.CLI.Command.Model != "gpt-5.6-sol" || image.CLI.Effort != "xhigh" {
		t.Fatalf("cx-image=%#v", image)
	}
	imageRequest, err := Prepare(image, "describe", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(imageRequest.CLI.Argv, " "); got != "codex -c model_reasoning_effort=xhigh exec --skip-git-repo-check --ignore-user-config --ephemeral --color never --image /tmp/image.png --model gpt-5.6-sol" {
		t.Fatalf("cx-image argv=%q", got)
	}
	spark := profiles["cx-spark"]
	if spark.TimeoutSeconds != 0 || spark.CLI.Command.Model != "gpt-5.3-codex-spark" || spark.CLI.Effort != "" || len(spark.CLI.Command.Args) != 9 {
		t.Fatalf("cx-spark=%#v", spark)
	}
	sparkRequest, err := Prepare(spark, "fast review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(sparkRequest.CLI.Argv, " "); got != `codex --search --sandbox danger-full-access --ask-for-approval never -c model_reasoning_effort=xhigh -c model_verbosity=high --model gpt-5.3-codex-spark exec` {
		t.Fatalf("cx-spark argv=%q", got)
	}
	commit := profiles["commit"]
	if commit.TimeoutSeconds != 900 || commit.CLI.Command.Model != "gpt-5.3-codex-spark" || commit.CLI.Effort != "xhigh" {
		t.Fatalf("commit=%#v", commit)
	}
	commitRequest, err := Prepare(commit, "plan", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(commitRequest.CLI.Argv, " "); got != "codex -c sandbox_mode=read-only -c approval_policy=never -c model_reasoning_effort=xhigh --model gpt-5.3-codex-spark exec" {
		t.Fatalf("commit argv=%q", got)
	}
	commitOverride, err := Prepare(commit, "plan", map[string]any{"sandbox_mode": "danger-full-access"})
	if err != nil {
		t.Fatalf("commit typed override: %v", err)
	}
	if got := strings.Join(commitOverride.CLI.Argv, " "); !strings.Contains(got, "-c sandbox_mode=danger-full-access") {
		t.Fatalf("commit override argv=%q", got)
	}
	mcx := profiles["mcx"]
	if mcx.TimeoutSeconds != 300 || mcx.CLI.Command.Model != "gpt-5.6-sol" || mcx.CLI.Effort != "high" || len(mcx.CLI.Command.Args) != 23 {
		t.Fatalf("mcx=%#v", mcx)
	}
	mcc := profiles["mcc"]
	if mcc.TimeoutSeconds != 600 || mcc.CLI.Command.Model != "opus" || len(mcc.CLI.Command.Args) != 12 {
		t.Fatalf("mcc=%#v", mcc)
	}
	wantMCCEnv := map[string]string{
		"API_TIMEOUT_MS":                           "300000",
		"BASH_DEFAULT_TIMEOUT_MS":                  "600000",
		"BASH_MAX_TIMEOUT_MS":                      "3600000",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"MCP_TIMEOUT_MS":                           "120000",
		"MCP_TOOL_TIMEOUT_MS":                      "600000",
	}
	if len(mcc.CLI.Command.Env) != len(wantMCCEnv) {
		t.Fatalf("mcc env=%#v", mcc.CLI.Command.Env)
	}
	for name, value := range wantMCCEnv {
		if mcc.CLI.Command.Env[name] != value {
			t.Fatalf("mcc env %s=%q, want %q", name, mcc.CLI.Command.Env[name], value)
		}
	}
	glm := profiles["cc-glm"]
	if glm.TimeoutSeconds != 600 || glm.CLI.Command.Model != "" || glm.CLI.Effort != "" || len(glm.CLI.Command.Args) != 3 || strings.Join(glm.CLI.Command.EnvUnset, ",") != "ANTHROPIC_AUTH_TOKEN" {
		t.Fatalf("cc-glm=%#v", glm)
	}
	wantGLMEnv := map[string]string{
		"ANTHROPIC_API_KEY":                        "${OPENROUTER_API_KEY}",
		"ANTHROPIC_BASE_URL":                       "https://openrouter.ai/api",
		"ANTHROPIC_DEFAULT_HAIKU_MODEL":            "z-ai/glm-4.7-flash",
		"ANTHROPIC_DEFAULT_OPUS_MODEL":             "z-ai/glm-5.1",
		"ANTHROPIC_DEFAULT_SONNET_MODEL":           "z-ai/glm-5",
		"ANTHROPIC_MODEL":                          "z-ai/glm-5.1",
		"API_TIMEOUT_MS":                           "300000",
		"BASH_DEFAULT_TIMEOUT_MS":                  "600000",
		"BASH_MAX_TIMEOUT_MS":                      "3600000",
		"CLAUDE_CODE_DISABLE_NONESSENTIAL_TRAFFIC": "1",
		"CLAUDE_CODE_EFFORT_LEVEL":                 "max",
		"CLAUDE_CODE_MAX_OUTPUT_TOKENS":            "128000",
		"CLAUDE_CODE_SUBAGENT_MODEL":               "z-ai/glm-5",
		"CLAUDE_CONFIG_DIR":                        "${HOME}/.claude.aip",
		"MAX_MCP_OUTPUT_TOKENS":                    "100000",
		"MAX_READ_FILE_TOKENS":                     "100000",
		"MCP_TIMEOUT_MS":                           "120000",
		"MCP_TOOL_TIMEOUT_MS":                      "600000",
	}
	if len(glm.CLI.Command.Env) != len(wantGLMEnv) {
		t.Fatalf("cc-glm env=%#v", glm.CLI.Command.Env)
	}
	for name, value := range wantGLMEnv {
		if glm.CLI.Command.Env[name] != value {
			t.Fatalf("cc-glm env %s=%q, want %q", name, glm.CLI.Command.Env[name], value)
		}
	}
	request, err := Prepare(glm, "hello", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(request.CLI.Argv, " "); got != "claude --dangerously-skip-permissions --disallowed-tools Bash(rm -rf:*),Bash(sudo:*),Bash(su:*),Bash(git push --force:*),Bash(kubectl delete:*),Bash(docker rm:*) -p" {
		t.Fatalf("cc-glm argv=%q", got)
	}
	environment, err := CommandEnvironment(glm.CLI.Command, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"ANTHROPIC_API_KEY=test-openrouter-key", "CLAUDE_CONFIG_DIR=/tmp/runtime-home/.claude.aip"} {
		if !contains(environment, value) {
			t.Fatalf("cc-glm environment missing %q", value)
		}
	}
	if contains(environment, "ANTHROPIC_AUTH_TOKEN=must-not-reach-cc-glm") {
		t.Fatal("cc-glm environment retained ANTHROPIC_AUTH_TOKEN")
	}
}

func TestLoadDirRejectsRemovedManagedArgs(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "cx-raw.json", `{"command":"codex","model":"gpt","managed_args":[]}`)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), `unknown field "managed_args"`) {
		t.Fatalf("err=%v", err)
	}
}

func TestCanonicalizeLegacyDocumentSplitsPresetsAndRemovesDefaults(t *testing.T) {
	dir := t.TempDir()
	path := filepath.Join(dir, "mcx.json")
	writeConfig(t, dir, "mcx.json", `{
  "type":"cli",
  "label":"Codex",
  "timeout_seconds":300,
  "cli":{
    "driver":"codex",
    "executor":"command",
    "command":{
      "binary":"codex",
      "args":["--dangerously-bypass-approvals-and-sandbox","-c","model_reasoning_effort=high"],
      "model":"gpt-base",
      "env":{}
    },
    "runtime":{
      "prompt_delivery":"stdin",
      "managed_args":["exec"],
      "result_contract":"required",
      "override_policy":{"allow":["model","reasoning_effort","sandbox_mode","approval_policy","service_tier","verbosity","images"],"locked":[]}
    }
  },
  "presets":{
    "cx-fast":{"overrides":{"model":"gpt-fast","reasoning_effort":"medium"}},
    "cx-env":{"cli":{"command":{"env":{"CODEX_HOME":"/tmp/codex"}}}}
  }
}`)
	raw, err := readObject(path)
	if err != nil {
		t.Fatal(err)
	}
	profiles, err := CanonicalizeLegacyDocument("mcx", raw, path)
	if err != nil {
		t.Fatal(err)
	}
	if len(profiles) != 3 {
		t.Fatalf("profiles=%v", profiles)
	}
	assertCanonicalJSON(t, profiles["mcx"], `{"args":["--dangerously-bypass-approvals-and-sandbox"],"command":"codex","effort":"high","model":"gpt-base","timeout_seconds":300}`)
	assertCanonicalJSON(t, profiles["cx-fast"], `{"args":["--dangerously-bypass-approvals-and-sandbox"],"command":"codex","effort":"medium","model":"gpt-fast","timeout_seconds":300}`)
	assertCanonicalJSON(t, profiles["cx-env"], `{"args":["--dangerously-bypass-approvals-and-sandbox"],"command":"codex","effort":"high","env":{"CODEX_HOME":"/tmp/codex"},"model":"gpt-base","timeout_seconds":300}`)
}

func TestCanonicalizeLegacyDocumentRejectsNonDefaultManagedArgs(t *testing.T) {
	raw := map[string]any{
		"type": "cli",
		"cli": map[string]any{
			"command":      "codex",
			"model":        "",
			"managed_args": []any{"exec", "--custom"},
		},
	}
	_, err := CanonicalizeLegacyDocument("custom", raw, "memory")
	if err == nil || !strings.Contains(err.Error(), "无法自动迁移非默认 cli.managed_args") {
		t.Fatalf("err=%v", err)
	}
}

func TestCanonicalizeLegacyDocumentMigratesTransitionalFlatCLI(t *testing.T) {
	raw := map[string]any{
		"type":            "cli",
		"command":         "codex",
		"model":           "gpt-test",
		"managed_args":    []any{"exec"},
		"override_policy": map[string]any{"allow": []any{"model"}},
		"env_passthrough": []any{"OPENAI_API_KEY"},
		"env_unset":       []any{"OPENAI_ORG_ID"},
	}
	profiles, err := CanonicalizeLegacyDocument("cx", raw, "memory")
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalJSON(t, profiles["cx"], `{"command":"codex","env":{"OPENAI_API_KEY":"${OPENAI_API_KEY}","OPENAI_ORG_ID":null},"model":"gpt-test"}`)
}

func TestCanonicalizeLegacyDocumentRejectsRemovedAdvancedFields(t *testing.T) {
	tests := map[string]map[string]any{
		"native": {
			"type":   "native",
			"native": map[string]any{"model_profile": "api"},
		},
		"depends": {
			"type":    "cli",
			"cli":     map[string]any{"command": "codex"},
			"depends": []any{map[string]any{"command": "helper"}},
		},
		"api-runtime": {
			"type": "api",
			"api": map[string]any{
				"protocol": "openai", "base_url": "https://example.test", "model": "m", "api_key": "${K}",
				"runtime": map[string]any{"enabled": true},
			},
		},
		"api-stream": {
			"type": "api",
			"api": map[string]any{
				"protocol": "openai", "base_url": "https://example.test", "model": "m", "api_key": "${K}",
				"stream": true,
			},
		},
	}
	for name, raw := range tests {
		t.Run(name, func(t *testing.T) {
			if _, err := CanonicalizeLegacyDocument(name, raw, "memory"); err == nil {
				t.Fatal("CanonicalizeLegacyDocument returned nil error")
			}
		})
	}
}

func TestCanonicalizeLegacyDocumentPreservesAPIHeaders(t *testing.T) {
	raw := map[string]any{
		"type": "api",
		"api": map[string]any{
			"protocol": "openai", "base_url": "https://example.test", "model": "m", "api_key": "${K}",
			"headers": map[string]any{"X-Client": "${CLIENT_ID}"},
		},
	}
	profiles, err := CanonicalizeLegacyDocument("api", raw, "memory")
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalJSON(t, profiles["api"], `{"api_key":"${K}","base_url":"https://example.test","headers":{"X-Client":"${CLIENT_ID}"},"model":"m","protocol":"openai"}`)
}

func TestCanonicalizeLegacyDocumentRejectsRemovedTmuxProfileFields(t *testing.T) {
	raw := map[string]any{
		"type": "cli",
		"cli": map[string]any{
			"driver":          "generic",
			"executor":        "tmux",
			"binary":          "custom-agent",
			"model":           "",
			"prompt_delivery": "paste",
			"override_policy": map[string]any{"allow": []any{"model"}},
			"tmux": map[string]any{
				"session_name":                     "custom",
				"ready_timeout_seconds":            12,
				"prompt_ready_settle_seconds":      0.4,
				"prompt_ready_settle_fast_seconds": 0.1,
				"prompt_stable_timeout_seconds":    3,
			},
		},
	}
	_, err := CanonicalizeLegacyDocument("custom", raw, "memory")
	if err == nil || !strings.Contains(err.Error(), "cli.executor=tmux 已移出 profile 配置") {
		t.Fatalf("err=%v", err)
	}
}

func TestCanonicalizeLegacyDocumentRemovesAPIAuth(t *testing.T) {
	raw := map[string]any{
		"type": "api",
		"api": map[string]any{
			"protocol": "anthropic",
			"base_url": "https://openrouter.ai/api/v1",
			"model":    "model",
			"api_key":  "${OPENROUTER_API_KEY}",
			"auth":     map[string]any{"header": "Authorization", "prefix": "Bearer "},
		},
	}
	profiles, err := CanonicalizeLegacyDocument("api-cc", raw, "memory")
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalJSON(t, profiles["api-cc"], `{"api_key":"${OPENROUTER_API_KEY}","base_url":"https://openrouter.ai/api/v1","model":"model","protocol":"anthropic"}`)
}

func TestCanonicalizeLegacyDocumentMergesEnvironmentControls(t *testing.T) {
	raw := map[string]any{
		"type": "cli",
		"cli": map[string]any{
			"command":         "claude",
			"env":             map[string]any{"CLAUDE_CONFIG_DIR": "${HOME}/.claude-aip"},
			"env_passthrough": []any{"ANTHROPIC_API_KEY"},
			"env_unset":       []any{"ANTHROPIC_AUTH_TOKEN"},
		},
	}
	profiles, err := CanonicalizeLegacyDocument("cc", raw, "memory")
	if err != nil {
		t.Fatal(err)
	}
	assertCanonicalJSON(t, profiles["cc"], `{"command":"claude","env":{"ANTHROPIC_API_KEY":"${ANTHROPIC_API_KEY}","ANTHROPIC_AUTH_TOKEN":null,"CLAUDE_CONFIG_DIR":"${HOME}/.claude-aip"}}`)
}

func TestReservedCommandsReflectCurrentCLI(t *testing.T) {
	for _, command := range []string{"run", "session", "profile", "system", "loop", "skill", "tool", "memory"} {
		if _, ok := ReservedCommands[command]; !ok {
			t.Fatalf("%s must be reserved", command)
		}
	}
	for _, removed := range []string{"task", "turn", "runs", "history", "config", "profiles", "providers", "upgrade", "clean", "capabilities", "tools"} {
		if _, ok := ReservedCommands[removed]; ok {
			t.Fatalf("removed command %q is still reserved", removed)
		}
	}
}

func TestLoadDirRejectsUnknownKeysAtEveryLevel(t *testing.T) {
	tests := map[string]string{
		"root":            `{"protocol":"openai","base_url":"https://example.test","model":"m","api_key":"${K}","typo":true}`,
		"api":             `{"protocol":"openai","base_url":"https://example.test","model":"m","api_key":"${K}","modle":"typo"}`,
		"api-auth":        `{"protocol":"openai","base_url":"https://example.test","model":"m","api_key":"${K}","auth":{"header":"Authorization"}}`,
		"old-api-key-env": `{"protocol":"openai","base_url":"https://example.test","model":"m","api_key_env":"K"}`,
		"legacy-alias":    `{"protocol":"openai","base_url":"https://example.test","model":"m","api_key":"${K}","aliases":["shortcut"]}`,
		"legacy-label":    `{"type":"cli","label":"x","cli":{"command":"x","model":""}}`,
		"legacy-command":  `{"type":"cli","cli":{"driver":"generic","command":{"binary":"x","args":[],"model":""}}}`,
		"legacy-runtime":  `{"type":"cli","cli":{"driver":"generic","model":"","binary":"x","runtime":{"managed_args":[]}}}`,
		"legacy-presets":  `{"type":"cli","cli":{"driver":"generic","model":"","binary":"x"},"presets":{"child":{}}}`,
		"legacy-driver":   `{"type":"cli","cli":{"driver":"codex","model":"m"}}`,
		"legacy-binary":   `{"type":"cli","cli":{"binary":"x","model":""}}`,
		"legacy-managed":  `{"type":"cli","cli":{"command":"codex","model":"m","managed_args":["exec"]}}`,
		"cli":             `{"command":"x","unknown":1}`,
	}
	for name, body := range tests {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			writeConfig(t, dir, "bad.json", body)
			_, err := LoadDir(dir)
			if err == nil {
				t.Fatal("LoadDir returned nil error")
			}
			if !strings.Contains(err.Error(), "unknown field") {
				t.Fatalf("err=%v", err)
			}
		})
	}
}

func TestLoadDirInfersAPIAndRejectsRemovedProfileFamilies(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "api.json", `{"protocol":"openai","base_url":"https://example.test/v1","model":"gpt-test","api_key":"${TEST_API_KEY}"}`)
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	if profiles["api"].Transport() != "api" {
		t.Fatalf("api transport=%q", profiles["api"].Transport())
	}

	removed := map[string]string{
		"tmux":   `{"command":"claude","executor":"tmux","tmux":{"session_name":"agent"}}`,
		"native": `{"type":"native","native":{"mock":{"responses":["ok"]}}}`,
	}
	for name, body := range removed {
		t.Run(name, func(t *testing.T) {
			removedDir := t.TempDir()
			writeConfig(t, removedDir, "bad.json", body)
			if _, loadErr := LoadDir(removedDir); loadErr == nil {
				t.Fatal("LoadDir returned nil error")
			}
		})
	}
}

func TestLoadDirAcceptsAPIHeadersWithEnvironmentReferences(t *testing.T) {
	t.Setenv("SN_TEST_HEADER", "resolved")
	dir := t.TempDir()
	writeConfig(t, dir, "api.json", `{
		"protocol":"openai",
		"base_url":"https://example.test",
		"model":"m",
		"api_key":"${K}",
		"headers":{"HTTP-Referer":"https://client.example","X-Test":"${SN_TEST_HEADER}"}
	}`)
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatal(err)
	}
	headers, err := resolveAPIHeaders(profiles["api"].API.Headers)
	if err != nil {
		t.Fatal(err)
	}
	want := map[string]string{"HTTP-Referer": "https://client.example", "X-Test": "resolved"}
	if !reflect.DeepEqual(headers, want) {
		t.Fatalf("headers=%#v want=%#v", headers, want)
	}
}

func TestLoadDirRejectsRemovedAPIAdvancedFields(t *testing.T) {
	tests := map[string]string{
		"stream":  `{"protocol":"openai","base_url":"https://example.test","model":"m","api_key":"${K}","stream":true}`,
		"mock":    `{"protocol":"openai","base_url":"https://example.test","model":"m","api_key":"${K}","mock":true}`,
		"runtime": `{"protocol":"openai","base_url":"https://example.test","model":"m","api_key":"${K}","runtime":{"enabled":true}}`,
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
		"secret":             `{"protocol":"openai","base_url":"x","model":"m","api_key":"secret"}`,
		"plain-api-key-name": `{"protocol":"openai","base_url":"x","model":"m","api_key":"sample-api-key"}`,
		"dollar-api-key":     `{"protocol":"openai","base_url":"x","model":"m","api_key":"$ANTHROPIC_API_KEY"}`,
		"business":           `{"command":"x","purpose":"business"}`,
		"modelArg":           `{"command":"codex","args":["--model","x"]}`,
		"invalidEnvName":     `{"command":"x","env":{"BAD=NAME":"value"}}`,
		"invalidEnvValue":    `{"command":"x","env":{"TOKEN":1}}`,
		"invalidHeaderName":  `{"protocol":"openai","base_url":"x","model":"m","api_key":"${K}","headers":{"Bad Header":"x"}}`,
		"invalidHeaderValue": `{"protocol":"openai","base_url":"x","model":"m","api_key":"${K}","headers":{"X-Test":1}}`,
		"authHeader":         `{"protocol":"openai","base_url":"x","model":"m","api_key":"${K}","headers":{"Authorization":"secret"}}`,
		"nullArgs":           `{"command":"x","args":null}`,
		"nullEnv":            `{"command":"x","env":null}`,
		"emptyModel":         `{"command":"x","model":""}`,
		"invalidEffort":      `{"command":"codex","model":"m","effort":"extreme"}`,
		"genericEffort":      `{"command":"custom-agent","model":"m","effort":"high"}`,
		"mixedFamilies":      `{"command":"codex","protocol":"openai","base_url":"x","model":"m","api_key":"${K}"}`,
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
		writeConfig(t, dir, "run.json", `{"command":"x"}`)
		if _, err := LoadDir(dir); err == nil || !strings.Contains(err.Error(), "内置命令冲突") {
			t.Fatalf("err=%v", err)
		}
	})
}

func TestLoadDirRejectsRemovedDaemonExecution(t *testing.T) {
	tests := map[string]string{
		"depends":   `{"command":"x","depends":[{"command":"helper --serve"}]}`,
		"execution": `{"command":"x","execution":{"audit_proxy":true}}`,
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

func assertCanonicalJSON(t *testing.T, value map[string]any, expected string) {
	t.Helper()
	data, err := json.Marshal(value)
	if err != nil {
		t.Fatal(err)
	}
	if string(data) != expected {
		t.Fatalf("canonical=%s\nwant=%s", data, expected)
	}
}
