package provider

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net"
	"net/url"
	"os"
	"path/filepath"
	"strings"

	"github.com/yy003x/runtime/internal/llm"
)

const (
	TypeCLI    = "cli"
	TypeAPI    = "api"
	TypeNative = "native"

	ExecutorCommand = "command"
	ExecutorTmux    = "tmux"
)

var ReservedCommands = map[string]struct{}{
	"run": {}, "session": {}, "profile": {}, "system": {}, "loop": {},
	"skill": {}, "tool": {}, "memory": {}, "help": {}, "version": {},
	"llm": {},
}

type Config struct {
	ID             string          `json:"id"`
	Type           string          `json:"type"`
	TimeoutSeconds int             `json:"timeout_seconds"`
	Context        ContextPolicy   `json:"context"`
	CLI            *CLIConfig      `json:"cli,omitempty"`
	API            *APIConfig      `json:"api,omitempty"`
	Native         *NativeConfig   `json:"native,omitempty"`
	Depends        []Dependency    `json:"depends,omitempty"`
	Execution      ExecutionConfig `json:"execution,omitempty"`
	Raw            map[string]any  `json:"-"`
}

type Dependency struct {
	Command  string `json:"command"`
	WaitTCP  string `json:"wait_tcp,omitempty"`
	WaitHTTP string `json:"wait_http,omitempty"`
	Restart  bool   `json:"restart,omitempty"`
	Silent   bool   `json:"silent,omitempty"`
	Optional bool   `json:"optional,omitempty"`
}

type ContextPolicy struct {
	ContextWindowTokens  int  `json:"context_window_tokens,omitempty"`
	ReservedOutputTokens int  `json:"reserved_output_tokens,omitempty"`
	KeepRecentTurns      int  `json:"keep_recent_turns,omitempty"`
	SummaryEnabled       bool `json:"summary_enabled"`
}

type ContextCapacity struct {
	ContextWindowTokens  int    `json:"context_window_tokens"`
	ReservedOutputTokens int    `json:"reserved_output_tokens"`
	InputBudgetTokens    int    `json:"input_budget_tokens"`
	CompactionAtTokens   int    `json:"compaction_at_tokens"`
	KeepRecentTurns      int    `json:"keep_recent_turns"`
	SummaryEnabled       bool   `json:"summary_enabled"`
	CapacitySource       string `json:"capacity_source"`
}

const (
	conservativeContextWindowTokens = 32768
	defaultReservedOutputTokens     = 8192
	defaultKeepRecentTurns          = 6
)

type ExecutionConfig struct {
	AuditProxy bool     `json:"audit_proxy,omitempty"`
	Upstreams  []string `json:"upstreams,omitempty"`
	Bypass     []string `json:"bypass,omitempty"`
	Shim       bool     `json:"shim,omitempty"`
	Dylib      string   `json:"dylib,omitempty"`
}

type NativeConfig struct {
	ModelProfile      string            `json:"model_profile,omitempty"`
	Persona           string            `json:"persona,omitempty"`
	SystemPrompt      string            `json:"system_prompt,omitempty"`
	MaxRounds         int               `json:"max_rounds,omitempty"`
	TokenBudget       int               `json:"token_budget,omitempty"`
	LLMTimeoutSeconds float64           `json:"llm_timeout_seconds,omitempty"`
	Mock              *NativeMockConfig `json:"mock,omitempty"`
}

type NativeMockConfig struct {
	Responses           []string `json:"responses,omitempty"`
	LatencyMilliseconds int      `json:"latency_milliseconds,omitempty"`
	DoneAfter           int      `json:"done_after,omitempty"`
}

type CLIConfig struct {
	Driver   string        `json:"driver"`
	Executor string        `json:"executor"`
	Effort   string        `json:"effort,omitempty"`
	Command  CommandConfig `json:"command"`
	Runtime  CLIRuntime    `json:"runtime"`
	Tmux     *TmuxConfig   `json:"tmux,omitempty"`
}

