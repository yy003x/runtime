// Package executionlog writes best-effort local diagnostics for actual CLI
// Profile executions and API Provider calls. These files are not canonical
// Session or Run state and are never used for replay.
package executionlog

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/pkg/contract"
	"github.com/yy003x/runtime/pkg/provider"
)

type Namespace string

const (
	NamespaceDirect  Namespace = "direct"
	NamespaceExec    Namespace = "exec"
	NamespaceRequest Namespace = "req"
	NamespaceSession Namespace = "session"
	NamespaceAgent   Namespace = "agent"
	NamespaceTmux    Namespace = "tmux"
)

type CLIRecord struct {
	Time      time.Time
	Namespace Namespace
	Profile   string
	Source    string
	Command   string
}

type APIRecord struct {
	Time      time.Time
	Namespace Namespace
	Profile   string
	Source    string
	CallID    string
	Request   provider.Request
	Response  *provider.Response
	Error     *contract.RuntimeError
}

type cliRecordJSON struct {
	Time      string    `json:"time"`
	Namespace Namespace `json:"namespace"`
	Profile   string    `json:"profile"`
	Source    string    `json:"source"`
	Command   string    `json:"command"`
}

type apiRecordJSON struct {
	Time      string                 `json:"time"`
	Namespace Namespace              `json:"namespace"`
	Profile   string                 `json:"profile"`
	Source    string                 `json:"source"`
	CallID    string                 `json:"call_id"`
	Request   provider.Request       `json:"request"`
	Response  *provider.Response     `json:"response"`
	Error     *contract.RuntimeError `json:"error"`
}

var appendMu sync.Mutex

func AppendCLI(logDir string, record CLIRecord) error {
	payload := cliRecordJSON{
		Time:      record.Time.Format("2006-01-02 15:04:05"),
		Namespace: record.Namespace, Profile: record.Profile,
		Source: record.Source, Command: record.Command,
	}
	return appendRecord(logDir, record.Time, "cli.jsonl", payload)
}

func AppendAPI(logDir string, record APIRecord) error {
	payload := apiRecordJSON{
		Time:      record.Time.Format("2006-01-02 15:04:05"),
		Namespace: record.Namespace, Profile: record.Profile,
		Source: record.Source, CallID: record.CallID,
		Request: record.Request, Response: record.Response, Error: record.Error,
	}
	return appendRecord(logDir, record.Time, "api.jsonl", payload)
}

func appendRecord(logDir string, when time.Time, name string, payload any) error {
	if logDir == "" {
		return fmt.Errorf("executionlog: log directory is required")
	}
	line, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("executionlog: encode record: %w", err)
	}
	line = append(line, '\n')
	appendMu.Lock()
	defer appendMu.Unlock()
	return appendLine(logDir, when.Format("060102"), name, line)
}

func appendLine(logDir, day, name string, line []byte) error {
	rootFD, err := openLogRoot(logDir)
	if err != nil {
		return err
	}
	defer unix.Close(rootFD) //nolint:errcheck
	if err := unix.Mkdirat(rootFD, day, 0o700); err != nil && err != unix.EEXIST {
		return fmt.Errorf("executionlog: create day directory: %w", err)
	}
	dayFD, err := unix.Openat(
		rootFD, day,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return fmt.Errorf("executionlog: open day directory: %w", err)
	}
	defer unix.Close(dayFD) //nolint:errcheck
	if err := unix.Fchmod(dayFD, 0o700); err != nil {
		return fmt.Errorf("executionlog: protect day directory: %w", err)
	}
	fileFD, err := unix.Openat(
		dayFD, name,
		unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|
			unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0o600,
	)
	if err != nil {
		return fmt.Errorf("executionlog: open log file: %w", err)
	}
	file := os.NewFile(uintptr(fileFD), filepath.Join(logDir, day, name))
	if file == nil {
		_ = unix.Close(fileFD)
		return fmt.Errorf("executionlog: wrap log file")
	}
	defer file.Close() //nolint:errcheck
	var stat unix.Stat_t
	if err := unix.Fstat(fileFD, &stat); err != nil {
		return fmt.Errorf("executionlog: inspect log file: %w", err)
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return fmt.Errorf("executionlog: target must be a single-link regular file")
	}
	if err := unix.Fchmod(fileFD, 0o600); err != nil {
		return fmt.Errorf("executionlog: protect log file: %w", err)
	}
	if err := unix.Flock(fileFD, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		return fmt.Errorf("executionlog: lock log file: %w", err)
	}
	defer unix.Flock(fileFD, unix.LOCK_UN) //nolint:errcheck
	originalSize, err := file.Seek(0, 2)
	if err != nil {
		return fmt.Errorf("executionlog: inspect log size: %w", err)
	}
	if err := writeAll(file, line); err != nil {
		_ = file.Truncate(originalSize)
		return fmt.Errorf("executionlog: write log: %w", err)
	}
	return nil
}

func openLogRoot(logDir string) (int, error) {
	if err := unix.Mkdir(logDir, 0o700); err != nil && err != unix.EEXIST {
		return -1, fmt.Errorf("executionlog: create log root: %w", err)
	}
	rootFD, err := unix.Open(
		logDir, unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return -1, fmt.Errorf(
			"executionlog: open log root as a real directory: %w", err,
		)
	}
	if err := unix.Fchmod(rootFD, 0o700); err != nil {
		_ = unix.Close(rootFD)
		return -1, fmt.Errorf("executionlog: protect log root: %w", err)
	}
	return rootFD, nil
}

func writeAll(file *os.File, value []byte) error {
	for len(value) > 0 {
		written, err := file.Write(value)
		if err != nil {
			return err
		}
		if written == 0 {
			return fmt.Errorf("zero-byte write")
		}
		value = value[written:]
	}
	return nil
}

// FormatCommand builds a readable representation of the command handed to
// the OS while preserving Profile environment ${VAR} references.
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

func SourceFromArgs(args []string) string {
	if len(args) <= 1 {
		return "sn-cli"
	}
	return "sn-cli " + strings.Join(args[1:], " ")
}

func quoteArg(arg string) string {
	if arg == "" {
		return `""`
	}
	if strings.ContainsAny(arg, " \t\"'\\$`") {
		return "\"" + strings.ReplaceAll(arg, "\"", "\\\"") + "\""
	}
	return arg
}
