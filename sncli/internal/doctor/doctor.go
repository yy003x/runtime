package doctor

import (
	"encoding/json"
	"os"
	"os/exec"

	"agent-arch/sncli/internal/config"
	"agent-arch/sncli/internal/runtime"
)

type Report struct {
	SchemaVersion int             `json:"schema_version"`
	OK            bool            `json:"ok"`
	Root          string          `json:"root"`
	Checks        map[string]bool `json:"checks"`
}

func Run(cfg *config.Config) Report {
	checks := map[string]bool{
		"sinan_root":     cfg.Root != "",
		"runtime_binary": executable(cfg.RuntimeCommandPath()),
		"sessions_root":  canCreate(cfg.SessionsRoot()),
		"go_runtime":     true,
		"runtime_tmux":   commandExists("tmux"),
		"codex":          commandExists("codex"),
		"claude":         commandExists("claude"),
	}
	for name, tool := range cfg.Tools {
		checks["tool_"+name] = commandExists(tool.Command)
	}
	client := runtime.Client{Command: cfg.RuntimeCommandPath(), Root: cfg.Root}
	if _, err := client.DoctorJSON(); err == nil {
		checks["agent_runtime"] = true
	} else {
		checks["agent_runtime"] = false
	}
	ok := checks["sinan_root"] && checks["runtime_binary"] && checks["sessions_root"] && checks["agent_runtime"]
	return Report{SchemaVersion: 1, OK: ok, Root: cfg.Root, Checks: checks}
}

func PrintJSON(report Report) error {
	enc := json.NewEncoder(os.Stdout)
	enc.SetIndent("", "  ")
	return enc.Encode(report)
}

func commandExists(name string) bool {
	_, err := exec.LookPath(name)
	return err == nil
}

func executable(path string) bool {
	info, err := os.Stat(path)
	return err == nil && !info.IsDir() && info.Mode()&0111 != 0
}

func canCreate(path string) bool {
	if err := os.MkdirAll(path, 0755); err != nil {
		return false
	}
	return true
}