type TmuxConfig struct {
	SessionName                string  `json:"session_name"`
	PasteBracketed             bool    `json:"paste_bracketed,omitempty"`
	PollIntervalSeconds        float64 `json:"poll_interval_seconds,omitempty"`
	PromptStableTimeoutSeconds float64 `json:"prompt_stable_timeout_seconds,omitempty"`
	SessionReadyTimeoutSeconds float64 `json:"session_ready_timeout_seconds,omitempty"`
	SessionReadySettleSeconds  float64 `json:"session_ready_settle_seconds,omitempty"`
	RestartMaxAttempts         int     `json:"restart_max_attempts,omitempty"`
	RestartDelaySeconds        float64 `json:"restart_delay_seconds,omitempty"`
}

type CommandConfig struct {
	Binary         string            `json:"binary"`
	Args           []string          `json:"args"`
	Model          string            `json:"model"`
	Env            map[string]string `json:"env,omitempty"`
	EnvPassthrough []string          `json:"env_passthrough,omitempty"`
	EnvUnset       []string          `json:"env_unset,omitempty"`
}

type CLIRuntime struct {
	PromptDelivery string         `json:"prompt_delivery"`
	PromptArgs     []string       `json:"prompt_args,omitempty"`
	OverridePolicy OverridePolicy `json:"override_policy,omitempty"`
}

type APIConfig struct {
	Protocol       string            `json:"protocol"`
	BaseURL        string            `json:"base_url"`
	Model          string            `json:"model"`
	APIKey         string            `json:"api_key"`
	Headers        map[string]string `json:"headers,omitempty"`
	MaxTokens      int               `json:"max_tokens,omitempty"`
	Stream         bool              `json:"stream,omitempty"`
	Mock           bool              `json:"mock,omitempty"`
	OverridePolicy OverridePolicy    `json:"override_policy,omitempty"`
	Runtime        *APIRuntimeConfig `json:"runtime,omitempty"`
}

// APIRuntimeConfig turns a direct API profile into an in-process Agent
// runtime while preserving the existing API request mode when it is absent.
type APIRuntimeConfig struct {
	Enabled           bool              `json:"enabled,omitempty"`
	SystemPrompt      string            `json:"system_prompt,omitempty"`
	MaxRounds         int               `json:"max_rounds,omitempty"`
	TokenBudget       int               `json:"token_budget,omitempty"`
	LLMTimeoutSeconds float64           `json:"llm_timeout_seconds,omitempty"`
	AutoRouteSkills   bool              `json:"auto_route_skills,omitempty"`
	Skills            []string          `json:"skills,omitempty"`
	Memory            *APIMemoryConfig  `json:"memory,omitempty"`
	MCPServers        []MCPServerConfig `json:"mcp_servers,omitempty"`
}

type APIMemoryConfig struct {
	Enabled bool   `json:"enabled,omitempty"`
	TopK    int    `json:"top_k,omitempty"`
	Type    string `json:"type,omitempty"`
}

type MCPServerConfig struct {
	Name           string            `json:"name"`
	Transport      string            `json:"transport,omitempty"`
	Command        string            `json:"command"`
	Args           []string          `json:"args,omitempty"`
	Env            map[string]string `json:"env,omitempty"`
	EnvPassthrough []string          `json:"env_passthrough,omitempty"`
	TimeoutSeconds float64           `json:"timeout_seconds,omitempty"`
}

type OverridePolicy struct {
	Allow  []string `json:"allow,omitempty"`
	Locked []string `json:"locked,omitempty"`
}

// profileDocument is the canonical user-facing profile schema. It is flat:
// command selects a CLI profile, while protocol/base_url/api_key select an API
// profile. Provider execution and carrier details stay internal.
type profileDocument struct {
	TimeoutSeconds       int                `json:"timeout_seconds,omitempty"`
	ContextWindowTokens  int                `json:"context_window_tokens,omitempty"`
	ReservedOutputTokens int                `json:"reserved_output_tokens,omitempty"`
	KeepRecentTurns      int                `json:"keep_recent_turns,omitempty"`
	SummaryEnabled       *bool              `json:"summary_enabled,omitempty"`
	Command              string             `json:"command,omitempty"`
	Model                *string            `json:"model,omitempty"`
	Effort               string             `json:"effort,omitempty"`
	Args                 []string           `json:"args,omitempty"`
	Env                  map[string]*string `json:"env,omitempty"`
	Protocol             string             `json:"protocol,omitempty"`
	BaseURL              string             `json:"base_url,omitempty"`
	APIKey               string             `json:"api_key,omitempty"`
	Headers              map[string]string  `json:"headers,omitempty"`
	MaxTokens            int                `json:"max_tokens,omitempty"`
}

