// Package executionlog writes best-effort local execution diagnostics and
// redacted control-plane audit records. These files are not canonical Session
// or Run state and are never used for replay.
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
	NamespaceRequest Namespace = "call"
	NamespaceSession Namespace = "session"
	NamespaceAgent   Namespace = "agent"
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

// AuditRecord contains only control-plane metadata. Callers must never put
// prompt content, send payloads, resolved secrets, or arbitrary argv in
// Targets.
type AuditRecord struct {
	Time       time.Time
	Source     string
	Namespace  string
	Action     string
	Outcome    string
	Targets    map[string]string
	ErrorCode  contract.ErrorCode
	ErrorPhase contract.ErrorPhase
	HTTPStatus int
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

type auditRecordJSON struct {
	SchemaVersion int                 `json:"schema_version"`
	Time          string              `json:"time"`
	Source        string              `json:"source"`
	Namespace     string              `json:"namespace"`
	Action        string              `json:"action"`
	Outcome       string              `json:"outcome"`
	Targets       map[string]string   `json:"targets,omitempty"`
	ErrorCode     contract.ErrorCode  `json:"error_code,omitempty"`
	ErrorPhase    contract.ErrorPhase `json:"error_phase,omitempty"`
	HTTPStatus    int                 `json:"http_status,omitempty"`
}

// appendLocks shards the in-process append lock by log file target so that
// concurrent writes to distinct files (a different day or a different name)
// no longer serialize each other. The shard lock only orders the
// Seek/Write/Truncate within a single process and a single file; cross-process
// exclusion still comes from the per-file Flock acquired in appendLine. The
// keyspace is bounded by active days times the fixed {cli,api,audit}.jsonl
// name set, so the map cannot grow unbounded during a process lifetime.
var appendLocks sync.Map

func appendLockFor(day, name string) *sync.Mutex {
	actual, _ := appendLocks.LoadOrStore(day+"\x00"+name, &sync.Mutex{})
	return actual.(*sync.Mutex)
}

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

// AppendAudit writes one redacted control-plane audit record. Audit failures
// are intentionally returned so doctor/tests can inspect them; normal callers
// treat them as best-effort and must not change the command result.
func AppendAudit(logDir string, record AuditRecord) error {
	payload := auditRecordJSON{
		SchemaVersion: 1,
		Time:          record.Time.UTC().Format(time.RFC3339Nano),
		Source:        record.Source,
		Namespace:     record.Namespace,
		Action:        record.Action,
		Outcome:       record.Outcome,
		Targets:       cloneTargets(record.Targets),
		ErrorCode:     record.ErrorCode,
		ErrorPhase:    record.ErrorPhase,
		HTTPStatus:    record.HTTPStatus,
	}
	return appendRecord(logDir, record.Time, "audit.jsonl", payload)
}

// OpenProcessLog opens a private append-only process log directly below the
// Runtime log root without following directory or file symlinks. The caller
// owns the returned file and may hand it to a child process.
func OpenProcessLog(logDir string, name string) (*os.File, error) {
	if name == "" || filepath.Base(name) != name || name == "." || name == ".." {
		return nil, fmt.Errorf("executionlog: invalid process log name")
	}
	rootFD, err := openLogRoot(logDir)
	if err != nil {
		return nil, err
	}
	defer unix.Close(rootFD) //nolint:errcheck
	fileFD, err := unix.Openat(
		rootFD, name,
		unix.O_WRONLY|unix.O_APPEND|unix.O_CREAT|unix.O_CLOEXEC|
			unix.O_NOFOLLOW|unix.O_NONBLOCK,
		0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("executionlog: open process log: %w", err)
	}
	file := os.NewFile(uintptr(fileFD), filepath.Join(logDir, name))
	if file == nil {
		_ = unix.Close(fileFD)
		return nil, fmt.Errorf("executionlog: wrap process log")
	}
	fail := func(err error) (*os.File, error) {
		_ = file.Close()
		return nil, err
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fileFD, &stat); err != nil {
		return fail(fmt.Errorf("executionlog: inspect process log: %w", err))
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG || stat.Nlink != 1 {
		return fail(fmt.Errorf(
			"executionlog: process log must be a single-link regular file",
		))
	}
	if err := unix.Fchmod(fileFD, 0o600); err != nil {
		return fail(fmt.Errorf("executionlog: protect process log: %w", err))
	}
	return file, nil
}

func cloneTargets(source map[string]string) map[string]string {
	if len(source) == 0 {
		return nil
	}
	result := make(map[string]string, len(source))
	for key, value := range source {
		if key != "" && value != "" {
			result[key] = value
		}
	}
	if len(result) == 0 {
		return nil
	}
	return result
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
	day := when.Format("060102")
	mu := appendLockFor(day, name)
	mu.Lock()
	defer mu.Unlock()
	return appendLine(logDir, day, name, line)
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
