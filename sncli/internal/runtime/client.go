package runtime

import (
	"bytes"
	"encoding/json"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strconv"
	"strings"

	"agent-arch/sncli/internal/redact"
)

type Client struct {
	Command string
	Root    string
}

type RunOptions struct {
	Provider string
	Prompt   string
	CWD      string
	Sandbox  string
	Timeout  int
	Model    string
}

type RunResult struct {
	RunID           string            `json:"run_id"`
	Provider        string            `json:"provider"`
	Requested       string            `json:"requested_provider"`
	Outcome         string            `json:"outcome"`
	ReturnCode      int               `json:"returncode"`
	FinalText       string            `json:"final_text"`
	Artifacts       map[string]string `json:"artifacts"`
	BlockedReason   string            `json:"blocked_reason"`
	FailureReason   string            `json:"failure_reason"`
	DurationSeconds float64           `json:"duration_seconds"`
}

func (c Client) ProvidersJSON() ([]byte, error) {
	return c.command("providers", "--json").Output()
}

func (c Client) DoctorJSON() ([]byte, error) {
	return c.command("doctor", "--json").Output()
}

func (c Client) Run(opts RunOptions) (*RunResult, error) {
	args := []string{
		"run",
		"--provider", opts.Provider,
		"--purpose", "sn-cli",
		"--cwd", opts.CWD,
		"--sandbox", opts.Sandbox,
		"--timeout", strconv.Itoa(opts.Timeout),
		"--prompt-file", "-",
		"--json",
	}
	if opts.Model != "" {
		args = append(args, "--model", opts.Model)
	}
	cmd := c.command(args...)
	cmd.Stdin = strings.NewReader(opts.Prompt)
	var stdout bytes.Buffer
	var stderr bytes.Buffer
	cmd.Stdout = &stdout
	cmd.Stderr = &stderr
	err := cmd.Run()
	if stderr.Len() > 0 && err != nil {
		return nil, fmt.Errorf("%w: %s", err, redact.Text(strings.TrimSpace(stderr.String())))
	}
	if err != nil {
		return nil, err
	}
	var result RunResult
	if err := json.Unmarshal(stdout.Bytes(), &result); err != nil {
		return nil, fmt.Errorf("parse runtime result: %w: %s", err, redact.Text(stdout.String()))
	}
	return &result, nil
}

func (c Client) command(args ...string) *exec.Cmd {
	cmd := exec.Command(c.resolveCommand(), args...)
	cmd.Dir = c.Root
	cmd.Env = c.env()
	return cmd
}

func (c Client) resolveCommand() string {
	if filepath.IsAbs(c.Command) {
		return c.Command
	}
	rootCandidates := []string{c.Root}
	if sinanRoot := os.Getenv("SINAN_ROOT"); sinanRoot != "" {
		rootCandidates = append(rootCandidates, sinanRoot)
	}
	for _, candidate := range rootCandidates {
		full := filepath.Join(candidate, c.Command)
		if _, err := os.Stat(full); err == nil {
			return full
		}
	}
	return c.Command
}

func (c Client) env() []string {
	env := os.Environ()
	root := c.Root
	if envRoot := os.Getenv("SINAN_ROOT"); envRoot != "" {
		root = envRoot
	}
	env = setEnv(env, "SINAN_ROOT", root)
	if root != "" {
		env = setEnv(env, "PYTHONPATH", prependPath(root, os.Getenv("PYTHONPATH")))
		if venvBin := filepath.Join(root, ".venv", "bin"); dirExists(venvBin) {
			env = setEnv(env, "PATH", prependPath(venvBin, os.Getenv("PATH")))
		}
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