func (c Config) Transport() string {
	if c.Type == TypeNative {
		return TypeNative
	}
	if c.Type == TypeAPI {
		return TypeAPI
	}
	if c.CLI != nil && c.CLI.Executor == ExecutorTmux {
		return ExecutorTmux
	}
	return TypeCLI
}

func (c Config) EffectiveContextCapacity() ContextCapacity {
	capacity, _ := c.ResolveContextCapacity(nil)
	return capacity
}

func (c Config) ResolveContextCapacity(overrides map[string]any) (ContextCapacity, error) {
	window := c.Context.ContextWindowTokens
	source := "profile"
	if window <= 0 {
		window = conservativeContextWindowTokens
		source = "conservative_default"
	}
	reserved := max(c.Context.ReservedOutputTokens, 0)
	if c.API != nil {
		reserved = max(reserved, c.API.MaxTokens)
	}
	if reserved <= 0 {
		reserved = defaultReservedOutputTokens
	}
	if value, exists := overrides["max_tokens"]; c.Type == TypeAPI && exists && value != nil {
		requested, valid := integerValue(value)
		if !valid || requested <= 0 {
			return ContextCapacity{}, fmt.Errorf("invalid_provider_override: max_tokens must be a positive integer")
		}
		reserved = max(reserved, requested)
	}
	inputBudget := window - reserved
	if inputBudget < 2 {
		return ContextCapacity{}, fmt.Errorf(
			"invalid_context_capacity: context_window_tokens=%d and reserved_output_tokens=%d leave input_budget_tokens=%d; require at least 2",
			window, reserved, inputBudget,
		)
	}
	keepRecent := c.Context.KeepRecentTurns
	if keepRecent <= 0 {
		keepRecent = defaultKeepRecentTurns
	}
	compactionAt := inputBudget * 70 / 100
	if compactionAt < 1 {
		compactionAt = 1
	}
	return ContextCapacity{
		ContextWindowTokens: window, ReservedOutputTokens: reserved,
		InputBudgetTokens: inputBudget, CompactionAtTokens: compactionAt,
		KeepRecentTurns: keepRecent, SummaryEnabled: c.Context.SummaryEnabled,
		CapacitySource: source,
	}, nil
}

func LoadDir(dir string) (map[string]Config, error) {
	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, fmt.Errorf("read provider config dir %s: %w", dir, err)
	}

	profiles := make(map[string]Config)
	sources := make(map[string]string)
	for _, entry := range entries {
		if filepath.Ext(entry.Name()) != ".json" {
			continue
		}
		path := filepath.Join(dir, entry.Name())
		if entry.IsDir() {
			return nil, fmt.Errorf("provider 配置必须是 regular file: %s", path)
		}
		if entry.Type()&os.ModeSymlink != 0 {
			return nil, fmt.Errorf("provider 配置不得是 symlink: %s", path)
		}
		info, err := entry.Info()
		if err != nil {
			return nil, fmt.Errorf("stat provider config %s: %w", path, err)
		}
		if !info.Mode().IsRegular() {
			return nil, fmt.Errorf("provider 配置必须是 regular file: %s", path)
		}
		if strings.HasSuffix(entry.Name(), ".local.json") {
			return nil, fmt.Errorf("%s: provider 配置不支持 .local.json 覆盖", path)
		}
		id := strings.TrimSuffix(entry.Name(), ".json")
		doc, err := readObject(path)
		if err != nil {
			return nil, err
		}
		cfg, err := normalize(id, doc, path)
		if err != nil {
			return nil, err
		}
		if previous, exists := sources[id]; exists {
			return nil, fmt.Errorf("duplicate provider id %q: %s and %s", id, previous, path)
		}
		if _, reserved := ReservedCommands[id]; reserved {
			return nil, fmt.Errorf("profile %q 的命令名与内置命令冲突", id)
		}
		profiles[id] = cfg
		sources[id] = path
	}
	if len(profiles) == 0 {
		return nil, fmt.Errorf("未加载到任何 provider JSON: %s", dir)
	}
	return profiles, nil
}

