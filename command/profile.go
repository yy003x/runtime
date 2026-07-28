// Package command owns Runtime vNext command profiles and the transparent
// process-replacement bridge used by dynamic top-level command IDs.
package command

import (
	"fmt"
	"io"
	"strings"

	"github.com/yy003x/runtime/internal/strictjson"
)

const maxProfileBytes int64 = 1 << 20

type Transport string

const (
	TransportTTY      Transport = "tty"
	TransportTmux     Transport = "tmux"
	TransportTerminal Transport = "terminal"
)

type PromptDelivery string

const (
	PromptArgv   PromptDelivery = "argv"
	PromptStdin  PromptDelivery = "stdin"
	PromptPaste  PromptDelivery = "paste"
	PromptManual PromptDelivery = "manual"
)

type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

type EffortAdapter string

const (
	EffortAdapterCodexConfig EffortAdapter = "codex-config"
	EffortAdapterClaudeFlag  EffortAdapter = "claude-flag"
)

type Profile struct {
	Binary         string             `json:"binary"`
	Args           []string           `json:"args,omitempty"`
	Env            map[string]*string `json:"env,omitempty"`
	Transport      Transport          `json:"transport"`
	PromptDelivery PromptDelivery     `json:"prompt_delivery"`
	EffortAdapter  EffortAdapter      `json:"effort_adapter,omitempty"`
}

func Decode(reader io.Reader) (Profile, error) {
	var profile Profile
	if err := strictjson.Decode(reader, maxProfileBytes, &profile); err != nil {
		return Profile{}, err
	}
	if err := profile.Validate(); err != nil {
		return Profile{}, err
	}
	return profile, nil
}

func LoadFile(path string) (Profile, error) {
	var profile Profile
	if err := strictjson.ReadRegularFile(path, maxProfileBytes, &profile); err != nil {
		return Profile{}, err
	}
	if err := profile.Validate(); err != nil {
		return Profile{}, fmt.Errorf("%s: %w", path, err)
	}
	return profile, nil
}

func (profile Profile) Validate() error {
	if strings.TrimSpace(profile.Binary) == "" {
		return fmt.Errorf("binary is required")
	}
	if strings.ContainsRune(profile.Binary, '\x00') {
		return fmt.Errorf("binary contains NUL")
	}
	if len(profile.Binary) > 4096 {
		return fmt.Errorf("binary exceeds 4096 bytes")
	}
	if len(profile.Args) > 4096 {
		return fmt.Errorf("args exceed 4096 items")
	}
	for index, argument := range profile.Args {
		if len(argument) > 256<<10 {
			return fmt.Errorf("args[%d] exceeds 262144 bytes", index)
		}
		if strings.ContainsRune(argument, '\x00') {
			return fmt.Errorf("args[%d] contains NUL", index)
		}
		if err := validateReferences(argument); err != nil {
			return fmt.Errorf("args[%d]: %w", index, err)
		}
	}
	if len(profile.Env) > 1024 {
		return fmt.Errorf("env exceeds 1024 items")
	}
	for name, value := range profile.Env {
		if err := validateEnvironmentName(name); err != nil {
			return fmt.Errorf("env: %w", err)
		}
		if value == nil {
			continue
		}
		if len(*value) > 256<<10 {
			return fmt.Errorf("env[%q] exceeds 262144 bytes", name)
		}
		if strings.ContainsRune(*value, '\x00') {
			return fmt.Errorf("env[%q] contains NUL", name)
		}
		if err := validateReferences(*value); err != nil {
			return fmt.Errorf("env[%q]: %w", name, err)
		}
	}
	switch profile.Transport {
	case TransportTTY:
		switch profile.PromptDelivery {
		case PromptArgv, PromptStdin, PromptManual:
		default:
			return fmt.Errorf("prompt_delivery %q is invalid for transport %q", profile.PromptDelivery, profile.Transport)
		}
	case TransportTmux, TransportTerminal:
		switch profile.PromptDelivery {
		case PromptArgv, PromptPaste, PromptManual:
		default:
			return fmt.Errorf("prompt_delivery %q is invalid for transport %q", profile.PromptDelivery, profile.Transport)
		}
	default:
		return fmt.Errorf("transport must be tty, tmux, or terminal")
	}
	switch profile.EffortAdapter {
	case "", EffortAdapterCodexConfig, EffortAdapterClaudeFlag:
	default:
		return fmt.Errorf(
			"effort_adapter must be %q or %q",
			EffortAdapterCodexConfig,
			EffortAdapterClaudeFlag,
		)
	}
	return nil
}

func ParseEffort(value string) (Effort, error) {
	effort := Effort(strings.TrimSpace(value))
	switch effort {
	case EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
		return effort, nil
	default:
		return "", fmt.Errorf(
			"effort must be low, medium, high, xhigh, or max",
		)
	}
}

func (profile Profile) WithEffort(effort Effort) (Profile, error) {
	if effort == "" {
		return profile, nil
	}
	if _, err := ParseEffort(string(effort)); err != nil {
		return Profile{}, err
	}
	resolved := profile
	resolved.Args = append([]string(nil), profile.Args...)
	switch profile.EffortAdapter {
	case EffortAdapterCodexConfig:
		resolved.Args = append(
			resolved.Args,
			"-c",
			"model_reasoning_effort="+string(effort),
		)
	case EffortAdapterClaudeFlag:
		resolved.Args = append(resolved.Args, "--effort", string(effort))
	case "":
		return Profile{}, fmt.Errorf(
			"profile does not declare an effort_adapter",
		)
	default:
		return Profile{}, fmt.Errorf(
			"unsupported effort_adapter %q",
			profile.EffortAdapter,
		)
	}
	return resolved, nil
}

func validateReferences(value string) error {
	for {
		start := strings.Index(value, "${")
		if start < 0 {
			return nil
		}
		remainder := value[start+2:]
		end := strings.IndexByte(remainder, '}')
		if end < 0 {
			return fmt.Errorf("environment reference is missing }")
		}
		if !validReferenceName(remainder[:end]) {
			return fmt.Errorf("invalid environment reference; only ${VAR_NAME} is supported")
		}
		value = remainder[end+1:]
	}
}

func validateEnvironmentName(value string) error {
	if value == "" || strings.TrimSpace(value) == "" {
		return fmt.Errorf("environment name is required")
	}
	if strings.ContainsAny(value, "=\x00") {
		return fmt.Errorf("invalid environment name %q", value)
	}
	return nil
}

func validReferenceName(value string) bool {
	if value == "" || !asciiLetter(value[0]) && value[0] != '_' {
		return false
	}
	for index := 1; index < len(value); index++ {
		if !asciiLetter(value[index]) && (value[index] < '0' || value[index] > '9') && value[index] != '_' {
			return false
		}
	}
	return true
}

func asciiLetter(value byte) bool {
	return value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z'
}
