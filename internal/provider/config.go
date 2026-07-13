package provider

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

const (
	TypeCLI = "cli"
	TypeAPI = "api"

	ExecutorCommand = "command"
	ExecutorTmux    = "tmux"
)

var ReservedCommands = map[string]struct{}{
	"doctor": {}, "profiles": {}, "config": {}, "capabilities": {},
	"skills": {}, "tools": {}, "memory": {}, "task": {}, "turn": {},
	"loop": {}, "session": {}, "command": {}, "prune": {}, "run": {},
	"status": {}, "logs": {}, "watch": {}, "cancel": {}, "start": {},
	"step": {}, "send": {}, "interrupt": {}, "stop": {}, "attach": {},
	"choices": {}, "validate": {}, "help": {}, "version": {}, "list": {},
	"completion": {},
}

type Config struct {
	ID             string         `json:"id"`
	Type           string         `json:"type"`
	Label          string         `json:"label"`
	Aliases        []string       `json:"aliases,omitempty"`
	TimeoutSeconds int            `json:"timeout_seconds"`
	CLI            *CLIConfig     `json:"cli,omitempty"`
	API            *APIConfig     `json:"api,omitempty"`
	Raw            map[string]any `json:"-"`
}

type CLIConfig struct {
	Driver   string        `json:"driver"`
	Executor string        `json:"executor"`
	Command  CommandConfig `json:"command"`
	Runtime  CLIRuntime    `json:"runtime"`
	Tmux     *TmuxConfig   `json:"tmux,omitempty"`
}

type TmuxConfig struct {
	SessionName                  string   `json:"session_name"`
	TmuxInputMode                string   `json:"tmux_input_mode,omitempty"`
	PasteBracketed               bool     `json:"paste_bracketed,omitempty"`
	PollIntervalSeconds          float64  `json:"poll_interval_seconds,omitempty"`
	ReadyTimeoutSeconds          float64  `json:"ready_timeout_seconds,omitempty"`
	PromptIdleTimeoutSeconds     float64  `json:"prompt_idle_timeout_seconds,omitempty"`
	PromptReadySettleSeconds     float64  `json:"prompt_ready_settle_seconds,omitempty"`
	PromptReadySettleFastSeconds float64  `json:"prompt_ready_settle_fast_seconds,omitempty"`
	PromptStableTimeoutSeconds   float64  `json:"prompt_stable_timeout_seconds,omitempty"`
	SessionWaitReady             bool     `json:"session_wait_ready,omitempty"`
	SessionReadyTimeoutSeconds   float64  `json:"session_ready_timeout_seconds,omitempty"`
	SessionReadySettleSeconds    float64  `json:"session_ready_settle_seconds,omitempty"`
	SilenceThresholdSeconds      float64  `json:"silence_threshold_seconds,omitempty"`
	OutputRateWindowSeconds      float64  `json:"output_rate_window_seconds,omitempty"`
	TailBytes                    int      `json:"tail_bytes,omitempty"`
	AutoTrustCWD                 []string `json:"auto_trust_cwd,omitempty"`
}

type CommandConfig struct {
	Binary         string            `json:"binary"`
	Args           []string          `json:"args"`
	Model          string            `json:"model"`
	Env            map[string]string `json:"env,omitempty"`
	EnvPassthrough []string          `json:"env_passthrough,omitempty"`
}

type CLIRuntime struct {
	PromptDelivery string         `json:"prompt_delivery"`
	PromptArgs     []string       `json:"prompt_args,omitempty"`
	ResultContract string         `json:"result_contract"`
	OverridePolicy OverridePolicy `json:"override_policy,omitempty"`
}

type APIConfig struct {
	Protocol       string            `json:"protocol"`
	BaseURL        string            `json:"base_url"`
	Model          string            `json:"model"`
	APIKeyEnv      string            `json:"api_key_env"`
	Auth           AuthConfig        `json:"auth,omitempty"`
	Headers        map[string]string `json:"headers,omitempty"`
	Stream         bool              `json:"stream,omitempty"`
	Mock           bool              `json:"mock,omitempty"`
	ResultContract string            `json:"result_contract,omitempty"`
	OverridePolicy OverridePolicy    `json:"override_policy,omitempty"`
}

type AuthConfig struct {
	Header string `json:"header,omitempty"`
	Prefix string `json:"prefix,omitempty"`
}

type OverridePolicy struct {
	Allow  []string `json:"allow,omitempty"`
	Locked []string `json:"locked,omitempty"`
}