func Resolve(profiles map[string]Config, name string) (Config, bool) {
	cfg, ok := profiles[name]
	return cfg, ok
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

func normalize(id string, raw map[string]any, source string) (Config, error) {
	if err := rejectForbiddenKeys(raw, nil); err != nil {
		return Config{}, fmt.Errorf("%s: %w", source, err)
	}
	if err := validateFlatDocumentTypes(raw); err != nil {
		return Config{}, fmt.Errorf("%s: %w", source, err)
	}
	data, err := json.Marshal(raw)
	if err != nil {
		return Config{}, fmt.Errorf("%s: marshal normalized provider: %w", source, err)
	}
	var document profileDocument
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&document); err != nil {
		return Config{}, fmt.Errorf("%s: parse provider: %w", source, err)
	}
	summaryEnabled := true
	if document.SummaryEnabled != nil {
		summaryEnabled = *document.SummaryEnabled
	}
	cfg := Config{
		ID: id, TimeoutSeconds: document.TimeoutSeconds, Raw: cloneMap(raw),
		Context: ContextPolicy{
			ContextWindowTokens: document.ContextWindowTokens, ReservedOutputTokens: document.ReservedOutputTokens,
			KeepRecentTurns: document.KeepRecentTurns, SummaryEnabled: summaryEnabled,
		},
	}
	if cfg.TimeoutSeconds < 0 {
		return Config{}, fmt.Errorf("%s: timeout_seconds must be >= 0", source)
	}
	_, hasCommand := raw["command"]
	hasAPIIdentity := rawHasAny(raw, "protocol", "base_url", "api_key")
	hasAPIField := hasAPIIdentity || rawHasAny(raw, "headers", "max_tokens")
	if hasCommand && hasAPIField {
		return Config{}, fmt.Errorf("%s: command 与 protocol/base_url/api_key/headers/max_tokens 不能同时出现", source)
	}
	if hasCommand {
		cfg.Type = TypeCLI
		cli, cliErr := normalizeCLIDocument(document, source)
		if cliErr != nil {
			return Config{}, cliErr
		}
		cfg.CLI = &cli
		if err := validateCLI(&cfg, source); err != nil {
			return Config{}, err
		}
		if _, err := cfg.ResolveContextCapacity(nil); err != nil {
			return Config{}, fmt.Errorf("%s: %w", source, err)
		}
		return cfg, nil
	}
	if hasAPIIdentity {
		if rawHasAny(raw, "effort", "args", "env") {
			return Config{}, fmt.Errorf("%s: API profile 禁止出现 effort/args/env", source)
		}
		cfg.Type = TypeAPI
		cfg.API = &APIConfig{
			Protocol: strings.TrimSpace(document.Protocol), BaseURL: strings.TrimSpace(document.BaseURL),
			Model: stringValue(document.Model), APIKey: strings.TrimSpace(document.APIKey),
			Headers: cloneEnvironment(document.Headers), MaxTokens: document.MaxTokens,
		}
		if err := validateAPI(&cfg, source); err != nil {
			return Config{}, err
		}
		if _, err := cfg.ResolveContextCapacity(nil); err != nil {
			return Config{}, fmt.Errorf("%s: %w", source, err)
		}
		return cfg, nil
	}
	return Config{}, fmt.Errorf("%s: profile 必须提供 command（CLI）或 protocol/base_url/api_key（API）", source)
}

