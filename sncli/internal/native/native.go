package native

import (
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"strings"

	"agent-arch/sncli/internal/config"
)

type startResult struct {
	RunID     string `json:"run_id"`
	ProjectID string `json:"project_id"`
	Attach    string `json:"attach"`
}

func Open(cfg *config.Config, providerName string, cwd string) error {
	profile := cfg.NativeProfile(providerName)
	if profile == "" {
		return fmt.Errorf("unknown native provider: %s", providerName)
	}
	start, err := startSession(cfg, profile, cwd)
	if err != nil {
		return err
	}
	fmt.Fprintf(os.Stderr, "runtime session: %s project=%s\n", start.RunID, start.ProjectID)
	if start.Attach != "" {
		fmt.Fprintf(os.Stderr, "attach: %s\n", start.Attach)
	}
	return attachSession(cfg, start.ProjectID, start.RunID)
}

func startSession(cfg *config.Config, profile string, cwd string) (startResult, error) {
	args := []string{
		"-m", "wb.runtime.cli",
		"--conf-dir", cfg.NativeConfDir(),
		"--runs-dir", cfg.NativeRunsDir(),
		"--json",
		"session", "start",
		"--profile", profile,
		"--project", cfg.Native.Project,
		"--cwd", cwd,
	}
	cmd := exec.Command(cfg.NativePython(), args...)
	cmd.Dir = cfg.Root
	cmd.Env = runtimeEnv(cfg)
	out, err := cmd.CombinedOutput()
	if err != nil {
		return startResult{}, fmt.Errorf("runtime session start: %w: %s", err, strings.TrimSpace(string(out)))
	}
	var result startResult
	if err := json.Unmarshal(out, &result); err != nil {
		return startResult{}, fmt.Errorf("parse runtime session start output: %w: %s", err, strings.TrimSpace(string(out)))
	}
	if result.RunID == "" {
		return startResult{}, fmt.Errorf("runtime session start returned empty run_id")
	}
	if result.ProjectID == "" {
		result.ProjectID = cfg.Native.Project
	}
	return result, nil
}

func attachSession(cfg *config.Config, projectID string, runID string) error {
	args := []string{
		"-m", "wb.runtime.cli",
		"--conf-dir", cfg.NativeConfDir(),
		"--runs-dir", cfg.NativeRunsDir(),
		"session", "attach",
		runID,
		"--project", projectID,
	}
	cmd := exec.Command(cfg.NativePython(), args...)
	cmd.Dir = cfg.Root
	cmd.Env = runtimeEnv(cfg)
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func runtimeEnv(cfg *config.Config) []string {
	env := os.Environ()
	rtRoot := cfg.SinanRoot()
	current := os.Getenv("PYTHONPATH")
	if current == "" {
		env = append(env, "PYTHONPATH="+rtRoot)
	} else {
		env = append(env, "PYTHONPATH="+rtRoot+":"+current)
	}
	return env
}
