package provider

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
)

type PreparedRequest struct {
	CLI *CLIRequest
	API *APIRequest
}

type CLIRequest struct {
	ProfileID          string
	Driver             string
	Argv               []string
	Stdin              string
	RequestedOverrides map[string]any
	EffectiveOptions   map[string]any
}

type APIRequest struct {
	ProfileID          string
	Protocol           string
	Endpoint           string
	Payload            map[string]any
	Stream             bool
	RequestedOverrides map[string]any
	EffectiveOptions   map[string]any
}

func Prepare(cfg Config, prompt string, overrides map[string]any) (PreparedRequest, error) {
	requested := cloneStringMap(overrides)
	if cfg.Type == TypeCLI {
		request, err := prepareCLI(cfg, prompt, requested)
		if err != nil {
			return PreparedRequest{}, err
		}
		return PreparedRequest{CLI: &request}, nil
	}
	if cfg.Type == TypeAPI {
		request, err := prepareAPI(cfg, prompt, requested)
		if err != nil {
			return PreparedRequest{}, err
		}
		return PreparedRequest{API: &request}, nil
	}
	return PreparedRequest{}, fmt.Errorf("profile %s: unsupported provider type %q", cfg.ID, cfg.Type)
}

func prepareCLI(cfg Config, prompt string, overrides map[string]any) (CLIRequest, error) {
	if cfg.CLI == nil {
		return CLIRequest{}, fmt.Errorf("profile %s: missing cli config", cfg.ID)
	}
	cli := cfg.CLI
	driver := cli.Driver
	if driver == "" {
		driver = inferDriver(cli.Command.Binary)
	}
	allowed := allowedOverrides(cliSupportedOverrides(driver), cli.Runtime.OverridePolicy)
	args, model, effective, err := applyCLIOverrides(driver, cli.Command.Args, cli.Command.Model, overrides, allowed)
	if err != nil {
		return CLIRequest{}, fmt.Errorf("profile %s: %w", cfg.ID, err)
	}
	if model != "" {
		args = append(args, "--model", model)
	}
	argv := append([]string{expandHome(cli.Command.Binary)}, args...)
	stdin := ""
	switch cli.Runtime.PromptDelivery {
	case "stdin":
		stdin = prompt
	case "arg":
		argv = append(argv, expandPromptArgs(cli.Runtime.PromptArgs, prompt)...)
	case "none", "paste":
	default:
		return CLIRequest{}, fmt.Errorf("unknown prompt_delivery %q", cli.Runtime.PromptDelivery)
	}
	effective["driver"] = driver
	return CLIRequest{
		ProfileID:          cfg.ID,
		Driver:             driver,
		Argv:               argv,
		Stdin:              stdin,
		RequestedOverrides: cloneStringMap(overrides),
		EffectiveOptions:   effective,
	}, nil
}

func prepareAPI(cfg Config, prompt string, overrides map[string]any) (APIRequest, error) {
	if cfg.API == nil {
		return APIRequest{}, fmt.Errorf("profile %s: missing api config", cfg.ID)
	}
	api := cfg.API
	allowed := allowedOverrides(apiSupportedOverrides(api.Protocol), api.OverridePolicy)
	if err := rejectUnknown(overrides, allowed, "API protocol "+api.Protocol); err != nil {
		return APIRequest{}, err
	}
	effective := map[string]any{
		"model":  api.Model,
		"stream": api.Stream,
	}
	for key, value := range overrides {
		effective[key] = cloneValue(value)
	}
	if err := validateAPIOptions(api.Protocol, effective); err != nil {
		return APIRequest{}, err
	}
	model := strings.TrimSpace(fmt.Sprint(effective["model"]))
	if model == "" {
		return APIRequest{}, fmt.Errorf("profile %s: API model is required", cfg.ID)
	}
	payload := map[string]any{"model": model}
	endpoint := "/chat/completions"
	if api.Protocol == "anthropic" {
		endpoint = "/messages"
		payload["messages"] = []any{map[string]any{"role": "user", "content": prompt}}
		payload["max_tokens"] = intValue(effective["max_tokens"], 1024)
		copyOption(payload, effective, "temperature")
	} else {
		payload["messages"] = []any{map[string]any{"role": "user", "content": prompt}}
		copyOption(payload, effective, "max_tokens", "temperature")
	}
	stream := boolValue(effective["stream"])
	if stream {
		payload["stream"] = true
	}
	return APIRequest{
		ProfileID:          cfg.ID,
		Protocol:           api.Protocol,
		Endpoint:           endpoint,
		Payload:            payload,
		Stream:             stream,
		RequestedOverrides: cloneStringMap(overrides),
		EffectiveOptions:   effective,
	}, nil
}