func validateFlatDocumentTypes(raw map[string]any) error {
	if value, exists := raw["timeout_seconds"]; exists && value == nil {
		return fmt.Errorf("timeout_seconds 必须是非负整数")
	}
	if value, exists := raw["max_tokens"]; exists {
		number, valid := integerValue(value)
		if !valid || number <= 0 {
			return fmt.Errorf("max_tokens 必须是正整数")
		}
	}
	for _, name := range []string{"context_window_tokens", "reserved_output_tokens", "keep_recent_turns"} {
		if value, exists := raw[name]; exists {
			number, valid := integerValue(value)
			if !valid || number <= 0 {
				return fmt.Errorf("%s 必须是正整数", name)
			}
		}
	}
	if value, exists := raw["summary_enabled"]; exists {
		if _, ok := value.(bool); !ok {
			return fmt.Errorf("summary_enabled 必须是 boolean")
		}
	}
	if value, exists := raw["command"]; exists {
		command, ok := value.(string)
		if !ok || strings.TrimSpace(command) == "" {
			return fmt.Errorf("command 必须是非空 string")
		}
	}
	if value, exists := raw["model"]; exists {
		model, ok := value.(string)
		if !ok || strings.TrimSpace(model) == "" {
			return fmt.Errorf("model 必须是非空 string；不固定模型时请省略该字段")
		}
	}
	if value, exists := raw["effort"]; exists {
		if effort, ok := value.(string); !ok || strings.TrimSpace(effort) == "" {
			return fmt.Errorf("effort 必须是非空 string")
		}
	}
	if value, exists := raw["args"]; exists {
		if _, ok := value.([]any); !ok {
			return fmt.Errorf("args 必须是 string array")
		}
	}
	if value, exists := raw["env"]; exists {
		if _, ok := value.(map[string]any); !ok {
			return fmt.Errorf("env 必须是 object")
		}
	}
	if value, exists := raw["headers"]; exists {
		headers, ok := value.(map[string]any)
		if !ok {
			return fmt.Errorf("headers 必须是 string object")
		}
		for name, rawValue := range headers {
			if !validHTTPHeaderName(name) {
				return fmt.Errorf("headers 包含非法 header 名称 %q", name)
			}
			if strings.EqualFold(name, "Authorization") || strings.EqualFold(name, "Proxy-Authorization") || strings.EqualFold(name, "x-api-key") {
				return fmt.Errorf("headers 不得覆盖认证 header %q；请使用 api_key", name)
			}
			value, ok := rawValue.(string)
			if !ok {
				return fmt.Errorf("headers.%s 必须是 string", name)
			}
			if strings.ContainsAny(value, "\r\n") {
				return fmt.Errorf("headers.%s 不得包含换行符", name)
			}
		}
	}
	for _, field := range []string{"protocol", "base_url", "api_key"} {
		if value, exists := raw[field]; exists {
			text, ok := value.(string)
			if !ok || strings.TrimSpace(text) == "" {
				return fmt.Errorf("%s 必须是非空 string", field)
			}
		}
	}
	return nil
}

func normalizeCLIDocument(document profileDocument, source string) (CLIConfig, error) {
	command := strings.TrimSpace(document.Command)
	if command == "" {
		return CLIConfig{}, fmt.Errorf("%s: command is required", source)
	}
	environment := make(map[string]string)
	var environmentUnset []string
	for name, value := range document.Env {
		if value == nil {
			environmentUnset = append(environmentUnset, name)
			continue
		}
		environment[name] = *value
	}
	sortStrings(environmentUnset)
	if len(environment) == 0 {
		environment = nil
	}
	driver := inferDriver(command)
	return CLIConfig{
		Driver: driver, Executor: ExecutorCommand, Effort: strings.TrimSpace(document.Effort),
		Command: CommandConfig{
			Binary: command, Args: append([]string(nil), document.Args...), Model: stringValue(document.Model),
			Env: environment, EnvUnset: environmentUnset,
		},
		Runtime: CLIRuntime{PromptDelivery: "stdin"},
	}, nil
}

func rawHasAny(raw map[string]any, names ...string) bool {
	for _, name := range names {
		if _, exists := raw[name]; exists {
			return true
		}
	}
	return false
}

func stringValue(value *string) string {
	if value == nil {
		return ""
	}
	return strings.TrimSpace(*value)
}

func defaultCLIManagedArgs(driver string) []string {
	switch driver {
	case "codex":
		return []string{"exec"}
	case "claude":
		return []string{"-p"}
	default:
		return nil
	}
}

