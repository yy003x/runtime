package provider

import (
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
	if len(profiles) != 8 {
		t.Fatalf("profiles=%d, want 8", len(profiles))
	}
	for _, id := range []string{"api-cc", "api-cx", "cc", "cc-bai", "commit", "cx", "cx-adv", "cx-image"} {
		if _, exists := profiles[id]; !exists {
			t.Fatalf("missing required repository profile %q", id)
		}
	}
	for _, id := range []string{"api-cc", "api-cx"} {
		api := profiles[id]
		if api.TimeoutSeconds != 300 || api.API == nil || api.API.MaxTokens != 16384 {
			t.Fatalf("%s=%#v", id, api)
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
	t.Setenv("KMM_API_KEY", "test-kmm-key")
	t.Setenv("ANTHROPIC_AUTH_TOKEN", "must-not-reach-cc-bai")
	t.Setenv("BAILIAN_API_KEY", "test-bailian-key")
	t.Setenv("WB_RUNTIME_IMAGE_PATH", "/tmp/image.png")

	cc := profiles["cc"]
	if cc.CLI.Command.Model != "" || cc.CLI.Effort != "" || len(cc.CLI.Command.Args) != 1 || cc.CLI.Command.Env["CLAUDE_CONFIG_DIR"] != "${HOME}/.claude.aip" {
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
	if bai.CLI.Command.Model != "glm-5.2" || bai.CLI.Effort != "high" || strings.Join(bai.CLI.Command.EnvUnset, ",") != "ANTHROPIC_AUTH_TOKEN" {
		t.Fatalf("cc-bai=%#v", bai)
	}
	baiEnvironment, err := CommandEnvironment(bai.CLI.Command, nil)
	if err != nil {
		t.Fatal(err)
	}
	for _, value := range []string{"ANTHROPIC_API_KEY=test-bailian-key", "ANTHROPIC_BASE_URL=https://dashscope.aliyuncs.com/apps/anthropic", "ANTHROPIC_MODEL=glm-5.2", "CLAUDE_CONFIG_DIR=/tmp/runtime-home/.claude.aip"} {
		if !contains(baiEnvironment, value) {
			t.Fatalf("cc-bai environment missing %q", value)
		}
	}
	if contains(baiEnvironment, "ANTHROPIC_AUTH_TOKEN=must-not-reach-cc-bai") {
		t.Fatal("cc-bai environment retained ANTHROPIC_AUTH_TOKEN")
	}

	for _, id := range []string{"cx", "cx-image"} {
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
	for _, id := range []string{"commit", "cx-adv"} {
		profile := profiles[id]
		if profile.CLI == nil || profile.CLI.Command.Env["CODEX_HOME"] != "${HOME}/.codex-ait" {
			t.Fatalf("%s CODEX_HOME=%#v", id, profile.CLI)
		}
	}

	cxProfile := profiles["cx"]
	if cxProfile.CLI.Command.Model != "gpt-5.6-sol" || cxProfile.CLI.Effort != "" || strings.Join(cxProfile.CLI.Command.Args, " ") != "--search" {
		t.Fatalf("cx=%#v", cxProfile)
	}
	cxRequest, err := Prepare(cxProfile, "review", nil)
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cxRequest.CLI.Argv, " "); got != `codex --search --model gpt-5.6-sol exec` {
		t.Fatalf("cx argv=%q", got)
	}
	cxDirect, err := PrepareInteractiveCLI(cxProfile, []string{"review"})
	if err != nil {
		t.Fatal(err)
	}
	if got := strings.Join(cxDirect.Argv, " "); got != `codex --search --model gpt-5.6-sol review` {
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

	advanced := profiles["cx-adv"]
	if advanced.CLI.Command.Model != "gpt-5.3-codex-spark" || len(advanced.CLI.Command.Args) != 8 {
		t.Fatalf("cx-adv=%#v", advanced)
	}
	commit := profiles["commit"]
	if commit.CLI.Command.Model != "gpt-5.3-codex-spark" || !contains(commit.CLI.Command.Args, "exec") {
		t.Fatalf("commit=%#v", commit)
	}
}

func TestLoadDirRejectsUnknownField(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "invalid.json", `{"command":"codex","unsupported":[]}`)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), `unknown field "unsupported"`) {
		t.Fatalf("err=%v", err)
	}
}

func TestReservedCommandsReflectCurrentCLI(t *testing.T) {
	expected := map[string]struct{}{
		"run": {}, "session": {}, "profile": {}, "system": {}, "loop": {},
		"skill": {}, "tool": {}, "memory": {}, "llm": {}, "help": {}, "version": {},
	}
	if !reflect.DeepEqual(ReservedCommands, expected) {
		t.Fatalf("ReservedCommands=%v want=%v", ReservedCommands, expected)
	}
}

func TestLoadDirRejectsUnknownKeysAtEveryLevel(t *testing.T) {
	tests := map[string]string{
		"api": `{"protocol":"openai","base_url":"https://example.test","model":"m","api_key":"${K}","unknown":true}`,
		"cli": `{"command":"x","unknown":true}`,
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

func TestLoadDirInfersAPIProfile(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "api.json", `{"protocol":"openai","base_url":"https://example.test/v1","model":"gpt-test","api_key":"${TEST_API_KEY}","max_tokens":16384}`)
	profiles, err := LoadDir(dir)
	if err != nil {
		t.Fatalf("LoadDir: %v", err)
	}
	api := profiles["api"]
	if api.Transport() != "api" || api.API.MaxTokens != 16384 {
		t.Fatalf("api=%#v", api)
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

func TestLoadDirRejectsUnsafeAndConflictingConfig(t *testing.T) {
	tests := map[string]string{
		"secret":             `{"protocol":"openai","base_url":"https://example.test/v1","model":"m","api_key":"secret"}`,
		"plain-api-key-name": `{"protocol":"openai","base_url":"https://example.test/v1","model":"m","api_key":"sample-api-key"}`,
		"dollar-api-key":     `{"protocol":"openai","base_url":"https://example.test/v1","model":"m","api_key":"$ANTHROPIC_API_KEY"}`,
		"business":           `{"command":"x","purpose":"business"}`,
		"modelArg":           `{"command":"codex","args":["--model","x"]}`,
		"invalidEnvName":     `{"command":"x","env":{"BAD=NAME":"value"}}`,
		"invalidEnvValue":    `{"command":"x","env":{"TOKEN":1}}`,
		"invalidHeaderName":  `{"protocol":"openai","base_url":"https://example.test/v1","model":"m","api_key":"${K}","headers":{"Bad Header":"x"}}`,
		"invalidHeaderValue": `{"protocol":"openai","base_url":"https://example.test/v1","model":"m","api_key":"${K}","headers":{"X-Test":1}}`,
		"authHeader":         `{"protocol":"openai","base_url":"https://example.test/v1","model":"m","api_key":"${K}","headers":{"Authorization":"secret"}}`,
		"zeroMaxTokens":      `{"protocol":"openai","base_url":"https://example.test/v1","model":"m","api_key":"${K}","max_tokens":0}`,
		"fractionMaxTokens":  `{"protocol":"openai","base_url":"https://example.test/v1","model":"m","api_key":"${K}","max_tokens":1.5}`,
		"nullArgs":           `{"command":"x","args":null}`,
		"nullEnv":            `{"command":"x","env":null}`,
		"emptyModel":         `{"command":"x","model":""}`,
		"invalidEffort":      `{"command":"codex","model":"m","effort":"extreme"}`,
		"genericEffort":      `{"command":"custom-agent","model":"m","effort":"high"}`,
		"mixedFamilies":      `{"command":"codex","protocol":"openai","base_url":"x","model":"m","api_key":"${K}"}`,
		"cliMaxTokens":       `{"command":"codex","max_tokens":100}`,
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

func TestLoadDirRejectsInvalidAPIBaseURL(t *testing.T) {
	dir := t.TempDir()
	writeConfig(t, dir, "bad.json", `{"protocol":"openai","base_url":"api.example.test/v1","model":"m","api_key":"${K}"}`)
	_, err := LoadDir(dir)
	if err == nil || !strings.Contains(err.Error(), "absolute http(s) URL is required") {
		t.Fatalf("err=%v", err)
	}
}

func writeConfig(t *testing.T, dir, name, body string) {
	t.Helper()
	if err := os.WriteFile(filepath.Join(dir, name), []byte(body), 0o644); err != nil {
		t.Fatal(err)
	}
}
