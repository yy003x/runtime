// Package command owns CLI Profiles, command adapters, and deterministic
// invocation construction. It does not own Session or Tmux lifecycle state.
package command

import (
	"fmt"
	"path/filepath"
	"strings"
	"unicode/utf8"

	"github.com/yy003x/runtime/internal/envref"
)

// MaxTokenBytes stays below Linux's common 128 KiB single-argv-string limit,
// which includes the terminating NUL.
const MaxTokenBytes = 128_000

type Effort string

const (
	EffortLow    Effort = "low"
	EffortMedium Effort = "medium"
	EffortHigh   Effort = "high"
	EffortXHigh  Effort = "xhigh"
	EffortMax    Effort = "max"
)

// Profile is the CLI-only side of the unified Profile protocol.
type Profile struct {
	Command string             `json:"command"`
	Args    []string           `json:"args,omitempty"`
	Env     map[string]*string `json:"env,omitempty"`
	Model   string             `json:"model,omitempty"`
	Effort  Effort             `json:"effort,omitempty"`
	Prompt  string             `json:"prompt,omitempty"`
	CWD     string             `json:"cwd,omitempty"`
}

// Validate performs structural validation only. CheckProfile additionally
// validates adapter option grammar by building symbolic plans.
func (profile Profile) Validate() error {
	if strings.TrimSpace(profile.Command) == "" {
		return fmt.Errorf("command is required")
	}
	base, exact := adapterBase(profile.Command)
	if !exact {
		return fmt.Errorf(
			"command must end with an exact codex or claude basename",
		)
	}
	if base != "codex" && base != "claude" {
		return fmt.Errorf("no command adapter for %q", base)
	}
	if err := validateTextToken("command", profile.Command, 4096, false); err != nil {
		return err
	}
	if len(profile.Args) > 4096 {
		return fmt.Errorf("args exceed 4096 items")
	}
	for index, argument := range profile.Args {
		if err := validateTextToken(
			fmt.Sprintf("args[%d]", index), argument, MaxTokenBytes, true,
		); err != nil {
			return err
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
		if err := validateTextToken(
			fmt.Sprintf("env[%q]", name), *value, MaxTokenBytes, true,
		); err != nil {
			return err
		}
		if err := validateReferences(*value); err != nil {
			return fmt.Errorf("env[%q]: %w", name, err)
		}
	}
	for _, field := range []struct {
		name  string
		value string
	}{
		{name: "model", value: profile.Model},
		{name: "prompt", value: profile.Prompt},
		{name: "cwd", value: profile.CWD},
	} {
		if err := validateTextToken(
			field.name, field.value, MaxTokenBytes, true,
		); err != nil {
			return err
		}
	}
	if profile.CWD != "" {
		if err := validateReferences(profile.CWD); err != nil {
			return fmt.Errorf("cwd: %w", err)
		}
	}
	if profile.Effort != "" {
		if _, err := ParseEffort(string(profile.Effort)); err != nil {
			return err
		}
	}
	return nil
}

func ParseEffort(value string) (Effort, error) {
	effort := Effort(value)
	switch effort {
	case EffortLow, EffortMedium, EffortHigh, EffortXHigh, EffortMax:
		return effort, nil
	default:
		return "", fmt.Errorf("effort must be low, medium, high, xhigh, or max")
	}
}

func validateTextToken(name, value string, limit int, allowEmpty bool) error {
	if !allowEmpty && value == "" {
		return fmt.Errorf("%s is required", name)
	}
	if !utf8.ValidString(value) {
		return fmt.Errorf("%s must be UTF-8", name)
	}
	if strings.ContainsRune(value, '\x00') {
		return fmt.Errorf("%s contains NUL", name)
	}
	if len(value) > limit {
		return fmt.Errorf("%s exceeds %d bytes", name, limit)
	}
	return nil
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
		if !envref.ValidName(remainder[:end]) {
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

func adapterBase(command string) (string, bool) {
	index := strings.LastIndex(command, string(filepath.Separator))
	rawBase := command[index+1:]
	base := filepath.Base(command)
	return base, rawBase != "" && rawBase == base
}