func validateNative(cfg *Config, source string) error {
	if cfg.Native == nil {
		return fmt.Errorf("%s: 缺少 native object", source)
	}
	value := cfg.Native
	if value.Mock == nil && strings.TrimSpace(value.ModelProfile) == "" {
		return fmt.Errorf("%s: native.model_profile 或 native.mock 必须提供一个", source)
	}
	if value.MaxRounds < 0 || value.TokenBudget < 0 || value.LLMTimeoutSeconds < 0 {
		return fmt.Errorf("%s: native max_rounds/token_budget/llm_timeout_seconds 必须 >= 0", source)
	}
	return nil
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
		return fmt.Errorf("%s: command is required", source)
	}
	for i, arg := range cli.Command.Args {
		if arg == "--model" || arg == "-m" || strings.HasPrefix(arg, "--model=") {
			return fmt.Errorf("%s: model 必须放在顶层 model，不能放在 args[%d]", source, i)
		}
	}
	if err := validateCommandEnvironment(cli.Command, source); err != nil {
		return err
	}
	if cli.Driver == "" {
		cli.Driver = inferDriver(cli.Command.Binary)
	}
	if !contains([]string{"codex", "claude", "generic"}, cli.Driver) {
		return fmt.Errorf("%s: internal CLI adapter 必须是 codex|claude|generic", source)
	}
	if cli.Effort != "" {
		field := "reasoning_effort"
		if cli.Driver == "claude" {
			field = "effort"
		} else if cli.Driver == "generic" {
			return fmt.Errorf("%s: effort 仅支持 codex|claude", source)
		}
		if _, _, _, err := applyCLIOverrides(cli.Driver, nil, cli.Command.Model, map[string]any{field: cli.Effort}, nil); err != nil {
			return fmt.Errorf("%s: effort 无效: %w", source, err)
		}
	}
	for index, dependency := range cfg.Depends {
		if strings.TrimSpace(dependency.Command) == "" {
			return fmt.Errorf("%s: depends[%d].command is required", source, index)
		}
		if dependency.WaitTCP != "" && dependency.WaitHTTP != "" {
			return fmt.Errorf("%s: depends[%d] wait_tcp 与 wait_http 互斥", source, index)
		}
		if dependency.WaitTCP != "" {
			if _, _, err := net.SplitHostPort(dependency.WaitTCP); err != nil {
				return fmt.Errorf("%s: depends[%d].wait_tcp 无效: %w", source, index, err)
			}
		}
		if dependency.WaitHTTP != "" {
			parsed, err := url.Parse(dependency.WaitHTTP)
			if err != nil || parsed.Host == "" || (parsed.Scheme != "http" && parsed.Scheme != "https") {
				return fmt.Errorf("%s: depends[%d].wait_http 必须是 http/https URL", source, index)
			}
		}
	}
	if len(cfg.Execution.Upstreams) > 0 && !cfg.Execution.AuditProxy {
		return fmt.Errorf("%s: execution.upstreams requires audit_proxy=true", source)
	}
	for index, upstream := range cfg.Execution.Upstreams {
		if strings.TrimSpace(upstream) == "" {
			return fmt.Errorf("%s: execution.upstreams[%d] 不能为空", source, index)
		}
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
			return fmt.Errorf("%s: tmux 不支持 cli.prompt_args", source)
		}
		if strings.TrimSpace(cli.Tmux.SessionName) == "" {
			return fmt.Errorf("%s: cli.tmux.session_name is required", source)
		}
		if cli.Tmux.RestartMaxAttempts < 0 || cli.Tmux.RestartDelaySeconds < 0 {
			return fmt.Errorf("%s: tmux restart_max_attempts/restart_delay_seconds must be >= 0", source)
		}
	}
	return validateOverridePolicy(cli.Runtime.OverridePolicy, cliSupportedOverrides(cli.Driver), source)
}

func executionConfigured(config ExecutionConfig) bool {
	return config.AuditProxy || config.Shim || config.Dylib != "" || len(config.Upstreams) > 0 || len(config.Bypass) > 0
}

