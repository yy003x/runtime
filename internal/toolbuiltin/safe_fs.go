package toolbuiltin

import (
	"crypto/rand"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"
)

const safeDirectoryOpenFlags = unix.O_RDONLY |
	unix.O_DIRECTORY |
	unix.O_CLOEXEC |
	unix.O_NOFOLLOW

var (
	errWorkspaceRootChanged = errors.New("workspace root identity changed")
	errPathNotRegular       = errors.New("path is not a regular file")
	errReadHardlink         = errors.New("file has multiple hard links")
	errFileTooLarge         = errors.New("file exceeds the read limit")
	errDirectoryTooLarge    = errors.New("directory exceeds the listing limit")
	errWriteTargetInvalid   = errors.New("write target must be a regular file, not a symlink")
	errWriteTargetChanged   = errors.New("write target changed during operation")
)

type resolverTestHooks struct {
	afterRootOpened       func()
	afterDirectoryOpened  func(string)
	afterReadLeafOpened   func()
	afterWriteTempCreated func() error
	afterWriteFileSynced  func()
	beforeWritePublish    func()
	syncWriteDirectory    func(int) error
	afterWriteDirSynced   func()
}

type resolvedWorkspacePath struct {
	root       *workspaceRoot
	components []string
}

type fileIdentity struct {
	exists bool
	device uint64
	inode  uint64
	mode   uint32
}

func workspaceRootIdentity(
	path string,
) (uint64, uint64, error) {
	var stat unix.Stat_t
	if err := unix.Stat(path, &stat); err != nil {
		return 0, 0, err
	}
	if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFDIR {
		return 0, 0, fmt.Errorf("workspace root must be a directory")
	}
	return uint64(stat.Dev), uint64(stat.Ino), nil
}

func statIdentity(stat unix.Stat_t) fileIdentity {
	return fileIdentity{
		exists: true,
		device: uint64(stat.Dev),
		inode:  uint64(stat.Ino),
		mode:   uint32(stat.Mode),
	}
}

func sameFileIdentity(left, right fileIdentity) bool {
	return left.exists == right.exists &&
		(!left.exists ||
			left.device == right.device &&
				left.inode == right.inode &&
				left.mode&unix.S_IFMT == right.mode&unix.S_IFMT)
}

func (resolver *resolver) resolveWorkspacePath(
	value string,
) (resolvedWorkspacePath, error) {
	if strings.TrimSpace(value) == "" {
		return resolvedWorkspacePath{}, errPathRequired
	}
	path := value
	if !filepath.IsAbs(path) {
		path = filepath.Join(resolver.cwd, path)
	}
	absolute, err := filepath.Abs(path)
	if err != nil {
		return resolvedWorkspacePath{}, err
	}
	absolute = filepath.Clean(absolute)
	for index := range resolver.roots {
		root := &resolver.roots[index]
		for _, base := range []string{root.canonical, root.lexical} {
			relative, err := filepath.Rel(base, absolute)
			if err != nil || relative == ".." ||
				strings.HasPrefix(
					relative,
					".."+string(os.PathSeparator),
				) {
				continue
			}
			components, err := safePathComponents(relative)
			if err != nil {
				return resolvedWorkspacePath{}, err
			}
			return resolvedWorkspacePath{
				root: root, components: components,
			}, nil
		}
	}
	return resolvedWorkspacePath{}, errOutsideWorkspace
}

func safePathComponents(relative string) ([]string, error) {
	if relative == "." || relative == "" {
		return nil, nil
	}
	values := strings.Split(relative, string(os.PathSeparator))
	for _, value := range values {
		if value == "" || value == "." || value == ".." ||
			strings.ContainsRune(value, 0) {
			return nil, errOutsideWorkspace
		}
	}
	return values, nil
}