func validateAPIOptions(protocol string, values map[string]any) error {
	if model, ok := values["model"]; ok {
		text, valid := model.(string)
		if !valid || strings.TrimSpace(text) == "" {
			return fmt.Errorf("%s override model must be a non-empty string", protocol)
		}
	}
	if value, ok := values["temperature"]; ok && value != nil {
		number, valid := numericValue(value)
		limit := 2.0
		if protocol == "anthropic" {
			limit = 1.0
		}
		if !valid || number < 0 || number > limit {
			return fmt.Errorf("%s override temperature must be between 0 and %g", protocol, limit)
		}
	}
	if value, ok := values["max_tokens"]; ok && value != nil {
		number, valid := integerValue(value)
		if !valid || number <= 0 {
			return fmt.Errorf("%s override max_tokens must be a positive integer", protocol)
		}
	}
	if value, ok := values["stream"]; ok {
		if _, valid := value.(bool); !valid {
			return fmt.Errorf("%s override stream must be boolean", protocol)
		}
	}
	return nil
}

func allowedOverrides(supported map[string]struct{}, policy OverridePolicy) map[string]struct{} {
	allowed := make(map[string]struct{})
	if len(policy.Allow) == 0 {
		for name := range supported {
			allowed[name] = struct{}{}
		}
	} else {
		for _, name := range policy.Allow {
			allowed[name] = struct{}{}
		}
	}
	for _, name := range policy.Locked {
		delete(allowed, name)
	}
	return allowed
}

func applyCLIOverrides(driver string, baseArgs []string, baseModel string, overrides map[string]any, allowed map[string]struct{}) ([]string, string, map[string]any, error) {
	supported := cliSupportedOverrides(driver)
	if allowed == nil {
		allowed = supported
	}
	if err := rejectUnknown(overrides, allowed, "CLI driver "+driver); err != nil {
		return nil, "", nil, err
	}
	args := append([]string(nil), baseArgs...)
	model := strings.TrimSpace(baseModel)
	effective := map[string]any{"model": model}
	if value, ok := overrides["model"]; ok && strings.TrimSpace(fmt.Sprint(value)) != "" {
		model = strings.TrimSpace(fmt.Sprint(value))
		effective["model"] = model
	}
	if driver == "claude" {
		if err := validateClaudeOverrides(overrides); err != nil {
			return nil, "", nil, err
		}
		if value, ok := textOverride(overrides, "effort"); ok {
			args = replaceOption(args, "--effort", []string{value})
		}
		if value, ok := textOverride(overrides, "permission_mode"); ok {
			args = replaceOption(args, "--permission-mode", []string{value})
		}
		if value, ok := textOverride(overrides, "append_system_prompt"); ok {
			args = replaceOption(args, "--append-system-prompt", []string{value})
		}
		for field, option := range map[string]string{"allowed_tools": "--allowed-tools", "disallowed_tools": "--disallowed-tools"} {
			if value, ok := overrides[field]; ok {
				values, err := overrideStrings(value, field)
				if err != nil {
					return nil, "", nil, err
				}
				args = replaceMultiOption(args, option, values)
			}
		}
	} else if driver == "codex" {
		if err := validateCodexOverrides(overrides); err != nil {
			return nil, "", nil, err
		}
		configKeys := map[string]string{
			"reasoning_effort": "model_reasoning_effort",
			"sandbox_mode":     "sandbox_mode",
			"approval_policy":  "approval_policy",
			"service_tier":     "service_tier",
			"verbosity":        "model_verbosity",
		}
		for field, configKey := range configKeys {
			if value, ok := textOverride(overrides, field); ok {
				args = replaceConfig(args, configKey, value)
			}
		}
		if value, ok := overrides["images"]; ok {
			images, err := overrideStrings(value, "images")
			if err != nil {
				return nil, "", nil, err
			}
			for _, image := range images {
				args = append(args, "--image", expandHome(image))
			}
		}
	}
	for key, value := range overrides {
		effective[key] = cloneValue(value)
	}
	effective["model"] = model
	return args, model, effective, nil
}