func validateAPI(cfg *Config, source string) error {
	if cfg.API == nil {
		return fmt.Errorf("%s: 缺少 api object", source)
	}
	api := cfg.API
	if !contains([]string{"openai", "anthropic"}, api.Protocol) {
		return fmt.Errorf("%s: protocol 必须是 openai|anthropic", source)
	}
	if strings.TrimSpace(api.BaseURL) == "" || strings.TrimSpace(api.Model) == "" || strings.TrimSpace(api.APIKey) == "" {
		return fmt.Errorf("%s: base_url、model、api_key 均为必填", source)
	}
	if api.MaxTokens < 0 {
		return fmt.Errorf("%s: max_tokens must be >= 0", source)
	}
	if _, ok := EnvironmentReferenceName(api.APIKey); !ok {
		return fmt.Errorf("%s: api_key 必须使用完整的 ${ENV_VAR} 环境变量占位符", source)
	}
	resource := "chat/completions"
	if api.Protocol == "anthropic" {
		resource = "messages"
	}
	if _, err := llm.ResolveCompatibleEndpoint(api.BaseURL, resource); err != nil {
		return fmt.Errorf("%s: %w", source, err)
	}
	if err := validateAPIRuntime(api, source); err != nil {
		return err
	}
	return validateOverridePolicy(api.OverridePolicy, apiSupportedOverrides(api.Protocol), source)
}

func validateAPIRuntime(api *APIConfig, source string) error {
	runtime := api.Runtime
	if runtime == nil {
		return nil
	}
	if runtime.MaxRounds < 0 || runtime.TokenBudget < 0 || runtime.LLMTimeoutSeconds < 0 {
		return fmt.Errorf("%s: api.runtime max_rounds/token_budget/llm_timeout_seconds 必须 >= 0", source)
	}
	if runtime.Memory != nil && runtime.Memory.TopK < 0 {
		return fmt.Errorf("%s: api.runtime.memory.top_k 必须 >= 0", source)
	}
	if runtime.Enabled && api.Stream {
		return fmt.Errorf("%s: api.runtime.enabled=true 暂不支持 api.stream=true", source)
	}
	seenSkills := make(map[string]struct{}, len(runtime.Skills))
	for index, name := range runtime.Skills {
		name = strings.TrimSpace(name)
		if name == "" {
			return fmt.Errorf("%s: api.runtime.skills[%d] 不能为空", source, index)
		}
		if _, exists := seenSkills[name]; exists {
			return fmt.Errorf("%s: api.runtime.skills 重复包含 %q", source, name)
		}
		seenSkills[name] = struct{}{}
	}
	seenServers := make(map[string]struct{}, len(runtime.MCPServers))
	if len(runtime.MCPServers) > 32 {
		return fmt.Errorf("%s: api.runtime.mcp_servers 最多支持 32 个 server", source)
	}
	for index := range runtime.MCPServers {
		server := &runtime.MCPServers[index]
		server.Name = strings.TrimSpace(server.Name)
		server.Command = strings.TrimSpace(server.Command)
		if server.Transport == "" {
			server.Transport = "stdio"
		}
		if !validRuntimeName(server.Name) {
			return fmt.Errorf("%s: api.runtime.mcp_servers[%d].name 必须只含字母、数字、_ 或 -", source, index)
		}
		if _, exists := seenServers[server.Name]; exists {
			return fmt.Errorf("%s: api.runtime.mcp_servers 重复包含 %q", source, server.Name)
		}
		seenServers[server.Name] = struct{}{}
		if server.Transport != "stdio" {
			return fmt.Errorf("%s: api.runtime.mcp_servers[%d].transport 目前仅支持 stdio", source, index)
		}
		if server.Command == "" {
			return fmt.Errorf("%s: api.runtime.mcp_servers[%d].command is required", source, index)
		}
		if server.TimeoutSeconds < 0 {
			return fmt.Errorf("%s: api.runtime.mcp_servers[%d].timeout_seconds 必须 >= 0", source, index)
		}
		for name := range server.Env {
			if !validEnvironmentName(name) {
				return fmt.Errorf("%s: api.runtime.mcp_servers[%d].env key %q 必须是环境变量名", source, index, name)
			}
		}
		seenEnv := make(map[string]struct{}, len(server.EnvPassthrough))
		for envIndex, name := range server.EnvPassthrough {
			if !validEnvironmentName(name) {
				return fmt.Errorf("%s: api.runtime.mcp_servers[%d].env_passthrough[%d] 必须是环境变量名", source, index, envIndex)
			}
			if _, exists := seenEnv[name]; exists {
				return fmt.Errorf("%s: api.runtime.mcp_servers[%d].env_passthrough 重复包含 %q", source, index, name)
			}
			seenEnv[name] = struct{}{}
		}
	}
	return nil
}

