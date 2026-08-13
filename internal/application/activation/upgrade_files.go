package activation

import (
	"crypto/rand"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"

	"golang.org/x/sys/unix"
)

type fileLock struct {
	file *os.File
}

func (lock *fileLock) Close() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	_ = lock.file.Close()
}

func acquireUpgradeLock(path string) (*fileLock, error) {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return nil, err
	}
	fd, err := unix.Open(
		path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open activation lock %s: %w", path, err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil || !info.Mode().IsRegular() {
		_ = file.Close()
		return nil, fmt.Errorf("activation lock must be a regular file: %s", path)
	}
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		_ = file.Close()
		if errors.Is(err, unix.EWOULDBLOCK) ||
			errors.Is(err, unix.EAGAIN) {
			return nil, fmt.Errorf(
				"another Runtime maintenance operation is in progress",
			)
		}
		return nil, err
	}
	return &fileLock{file: file}, nil
}

func writeActivationGuard(path string, value []byte) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	file, err := os.OpenFile(
		path, os.O_WRONLY|os.O_CREATE|os.O_EXCL, 0o600,
	)
	if err != nil {
		return fmt.Errorf("create activation guard: %w", err)
	}
	if _, err := file.Write(value); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Sync(); err != nil {
		_ = file.Close()
		_ = os.Remove(path)
		return err
	}
	if err := file.Close(); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func copyMissingNames(source, target string) ([]string, error) {
	sourceFiles, err := regularRelativeFiles(source)
	if err != nil {
		return nil, err
	}
	copied := make([]string, 0, len(sourceFiles))
	for _, relative := range sourceFiles {
		destination := filepath.Join(target, filepath.FromSlash(relative))
		if _, err := os.Lstat(destination); err == nil {
			continue
		} else if !errors.Is(err, os.ErrNotExist) {
			return nil, err
		}
		sourcePath := filepath.Join(source, filepath.FromSlash(relative))
		info, err := os.Stat(sourcePath)
		if err != nil {
			return nil, err
		}
		if err := copyRegular(
			sourcePath, destination, info.Mode().Perm(),
		); err != nil {
			return nil, err
		}
		copied = append(copied, relative)
	}
	return copied, nil
}

func copyTree(source, target string, overwrite bool) error {
	if overwrite {
		if err := os.RemoveAll(target); err != nil {
			return err
		}
		if err := os.MkdirAll(target, 0o700); err != nil {
			return err
		}
	}
	return filepath.WalkDir(
		source,
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(source, path)
			if err != nil || relative == "." {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("source tree contains symlink: %s", path)
			}
			destination := filepath.Join(target, relative)
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if entry.IsDir() {
				return os.MkdirAll(destination, 0o700)
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf("source tree contains unsupported file: %s", path)
			}
			if !overwrite {
				if _, err := os.Lstat(destination); err == nil {
					return nil
				} else if !errors.Is(err, os.ErrNotExist) {
					return err
				}
			}
			return copyRegular(path, destination, info.Mode().Perm())
		},
	)
}

func copyRegular(source, target string, mode fs.FileMode) error {
	info, err := os.Lstat(source)
	if err != nil || info.Mode()&os.ModeSymlink != 0 ||
		!info.Mode().IsRegular() {
		return fmt.Errorf("copy source must be a regular file: %s", source)
	}
	if err := os.MkdirAll(filepath.Dir(target), 0o700); err != nil {
		return err
	}
	fd, err := unix.Open(
		source, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return err
	}
	input := os.NewFile(uintptr(fd), source)
	if input == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("open copy source %s", source)
	}
	defer input.Close()
	openedInfo, err := input.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() ||
		!os.SameFile(info, openedInfo) {
		return fmt.Errorf("copy source changed while opening: %s", source)
	}
	output, err := os.OpenFile(
		target, os.O_WRONLY|os.O_CREATE|os.O_EXCL, mode,
	)
	if err != nil {
		return err
	}
	ok := false
	defer func() {
		_ = output.Close()
		if !ok {
			_ = os.Remove(target)
		}
	}()
	if _, err := io.Copy(output, input); err != nil {
		return err
	}
	if err := output.Sync(); err != nil {
		return err
	}
	if err := output.Close(); err != nil {
		return err
	}
	ok = true
	return nil
}