func validateCodexOverrides(overrides map[string]any) error {
	for _, field := range []string{"model", "reasoning_effort", "sandbox_mode", "approval_policy", "service_tier", "verbosity"} {
		if value, ok := overrides[field]; ok && value != nil {
			if _, valid := value.(string); !valid {
				return fmt.Errorf("Codex override %s must be a string", field)
			}
		}
	}
	enums := map[string]map[string]struct{}{
		"reasoning_effort": set("low", "medium", "high", "xhigh", "max", "ultra"),
		"sandbox_mode":     set("read-only", "workspace-write", "danger-full-access"),
		"approval_policy":  set("untrusted", "on-failure", "on-request", "never"),
		"verbosity":        set("low", "medium", "high"),
	}
	for field, choices := range enums {
		if value, ok := textOverride(overrides, field); ok {
			if _, valid := choices[value]; !valid {
				return fmt.Errorf("Codex override %s has invalid value %q", field, value)
			}
		}
	}
	if value, ok := overrides["images"]; ok {
		_, err := overrideStrings(value, "images")
		return err
	}
	return nil
}

func validateClaudeOverrides(overrides map[string]any) error {
	for _, field := range []string{"model", "effort", "permission_mode", "append_system_prompt"} {
		if value, ok := overrides[field]; ok && value != nil {
			if _, valid := value.(string); !valid {
				return fmt.Errorf("Claude override %s must be a string", field)
			}
		}
	}
	if value, ok := textOverride(overrides, "effort"); ok {
		if _, valid := set("low", "medium", "high", "xhigh", "max")[value]; !valid {
			return fmt.Errorf("Claude override effort has invalid value %q", value)
		}
	}
	if value, ok := textOverride(overrides, "permission_mode"); ok {
		if _, valid := set("default", "acceptEdits", "auto", "plan", "dontAsk", "bypassPermissions")[value]; !valid {
			return fmt.Errorf("Claude override permission_mode has invalid value %q", value)
		}
	}
	return nil
}

func rejectUnknown(overrides map[string]any, allowed map[string]struct{}, owner string) error {
	var unknown []string
	for name := range overrides {
		if _, ok := allowed[name]; !ok {
			unknown = append(unknown, name)
		}
	}
	if len(unknown) == 0 {
		return nil
	}
	sortStrings(unknown)
	return fmt.Errorf("%s override field(s) not allowed: %s", owner, strings.Join(unknown, ", "))
}

func replaceConfig(args []string, key, value string) []string {
	replacement := key + "=" + strings.TrimSpace(value)
	out := make([]string, 0, len(args)+2)
	replaced := false
	for i := 0; i < len(args); {
		arg := args[i]
		if (arg == "-c" || arg == "--config") && i+1 < len(args) && strings.TrimSpace(strings.SplitN(args[i+1], "=", 2)[0]) == key {
			if !replaced {
				out = append(out, arg, replacement)
				replaced = true
			}
			i += 2
			continue
		}
		if strings.HasPrefix(arg, "--config=") && strings.TrimSpace(strings.SplitN(strings.TrimPrefix(arg, "--config="), "=", 2)[0]) == key {
			if !replaced {
				out = append(out, "--config="+replacement)
				replaced = true
			}
			i++
			continue
		}
		out = append(out, arg)
		i++
	}
	if !replaced {
		insertAt := len(out)
		for i, arg := range out {
			if arg == "exec" {
				insertAt = i
				break
			}
		}
		out = append(out, "", "")
		copy(out[insertAt+2:], out[insertAt:])
		out[insertAt] = "-c"
		out[insertAt+1] = replacement
	}
	return out
}

