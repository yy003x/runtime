package executor

import (
	"bufio"
	"fmt"
	"os"
	"path/filepath"
	"runtime"
	"strings"
)

func resolveExecutable(command string, env []string, cwd string) (string, error) {
	if strings.ContainsRune(command, filepath.Separator) {
		path := command
		if !filepath.IsAbs(path) && cwd != "" {
			path = filepath.Join(cwd, path)
		}
		if executable(path) {
			return path, nil
		}
		return "", fmt.Errorf("command %q is not executable", command)
	}
	pathValue := envGet(env, "PATH")
	if pathValue == "" {
		pathValue = os.Getenv("PATH")
	}
	for _, dir := range filepath.SplitList(pathValue) {
		if dir == "" {
			dir = "."
		}
		if !filepath.IsAbs(dir) && cwd != "" {
			dir = filepath.Join(cwd, dir)
		}
		candidate := filepath.Join(dir, command)
		if executable(candidate) {
			return candidate, nil
		}
	}
	return "", fmt.Errorf("command %q not found in PATH", command)
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0o111 != 0
}

func envGet(env []string, key string) string {
	prefix := key + "="
	value := ""
	for _, item := range env {
		if strings.HasPrefix(item, prefix) {
			value = strings.TrimPrefix(item, prefix)
		}
	}
	return value
}

func resolveEnvShebang(path string, env []string) (string, []string) {
	if runtime.GOOS != "darwin" || envGet(env, "DYLD_INSERT_LIBRARIES") == "" {
		return path, nil
	}
	file, err := os.Open(path)
	if err != nil {
		return path, nil
	}
	defer file.Close()
	scanner := bufio.NewScanner(file)
	if !scanner.Scan() {
		return path, nil
	}
	line := strings.TrimSpace(scanner.Text())
	if !strings.HasPrefix(line, "#!/usr/bin/env ") {
		return path, nil
	}
	rest := strings.TrimSpace(strings.TrimPrefix(line, "#!/usr/bin/env "))
	if strings.HasPrefix(rest, "-S ") {
		rest = strings.TrimSpace(strings.TrimPrefix(rest, "-S "))
	}
	fields := strings.Fields(rest)
	if len(fields) == 0 {
		return path, nil
	}
	interpreter, err := resolveExecutable(fields[0], env, "")
	if err != nil {
		return path, nil
	}
	args := append(append([]string(nil), fields[1:]...), path)
	return interpreter, args
}