func validateTree(root string) error {
	return filepath.WalkDir(
		root,
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("payload contains symlink: %s", path)
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if !info.IsDir() && !info.Mode().IsRegular() {
				return fmt.Errorf("payload contains unsupported file: %s", path)
			}
			return nil
		},
	)
}

func regularRelativeFiles(root string) ([]string, error) {
	var values []string
	err := filepath.WalkDir(
		root,
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(root, path)
			if err != nil || relative == "." {
				return err
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("source tree contains symlink: %s", path)
			}
			if entry.IsDir() {
				return nil
			}
			info, err := entry.Info()
			if err != nil || !info.Mode().IsRegular() {
				return fmt.Errorf("source tree contains unsupported file: %s", path)
			}
			values = append(values, filepath.ToSlash(relative))
			return nil
		},
	)
	sort.Strings(values)
	return values, err
}

func treeDigest(path string) (string, error) {
	hash := sha256.New()
	info, err := os.Lstat(path)
	if err != nil {
		return "", err
	}
	if info.Mode()&os.ModeSymlink != 0 {
		return "", fmt.Errorf("cannot digest symlink: %s", path)
	}
	if info.Mode().IsRegular() {
		if err := digestRegularFile(hash, ".", path, info); err != nil {
			return "", err
		}
		return hex.EncodeToString(hash.Sum(nil)), nil
	}
	if !info.IsDir() {
		return "", fmt.Errorf("cannot digest unsupported path: %s", path)
	}
	err = filepath.WalkDir(
		path,
		func(entryPath string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			relative, err := filepath.Rel(path, entryPath)
			if err != nil {
				return err
			}
			info, err := entry.Info()
			if err != nil {
				return err
			}
			if info.Mode()&os.ModeSymlink != 0 {
				return fmt.Errorf("cannot digest symlink: %s", entryPath)
			}
			relative = filepath.ToSlash(relative)
			if info.IsDir() {
				_, _ = fmt.Fprintf(
					hash, "dir\x00%s\x00%04o\x00",
					relative, info.Mode().Perm(),
				)
				return nil
			}
			if !info.Mode().IsRegular() {
				return fmt.Errorf(
					"cannot digest unsupported path: %s", entryPath,
				)
			}
			return digestRegularFile(
				hash, relative, entryPath, info,
			)
		},
	)
	if err != nil {
		return "", err
	}
	return hex.EncodeToString(hash.Sum(nil)), nil
}

func digestRegularFile(
	hash io.Writer,
	relative, path string,
	expected os.FileInfo,
) error {
	fd, err := unix.Open(
		path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return fmt.Errorf("open digest file %s", path)
	}
	defer file.Close()
	actual, err := file.Stat()
	if err != nil {
		return err
	}
	if !actual.Mode().IsRegular() || !os.SameFile(expected, actual) {
		return fmt.Errorf("digest file changed while reading: %s", path)
	}
	if _, err := fmt.Fprintf(
		hash, "file\x00%s\x00%04o\x00", relative, actual.Mode().Perm(),
	); err != nil {
		return err
	}
	if _, err := io.Copy(hash, file); err != nil {
		return err
	}
	_, err = io.WriteString(hash, "\x00")
	return err
}

func atomicWriteFile(path string, value []byte, mode fs.FileMode) error {
	if err := os.MkdirAll(filepath.Dir(path), 0o700); err != nil {
		return err
	}
	temp, err := os.CreateTemp(filepath.Dir(path), ".activation-write-*")
	if err != nil {
		return err
	}
	tempPath := temp.Name()
	defer os.Remove(tempPath)
	if err := temp.Chmod(mode); err != nil {
		_ = temp.Close()
		return err
	}
	if _, err := temp.Write(value); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if err := os.Rename(tempPath, path); err != nil {
		return err
	}
	return syncDirectory(filepath.Dir(path))
}