func replaceOption(args []string, option string, values []string) []string {
	out := make([]string, 0, len(args)+len(values)+1)
	replaced := false
	for i := 0; i < len(args); {
		if args[i] == option {
			if !replaced {
				out = append(out, option)
				out = append(out, values...)
				replaced = true
			}
			i++
			if i < len(args) {
				i++
			}
			continue
		}
		out = append(out, args[i])
		i++
	}
	if !replaced {
		out = append(out, option)
		out = append(out, values...)
	}
	return out
}

func replaceMultiOption(args []string, option string, values []string) []string {
	out := make([]string, 0, len(args)+len(values)+1)
	replaced := false
	for i := 0; i < len(args); {
		if args[i] != option {
			out = append(out, args[i])
			i++
			continue
		}
		if !replaced && len(values) > 0 {
			out = append(out, option)
			out = append(out, values...)
			replaced = true
		}
		i++
		for i < len(args) && !strings.HasPrefix(args[i], "-") {
			i++
		}
	}
	if !replaced && len(values) > 0 {
		out = append(out, option)
		out = append(out, values...)
	}
	return out
}

func expandPromptArgs(template []string, prompt string) []string {
	if len(template) == 0 {
		return []string{prompt}
	}
	out := make([]string, 0, len(template)+1)
	inserted := false
	for _, arg := range template {
		if strings.Contains(arg, "{prompt}") {
			out = append(out, strings.ReplaceAll(arg, "{prompt}", prompt))
			inserted = true
		} else {
			out = append(out, arg)
		}
	}
	if !inserted {
		out = append(out, prompt)
	}
	return out
}

func textOverride(values map[string]any, key string) (string, bool) {
	value, ok := values[key]
	if !ok || value == nil {
		return "", false
	}
	text, ok := value.(string)
	if !ok {
		return "", false
	}
	text = strings.TrimSpace(text)
	return text, text != ""
}

func overrideStrings(value any, field string) ([]string, error) {
	var values []string
	switch typed := value.(type) {
	case []string:
		values = append(values, typed...)
	case []any:
		for _, item := range typed {
			values = append(values, fmt.Sprint(item))
		}
	default:
		return nil, fmt.Errorf("override %s must be an array", field)
	}
	for _, item := range values {
		if strings.TrimSpace(item) == "" {
			return nil, fmt.Errorf("override %s cannot contain empty values", field)
		}
	}
	return values, nil
}

func cloneStringMap(input map[string]any) map[string]any {
	out := make(map[string]any, len(input))
	for key, value := range input {
		out[key] = cloneValue(value)
	}
	return out
}

func copyOption(dst, src map[string]any, keys ...string) {
	for _, key := range keys {
		if value, ok := src[key]; ok && value != nil && fmt.Sprint(value) != "" {
			dst[key] = cloneValue(value)
		}
	}
}

func intValue(value any, fallback int) int {
	switch typed := value.(type) {
	case int:
		return typed
	case float64:
		return int(typed)
	default:
		return fallback
	}
}

func numericValue(value any) (float64, bool) {
	switch typed := value.(type) {
	case int:
		return float64(typed), true
	case int64:
		return float64(typed), true
	case float64:
		return typed, true
	default:
		return 0, false
	}
}

func integerValue(value any) (int, bool) {
	switch typed := value.(type) {
	case int:
		return typed, true
	case int64:
		return int(typed), true
	case float64:
		if typed == float64(int(typed)) {
			return int(typed), true
		}
	}
	return 0, false
}

func boolValue(value any) bool {
	valueBool, _ := value.(bool)
	return valueBool
}

func expandHome(path string) string {
	if path == "~" || strings.HasPrefix(path, "~/") {
		if home, err := osUserHomeDir(); err == nil {
			return filepath.Join(home, strings.TrimPrefix(strings.TrimPrefix(path, "~"), "/"))
		}
	}
	return path
}

var osUserHomeDir = os.UserHomeDir

func sortStrings(values []string) {
	sort.Strings(values)
}
