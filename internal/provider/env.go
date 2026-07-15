package provider

import (
	"os"
	"strings"
)

func ExpandEnv(value string) string {
	return os.Expand(value, func(key string) string { return os.Getenv(key) })
}

func CommandEnvironment(cfg CommandConfig, extra map[string]string) []string {
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
		values[key] = ExpandEnv(value)
	}
	for key, value := range extra {
		values[key] = value
	}
	out := make([]string, 0, len(values))
	for key, value := range values {
		out = append(out, key+"="+value)
	}
	sortStrings(out)
	return out
}
