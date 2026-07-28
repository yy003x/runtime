package command

import (
	"fmt"
	"os"
	"os/exec"
	"sort"
	"strings"
)

type Invocation struct {
	Path        string
	Argv        []string
	Environment []string
}

// ReplaceProcess resolves a command profile and replaces the current process.
// It does not create Runtime state, capture stdio, or interpret user arguments.
func ReplaceProcess(profile Profile, userArgs []string) error {
	if profile.Transport != TransportTTY {
		return fmt.Errorf("transparent command bridge requires transport %q, got %q", TransportTTY, profile.Transport)
	}
	resolved, err := prepareInvocation(profile, userArgs, os.Environ())
	if err != nil {
		return err
	}
	return replaceProcess(resolved)
}

// ReplaceProcessPrompt replaces the current process while applying the
// profile's explicit automatic prompt delivery. It is used only by the
// `profile <command-id>` facade; top-level command IDs always call
// ReplaceProcess and preserve native argv unchanged.
func ReplaceProcessPrompt(profile Profile, prompt string) error {
	if profile.Transport != TransportTTY {
		return fmt.Errorf(
			"prompt process replacement requires transport %q, got %q",
			TransportTTY, profile.Transport,
		)
	}
	var userArgs []string
	switch profile.PromptDelivery {
	case PromptArgv:
		if prompt != "" {
			userArgs = []string{prompt}
		}
	case PromptStdin:
	case PromptManual:
		if prompt != "" {
			return fmt.Errorf("manual prompt delivery does not accept automatic input")
		}
	default:
		return fmt.Errorf(
			"prompt delivery %q cannot use transparent process replacement",
			profile.PromptDelivery,
		)
	}
	resolved, err := prepareInvocation(profile, userArgs, os.Environ())
	if err != nil {
		return err
	}
	if profile.PromptDelivery == PromptStdin && prompt != "" {
		return replaceProcessWithInput(resolved, prompt)
	}
	return replaceProcess(resolved)
}

func prepareInvocation(profile Profile, userArgs, inheritedEnvironment []string) (Invocation, error) {
	if err := profile.Validate(); err != nil {
		return Invocation{}, err
	}
	path, err := exec.LookPath(profile.Binary)
	if err != nil {
		return Invocation{}, fmt.Errorf("resolve command binary %q: %w", profile.Binary, err)
	}

	inherited := environmentMap(inheritedEnvironment)
	lookup := func(name string) (string, bool) {
		value, exists := inherited[name]
		return value, exists
	}
	argv := make([]string, 1, 1+len(profile.Args)+len(userArgs))
	argv[0] = profile.Binary
	for index, argument := range profile.Args {
		resolved, resolveErr := expandReferences(argument, lookup)
		if resolveErr != nil {
			return Invocation{}, fmt.Errorf("args[%d]: %w", index, resolveErr)
		}
		argv = append(argv, resolved)
	}
	argv = append(argv, userArgs...)

	effective := make(map[string]string, len(inherited)+len(profile.Env))
	for name, value := range inherited {
		effective[name] = value
	}
	for name, value := range profile.Env {
		if value == nil {
			delete(effective, name)
			continue
		}
		resolved, resolveErr := expandReferences(*value, lookup)
		if resolveErr != nil {
			return Invocation{}, fmt.Errorf("env[%q]: %w", name, resolveErr)
		}
		effective[name] = resolved
	}
	environment := make([]string, 0, len(effective))
	for name, value := range effective {
		environment = append(environment, name+"="+value)
	}
	sort.Strings(environment)
	return Invocation{Path: path, Argv: argv, Environment: environment}, nil
}

func environmentMap(values []string) map[string]string {
	result := make(map[string]string, len(values))
	for _, item := range values {
		name, value, exists := strings.Cut(item, "=")
		if exists {
			result[name] = value
		}
	}
	return result
}

func expandReferences(value string, lookup func(string) (string, bool)) (string, error) {
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
			return "", fmt.Errorf("environment reference is missing }")
		}
		name := remainder[:end]
		if !validReferenceName(name) {
			return "", fmt.Errorf("invalid environment reference; only ${VAR_NAME} is supported")
		}
		replacement, exists := lookup(name)
		if !exists {
			return "", fmt.Errorf("environment variable is not set: %s", name)
		}
		resolved.WriteString(replacement)
		value = remainder[end+1:]
	}
}
