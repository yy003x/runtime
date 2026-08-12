package command

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"sort"
	"strconv"
	"strings"
	"sync"
	"unsafe"

	"github.com/yy003x/runtime/internal/infrastructure/envref"
)

const (
	maxInvocationBytes      = 512 << 10
	fallbackInvocationBytes = 128 << 10
	argMaxReserveBytes      = 32 << 10
)

type preparedConfig struct {
	args        []string
	environment []string
	cwd         string
	path        string
}

// ResolveExecutable applies the same cwd, env, PATH, and reference rules used
// by Build without constructing or executing a final argv. Profile arguments
// are invocation inputs, so their environment references are validated
// structurally by Profile.Validate but are not resolved here.
func ResolveExecutable(
	profile Profile,
	invocationBase string,
	inheritedEnvironment []string,
) (string, error) {
	if err := profile.Validate(); err != nil {
		return "", err
	}
	executableProfile := profile
	executableProfile.Args = nil
	prepared, err := prepareEffectiveConfig(BuildRequest{
		Profile:              executableProfile,
		InvocationBase:       invocationBase,
		InheritedEnvironment: inheritedEnvironment,
	})
	if err != nil {
		return "", err
	}
	return prepared.path, nil
}

func prepareEffectiveConfig(request BuildRequest) (preparedConfig, error) {
	_, _, rawCWD, err := effectiveTyped(request)
	if err != nil {
		return preparedConfig{}, err
	}
	if request.Symbolic {
		cwd := request.InvocationBase
		if cwd == "" {
			cwd = "/__sn_cli_symbolic_cwd__"
		}
		return preparedConfig{
			args: append([]string(nil), request.Profile.Args...),
			cwd:  cwd,
			path: request.Profile.Command,
		}, nil
	}
	if request.InvocationBase == "" || !filepath.IsAbs(request.InvocationBase) {
		return preparedConfig{}, fmt.Errorf("invocation base must be an absolute path")
	}
	inherited := environmentMap(request.InheritedEnvironment)
	lookup := func(name string) (string, bool) {
		value, exists := inherited[name]
		return value, exists
	}
	args := make([]string, 0, len(request.Profile.Args))
	for index, argument := range request.Profile.Args {
		resolved, resolveErr := envref.Expand(argument, lookup)
		if resolveErr != nil {
			return preparedConfig{}, fmt.Errorf("args[%d]: %w", index, resolveErr)
		}
		args = append(args, resolved)
	}
	effective := make(map[string]string, len(inherited)+len(request.Profile.Env))
	for name, value := range inherited {
		effective[name] = value
	}
	for name, value := range request.Profile.Env {
		if value == nil {
			delete(effective, name)
			continue
		}
		resolved, resolveErr := envref.Expand(*value, lookup)
		if resolveErr != nil {
			return preparedConfig{}, fmt.Errorf("env[%q]: %w", name, resolveErr)
		}
		effective[name] = resolved
	}
	cwd := request.InvocationBase
	if rawCWD != "" {
		resolved, resolveErr := envref.Expand(rawCWD, lookup)
		if resolveErr != nil {
			return preparedConfig{}, fmt.Errorf("cwd: %w", resolveErr)
		}
		if filepath.IsAbs(resolved) {
			cwd = filepath.Clean(resolved)
		} else {
			cwd = filepath.Clean(filepath.Join(request.InvocationBase, resolved))
		}
	}
	if err := validateWorkingDirectory(cwd); err != nil {
		return preparedConfig{}, err
	}
	path, err := lookPath(request.Profile.Command, effective, cwd)
	if err != nil {
		return preparedConfig{}, err
	}
	environment := make([]string, 0, len(effective))
	for name, value := range effective {
		environment = append(environment, name+"="+value)
	}
	sort.Strings(environment)
	return preparedConfig{
		args: args, environment: environment,
		cwd: cwd, path: path,
	}, nil
}

func finishInvocation(
	request BuildRequest,
	prepared preparedConfig,
	argv []string,
) (Invocation, error) {
	result := Invocation{
		Path: prepared.path, Argv: argv,
		Environment: prepared.environment, CWD: prepared.cwd,
	}
	if request.Symbolic {
		return result, nil
	}
	if err := validateInvocationBudget(result); err != nil {
		return Invocation{}, err
	}
	return result, nil
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

func validateWorkingDirectory(path string) error {
	info, err := os.Stat(path)
	if err != nil {
		return fmt.Errorf("resolve cwd %q: %w", path, err)
	}
	if !info.IsDir() {
		return fmt.Errorf("cwd %q is not a directory", path)
	}
	if err := directoryEnterable(path); err != nil {
		return fmt.Errorf("cwd %q is not enterable: %w", path, err)
	}
	return nil
}

func lookPath(command string, environment map[string]string, cwd string) (string, error) {
	if strings.ContainsRune(command, filepath.Separator) {
		path := command
		if !filepath.IsAbs(path) {
			path = filepath.Join(cwd, path)
		}
		if executableFile(path) {
			return filepath.Clean(path), nil
		}
		return "", fmt.Errorf("resolve command %q: %w", command, exec.ErrNotFound)
	}
	pathValue := environment["PATH"]
	for _, directory := range filepath.SplitList(pathValue) {
		if directory == "" {
			directory = cwd
		} else if !filepath.IsAbs(directory) {
			directory = filepath.Join(cwd, directory)
		}
		candidate := filepath.Join(directory, command)
		if executableFile(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("resolve command %q: %w", command, exec.ErrNotFound)
}

func executableFile(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.Mode().IsRegular() && info.Mode().Perm()&0o111 != 0
}

func validateInvocationBudget(invocation Invocation) error {
	total := len(invocation.Path) + 1
	for index, token := range invocation.Argv {
		if strings.ContainsRune(token, '\x00') {
			return fmt.Errorf("argv[%d] contains NUL", index)
		}
		if len(token) > MaxTokenBytes {
			return &invocationLimitError{message: fmt.Sprintf(
				"argv[%d] exceeds %d bytes", index, MaxTokenBytes,
			)}
		}
		total += len(token) + 1
	}
	for index, token := range invocation.Environment {
		if strings.ContainsRune(token, '\x00') {
			return fmt.Errorf("env[%d] contains NUL", index)
		}
		if len(token) > MaxTokenBytes {
			return &invocationLimitError{message: fmt.Sprintf(
				"env[%d] exceeds %d bytes", index, MaxTokenBytes,
			)}
		}
		total += len(token) + 1
	}
	pointerBytes := int(unsafe.Sizeof(uintptr(0))) *
		(len(invocation.Argv) + len(invocation.Environment) + 2)
	total += pointerBytes
	budget := invocationBudget()
	if budget <= 0 || total > budget {
		return &invocationLimitError{message: fmt.Sprintf(
			"argv+env requires %d bytes, exceeds invocation budget %d",
			total, budget,
		)}
	}
	return nil
}

var (
	argMaxOnce sync.Once
	argMax     int
)

func invocationBudget() int {
	argMaxOnce.Do(func() {
		output, err := exec.Command("/usr/bin/getconf", "ARG_MAX").Output()
		if err != nil {
			return
		}
		value, err := strconv.Atoi(strings.TrimSpace(string(output)))
		if err == nil {
			argMax = value
		}
	})
	if argMax <= argMaxReserveBytes {
		return fallbackInvocationBytes
	}
	value := argMax - argMaxReserveBytes
	if value > maxInvocationBytes {
		return maxInvocationBytes
	}
	return value
}