func (resolver *resolver) openPinnedRoot(root *workspaceRoot) (int, error) {
	fd, err := unix.Open(root.canonical, safeDirectoryOpenFlags, 0)
	if err != nil {
		return -1, normalizeNoFollowError(err)
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFDIR ||
		uint64(stat.Dev) != root.device ||
		uint64(stat.Ino) != root.inode {
		_ = unix.Close(fd)
		return -1, errWorkspaceRootChanged
	}
	if resolver.testHooks != nil &&
		resolver.testHooks.afterRootOpened != nil {
		resolver.testHooks.afterRootOpened()
	}
	return fd, nil
}

func (resolver *resolver) openPinnedDirectory(
	root *workspaceRoot,
	components []string,
) (int, error) {
	current, err := resolver.openPinnedRoot(root)
	if err != nil {
		return -1, err
	}
	for _, component := range components {
		if err := rejectVisibleSymlink(current, component); err != nil {
			_ = unix.Close(current)
			return -1, err
		}
		next, err := unix.Openat(
			current,
			component,
			safeDirectoryOpenFlags,
			0,
		)
		if err != nil {
			_ = unix.Close(current)
			return -1, normalizeNoFollowError(err)
		}
		var stat unix.Stat_t
		if err := unix.Fstat(next, &stat); err != nil {
			_ = unix.Close(next)
			_ = unix.Close(current)
			return -1, err
		}
		if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFDIR {
			_ = unix.Close(next)
			_ = unix.Close(current)
			return -1, errPathNotDirectory
		}
		if err := unix.Close(current); err != nil {
			_ = unix.Close(next)
			return -1, err
		}
		current = next
		if resolver.testHooks != nil &&
			resolver.testHooks.afterDirectoryOpened != nil {
			resolver.testHooks.afterDirectoryOpened(component)
		}
	}
	return current, nil
}

func rejectVisibleSymlink(parentFD int, name string) error {
	var visible unix.Stat_t
	err := unix.Fstatat(
		parentFD,
		name,
		&visible,
		unix.AT_SYMLINK_NOFOLLOW,
	)
	if err != nil {
		return err
	}
	if uint32(visible.Mode)&unix.S_IFMT == unix.S_IFLNK {
		return errSymlinkNotAllowed
	}
	return nil
}

func normalizeNoFollowError(err error) error {
	switch {
	case errors.Is(err, unix.ELOOP):
		return errSymlinkNotAllowed
	case errors.Is(err, unix.ENOTDIR):
		return errPathNotDirectory
	default:
		return err
	}
}

func (resolver *resolver) openReadFile(
	path resolvedWorkspacePath,
) (*os.File, unix.Stat_t, error) {
	if len(path.components) == 0 {
		fd, err := resolver.openPinnedRoot(path.root)
		if err != nil {
			return nil, unix.Stat_t{}, err
		}
		return resolver.finishReadFileOpen(fd)
	}
	parent, err := resolver.openPinnedDirectory(
		path.root,
		path.components[:len(path.components)-1],
	)
	if err != nil {
		return nil, unix.Stat_t{}, err
	}
	leaf := path.components[len(path.components)-1]
	if err := rejectVisibleSymlink(parent, leaf); err != nil {
		_ = unix.Close(parent)
		return nil, unix.Stat_t{}, err
	}
	fd, err := unix.Openat(
		parent,
		leaf,
		unix.O_RDONLY|
			unix.O_NONBLOCK|
			unix.O_CLOEXEC|
			unix.O_NOFOLLOW,
		0,
	)
	closeErr := unix.Close(parent)
	if err != nil {
		return nil, unix.Stat_t{}, normalizeNoFollowError(err)
	}
	if closeErr != nil {
		_ = unix.Close(fd)
		return nil, unix.Stat_t{}, closeErr
	}
	return resolver.finishReadFileOpen(fd)
}

func (resolver *resolver) finishReadFileOpen(
	fd int,
) (*os.File, unix.Stat_t, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		_ = unix.Close(fd)
		return nil, unix.Stat_t{}, err
	}
	if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFREG {
		_ = unix.Close(fd)
		return nil, unix.Stat_t{}, errPathNotRegular
	}
	if uint64(stat.Nlink) != 1 {
		_ = unix.Close(fd)
		return nil, unix.Stat_t{}, errReadHardlink
	}
	if stat.Size > maxToolOutputBytes {
		_ = unix.Close(fd)
		return nil, unix.Stat_t{}, errFileTooLarge
	}
	if resolver.testHooks != nil &&
		resolver.testHooks.afterReadLeafOpened != nil {
		resolver.testHooks.afterReadLeafOpened()
	}
	file := os.NewFile(uintptr(fd), "read_file")
	if file == nil {
		_ = unix.Close(fd)
		return nil, unix.Stat_t{}, fmt.Errorf("adopt read file descriptor")
	}
	return file, stat, nil
}

func boundedRead(file *os.File) ([]byte, error) {
	data, err := io.ReadAll(io.LimitReader(file, maxToolOutputBytes+1))
	if err != nil {
		return nil, err
	}
	if len(data) > maxToolOutputBytes {
		return nil, errFileTooLarge
	}
	return data, nil
}

func (resolver *resolver) openListDirectory(
	path resolvedWorkspacePath,
) (*os.File, error) {
	fd, err := resolver.openPinnedDirectory(path.root, path.components)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), "list_directory")
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("adopt directory file descriptor")
	}
	return file, nil
}

func boundedDirectoryEntries(
	directory *os.File,
) ([]os.DirEntry, error) {
	const batchSize = 256
	entries := make([]os.DirEntry, 0, batchSize)
	estimatedJSONBytes := 2
	for {
		batch, err := directory.ReadDir(batchSize)
		for _, entry := range batch {
			// JSON may expand every name byte into a six-byte escape. Keep a
			// conservative fixed allowance for the object keys and type value
			// so memory is bounded before the final exact marshal check.
			addition := 64 + 6*len(entry.Name())
			if addition > maxToolOutputBytes ||
				estimatedJSONBytes > maxToolOutputBytes-addition {
				return nil, errDirectoryTooLarge
			}
			estimatedJSONBytes += addition
			entries = append(entries, entry)
		}
		switch {
		case errors.Is(err, io.EOF):
			return entries, nil
		case err != nil:
			return nil, err
		case len(batch) == 0:
			return entries, nil
		}
	}
}