func (c Config) Transport() string {
	if c.Type == TypeAPI {
		return TypeAPI
	}
	if c.CLI != nil && c.CLI.Executor == ExecutorTmux {
		return ExecutorTmux
	}
	return TypeCLI
}

func (c Config) ResultContract() string {
	if c.Type == TypeAPI && c.API != nil {
		if c.API.ResultContract == "" {
			return "none"
		}
		return c.API.ResultContract
	}
	if c.CLI != nil {
		return c.CLI.Runtime.ResultContract
	}
	return ""
}

func LoadDir(dir string) (map[string]Config, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read provider config dir %s: %w", dir, err)
	}

	rawProfiles := make(map[string]map[string]any)
	sources := make(map[string]string)
	for _, entry := range entries {
		if entry.IsDir() || filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		if strings.HasSuffix(entry.Name(), ".local.json") {
			return nil, fmt.Errorf("%s: provider 配置不支持 .local.json 覆盖", filepath.Join(dir, entry.Name()))
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		path := filepath.Join(dir, entry.Name())
		doc, err := readObject(path)
		if err != nil {
			return nil, err
		}
		expanded, err := expandDocument(id, doc, path)
		if err != nil {
			return nil, err
		}
		for expandedID, raw := range expanded {
			if previous, exists := sources[expandedID]; exists {
				return nil, fmt.Errorf("duplicate provider id %q: %s and %s", expandedID, previous, path)
			}
			rawProfiles[expandedID] = raw
			sources[expandedID] = path
		}
	}
	if len(rawProfiles) == 0 {
		return nil, fmt.Errorf("未加载到任何 provider JSON: %s", dir)
	}

	profiles := make(map[string]Config, len(rawProfiles))
	owners := make(map[string]string)
	for id, raw := range rawProfiles {
		cfg, err := normalize(id, raw, sources[id])
		if err != nil {
			return nil, err
		}
		for _, name := range append([]string{id}, cfg.Aliases...) {
			if _, reserved := ReservedCommands[name]; reserved {
				return nil, fmt.Errorf("profile %q 的命令名 %q 与内置命令冲突", id, name)
			}
			if owner, exists := owners[name]; exists {
				return nil, fmt.Errorf("profile command %q is owned by both %q and %q", name, owner, id)
			}
			owners[name] = id
		}
		profiles[id] = cfg
	}
	return profiles, nil
}

func Resolve(profiles map[string]Config, name string) (Config, bool) {
	if cfg, ok := profiles[name]; ok {
		return cfg, true
	}
	ids := make([]string, 0, len(profiles))
	for id := range profiles {
		ids = append(ids, id)
	}
	sort.Strings(ids)
	for _, id := range ids {
		for _, alias := range profiles[id].Aliases {
			if alias == name {
				return profiles[id], true
			}
		}
	}
	return Config{}, false
}

func readObject(path string) (map[string]any, error) {
	data, err := os.ReadFile(path)
	if err != nil {
		return nil, fmt.Errorf("read %s: %w", path, err)
	}
	var raw map[string]any
	if err := json.Unmarshal(data, &raw); err != nil {
		return nil, fmt.Errorf("%s: JSON 解析失败: %w", path, err)
	}
	if raw == nil {
		return nil, fmt.Errorf("%s: provider 配置必须是 JSON object", path)
	}
	return raw, nil
}

