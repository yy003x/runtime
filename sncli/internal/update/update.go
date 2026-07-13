package update

import (
	"context"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"

	"agent-arch/sncli/internal/config"
)

const gitTimeout = 2 * time.Second

type Status struct {
	SchemaVersion   int       `json:"schema_version"`
	Enabled         bool      `json:"enabled"`
	Root            string    `json:"root"`
	Ref             string    `json:"ref"`
	CurrentCommit   string    `json:"current_commit,omitempty"`
	LatestCommit    string    `json:"latest_commit,omitempty"`
	Relation        string    `json:"relation,omitempty"`
	UpdateAvailable bool      `json:"update_available"`
	CheckedAt       time.Time `json:"checked_at"`
	Message         string    `json:"message,omitempty"`
	Error           string    `json:"error,omitempty"`
}

type state struct {
	CheckedAt     time.Time `json:"checked_at"`
	CurrentCommit string    `json:"current_commit,omitempty"`
	LatestCommit  string    `json:"latest_commit,omitempty"`
}

func MaybePrintHint(cfg *config.Config, w io.Writer) {
	if !cfg.UpdateEnabled() || !checkDue(cfg) {
		return
	}
	status := Check(cfg)
	if status.UpdateAvailable {
		fmt.Fprintf(w, "sn-cli 有更新：%s -> %s，运行 `sn-cli update` 升级。\n", short(status.CurrentCommit), short(status.LatestCommit))
	}
}

func Check(cfg *config.Config) Status {
	status := Status{
		SchemaVersion: 1,
		Enabled:       cfg.UpdateEnabled(),
		Root:          cfg.Root,
		Ref:           cfg.Update.Ref,
		CheckedAt:     time.Now().UTC(),
	}
	if !status.Enabled {
		status.Message = "update check disabled"
		return status
	}
	current, err := gitOutput(cfg.Root, "rev-parse", "HEAD")
	if err != nil {
		status.Error = err.Error()
		_ = writeState(cfg, status)
		return status
	}
	latest, err := latestRemoteCommit(cfg.Root, cfg.Update.Ref)
	if err != nil {
		status.Error = err.Error()
		status.CurrentCommit = current
		_ = writeState(cfg, status)
		return status
	}
	status.CurrentCommit = current
	status.LatestCommit = latest
	status.Relation = commitRelation(cfg.Root, current, latest)
	status.UpdateAvailable = status.Relation == "behind"
	switch status.Relation {
	case "behind":
		status.Message = "update available"
	case "up_to_date":
		status.Message = "up to date"
	case "ahead":
		status.Message = "local checkout is ahead of remote"
	case "diverged":
		status.Message = "local checkout diverged from remote"
	default:
		status.Message = "remote relation unknown"
	}
	_ = writeState(cfg, status)
	return status
}

func Apply(cfg *config.Config, installDir string, ref string) error {
	if ref == "" {
		ref = cfg.Update.Ref
	}
	if ref == "" {
		ref = "main"
	}
	if dirty, err := dirtyTrackedFiles(cfg.Root); err != nil {
		return err
	} else if dirty {
		return fmt.Errorf("Sinan checkout has uncommitted tracked changes; commit or stash before update: %s", cfg.Root)
	}
	if _, err := gitOutputWithTimeout(30*time.Second, cfg.Root, "fetch", "origin", ref); err != nil {
		return err
	}
	branch, err := gitOutput(cfg.Root, "rev-parse", "--abbrev-ref", "HEAD")
	if err != nil {
		return err
	}
	if branch == ref {
		if _, err := gitOutputWithTimeout(60*time.Second, cfg.Root, "pull", "--ff-only", "origin", ref); err != nil {
			return err
		}
	} else {
		if _, err := gitOutputWithTimeout(30*time.Second, cfg.Root, "checkout", "--detach", "FETCH_HEAD"); err != nil {
			return err
		}
	}
	script := cfg.UpdateInstallScript()
	if info, err := os.Stat(script); err != nil || info.IsDir() {
		return fmt.Errorf("install script not found: %s", script)
	}
	args := []string{script, "--local-repo", cfg.Root, "--ref", ref}
	cmd := exec.Command("bash", args...)
	cmd.Dir = cfg.Root
	cmd.Env = os.Environ()
	if installDir != "" {
		cmd.Env = append(cmd.Env, "SN_CLI_INSTALL_DIR="+installDir)
	}
	if cfg.Update.RepoURL != "" {
		cmd.Env = append(cmd.Env, "SINAN_REPO_URL="+cfg.Update.RepoURL)
	}
	cmd.Stdin = os.Stdin
	cmd.Stdout = os.Stdout
	cmd.Stderr = os.Stderr
	return cmd.Run()
}

