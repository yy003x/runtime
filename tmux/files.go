package tmux

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"
	"syscall"

	"golang.org/x/sys/unix"
)

func canonicalHome(value string) (string, error) {
	value = strings.TrimSpace(value)
	if value == "" {
		return "", fmt.Errorf("runtime home is required")
	}
	absolute, err := filepath.Abs(value)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if info, statErr := os.Lstat(absolute); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("runtime home must not be a symlink")
		}
		resolved, resolveErr := filepath.EvalSymlinks(absolute)
		if resolveErr != nil {
			return "", resolveErr
		}
		return filepath.Clean(resolved), nil
	} else if !os.IsNotExist(statErr) {
		return "", statErr
	}
	var suffix []string
	ancestor := absolute
	for {
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", fmt.Errorf("runtime home has no existing ancestor")
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
		info, statErr := os.Lstat(ancestor)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink == 0 && !info.IsDir() {
				return "", fmt.Errorf(
					"runtime home parent is not a directory: %s", ancestor,
				)
			}
			resolved, resolveErr := filepath.EvalSymlinks(ancestor)
			if resolveErr != nil {
				return "", resolveErr
			}
			ancestor = resolved
			break
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
	}
	for index := len(suffix) - 1; index >= 0; index-- {
		ancestor = filepath.Join(ancestor, suffix[index])
	}
	return filepath.Clean(ancestor), nil
}

func digestString(value string) string {
	sum := sha256.Sum256([]byte(value))
	return hex.EncodeToString(sum[:])
}

func digestBytes(value []byte) string {
	sum := sha256.Sum256(value)
	return hex.EncodeToString(sum[:])
}

func readPrivateRegular(path string, limit int64, ownerUID int) ([]byte, error) {
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	info, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !info.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file", path)
	}
	if info.Mode().Perm() != 0o600 {
		return nil, fmt.Errorf("%s must have mode 0600", path)
	}
	if err := requireOwner(info, ownerUID, path); err != nil {
		return nil, err
	}
	if info.Size() > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return value, nil
}

func readSourceRegular(path string, limit int64) ([]byte, error) {
	before, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if before.Mode()&os.ModeSymlink != 0 || !before.Mode().IsRegular() {
		return nil, fmt.Errorf("%s must be a regular file, not a symlink", path)
	}
	if before.Size() > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	fd, err := unix.Open(path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	defer file.Close()
	after, err := file.Stat()
	if err != nil {
		return nil, err
	}
	if !after.Mode().IsRegular() || !os.SameFile(before, after) {
		return nil, fmt.Errorf("%s changed while opening", path)
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil {
		return nil, err
	}
	if int64(len(value)) > limit {
		return nil, fmt.Errorf("%s exceeds %d bytes", path, limit)
	}
	return value, nil
}

func atomicWritePrivate(path string, value []byte, ownerUID int) error {
	directory := filepath.Dir(path)
	if err := ensurePrivateDir(directory, ownerUID); err != nil {
		return err
	}
	if info, err := os.Lstat(path); err == nil {
		if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() {
			return fmt.Errorf("%s must be a regular file", path)
		}
		return fmt.Errorf("%s already exists", path)
	} else if !os.IsNotExist(err) {
		return err
	}
	temp, err := os.CreateTemp(directory, ".sn-tmux-write-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	ok := false
	defer func() {
		_ = temp.Close()
		if !ok {
			_ = os.Remove(tempPath)
		}
	}()
	if err := temp.Chmod(0o600); err != nil {
		return err
	}
	if _, err := temp.Write(value); err != nil {
		return err
	}
	if err := temp.Sync(); err != nil {
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Link(tempPath, path); err != nil {
		return err
	}
	ok = true
	_ = os.Remove(tempPath)
	return nil
}

func writeJSONPrivate(path string, value any, ownerUID int) error {
	data, err := json.Marshal(value)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return atomicWritePrivate(path, data, ownerUID)
}

func ensurePrivateDir(path string, ownerUID int) error {
	info, err := os.Lstat(path)
	if os.IsNotExist(err) {
		if err := os.MkdirAll(path, 0o700); err != nil {
			return err
		}
		info, err = os.Lstat(path)
	}
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s must be a directory, not a symlink", path)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s must have mode 0700", path)
	}
	return requireOwner(info, ownerUID, path)
}

func requirePrivateDir(path string, ownerUID int) error {
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s must be a directory, not a symlink", path)
	}
	if info.Mode().Perm() != 0o700 {
		return fmt.Errorf("%s must have mode 0700", path)
	}
	return requireOwner(info, ownerUID, path)
}

func requireOwner(info os.FileInfo, ownerUID int, path string) error {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return fmt.Errorf("cannot determine owner of %s", path)
	}
	if int(stat.Uid) != ownerUID {
		return fmt.Errorf("%s is owned by uid %d, expected %d", path, stat.Uid, ownerUID)
	}
	return nil
}

func removeExact(paths ...string) {
	for _, path := range paths {
		if path == "" {
			continue
		}
		_ = os.Remove(path)
	}
}

func decodePrivateJSON(path string, limit int64, ownerUID int, target any) error {
	data, err := readPrivateRegular(path, limit, ownerUID)
	if err != nil {
		return err
	}
	return strictDecode(data, target)
}

func executableIdentity(path string) (string, string, error) {
	canonical, err := filepath.EvalSymlinks(path)
	if err != nil {
		return "", "", err
	}
	canonical, err = filepath.Abs(canonical)
	if err != nil {
		return "", "", err
	}
	info, err := os.Stat(canonical)
	if err != nil {
		return "", "", err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm()&0o111 == 0 {
		return "", "", fmt.Errorf("%s must be a regular executable", canonical)
	}
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return "", "", fmt.Errorf("cannot identify executable %s", canonical)
	}
	return canonical, formatExecutableIdentity(canonical, info, stat), nil
}

func formatExecutableIdentity(
	path string,
	info os.FileInfo,
	stat *syscall.Stat_t,
) string {
	return fmt.Sprintf(
		"%s:%d:%d:%d:%d",
		path, stat.Dev, stat.Ino, info.Size(), info.ModTime().UnixNano(),
	)
}

func validatePathWithin(path, directory string) error {
	relative, err := filepath.Rel(directory, path)
	if err != nil {
		return err
	}
	if relative == "." || relative == "" ||
		relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) ||
		filepath.IsAbs(relative) {
		return fmt.Errorf("path %s is outside %s", path, directory)
	}
	return nil
}

func isNotExist(err error) bool {
	return err != nil && (os.IsNotExist(err) || errors.Is(err, unix.ENOENT))
}