func expandDocument(id string, input map[string]any, source string) (map[string]map[string]any, error) {
	base := cloneMap(input)
	presetsValue, hasPresets := base["presets"]
	delete(base, "presets")
	base["id"] = id
	resolved := map[string]map[string]any{id: base}
	if !hasPresets {
		return resolved, nil
	}
	presets, ok := presetsValue.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: presets 必须是 object", source)
	}
	resolving := make(map[string]bool)
	var resolve func(string) (map[string]any, error)
	resolve = func(presetID string) (map[string]any, error) {
		if value, ok := resolved[presetID]; ok {
			return cloneMap(value), nil
		}
		value, ok := presets[presetID]
		if !ok {
			return nil, fmt.Errorf("%s: preset %q 不存在", source, presetID)
		}
		overlay, ok := value.(map[string]any)
		if !ok {
			return nil, fmt.Errorf("%s: presets.%s 必须是 object", source, presetID)
		}
		if resolving[presetID] {
			return nil, fmt.Errorf("%s: preset extends 循环包含 %q", source, presetID)
		}
		resolving[presetID] = true
		childOverlay := cloneMap(overlay)
		parentID := id
		if value, ok := childOverlay["extends"]; ok && strings.TrimSpace(fmt.Sprint(value)) != "" {
			parentID = fmt.Sprint(value)
		}
		delete(childOverlay, "extends")
		typedOverrides, hasOverrides := childOverlay["overrides"]
		delete(childOverlay, "overrides")
		parent, err := resolve(parentID)
		if err != nil {
			return nil, err
		}
		child := deepMerge(parent, childOverlay)
		if _, overridesAliases := childOverlay["aliases"]; !overridesAliases {
			delete(child, "aliases")
		}
		if err := applyAppendFields(child, source, presetID); err != nil {
			return nil, err
		}
		if family(parent) != family(child) {
			return nil, fmt.Errorf("%s: presets.%s 不得改变 provider family", source, presetID)
		}
		if hasOverrides {
			overrides, ok := typedOverrides.(map[string]any)
			if !ok {
				return nil, fmt.Errorf("%s: presets.%s.overrides 必须是 object", source, presetID)
			}
			if err := applyTypedOverrides(child, overrides); err != nil {
				return nil, fmt.Errorf("%s: presets.%s.overrides 无效: %w", source, presetID, err)
			}
		}
		child["id"] = presetID
		resolved[presetID] = child
		delete(resolving, presetID)
		return cloneMap(child), nil
	}
	for presetID := range presets {
		if presetID == id {
			return nil, fmt.Errorf("%s: presets 不得覆盖基础 profile %q", source, id)
		}
		if _, err := resolve(presetID); err != nil {
			return nil, err
		}
	}
	return resolved, nil
}

func normalize(id string, raw map[string]any, source string) (Config, error) {
	if err := rejectForbiddenKeys(raw, nil); err != nil {
		return Config{}, fmt.Errorf("%s: %w", source, err)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return Config{}, fmt.Errorf("%s: marshal normalized provider: %w", source, err)
	}
	var cfg Config
	if err := json.Unmarshal(data, &cfg); err != nil {
		return Config{}, fmt.Errorf("%s: parse provider: %w", source, err)
	}
	cfg.ID = id
	cfg.Raw = cloneMap(raw)
	if cfg.Label == "" {
		cfg.Label = id
	}
	if cfg.TimeoutSeconds < 0 {
		return Config{}, fmt.Errorf("%s: timeout_seconds must be >= 0", source)
	}
	switch cfg.Type {
	case TypeCLI:
		if cfg.API != nil {
			return Config{}, fmt.Errorf("%s: type=cli 禁止出现 api object", source)
		}
		if err := validateCLI(&cfg, source); err != nil {
			return Config{}, err
		}
	case TypeAPI:
		if cfg.CLI != nil {
			return Config{}, fmt.Errorf("%s: type=api 禁止出现 cli object", source)
		}
		if err := validateAPI(&cfg, source); err != nil {
			return Config{}, err
		}
	default:
		return Config{}, fmt.Errorf("%s: type 必须是 api|cli", source)
	}
	return cfg, nil
}