func checkDue(cfg *config.Config) bool {
	path := cfg.UpdateStateFile()
	data, err := os.ReadFile(path)
	if err != nil {
		return true
	}
	var st state
	if err := json.Unmarshal(data, &st); err != nil {
		return true
	}
	interval := time.Duration(cfg.Update.CheckIntervalHours) * time.Hour
	if interval <= 0 {
		interval = 24 * time.Hour
	}
	return time.Since(st.CheckedAt) >= interval
}

func writeState(cfg *config.Config, status Status) error {
	path := cfg.UpdateStateFile()
	if err := os.MkdirAll(filepath.Dir(path), 0755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(state{
		CheckedAt:     status.CheckedAt,
		CurrentCommit: status.CurrentCommit,
		LatestCommit:  status.LatestCommit,
	}, "", "  ")
	if err != nil {
		return err
	}
	return os.WriteFile(path, append(data, '\n'), 0644)
}

func latestRemoteCommit(root string, ref string) (string, error) {
	if ref == "" {
		ref = "main"
	}
	out, err := gitOutput(root, "ls-remote", "origin", "refs/heads/"+ref)
	if err != nil {
		return "", err
	}
	if out == "" {
		out, err = gitOutput(root, "ls-remote", "origin", ref)
		if err != nil {
			return "", err
		}
	}
	fields := strings.Fields(out)
	if len(fields) == 0 {
		return "", fmt.Errorf("remote ref not found: %s", ref)
	}
	return fields[0], nil
}

func dirtyTrackedFiles(root string) (bool, error) {
	out, err := gitOutput(root, "status", "--porcelain", "--untracked-files=no")
	if err != nil {
		return false, err
	}
	return strings.TrimSpace(out) != "", nil
}

func commitRelation(root string, current string, latest string) string {
	if current == "" || latest == "" {
		return "unknown"
	}
	if current == latest {
		return "up_to_date"
	}
	if isAncestor(root, current, latest) {
		return "behind"
	}
	if isAncestor(root, latest, current) {
		return "ahead"
	}
	return "diverged"
}

func isAncestor(root string, ancestor string, descendant string) bool {
	ctx, cancel := context.WithTimeout(context.Background(), gitTimeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", "-C", root, "merge-base", "--is-ancestor", ancestor, descendant)
	return cmd.Run() == nil
}

func gitOutput(root string, args ...string) (string, error) {
	return gitOutputWithTimeout(gitTimeout, root, args...)
}

func gitOutputWithTimeout(timeout time.Duration, root string, args ...string) (string, error) {
	ctx, cancel := context.WithTimeout(context.Background(), timeout)
	defer cancel()
	cmd := exec.CommandContext(ctx, "git", append([]string{"-C", root}, args...)...)
	out, err := cmd.CombinedOutput()
	if ctx.Err() == context.DeadlineExceeded {
		return "", fmt.Errorf("git %s timed out", strings.Join(args, " "))
	}
	if err != nil {
		return "", fmt.Errorf("git %s: %w: %s", strings.Join(args, " "), err, strings.TrimSpace(string(out)))
	}
	return strings.TrimSpace(string(out)), nil
}

func short(commit string) string {
	if len(commit) <= 7 {
		return commit
	}
	return commit[:7]
}
