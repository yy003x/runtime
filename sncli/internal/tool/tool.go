package tool

import (
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"agent-arch/sncli/internal/config"
)

type Runner struct {
	Root string
}

func (r Runner) Run(name string, cfg config.ToolConfig, extraArgs []string) (int, error) {
	binary, err := exec.LookPath(cfg.Command)
	if err != nil {
		return 1, fmt.Errorf("tool %s command %q not found in PATH: %w", name, cfg.Command, err)
	}
	args := append([]string{}, cfg.Args...)
	args = append(args, extraArgs...)
	cmd := exec.Command(binary, args...)
	if cwd, err := os.Getwd(); err == nil {
		cmd.Dir = cwd
	}
	cmd.Env = r.env(cfg.Env)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	if err := cmd.Run(); err != nil {
		if exitErr, ok := err.(*exec.ExitError); ok {
			return exitErr.ExitCode(), nil
		}
		return 1, fmt.Errorf("run tool %s: %w", name, err)
	}
	return 0, nil
}

func (r Runner) env(overrides map[string]string) []string {
	env := os.Environ()
	if r.Root != "" {
		root := r.Root
		if envRoot := os.Getenv("SINAN_ROOT"); envRoot != "" {
			root = envRoot
		}
		env = setEnv(env, "SINAN_ROOT", root)
		if venvBin := filepath.Join(root, ".venv", "bin"); dirExists(venvBin) {
			env = setEnv(env, "PATH", prependPath(venvBin, os.Getenv("PATH")))
		}
	}
	for key, value := range overrides {
		env = setEnv(env, key, value)
	}
	return env
}

func setEnv(env []string, key string, value string) []string {
	prefix := key + "="
	out := make([]string, 0, len(env)+1)
	for _, item := range env {
		if !strings.HasPrefix(item, prefix) {
			out = append(out, item)
		}
	}
	return append(out, prefix+value)
}

func prependPath(entry, existing string) string {
	if existing == "" {
		return entry
	}
	return entry + string(os.PathListSeparator) + existing
}

func dirExists(path string) bool {
	info, err := os.Stat(path)
	return err == nil && info.IsDir()
}