func validateCLI(cfg *Config, source string) error {
	if cfg.CLI == nil {
		return fmt.Errorf("%s: 缺少 cli object", source)
	}
	cli := cfg.CLI
	if cli.Executor == "" {
		cli.Executor = ExecutorCommand
	}
	if cli.Executor != ExecutorCommand && cli.Executor != ExecutorTmux {
		return fmt.Errorf("%s: cli.executor 必须是 command|tmux", source)
	}
	if strings.TrimSpace(cli.Command.Binary) == "" {
		return fmt.Errorf("%s: cli.command.binary is required", source)
	}
	if cli.Command.Model == "" {
		if rawCLI, ok := cfg.Raw["cli"].(map[string]any); ok {
			if rawCommand, ok := rawCLI["command"].(map[string]any); ok {
				if _, exists := rawCommand["model"]; !exists {
					return fmt.Errorf("%s: cli.command.model 必须显式提供，可为空字符串", source)
				}
			}
		}
	}
	for i, arg := range cli.Command.Args {
		if arg == "--model" || arg == "-m" || strings.HasPrefix(arg, "--model=") {
			return fmt.Errorf("%s: model 必须放在 cli.command.model，不能放在 args[%d]", source, i)
		}
	}
	if cli.Driver == "" {
		cli.Driver = inferDriver(cli.Command.Binary)
	}
	if cli.Runtime.ResultContract == "" {
		cli.Runtime.ResultContract = "optional"
	}
	if err := validateResultContract(cli.Runtime.ResultContract, source); err != nil {
		return err
	}
	if cli.Executor == ExecutorCommand {
		if cli.Tmux != nil {
			return fmt.Errorf("%s: cli.executor=command 禁止出现 cli.tmux", source)
		}
		if cli.Runtime.PromptDelivery == "" {
			cli.Runtime.PromptDelivery = "stdin"
		}
		if !contains([]string{"stdin", "arg", "none"}, cli.Runtime.PromptDelivery) {
			return fmt.Errorf("%s: command prompt_delivery 仅支持 stdin|arg|none", source)
		}
	} else {
		if cli.Tmux == nil {
			return fmt.Errorf("%s: cli.executor=tmux 时必须提供 cli.tmux object", source)
		}
		if cli.Runtime.PromptDelivery == "" {
			cli.Runtime.PromptDelivery = "paste"
		}
		if !contains([]string{"paste", "none"}, cli.Runtime.PromptDelivery) {
			return fmt.Errorf("%s: tmux prompt_delivery 仅支持 paste|none", source)
		}
		if len(cli.Runtime.PromptArgs) > 0 {
			return fmt.Errorf("%s: tmux 不支持 cli.runtime.prompt_args", source)
		}
		if strings.TrimSpace(cli.Tmux.SessionName) == "" {
			return fmt.Errorf("%s: cli.tmux.session_name is required", source)
		}
	}
	return validateOverridePolicy(cli.Runtime.OverridePolicy, cliSupportedOverrides(cli.Driver), source)
}

func validateAPI(cfg *Config, source string) error {
	if cfg.API == nil {
		return fmt.Errorf("%s: 缺少 api object", source)
	}
	api := cfg.API
	if !contains([]string{"openai", "anthropic"}, api.Protocol) {
		return fmt.Errorf("%s: api.protocol 必须是 openai|anthropic", source)
	}
	if strings.TrimSpace(api.BaseURL) == "" || strings.TrimSpace(api.Model) == "" || strings.TrimSpace(api.APIKeyEnv) == "" {
		return fmt.Errorf("%s: api.base_url、api.model、api.api_key_env 均为必填", source)
	}
	if api.ResultContract == "" {
		api.ResultContract = "none"
	}
	if err := validateResultContract(api.ResultContract, source); err != nil {
		return err
	}
	return validateOverridePolicy(api.OverridePolicy, apiSupportedOverrides(api.Protocol), source)
}

func validateOverridePolicy(policy OverridePolicy, supported map[string]struct{}, source string) error {
	for _, name := range append(append([]string{}, policy.Allow...), policy.Locked...) {
		if _, ok := supported[name]; !ok {
			return fmt.Errorf("%s: override field %q is not supported", source, name)
		}
	}
	return nil
}

func validateResultContract(value, source string) error {
	if !contains([]string{"none", "optional", "required"}, value) {
		return fmt.Errorf("%s: 非法 result_contract %q", source, value)
	}
	return nil
}

func cliSupportedOverrides(driver string) map[string]struct{} {
	if driver == "claude" {
		return set("model", "effort", "permission_mode", "append_system_prompt", "allowed_tools", "disallowed_tools")
	}
	return set("model", "reasoning_effort", "sandbox_mode", "approval_policy", "service_tier", "verbosity", "images")
}

func apiSupportedOverrides(protocol string) map[string]struct{} {
	return set("model", "max_tokens", "temperature", "stream")
}

func applyTypedOverrides(raw map[string]any, overrides map[string]any) error {
	typeName := fmt.Sprint(raw["type"])
	if typeName == TypeAPI {
		api, ok := raw["api"].(map[string]any)
		if !ok {
			return fmt.Errorf("missing api object")
		}
		for key, value := range overrides {
			api[key] = cloneValue(value)
		}
		return nil
	}
	cli, ok := raw["cli"].(map[string]any)
	if !ok {
		return fmt.Errorf("missing cli object")
	}
	command, ok := cli["command"].(map[string]any)
	if !ok {
		return fmt.Errorf("missing cli.command object")
	}
	driver := strings.TrimSpace(fmt.Sprint(cli["driver"]))
	if driver == "" {
		driver = inferDriver(fmt.Sprint(command["binary"]))
	}
	args, err := stringSlice(command["args"])
	if err != nil {
		return fmt.Errorf("cli.command.args: %w", err)
	}
	model := fmt.Sprint(command["model"])
	resolvedArgs, resolvedModel, _, err := applyCLIOverrides(driver, args, model, overrides, nil)
	if err != nil {
		return err
	}
	command["args"] = stringsToAny(resolvedArgs)
	command["model"] = resolvedModel
	return nil
}

