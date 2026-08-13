package nativeconsole

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"unicode/utf8"

	"github.com/yy003x/runtime/internal/domain/identity"
	"github.com/yy003x/runtime/internal/infrastructure/layout"
	"golang.org/x/sys/unix"
)

const (
	SupervisorCommand = "__sn_native_tui_supervisor"
	manifestSchema    = 1
	maxManifestBytes  = 4 << 20
)

type targetInvocation struct {
	Path        string   `json:"path"`
	Argv        []string `json:"argv"`
	Environment []string `json:"environment"`
	CWD         string   `json:"cwd"`
}

type supervisorManifest struct {
	SchemaVersion int              `json:"schema_version"`
	Home          string           `json:"home"`
	SessionID     string           `json:"session_id"`
	RunID         string           `json:"run_id"`
	ExecutionID   string           `json:"execution_id"`
	Target        targetInvocation `json:"target"`
}

func writeSupervisorManifest(
	paths layout.Paths,
	value supervisorManifest,
) (string, string, error) {
	directory := filepath.Join(paths.StateDir, "native-tui-invocations")
	if err := os.MkdirAll(directory, 0o700); err != nil {
		return "", "", fmt.Errorf("create native_tui invocation directory: %w", err)
	}
	if err := os.Chmod(directory, 0o700); err != nil {
		return "", "", err
	}
	data, err := json.Marshal(value)
	if err != nil {
		return "", "", err
	}
	data = append(data, '\n')
	if len(data) > maxManifestBytes {
		return "", "", fmt.Errorf("native_tui invocation manifest is too large")
	}
	sum := sha256.Sum256(data)
	digest := hex.EncodeToString(sum[:])
	path := filepath.Join(directory, value.ExecutionID+".json")
	file, err := os.OpenFile(
		path,
		os.O_WRONLY|os.O_CREATE|os.O_EXCL|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return "", "", fmt.Errorf("create native_tui invocation manifest: %w", err)
	}
	writeErr := func() error {
		if _, err := file.Write(data); err != nil {
			return err
		}
		if err := file.Sync(); err != nil {
			return err
		}
		return file.Close()
	}()
	if writeErr != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return "", "", writeErr
	}
	if err := syncDirectory(directory); err != nil {
		_ = os.Remove(path)
		return "", "", err
	}
	return path, digest, nil
}

func readSupervisorManifest(
	path string,
	digest string,
) (supervisorManifest, layout.Paths, error) {
	var value supervisorManifest
	if !filepath.IsAbs(path) || len(digest) != sha256.Size*2 {
		return value, layout.Paths{}, fmt.Errorf("native_tui supervisor identity is invalid")
	}
	if _, err := hex.DecodeString(digest); err != nil {
		return value, layout.Paths{}, fmt.Errorf("native_tui supervisor digest is invalid")
	}
	file, err := os.OpenFile(path, os.O_RDONLY|unix.O_NOFOLLOW, 0)
	if err != nil {
		return value, layout.Paths{}, fmt.Errorf("open native_tui invocation manifest: %w", err)
	}
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return value, layout.Paths{}, err
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok || !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 ||
		stat.Uid != uint32(os.Getuid()) || stat.Nlink != 1 ||
		info.Size() <= 0 || info.Size() > maxManifestBytes {
		file.Close()
		return value, layout.Paths{}, fmt.Errorf(
			"native_tui invocation manifest is not a private regular file",
		)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxManifestBytes+1))
	closeErr := file.Close()
	if err != nil {
		return value, layout.Paths{}, err
	}
	if closeErr != nil {
		return value, layout.Paths{}, closeErr
	}
	sum := sha256.Sum256(data)
	if hex.EncodeToString(sum[:]) != strings.ToLower(digest) {
		return value, layout.Paths{}, fmt.Errorf(
			"native_tui invocation manifest digest mismatch",
		)
	}
	decoder := json.NewDecoder(bytes.NewReader(data))
	decoder.DisallowUnknownFields()
	if err := decoder.Decode(&value); err != nil {
		return value, layout.Paths{}, fmt.Errorf(
			"decode native_tui invocation manifest: %w", err,
		)
	}
	var trailing any
	if err := decoder.Decode(&trailing); err != io.EOF {
		return value, layout.Paths{}, fmt.Errorf(
			"native_tui invocation manifest has trailing JSON",
		)
	}
	paths, err := layout.FromHome(value.Home)
	if err != nil {
		return value, layout.Paths{}, err
	}
	expectedDirectory := filepath.Join(
		paths.StateDir, "native-tui-invocations",
	)
	if filepath.Clean(filepath.Dir(path)) != expectedDirectory ||
		filepath.Base(path) != value.ExecutionID+".json" {
		return value, layout.Paths{}, fmt.Errorf(
			"native_tui invocation manifest is outside Runtime state",
		)
	}
	if err := validateSupervisorManifest(value); err != nil {
		return value, layout.Paths{}, err
	}
	if err := os.Remove(path); err != nil {
		return value, layout.Paths{}, fmt.Errorf(
			"consume native_tui invocation manifest: %w", err,
		)
	}
	if err := syncDirectory(expectedDirectory); err != nil {
		return value, layout.Paths{}, err
	}
	return value, paths, nil
}

func validateSupervisorManifest(value supervisorManifest) error {
	if value.SchemaVersion != manifestSchema || value.Home == "" {
		return fmt.Errorf("native_tui invocation manifest is incomplete")
	}
	for _, pair := range []struct{ value, prefix string }{
		{value.SessionID, "session"}, {value.RunID, "run"},
		{value.ExecutionID, "execution"},
	} {
		if err := identity.Validate(pair.value, pair.prefix); err != nil {
			return err
		}
	}
	if !filepath.IsAbs(value.Target.Path) ||
		!filepath.IsAbs(value.Target.CWD) || len(value.Target.Argv) == 0 ||
		value.Target.Argv[0] == "" {
		return fmt.Errorf("native_tui target invocation is incomplete")
	}
	for _, current := range append(
		append([]string{}, value.Target.Argv...), value.Target.Environment...,
	) {
		if !utf8.ValidString(current) || strings.ContainsRune(current, '\x00') {
			return fmt.Errorf("native_tui target invocation is invalid")
		}
	}
	return nil
}

func removeSupervisorManifest(path string) {
	if path == "" {
		return
	}
	_ = os.Remove(path)
	_ = syncDirectory(filepath.Dir(path))
}

func syncDirectory(path string) error {
	directory, err := os.Open(path)
	if err != nil {
		return err
	}
	defer directory.Close()
	return directory.Sync()
}