func readRegular(path string, limit int64) ([]byte, error) {
	info, err := os.Lstat(path)
	if err != nil {
		return nil, err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.Mode().IsRegular() ||
		info.Size() > limit {
		return nil, fmt.Errorf("%s must be a bounded regular file", path)
	}
	fd, err := unix.Open(
		path, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), path)
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open regular file %s", path)
	}
	defer file.Close()
	openedInfo, err := file.Stat()
	if err != nil || !openedInfo.Mode().IsRegular() ||
		!os.SameFile(info, openedInfo) {
		return nil, fmt.Errorf("%s changed while opening", path)
	}
	value, err := io.ReadAll(io.LimitReader(file, limit+1))
	if err != nil || int64(len(value)) > limit {
		return nil, fmt.Errorf("%s exceeds read limit", path)
	}
	return value, nil
}

func ensurePrivateDirectory(path string) error {
	if err := os.MkdirAll(path, 0o700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if info.Mode()&os.ModeSymlink != 0 || !info.IsDir() {
		return fmt.Errorf("%s must be a directory, not a symlink", path)
	}
	if info.Mode().Perm()&0o077 != 0 {
		return fmt.Errorf("%s must not be accessible by group or others", path)
	}
	return nil
}

func removeTransactionPath(path, root string) error {
	if !pathWithin(path, root) || filepath.Clean(path) == filepath.Clean(root) {
		return fmt.Errorf("refusing unsafe transaction removal: %s", path)
	}
	return os.RemoveAll(path)
}

func pathWithin(path, root string) bool {
	path = filepath.Clean(path)
	root = filepath.Clean(root)
	relative, err := filepath.Rel(root, path)
	return err == nil && relative != "." && relative != "" &&
		relative != ".." &&
		!strings.HasPrefix(relative, ".."+string(filepath.Separator)) &&
		!filepath.IsAbs(relative)
}

func randomNonce() (string, error) {
	value := make([]byte, 16)
	if _, err := io.ReadFull(rand.Reader, value); err != nil {
		return "", err
	}
	return hex.EncodeToString(value), nil
}

func syncDirectory(path string) error {
	file, err := os.Open(path)
	if err != nil {
		return err
	}
	defer file.Close()
	return file.Sync()
}

func durableRename(source, target string) error {
	return durableRenameWith(source, target, os.Rename, syncDirectory)
}

func durableRenameWith(
	source, target string,
	rename func(string, string) error,
	syncDir func(string) error,
) error {
	if err := rename(source, target); err != nil {
		return err
	}
	sourceDir := filepath.Dir(source)
	targetDir := filepath.Dir(target)
	if err := syncDir(targetDir); err != nil {
		return fmt.Errorf("persist rename target directory: %w", err)
	}
	if sourceDir != targetDir {
		if err := syncDir(sourceDir); err != nil {
			return fmt.Errorf("persist rename source directory: %w", err)
		}
	}
	return nil
}

func syncTree(root string) error {
	var directories []string
	err := filepath.WalkDir(
		root,
		func(path string, entry fs.DirEntry, walkErr error) error {
			if walkErr != nil {
				return walkErr
			}
			if entry.Type()&os.ModeSymlink != 0 {
				return fmt.Errorf("activation stage contains symlink: %s", path)
			}
			if entry.IsDir() {
				directories = append(directories, path)
			}
			return nil
		},
	)
	if err != nil {
		return err
	}
	sort.Slice(directories, func(left, right int) bool {
		return len(directories[left]) > len(directories[right])
	})
	for _, directory := range directories {
		if err := syncDirectory(directory); err != nil {
			return err
		}
	}
	return nil
}