func applyAppendFields(value any, source, presetID string) error {
	object, ok := value.(map[string]any)
	if !ok {
		return nil
	}
	for key, child := range object {
		if strings.HasSuffix(key, "_append") {
			target := strings.TrimSuffix(key, "_append")
			if target != "args" && target != "env_passthrough" {
				return fmt.Errorf("%s: presets.%s 不支持 append 字段 %q", source, presetID, key)
			}
			addition, ok := child.([]any)
			if !ok {
				return fmt.Errorf("%s: presets.%s.%s 必须是数组", source, presetID, key)
			}
			current, _ := object[target].([]any)
			object[target] = append(append([]any{}, current...), addition...)
			delete(object, key)
			continue
		}
		if err := applyAppendFields(child, source, presetID); err != nil {
			return err
		}
	}
	return nil
}

func rejectForbiddenKeys(value any, path []string) error {
	sensitive := set("api_key", "token", "secret", "cookie", "password", "private_key", "jwt", "webhook")
	business := set("project", "skill", "purpose", "output", "business_route", "prompt_template", "publish_target")
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := append(append([]string{}, path...), key)
			if _, forbidden := sensitive[strings.ToLower(key)]; forbidden {
				return fmt.Errorf("禁止在 provider 配置内写入敏感字段 %s", strings.Join(childPath, "."))
			}
			if _, forbidden := business[strings.ToLower(key)]; forbidden {
				return fmt.Errorf("provider 配置不得包含业务字段 %s", strings.Join(childPath, "."))
			}
			if err := rejectForbiddenKeys(child, childPath); err != nil {
				return err
			}
		}
	case []any:
		for i, child := range typed {
			if err := rejectForbiddenKeys(child, append(path, fmt.Sprint(i))); err != nil {
				return err
			}
		}
	}
	return nil
}

func family(raw map[string]any) string {
	typeName := fmt.Sprint(raw["type"])
	if typeName == TypeAPI {
		api, _ := raw["api"].(map[string]any)
		return typeName + "/" + fmt.Sprint(api["protocol"])
	}
	cli, _ := raw["cli"].(map[string]any)
	return typeName + "/" + fmt.Sprint(cli["executor"]) + "/" + fmt.Sprint(cli["driver"])
}

func inferDriver(binary string) string {
	base := strings.ToLower(filepath.Base(binary))
	if strings.Contains(base, "claude") {
		return "claude"
	}
	if strings.Contains(base, "codex") {
		return "codex"
	}
	return "generic"
}

func deepMerge(base, overlay map[string]any) map[string]any {
	out := cloneMap(base)
	for key, value := range overlay {
		if child, ok := value.(map[string]any); ok {
			if current, ok := out[key].(map[string]any); ok {
				out[key] = deepMerge(current, child)
				continue
			}
		}
		out[key] = cloneValue(value)
	}
	return out
}

func cloneMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = cloneValue(value)
	}
	return out
}

func cloneValue(value any) any {
	switch typed := value.(type) {
	case map[string]any:
		return cloneMap(typed)
	case []any:
		out := make([]any, len(typed))
		for i := range typed {
			out[i] = cloneValue(typed[i])
		}
		return out
	default:
		return typed
	}
}

func contains(values []string, target string) bool {
	for _, value := range values {
		if value == target {
			return true
		}
	}
	return false
}

func set(values ...string) map[string]struct{} {
	out := make(map[string]struct{}, len(values))
	for _, value := range values {
		out[value] = struct{}{}
	}
	return out
}

func stringSlice(value any) ([]string, error) {
	if value == nil {
		return nil, nil
	}
	items, ok := value.([]any)
	if !ok {
		return nil, fmt.Errorf("must be an array")
	}
	out := make([]string, 0, len(items))
	for _, item := range items {
		out = append(out, fmt.Sprint(item))
	}
	return out, nil
}

func stringsToAny(values []string) []any {
	out := make([]any, len(values))
	for i, value := range values {
		out[i] = value
	}
	return out
}