func validRuntimeName(value string) bool {
	if value == "" {
		return false
	}
	for _, character := range value {
		if character >= 'a' && character <= 'z' || character >= 'A' && character <= 'Z' || character >= '0' && character <= '9' || character == '_' || character == '-' {
			continue
		}
		return false
	}
	return true
}

func validateOverridePolicy(policy OverridePolicy, supported map[string]struct{}, source string) error {
	for _, name := range append(append([]string{}, policy.Allow...), policy.Locked...) {
		if _, ok := supported[name]; !ok {
			return fmt.Errorf("%s: override field %q is not supported", source, name)
		}
	}
	return nil
}

func validateCommandEnvironment(command CommandConfig, source string) error {
	for name := range command.Env {
		if !validEnvironmentName(name) {
			return fmt.Errorf("%s: env key %q 必须是环境变量名", source, name)
		}
	}
	passthrough := make(map[string]struct{}, len(command.EnvPassthrough))
	for index, name := range command.EnvPassthrough {
		if !validEnvironmentName(name) {
			return fmt.Errorf("%s: cli.env_passthrough[%d] 必须是环境变量名", source, index)
		}
		if _, exists := passthrough[name]; exists {
			return fmt.Errorf("%s: cli.env_passthrough 重复包含 %q", source, name)
		}
		passthrough[name] = struct{}{}
	}
	unset := make(map[string]struct{}, len(command.EnvUnset))
	for index, name := range command.EnvUnset {
		if !validEnvironmentName(name) {
			return fmt.Errorf("%s: cli.env_unset[%d] 必须是环境变量名", source, index)
		}
		if _, exists := unset[name]; exists {
			return fmt.Errorf("%s: cli.env_unset 重复包含 %q", source, name)
		}
		if _, exists := command.Env[name]; exists {
			return fmt.Errorf("%s: cli.env 与 env_unset 不能同时包含 %q", source, name)
		}
		if _, exists := passthrough[name]; exists {
			return fmt.Errorf("%s: cli.env_passthrough 与 env_unset 不能同时包含 %q", source, name)
		}
		unset[name] = struct{}{}
	}
	return nil
}

func validEnvironmentName(name string) bool {
	return strings.TrimSpace(name) != "" && !strings.ContainsAny(name, "=\x00")
}

func validHTTPHeaderName(name string) bool {
	if name == "" || strings.TrimSpace(name) != name {
		return false
	}
	for _, character := range name {
		if character >= 'a' && character <= 'z' ||
			character >= 'A' && character <= 'Z' ||
			character >= '0' && character <= '9' ||
			strings.ContainsRune("!#$%&'*+-.^_`|~", character) {
			continue
		}
		return false
	}
	return true
}

func cliSupportedOverrides(driver string) map[string]struct{} {
	if driver == "claude" {
		return set("model", "effort", "permission_mode", "append_system_prompt", "allowed_tools", "disallowed_tools")
	}
	if driver == "generic" {
		return set("model")
	}
	return set("model", "reasoning_effort", "sandbox_mode", "approval_policy", "service_tier", "verbosity", "images")
}

func apiSupportedOverrides(protocol string) map[string]struct{} {
	return set("model", "max_tokens", "temperature", "stream")
}

func rejectForbiddenKeys(value any, path []string) error {
	sensitive := set("api_key", "token", "secret", "cookie", "password", "private_key", "jwt", "webhook")
	business := set("project", "skill", "purpose", "output", "business_route", "prompt_template", "publish_target")
	switch typed := value.(type) {
	case map[string]any:
		for key, child := range typed {
			childPath := append(append([]string{}, path...), key)
			if _, forbidden := sensitive[strings.ToLower(key)]; forbidden && !isAPIKeyReferencePath(childPath) {
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

func isAPIKeyReferencePath(path []string) bool {
	return (len(path) == 1 && path[0] == "api_key") || (len(path) == 2 && path[0] == "api" && path[1] == "api_key")
}

func inferDriver(binary string) string {
	base := strings.TrimSuffix(strings.ToLower(filepath.Base(strings.TrimSpace(binary))), ".exe")
	if base == "claude" {
		return "claude"
	}
	if base == "codex" {
		return "codex"
	}
	return "generic"
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
