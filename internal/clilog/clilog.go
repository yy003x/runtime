// Package clilog appends CLI execution records to a per-day JSONL file under
// the runtime logs directory. Each record captures how a profile command was
// invoked: the sn-cli namespace, the profile-id, the original sn-cli command
// line, and the resolved command handed to the OS (profile env kept as ${VAR}
// references, never expanded, so no secret is persisted).
package clilog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

// Namespace identifies which sn-cli namespace launched the profile command.
type Namespace string

const (
	NamespaceProfile Namespace = "profile"
	NamespaceSession Namespace = "session"
	NamespaceTmux    Namespace = "tmux"
)

// Record is a single CLI execution record.
type Record struct {
	Time      time.Time
	Namespace Namespace
	Profile   string // profile-id
	Source    string // original sn-cli command line
	Command   string // resolved command handed to the OS (readable)
}

type recordJSON struct {
	Time      string    `json:"time"`
	Namespace Namespace `json:"namespace"`
	Profile   string    `json:"profile"`
	Source    string    `json:"source"`
	Command   string    `json:"command"`
}

// Append writes record as one JSON line to logDir/YYYY-MM-DD.jsonl. The file is
// created with 0600 and appended atomically; a write error is returned so the
// caller can ignore it — logging must never block execution.
func Append(logDir string, record Record) error {
	if logDir == "" {
		return fmt.Errorf("clilog: log directory is required")
	}
	payload := recordJSON{
		Time:      record.Time.Format("2006-01-02 15:04:05"),
		Namespace: record.Namespace,
		Profile:   record.Profile,
		Source:    record.Source,
		Command:   record.Command,
	}
	line, err := encode(payload)
	if err != nil {
		return err
	}
	path := filepath.Join(logDir, record.Time.Format("2006-01-02")+".jsonl")
	file, err := os.OpenFile(path, os.O_APPEND|os.O_CREATE|os.O_WRONLY, 0o600)
	if err != nil {
		return fmt.Errorf("clilog: open log: %w", err)
	}
	defer file.Close()
	if _, err := file.Write(line); err != nil {
		return fmt.Errorf("clilog: write log: %w", err)
	}
	return nil
}

func encode(payload recordJSON) ([]byte, error) {
	line, err := json.Marshal(payload)
	if err != nil {
		return nil, fmt.Errorf("clilog: encode record: %w", err)
	}
	return append(line, '\n'), nil
}

// FormatCommand builds a readable representation of the command the runtime
// hands to the OS: profile-configured environment references (kept as ${VAR},
// never expanded), the working directory, and the resolved executable with its
// arguments. Output form:
//
//	KEY=${VAR} ... && cd <cwd> && <path> <arg> ...
//
// A nil env value (profile unsets the variable) is rendered as KEY=.
func FormatCommand(env map[string]*string, cwd, path string, argv []string) string {
	var builder strings.Builder
	keys := make([]string, 0, len(env))
	for name := range env {
		keys = append(keys, name)
	}
	sort.Strings(keys)
	for _, name := range keys {
		if builder.Len() > 0 {
			builder.WriteByte(' ')
		}
		builder.WriteString(name)
		builder.WriteByte('=')
		if value := env[name]; value != nil {
			builder.WriteString(*value)
		}
	}
	if cwd != "" {
		if builder.Len() > 0 {
			builder.WriteString(" && ")
		}
		builder.WriteString("cd ")
		builder.WriteString(quoteArg(cwd))
	}
	if builder.Len() > 0 {
		builder.WriteString(" && ")
	}
	builder.WriteString(quoteArg(path))
	for _, arg := range argv {
		builder.WriteByte(' ')
		builder.WriteString(quoteArg(arg))
	}
	return builder.String()
}

// SourceFromArgs renders the original sn-cli command line from the process
// argv. args[0] is replaced with the literal program name "sn-cli"; the shell's
// original quoting is not recoverable from argv, so arguments are joined by
// spaces.
func SourceFromArgs(args []string) string {
	if len(args) <= 1 {
		return "sn-cli"
	}
	return "sn-cli " + strings.Join(args[1:], " ")
}

// quoteArg wraps arg in double quotes when it contains characters that would
// blur argument boundaries in the readable command string.
func quoteArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if strings.ContainsAny(arg, " \t\"'\\$`") {
		return "\"" + strings.ReplaceAll(arg, "\"", "\\\"") + "\""
	}
	return arg
}