func inspectWriteTarget(
	parentFD int,
	leaf string,
) (fileIdentity, error) {
	var stat unix.Stat_t
	err := unix.Fstatat(
		parentFD,
		leaf,
		&stat,
		unix.AT_SYMLINK_NOFOLLOW,
	)
	if errors.Is(err, unix.ENOENT) {
		return fileIdentity{}, nil
	}
	if err != nil {
		return fileIdentity{}, err
	}
	if uint32(stat.Mode)&unix.S_IFMT == unix.S_IFLNK ||
		uint32(stat.Mode)&unix.S_IFMT != unix.S_IFREG {
		return fileIdentity{}, errWriteTargetInvalid
	}
	return statIdentity(stat), nil
}

func (resolver *resolver) writeFileAt(
	path resolvedWorkspacePath,
	content string,
) error {
	if len(path.components) == 0 {
		return errWriteTargetInvalid
	}
	parent, err := resolver.openPinnedDirectory(
		path.root,
		path.components[:len(path.components)-1],
	)
	if err != nil {
		return err
	}
	defer unix.Close(parent) //nolint:errcheck
	leaf := path.components[len(path.components)-1]
	initialTarget, err := inspectWriteTarget(parent, leaf)
	if err != nil {
		return err
	}
	tempName, tempFD, err := createWriteTemp(parent)
	if err != nil {
		return err
	}
	published := false
	defer func() {
		if !published {
			_ = unix.Unlinkat(parent, tempName, 0)
		}
	}()
	if resolver.testHooks != nil &&
		resolver.testHooks.afterWriteTempCreated != nil {
		if err := resolver.testHooks.afterWriteTempCreated(); err != nil {
			_ = unix.Close(tempFD)
			return err
		}
	}
	temp := os.NewFile(uintptr(tempFD), tempName)
	if temp == nil {
		_ = unix.Close(tempFD)
		return fmt.Errorf("adopt write file descriptor")
	}
	if _, err := io.WriteString(temp, content); err != nil {
		_ = temp.Close()
		return err
	}
	if err := temp.Sync(); err != nil {
		_ = temp.Close()
		return err
	}
	if resolver.testHooks != nil &&
		resolver.testHooks.afterWriteFileSynced != nil {
		resolver.testHooks.afterWriteFileSynced()
	}
	if err := temp.Close(); err != nil {
		return err
	}
	if resolver.testHooks != nil &&
		resolver.testHooks.beforeWritePublish != nil {
		resolver.testHooks.beforeWritePublish()
	}
	currentTarget, err := inspectWriteTarget(parent, leaf)
	if err != nil {
		return err
	}
	if !sameFileIdentity(initialTarget, currentTarget) {
		return errWriteTargetChanged
	}
	if err := unix.Renameat(parent, tempName, parent, leaf); err != nil {
		return err
	}
	published = true
	syncDirectory := unix.Fsync
	if resolver.testHooks != nil &&
		resolver.testHooks.syncWriteDirectory != nil {
		syncDirectory = resolver.testHooks.syncWriteDirectory
	}
	if err := syncDirectory(parent); err != nil {
		return err
	}
	if resolver.testHooks != nil &&
		resolver.testHooks.afterWriteDirSynced != nil {
		resolver.testHooks.afterWriteDirSynced()
	}
	return nil
}

func createWriteTemp(parentFD int) (string, int, error) {
	for attempt := 0; attempt < 16; attempt++ {
		random := make([]byte, 16)
		if _, err := rand.Read(random); err != nil {
			return "", -1, fmt.Errorf("generate write temp name: %w", err)
		}
		name := ".runtime-tool-" + hex.EncodeToString(random) + ".tmp"
		fd, err := unix.Openat(
			parentFD,
			name,
			unix.O_WRONLY|
				unix.O_CREAT|
				unix.O_EXCL|
				unix.O_CLOEXEC|
				unix.O_NOFOLLOW,
			0o600,
		)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", -1, err
		}
		if err := unix.Fchmod(fd, 0o600); err != nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(parentFD, name, 0)
			return "", -1, err
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(parentFD, name, 0)
			return "", -1, err
		}
		if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFREG ||
			uint64(stat.Nlink) != 1 ||
			uint32(stat.Mode)&0o777 != 0o600 {
			_ = unix.Close(fd)
			_ = unix.Unlinkat(parentFD, name, 0)
			return "", -1, fmt.Errorf(
				"write temp is not a private regular file",
			)
		}
		return name, fd, nil
	}
	return "", -1, fmt.Errorf("allocate unique write temp")
}
