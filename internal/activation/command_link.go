package activation

import (
	"errors"
	"fmt"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

// EnsureCommandLink creates linkPath without replacing an existing filesystem
// entry. An existing symlink is accepted only when it already has target as its
// exact payload. The parent directory is opened without following a symlink so
// validation and creation use one stable directory descriptor.
func EnsureCommandLink(linkPath string, target string) error {
	fd, name, err := openCommandLinkParent(linkPath, target)
	if err != nil {
		return err
	}
	defer unix.Close(fd)

	if err := verifyCommandLinkAt(fd, name, target); err == nil {
		return nil
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	if err := unix.Symlinkat(target, fd, name); err != nil {
		if errors.Is(err, unix.EEXIST) {
			return verifyCommandLinkAt(fd, name, target)
		}
		return fmt.Errorf("create command link: %w", err)
	}
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("sync command link directory: %w", err)
	}
	return verifyCommandLinkAt(fd, name, target)
}

// ValidateCommandLink performs the same path and existing-entry validation as
// EnsureCommandLink without creating a missing link.
func ValidateCommandLink(linkPath string, target string) error {
	fd, name, err := openCommandLinkParent(linkPath, target)
	if err != nil {
		return err
	}
	defer unix.Close(fd)
	if err := verifyCommandLinkAt(fd, name, target); err != nil &&
		!errors.Is(err, unix.ENOENT) {
		return err
	}
	return nil
}

func openCommandLinkParent(
	linkPath string,
	target string,
) (int, string, error) {
	if !filepath.IsAbs(linkPath) || filepath.Clean(linkPath) != linkPath {
		return -1, "", fmt.Errorf(
			"command link must be an absolute clean path",
		)
	}
	if !filepath.IsAbs(target) || filepath.Clean(target) != target {
		return -1, "", fmt.Errorf(
			"command link target must be an absolute clean path",
		)
	}
	parent := filepath.Dir(linkPath)
	name := filepath.Base(linkPath)
	if name == "." || name == string(filepath.Separator) {
		return -1, "", fmt.Errorf("command link must name a file")
	}
	fd, err := openDirectoryNoFollow(parent)
	if err != nil {
		return -1, "", fmt.Errorf("open command link directory: %w", err)
	}
	return fd, name, nil
}

func openDirectoryNoFollow(path string) (int, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return -1, fmt.Errorf("directory must be an absolute clean path")
	}
	flags := unix.O_RDONLY | unix.O_DIRECTORY |
		unix.O_CLOEXEC | unix.O_NOFOLLOW
	fd, err := unix.Open(string(filepath.Separator), flags, 0)
	if err != nil {
		return -1, err
	}
	for _, component := range strings.Split(
		strings.TrimPrefix(path, string(filepath.Separator)),
		string(filepath.Separator),
	) {
		if component == "" {
			continue
		}
		next, openErr := unix.Openat(fd, component, flags, 0)
		if openErr != nil {
			_ = unix.Close(fd)
			return -1, openErr
		}
		closeErr := unix.Close(fd)
		if closeErr != nil {
			_ = unix.Close(next)
			return -1, closeErr
		}
		fd = next
	}
	return fd, nil
}

func verifyCommandLinkAt(fd int, name string, target string) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW); err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFLNK {
		return fmt.Errorf("command link target already exists and is not a symlink")
	}
	buffer := make([]byte, 4096)
	count, err := unix.Readlinkat(fd, name, buffer)
	if err != nil {
		return fmt.Errorf("read command link: %w", err)
	}
	if count == len(buffer) {
		return fmt.Errorf("command link target is too long")
	}
	if string(buffer[:count]) != target {
		return fmt.Errorf("command link points outside this Runtime home")
	}
	return nil
}
