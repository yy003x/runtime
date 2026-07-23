package provider

import (
	"fmt"
	"os"
	"strings"
)

// ResolveEnv 只展开显式的 ${NAME}；$NAME 和 NAME 均保持为普通字符串。
func ResolveEnv(value string) (string, error) {
	var resolved strings.Builder
	for {
		start := strings.Index(value, "${")
		if start < 0 {
			resolved.WriteString(value)
			return resolved.String(), nil
		}
		resolved.WriteString(value[:start])
		remainder := value[start+2:]
		end := strings.IndexByte(remainder, '}')
		if end < 0 {
			return "", fmt.Errorf("环境变量占位符缺少右花括号")
		}
		name := remainder[:end]
		if !validEnvironmentReferenceName(name) {
			return "", fmt.Errorf("非法环境变量占位符；仅支持 ${VAR_NAME}")
		}
		environmentValue, ok := os.LookupEnv(name)
		if !ok {
			return "", fmt.Errorf("环境变量未设置: %s", name)
		}
		resolved.WriteString(environmentValue)
		value = remainder[end+1:]
	}
}

// EnvironmentReferenceName 只接受由单个 ${NAME} 组成的完整配置值。
func EnvironmentReferenceName(value string) (string, bool) {
	if len(value) < 4 || !strings.HasPrefix(value, "${") || !strings.HasSuffix(value, "}") {
		return "", false
	}
	name := value[2 : len(value)-1]
	return name, validEnvironmentReferenceName(name)
}

func validEnvironmentReferenceName(name string) bool {
	if name == "" {
		return false
	}
	for index, character := range name {
		if character == '_' || character >= 'A' && character <= 'Z' || character >= 'a' && character <= 'z' {
			continue
		}
		if index > 0 && character >= '0' && character <= '9' {
			continue
		}
		return false
	}
	return true
}

func CommandEnvironment(cfg CommandConfig, extra map[string]string) ([]string, error) {
	values := make(map[string]string)
	for _, item := range os.Environ() {
		key, value, ok := strings.Cut(item, "=")
		if ok {
			values[key] = value
		}
	}
	for _, key := range cfg.EnvUnset {
		delete(values, key)
	}
	for _, key := range cfg.EnvPassthrough {
		if value, ok := os.LookupEnv(key); ok {
			values[key] = value
		}
	}
	for key, value := range cfg.Env {
		resolved, err := ResolveEnv(value)
		if err != nil {
			return nil, fmt.Errorf("env.%s: %w", key, err)
		}
		values[key] = resolved
	}
	for key, value := range extra {
		values[key] = value
	}
	out := make([]string, 0, len(values))
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	sortStrings(out)
	return out, nil
}
