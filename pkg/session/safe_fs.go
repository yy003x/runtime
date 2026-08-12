package session

import (
	"bufio"
	"bytes"
	"crypto/rand"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"runtime"
	"strconv"
	"strings"
	"time"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/internal/infrastructure/strictjson"
)

const safeDirectoryOpenFlags = unix.O_RDONLY |
	unix.O_DIRECTORY |
	unix.O_CLOEXEC |
	unix.O_NOFOLLOW

type safeDirectory struct {
	fd   int
	path string
}

type safeDirectoryEntry struct {
	name  string
	mode  uint32
	size  int64
	dev   uint64
	ino   uint64
	nlink uint64
	mtime time.Time
}

type safeFileIdentity struct {
	dev uint64
	ino uint64
}

func encodeSafeFileIdentity(identity safeFileIdentity) string {
	return strconv.FormatUint(identity.dev, 16) + ":" +
		strconv.FormatUint(identity.ino, 16)
}

func decodeSafeFileIdentity(value string) (safeFileIdentity, error) {
	device, inode, found := strings.Cut(value, ":")
	if !found || device == "" || inode == "" {
		return safeFileIdentity{}, fmt.Errorf("invalid file identity")
	}
	dev, err := strconv.ParseUint(device, 16, 64)
	if err != nil {
		return safeFileIdentity{}, fmt.Errorf("invalid file identity: %w", err)
	}
	ino, err := strconv.ParseUint(inode, 16, 64)
	if err != nil {
		return safeFileIdentity{}, fmt.Errorf("invalid file identity: %w", err)
	}
	return safeFileIdentity{dev: dev, ino: ino}, nil
}

func openSafeDirectory(path string) (*safeDirectory, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf("safe directory path must be absolute and clean: %s", path)
	}
	fd, err := unix.Open(
		string(filepath.Separator), safeDirectoryOpenFlags, 0,
	)
	if err != nil {
		return nil, fmt.Errorf("open filesystem root: %w", err)
	}
	currentPath := string(filepath.Separator)
	for _, component := range strings.Split(
		strings.TrimPrefix(path, string(filepath.Separator)),
		string(filepath.Separator),
	) {
		if component == "" {
			continue
		}
		nextPath := filepath.Join(currentPath, component)
		next, openErr := unix.Openat(
			fd, component, safeDirectoryOpenFlags, 0,
		)
		if openErr != nil {
			_ = unix.Close(fd)
			return nil, fmt.Errorf(
				"open directory %s without following symlinks: %w",
				nextPath, openErr,
			)
		}
		if closeErr := unix.Close(fd); closeErr != nil {
			_ = unix.Close(next)
			return nil, closeErr
		}
		fd = next
		currentPath = nextPath
	}
	return &safeDirectory{fd: fd, path: path}, nil
}

func (directory *safeDirectory) close() error {
	if directory == nil || directory.fd < 0 {
		return nil
	}
	err := unix.Close(directory.fd)
	directory.fd = -1
	return err
}

func (directory *safeDirectory) sync() error {
	if err := unix.Fsync(directory.fd); err != nil {
		return fmt.Errorf("sync directory %s: %w", directory.path, err)
	}
	return nil
}

func (directory *safeDirectory) identity() (safeFileIdentity, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(directory.fd, &stat); err != nil {
		return safeFileIdentity{}, err
	}
	if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFDIR {
		return safeFileIdentity{}, fmt.Errorf("%s is not a directory", directory.path)
	}
	return safeFileIdentity{
		dev: uint64(stat.Dev),
		ino: uint64(stat.Ino),
	}, nil
}

func (directory *safeDirectory) openDirectory(
	relativePath string,
	create bool,
) (*safeDirectory, error) {
	return directory.openDirectoryWithCreateHook(
		relativePath, create, nil,
	)
}

func (directory *safeDirectory) openDirectoryWithCreateHook(
	relativePath string,
	create bool,
	beforePublish func() error,
	afterPublish ...func() error,
) (*safeDirectory, error) {
	parts, err := safeRelativeParts(relativePath, true)
	if err != nil {
		return nil, err
	}
	currentFD, err := unix.Openat(
		directory.fd, ".", safeDirectoryOpenFlags, 0,
	)
	if err != nil {
		return nil, fmt.Errorf("duplicate directory %s: %w", directory.path, err)
	}
	currentPath := directory.path
	for _, part := range parts {
		nextPath := filepath.Join(currentPath, part)
		nextFD, openErr := unix.Openat(
			currentFD, part, safeDirectoryOpenFlags, 0,
		)
		if errors.Is(openErr, unix.ENOENT) && create {
			tempName, tempIdentity, mkdirErr := createRandomDirectoryAt(
				currentFD, currentPath,
			)
			if mkdirErr != nil {
				_ = unix.Close(currentFD)
				return nil, fmt.Errorf("create directory %s: %w", nextPath, mkdirErr)
			}
			current := &safeDirectory{fd: currentFD, path: currentPath}
			published := false
			cleanup := func() {
				if !published {
					_ = current.removeEmptyDirectory(
						tempName, &tempIdentity,
					)
				}
			}
			if beforePublish != nil {
				if hookErr := beforePublish(); hookErr != nil {
					cleanup()
					_ = unix.Close(currentFD)
					return nil, hookErr
				}
			}
			if renameErr := renameNoReplaceAt(
				currentFD, tempName, currentFD, part,
			); renameErr != nil {
				cleanup()
				_ = unix.Close(currentFD)
				return nil, fmt.Errorf(
					"publish directory %s without replacement: %w",
					nextPath, renameErr,
				)
			}
			published = true
			if syncErr := unix.Fsync(currentFD); syncErr != nil {
				_ = unix.Close(currentFD)
				return nil, fmt.Errorf(
					"persist directory creation %s: %w", nextPath, syncErr,
				)
			}
			for _, hook := range afterPublish {
				if hook != nil {
					if hookErr := hook(); hookErr != nil {
						_ = unix.Close(currentFD)
						return nil, hookErr
					}
				}
			}
			nextFD, openErr = unix.Openat(
				currentFD, part, safeDirectoryOpenFlags, 0,
			)
			if openErr == nil {
				var visible unix.Stat_t
				if statErr := unix.Fstat(nextFD, &visible); statErr != nil {
					_ = unix.Close(nextFD)
					_ = unix.Close(currentFD)
					return nil, statErr
				}
				visibleIdentity := safeFileIdentity{
					dev: uint64(visible.Dev),
					ino: uint64(visible.Ino),
				}
				if uint32(visible.Mode)&unix.S_IFMT != unix.S_IFDIR ||
					visibleIdentity != tempIdentity {
					_ = unix.Close(nextFD)
					_ = unix.Close(currentFD)
					return nil, fmt.Errorf(
						"published directory %s changed identity",
						nextPath,
					)
				}
			}
		}
		if openErr != nil {
			_ = unix.Close(currentFD)
			return nil, fmt.Errorf(
				"open directory %s without following symlinks: %w",
				nextPath, openErr,
			)
		}
		if closeErr := unix.Close(currentFD); closeErr != nil {
			_ = unix.Close(nextFD)
			return nil, closeErr
		}
		currentFD = nextFD
		currentPath = nextPath
	}
	return &safeDirectory{fd: currentFD, path: currentPath}, nil
}

func createRandomDirectoryAt(
	parentFD int,
	parentPath string,
) (string, safeFileIdentity, error) {
	for attempt := 0; attempt < 100; attempt++ {
		name, err := randomSafeName(
			".runtime-directory-", ".tmp",
		)
		if err != nil {
			return "", safeFileIdentity{}, err
		}
		if err := unix.Mkdirat(parentFD, name, 0o700); errors.Is(err, unix.EEXIST) {
			continue
		} else if err != nil {
			return "", safeFileIdentity{}, err
		}
		var published unix.Stat_t
		if err := unix.Fstatat(
			parentFD, name, &published, unix.AT_SYMLINK_NOFOLLOW,
		); err != nil {
			return "", safeFileIdentity{}, err
		}
		identity := safeFileIdentity{
			dev: uint64(published.Dev),
			ino: uint64(published.Ino),
		}
		parent := &safeDirectory{fd: parentFD, path: parentPath}
		fd, err := unix.Openat(
			parentFD, name, safeDirectoryOpenFlags, 0,
		)
		if err != nil {
			_ = parent.removeEmptyDirectory(name, &identity)
			return "", safeFileIdentity{}, err
		}
		var stat unix.Stat_t
		statErr := unix.Fstat(fd, &stat)
		closeErr := unix.Close(fd)
		if statErr != nil || closeErr != nil {
			_ = parent.removeEmptyDirectory(name, &identity)
			return "", safeFileIdentity{}, errors.Join(statErr, closeErr)
		}
		openedIdentity := safeFileIdentity{
			dev: uint64(stat.Dev),
			ino: uint64(stat.Ino),
		}
		if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFDIR ||
			openedIdentity != identity {
			_ = parent.removeEmptyDirectory(name, &identity)
			return "", safeFileIdentity{}, fmt.Errorf(
				"temporary directory %s is not a directory",
				filepath.Join(parentPath, name),
			)
		}
		return name, identity, nil
	}
	return "", safeFileIdentity{}, fmt.Errorf(
		"allocate temporary directory in %s", parentPath,
	)
}

func (directory *safeDirectory) openParent(
	relativePath string,
	create bool,
) (*safeDirectory, string, error) {
	parts, err := safeRelativeParts(relativePath, false)
	if err != nil {
		return nil, "", err
	}
	name := parts[len(parts)-1]
	parentPath := "."
	if len(parts) > 1 {
		parentPath = filepath.Join(parts[:len(parts)-1]...)
	}
	parent, err := directory.openDirectory(parentPath, create)
	if err != nil {
		return nil, "", err
	}
	return parent, name, nil
}

func (directory *safeDirectory) statEntry(
	name string,
) (safeDirectoryEntry, error) {
	if err := safeName(name); err != nil {
		return safeDirectoryEntry{}, err
	}
	var stat unix.Stat_t
	if err := unix.Fstatat(
		directory.fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return safeDirectoryEntry{}, err
	}
	return safeDirectoryEntry{
		name:  name,
		mode:  uint32(stat.Mode),
		size:  stat.Size,
		dev:   uint64(stat.Dev),
		ino:   uint64(stat.Ino),
		nlink: uint64(stat.Nlink),
		mtime: statModifiedTime(stat),
	}, nil
}

func (directory *safeDirectory) stat(
	relativePath string,
) (safeDirectoryEntry, error) {
	parent, name, err := directory.openParent(relativePath, false)
	if err != nil {
		return safeDirectoryEntry{}, err
	}
	defer parent.close()
	return parent.statEntry(name)
}

func (entry safeDirectoryEntry) isDirectory() bool {
	return entry.mode&unix.S_IFMT == unix.S_IFDIR
}

func (entry safeDirectoryEntry) isRegular() bool {
	return entry.mode&unix.S_IFMT == unix.S_IFREG
}

func (entry safeDirectoryEntry) isSymlink() bool {
	return entry.mode&unix.S_IFMT == unix.S_IFLNK
}

func (entry safeDirectoryEntry) identity() safeFileIdentity {
	return safeFileIdentity{dev: entry.dev, ino: entry.ino}
}

func (entry safeDirectoryEntry) sameIdentity(other safeFileIdentity) bool {
	return entry.dev == other.dev && entry.ino == other.ino
}

func (directory *safeDirectory) entries() ([]safeDirectoryEntry, error) {
	duplicate, err := unix.Openat(
		directory.fd, ".", safeDirectoryOpenFlags, 0,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(duplicate), directory.path)
	if file == nil {
		_ = unix.Close(duplicate)
		return nil, fmt.Errorf("open directory stream %s", directory.path)
	}
	defer file.Close()
	rawEntries, err := file.ReadDir(-1)
	if err != nil {
		return nil, err
	}
	entries := make([]safeDirectoryEntry, 0, len(rawEntries))
	for _, rawEntry := range rawEntries {
		entry, statErr := directory.statEntry(rawEntry.Name())
		if statErr != nil {
			return nil, fmt.Errorf(
				"inspect directory entry %s: %w",
				filepath.Join(directory.path, rawEntry.Name()), statErr,
			)
		}
		entries = append(entries, entry)
	}
	return entries, nil
}

func (directory *safeDirectory) walk(
	relativePath string,
	visit func(string, safeDirectoryEntry) error,
) error {
	current, err := directory.openDirectory(relativePath, false)
	if err != nil {
		return err
	}
	entries, err := current.entries()
	closeErr := current.close()
	if err != nil {
		return err
	}
	if closeErr != nil {
		return closeErr
	}
	for _, entry := range entries {
		childPath := entry.name
		if relativePath != "." {
			childPath = filepath.Join(relativePath, entry.name)
		}
		if err := visit(childPath, entry); err != nil {
			return err
		}
		if entry.isDirectory() {
			if err := directory.walk(childPath, visit); err != nil {
				return err
			}
		}
	}
	return nil
}

func (directory *safeDirectory) readRegular(
	relativePath string,
	maxBytes int64,
) ([]byte, error) {
	data, _, err := directory.readRegularFact(relativePath, maxBytes)
	return data, err
}

func (directory *safeDirectory) readRegularFact(
	relativePath string,
	maxBytes int64,
) ([]byte, safeDirectoryEntry, error) {
	parent, name, err := directory.openParent(relativePath, false)
	if err != nil {
		return nil, safeDirectoryEntry{}, err
	}
	defer parent.close()
	fd, err := unix.Openat(
		parent.fd, name, unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0,
	)
	if err != nil {
		return nil, safeDirectoryEntry{}, fmt.Errorf(
			"open %s without following symlinks: %w",
			filepath.Join(parent.path, name), err,
		)
	}
	file := os.NewFile(uintptr(fd), filepath.Join(parent.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, safeDirectoryEntry{}, fmt.Errorf(
			"open regular file %s", filepath.Join(parent.path, name),
		)
	}
	defer file.Close()
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return nil, safeDirectoryEntry{}, err
	}
	entry := safeDirectoryEntry{
		name:  name,
		mode:  uint32(stat.Mode),
		size:  stat.Size,
		dev:   uint64(stat.Dev),
		ino:   uint64(stat.Ino),
		nlink: uint64(stat.Nlink),
		mtime: statModifiedTime(stat),
	}
	if !entry.isRegular() || entry.nlink != 1 {
		return nil, safeDirectoryEntry{}, fmt.Errorf(
			"%s must be a single-link regular file, not a symlink or hardlink",
			filepath.Join(parent.path, name),
		)
	}
	pathEntry, err := parent.statEntry(name)
	if err != nil {
		return nil, safeDirectoryEntry{}, err
	}
	if !pathEntry.sameIdentity(entry.identity()) ||
		!pathEntry.isRegular() ||
		pathEntry.nlink != 1 {
		return nil, safeDirectoryEntry{}, fmt.Errorf(
			"%s changed while opening", filepath.Join(parent.path, name),
		)
	}
	if entry.size > maxBytes {
		return nil, safeDirectoryEntry{}, fmt.Errorf(
			"%s exceeds %d bytes", filepath.Join(parent.path, name), maxBytes,
		)
	}
	data, err := io.ReadAll(io.LimitReader(file, maxBytes+1))
	if err != nil {
		return nil, safeDirectoryEntry{}, err
	}
	if int64(len(data)) > maxBytes {
		return nil, safeDirectoryEntry{}, fmt.Errorf(
			"%s exceeds %d bytes", filepath.Join(parent.path, name), maxBytes,
		)
	}
	return data, entry, nil
}

func (directory *safeDirectory) readStrictJSON(
	relativePath string,
	maxBytes int64,
	value any,
) error {
	data, err := directory.readRegular(relativePath, maxBytes)
	if err != nil {
		return err
	}
	if err := strictjson.Decode(bytes.NewReader(data), maxBytes, value); err != nil {
		return fmt.Errorf("%s: %w", filepath.Join(directory.path, relativePath), err)
	}
	return nil
}

func (directory *safeDirectory) readStrictJSONFact(
	relativePath string,
	maxBytes int64,
	value any,
) (safeDirectoryEntry, error) {
	data, entry, err := directory.readRegularFact(relativePath, maxBytes)
	if err != nil {
		return safeDirectoryEntry{}, err
	}
	if err := strictjson.Decode(bytes.NewReader(data), maxBytes, value); err != nil {
		return safeDirectoryEntry{}, fmt.Errorf(
			"%s: %w", filepath.Join(directory.path, relativePath), err,
		)
	}
	return entry, nil
}

func (directory *safeDirectory) readJSONLines(
	relativePath string,
	maxFileBytes int64,
	maxLineBytes int,
	accept func([]byte) error,
) error {
	data, err := directory.readRegular(relativePath, maxFileBytes)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	if err != nil {
		return err
	}
	scanner := bufio.NewScanner(bytes.NewReader(data))
	scanner.Buffer(make([]byte, 64<<10), maxLineBytes)
	line := 0
	for scanner.Scan() {
		line++
		if err := accept(scanner.Bytes()); err != nil {
			return fmt.Errorf(
				"%s line %d: %w",
				filepath.Join(directory.path, relativePath), line, err,
			)
		}
	}
	return scanner.Err()
}

func (directory *safeDirectory) atomicWrite(
	relativePath string,
	data []byte,
	mode os.FileMode,
	expectMissing bool,
	expectedIdentity *safeFileIdentity,
	beforeRename func(safeFileIdentity) error,
	beforePublish func() error,
	afterRename func() error,
) (safeFileIdentity, error) {
	parent, name, err := directory.openParent(relativePath, true)
	if err != nil {
		return safeFileIdentity{}, err
	}
	defer parent.close()
	current, statErr := parent.statEntry(name)
	var initialIdentity safeFileIdentity
	switch {
	case statErr == nil:
		if expectMissing {
			return safeFileIdentity{}, fmt.Errorf(
				"refusing to replace unexpected target %s",
				filepath.Join(parent.path, name),
			)
		}
		if !current.isRegular() || current.nlink != 1 {
			return safeFileIdentity{}, fmt.Errorf(
				"refusing to replace non-regular target %s",
				filepath.Join(parent.path, name),
			)
		}
		initialIdentity = current.identity()
		if expectedIdentity != nil && initialIdentity != *expectedIdentity {
			return safeFileIdentity{}, fmt.Errorf(
				"refusing to replace changed target %s",
				filepath.Join(parent.path, name),
			)
		}
	case errors.Is(statErr, unix.ENOENT):
		if !expectMissing {
			return safeFileIdentity{}, fmt.Errorf(
				"expected target %s is missing",
				filepath.Join(parent.path, name),
			)
		}
	default:
		return safeFileIdentity{}, statErr
	}
	tempName, temp, err := createTemporaryRegularAt(parent, mode)
	if err != nil {
		return safeFileIdentity{}, err
	}
	var initialTempStat unix.Stat_t
	if err := unix.Fstat(int(temp.Fd()), &initialTempStat); err != nil {
		_ = temp.Close()
		return safeFileIdentity{}, err
	}
	tempIdentity := safeFileIdentity{
		dev: uint64(initialTempStat.Dev),
		ino: uint64(initialTempStat.Ino),
	}
	tempOpen := true
	tempPublished := false
	defer func() {
		if tempOpen {
			_ = temp.Close()
		}
		if !tempPublished {
			_ = parent.removeRegular(tempName, &tempIdentity)
		}
	}()
	if _, err := temp.Write(data); err != nil {
		return safeFileIdentity{}, err
	}
	if err := temp.Sync(); err != nil {
		return safeFileIdentity{}, err
	}
	var tempStat unix.Stat_t
	if err := unix.Fstat(int(temp.Fd()), &tempStat); err != nil {
		return safeFileIdentity{}, err
	}
	if uint32(tempStat.Mode)&unix.S_IFMT != unix.S_IFREG ||
		uint64(tempStat.Nlink) != 1 {
		return safeFileIdentity{}, fmt.Errorf(
			"temporary target %s is not a single-link regular file",
			filepath.Join(parent.path, tempName),
		)
	}
	resultIdentity := safeFileIdentity{
		dev: uint64(tempStat.Dev),
		ino: uint64(tempStat.Ino),
	}
	if resultIdentity != tempIdentity {
		return safeFileIdentity{}, fmt.Errorf(
			"temporary target %s changed identity",
			filepath.Join(parent.path, tempName),
		)
	}
	if err := temp.Close(); err != nil {
		return safeFileIdentity{}, err
	}
	tempOpen = false
	if beforeRename != nil {
		if err := beforeRename(resultIdentity); err != nil {
			return safeFileIdentity{}, err
		}
	}

	// Re-check the target immediately before rename. Renameat never follows the
	// target, but rejecting a replacement symlink makes adversarial swaps fail
	// closed instead of silently unlinking the attacker's marker.
	current, statErr = parent.statEntry(name)
	switch {
	case statErr == nil:
		if expectMissing ||
			!current.isRegular() ||
			current.nlink != 1 ||
			current.identity() != initialIdentity {
			return safeFileIdentity{}, fmt.Errorf(
				"refusing to replace changed target %s",
				filepath.Join(parent.path, name),
			)
		}
	case errors.Is(statErr, unix.ENOENT):
		if !expectMissing {
			return safeFileIdentity{}, fmt.Errorf(
				"expected target %s disappeared before replacement",
				filepath.Join(parent.path, name),
			)
		}
	default:
		return safeFileIdentity{}, statErr
	}
	if beforePublish != nil {
		if err := beforePublish(); err != nil {
			return safeFileIdentity{}, err
		}
	}
	if expectMissing {
		if err := renameNoReplaceAt(
			parent.fd, tempName, parent.fd, name,
		); err != nil {
			return safeFileIdentity{}, err
		}
		tempPublished = true
	} else {
		if err := renameExchangeAt(
			parent.fd, tempName, parent.fd, name,
		); err != nil {
			return safeFileIdentity{}, err
		}
		visible, visibleErr := parent.statEntry(name)
		previous, previousErr := parent.statEntry(tempName)
		if visibleErr != nil ||
			previousErr != nil ||
			!visible.isRegular() ||
			visible.nlink != 1 ||
			visible.identity() != resultIdentity ||
			!previous.isRegular() ||
			previous.nlink != 1 ||
			previous.identity() != initialIdentity {
			restoreErr := renameExchangeAt(
				parent.fd, tempName, parent.fd, name,
			)
			if restoreErr == nil {
				if syncErr := parent.sync(); syncErr != nil {
					restoreErr = syncErr
				}
			}
			return safeFileIdentity{}, errors.Join(
				fmt.Errorf(
					"refusing to replace target %s changed at publication",
					filepath.Join(parent.path, name),
				),
				visibleErr, previousErr, restoreErr,
			)
		}
		tempPublished = true
		previousIdentity := previous.identity()
		if err := parent.removeRegular(
			tempName, &previousIdentity,
		); err != nil {
			return safeFileIdentity{}, fmt.Errorf(
				"remove replaced target %s: %w",
				filepath.Join(parent.path, name), err,
			)
		}
	}
	visible, err := parent.statEntry(name)
	if err != nil ||
		!visible.isRegular() ||
		visible.nlink != 1 ||
		visible.identity() != resultIdentity {
		return safeFileIdentity{}, errors.Join(
			fmt.Errorf(
				"published target %s changed identity",
				filepath.Join(parent.path, name),
			),
			err,
		)
	}
	if afterRename != nil {
		if err := afterRename(); err != nil {
			return safeFileIdentity{}, err
		}
	}
	if err := parent.sync(); err != nil {
		return safeFileIdentity{}, err
	}
	return resultIdentity, nil
}

func (directory *safeDirectory) atomicBytes(
	relativePath string,
	data []byte,
	mode os.FileMode,
	afterRename func() error,
) error {
	entry, err := directory.stat(relativePath)
	expectMissing := false
	var expectedIdentity *safeFileIdentity
	switch {
	case err == nil:
		if !entry.isRegular() || entry.nlink != 1 {
			return fmt.Errorf(
				"%s must be a single-link regular file",
				filepath.Join(directory.path, relativePath),
			)
		}
		identity := entry.identity()
		expectedIdentity = &identity
	case errors.Is(err, os.ErrNotExist):
		expectMissing = true
	default:
		return err
	}
	_, err = directory.atomicWrite(
		relativePath,
		data,
		mode,
		expectMissing,
		expectedIdentity,
		nil,
		nil,
		afterRename,
	)
	return err
}

func (directory *safeDirectory) atomicJSON(
	relativePath string,
	value any,
	mode os.FileMode,
) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	return directory.atomicBytes(relativePath, data, mode, nil)
}

func (directory *safeDirectory) openRegularForLock(
	name string,
) (*os.File, error) {
	if err := safeName(name); err != nil {
		return nil, err
	}
	fd, err := unix.Openat(
		directory.fd,
		name,
		unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(fd), filepath.Join(directory.path, name))
	if file == nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("open lock %s", filepath.Join(directory.path, name))
	}
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		file.Close()
		return nil, err
	}
	identity := safeFileIdentity{
		dev: uint64(stat.Dev),
		ino: uint64(stat.Ino),
	}
	pathEntry, err := directory.statEntry(name)
	if err != nil ||
		uint32(stat.Mode)&unix.S_IFMT != unix.S_IFREG ||
		uint64(stat.Nlink) != 1 ||
		!pathEntry.sameIdentity(identity) ||
		pathEntry.nlink != 1 {
		file.Close()
		if err != nil {
			return nil, err
		}
		return nil, fmt.Errorf(
			"lock path %s must be a single-link regular file",
			filepath.Join(directory.path, name),
		)
	}
	return file, nil
}

func (directory *safeDirectory) verifyVisibleRegular(
	name string,
	file *os.File,
) error {
	if file == nil {
		return fmt.Errorf("lock file is unavailable")
	}
	var stat unix.Stat_t
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	identity := safeFileIdentity{
		dev: uint64(stat.Dev),
		ino: uint64(stat.Ino),
	}
	visible, err := directory.statEntry(name)
	if err != nil {
		return err
	}
	if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFREG ||
		uint64(stat.Nlink) != 1 ||
		!visible.isRegular() ||
		visible.nlink != 1 ||
		!visible.sameIdentity(identity) {
		return fmt.Errorf(
			"lock path %s changed identity",
			filepath.Join(directory.path, name),
		)
	}
	return nil
}

func (directory *safeDirectory) removeRegular(
	relativePath string,
	expectedIdentity *safeFileIdentity,
	beforeMove ...func() error,
) error {
	return directory.removeRegularInternal(
		relativePath, expectedIdentity, true, beforeMove...,
	)
}

func (directory *safeDirectory) removeRegularRequired(
	relativePath string,
	expectedIdentity *safeFileIdentity,
	beforeMove ...func() error,
) error {
	return directory.removeRegularInternal(
		relativePath, expectedIdentity, false, beforeMove...,
	)
}

func (directory *safeDirectory) removeRegularInternal(
	relativePath string,
	expectedIdentity *safeFileIdentity,
	missingOK bool,
	beforeMove ...func() error,
) error {
	parent, name, err := directory.openParent(relativePath, false)
	if errors.Is(err, unix.ENOENT) {
		if missingOK {
			return nil
		}
		return fmt.Errorf("%s: %w", relativePath, os.ErrNotExist)
	}
	if err != nil {
		return err
	}
	defer parent.close()
	entry, err := parent.statEntry(name)
	if errors.Is(err, unix.ENOENT) {
		if missingOK {
			return nil
		}
		return fmt.Errorf("%s: %w", relativePath, os.ErrNotExist)
	}
	if err != nil {
		return err
	}
	if !entry.isRegular() || entry.nlink != 1 {
		return fmt.Errorf(
			"%s must be a single-link regular file, not a symlink or hardlink",
			filepath.Join(parent.path, name),
		)
	}
	if expectedIdentity != nil && !entry.sameIdentity(*expectedIdentity) {
		return fmt.Errorf(
			"%s does not match its mutation identity",
			filepath.Join(parent.path, name),
		)
	}
	for _, hook := range beforeMove {
		if hook != nil {
			if err := hook(); err != nil {
				return err
			}
		}
	}
	return parent.quarantineAndRemove(
		name, entry.identity(), false,
	)
}

func (directory *safeDirectory) removeEmptyDirectory(
	relativePath string,
	expectedIdentity *safeFileIdentity,
	beforeMove ...func() error,
) error {
	parent, name, err := directory.openParent(relativePath, false)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	defer parent.close()
	entry, err := parent.statEntry(name)
	if errors.Is(err, unix.ENOENT) {
		return nil
	}
	if err != nil {
		return err
	}
	if !entry.isDirectory() {
		return fmt.Errorf(
			"%s must be a directory, not a symlink",
			filepath.Join(parent.path, name),
		)
	}
	if expectedIdentity != nil && !entry.sameIdentity(*expectedIdentity) {
		return fmt.Errorf(
			"%s does not match its expected directory identity",
			filepath.Join(parent.path, name),
		)
	}
	for _, hook := range beforeMove {
		if hook != nil {
			if err := hook(); err != nil {
				return err
			}
		}
	}
	return parent.quarantineAndRemove(
		name, entry.identity(), true,
	)
}

func (directory *safeDirectory) quarantineAndRemove(
	name string,
	expected safeFileIdentity,
	removeDirectory bool,
) error {
	// POSIX has no unlink-by-inode primitive. Moving the visible entry to a
	// 128-bit random private name first closes deterministic target-name swaps:
	// a mismatched inode is restored without being unlinked. The final unlink
	// assumes no continuously adversarial same-UID process is enumerating and
	// replacing unpredictable quarantine names between verification and unlink.
	var quarantineName string
	for attempt := 0; attempt < 100; attempt++ {
		candidate, err := randomSafeName(
			".runtime-quarantine-", ".tmp",
		)
		if err != nil {
			return err
		}
		err = renameNoReplaceAt(
			directory.fd, name,
			directory.fd, candidate,
		)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return err
		}
		quarantineName = candidate
		break
	}
	if quarantineName == "" {
		return fmt.Errorf(
			"allocate private quarantine in %s", directory.path,
		)
	}
	moved, err := directory.statEntry(quarantineName)
	kindMatches := moved.isRegular() && !removeDirectory ||
		moved.isDirectory() && removeDirectory
	if err != nil ||
		!kindMatches ||
		!moved.sameIdentity(expected) ||
		!removeDirectory && moved.nlink != 1 {
		restoreErr := renameNoReplaceAt(
			directory.fd, quarantineName,
			directory.fd, name,
		)
		if syncErr := directory.sync(); syncErr != nil {
			restoreErr = errors.Join(restoreErr, syncErr)
		}
		if err != nil {
			return errors.Join(
				fmt.Errorf("%s changed while being removed", name),
				err, restoreErr,
			)
		}
		return errors.Join(
			fmt.Errorf("%s changed while being removed", name),
			restoreErr,
		)
	}
	flags := 0
	if removeDirectory {
		flags = unix.AT_REMOVEDIR
	}
	if err := unix.Unlinkat(
		directory.fd, quarantineName, flags,
	); err != nil {
		restoreErr := renameNoReplaceAt(
			directory.fd, quarantineName,
			directory.fd, name,
		)
		return errors.Join(err, restoreErr)
	}
	return directory.sync()
}

func (directory *safeDirectory) removeEmptyParents(relativePath string) error {
	current := filepath.Dir(relativePath)
	for current != "." {
		parent, name, err := directory.openParent(current, false)
		if errors.Is(err, unix.ENOENT) {
			current = filepath.Dir(current)
			continue
		}
		if err != nil {
			return err
		}
		entry, statErr := parent.statEntry(name)
		if errors.Is(statErr, unix.ENOENT) {
			parent.close()
			current = filepath.Dir(current)
			continue
		}
		if statErr != nil {
			parent.close()
			return statErr
		}
		if !entry.isDirectory() {
			parent.close()
			return fmt.Errorf(
				"%s must be a directory, not a symlink",
				filepath.Join(parent.path, name),
			)
		}
		identity := entry.identity()
		removeErr := parent.removeEmptyDirectory(name, &identity)
		if errors.Is(removeErr, unix.ENOTEMPTY) ||
			errors.Is(removeErr, unix.EEXIST) {
			parent.close()
			return nil
		}
		if removeErr != nil {
			parent.close()
			return removeErr
		}
		if err := parent.close(); err != nil {
			return err
		}
		current = filepath.Dir(current)
	}
	return nil
}

func createTemporaryRegularAt(
	parent *safeDirectory,
	mode os.FileMode,
) (string, *os.File, error) {
	return createRandomRegularAt(parent, ".runtime-", ".tmp", mode)
}

func createRandomRegularAt(
	parent *safeDirectory,
	prefix string,
	suffix string,
	mode os.FileMode,
) (string, *os.File, error) {
	for attempt := 0; attempt < 100; attempt++ {
		name, err := randomSafeName(prefix, suffix)
		if err != nil {
			return "", nil, err
		}
		fd, err := unix.Openat(
			parent.fd,
			name,
			unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|unix.O_CLOEXEC|unix.O_NOFOLLOW,
			uint32(mode.Perm()),
		)
		if errors.Is(err, unix.EEXIST) {
			continue
		}
		if err != nil {
			return "", nil, err
		}
		var stat unix.Stat_t
		if err := unix.Fstat(fd, &stat); err != nil {
			_ = unix.Close(fd)
			return "", nil, err
		}
		identity := safeFileIdentity{
			dev: uint64(stat.Dev),
			ino: uint64(stat.Ino),
		}
		file := os.NewFile(uintptr(fd), filepath.Join(parent.path, name))
		if file == nil {
			_ = unix.Close(fd)
			_ = parent.removeRegular(name, &identity)
			return "", nil, fmt.Errorf("open temporary file %s", name)
		}
		if err := file.Chmod(mode); err != nil {
			file.Close()
			_ = parent.removeRegular(name, &identity)
			return "", nil, err
		}
		return name, file, nil
	}
	return "", nil, fmt.Errorf("allocate temporary file in %s", parent.path)
}

func randomSafeName(prefix, suffix string) (string, error) {
	random := make([]byte, 16)
	if _, err := rand.Read(random); err != nil {
		return "", err
	}
	name := prefix + hex.EncodeToString(random) + suffix
	if err := safeName(name); err != nil {
		return "", err
	}
	return name, nil
}

func safeRelativeParts(relativePath string, allowDot bool) ([]string, error) {
	if filepath.IsAbs(relativePath) ||
		filepath.Clean(relativePath) != relativePath ||
		relativePath == "" ||
		relativePath == ".." ||
		strings.HasPrefix(relativePath, ".."+string(filepath.Separator)) {
		return nil, fmt.Errorf("unsafe relative path %q", relativePath)
	}
	if relativePath == "." {
		if allowDot {
			return nil, nil
		}
		return nil, fmt.Errorf("relative path must name a file")
	}
	parts := strings.Split(relativePath, string(filepath.Separator))
	for _, part := range parts {
		if err := safeName(part); err != nil {
			return nil, err
		}
	}
	return parts, nil
}

func safeName(name string) error {
	if name == "" || name == "." || name == ".." ||
		strings.ContainsRune(name, filepath.Separator) {
		return fmt.Errorf("unsafe path component %q", name)
	}
	return nil
}

func canonicalStorePath(path string) (string, error) {
	absolute, err := filepath.Abs(path)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	// macOS exposes /var and /tmp as immutable compatibility aliases. Normalize
	// only those OS-owned aliases so ordinary TempDir-based tests and callers
	// get a canonical path; every project-controlled component remains subject
	// to component-by-component O_NOFOLLOW validation below.
	if runtime.GOOS == "darwin" {
		switch {
		case absolute == "/var" || strings.HasPrefix(absolute, "/var/"):
			absolute = "/private" + absolute
		case absolute == "/tmp" || strings.HasPrefix(absolute, "/tmp/"):
			absolute = "/private" + absolute
		}
	}
	if err := validateSafeExistingAncestors(absolute); err != nil {
		return "", err
	}
	return absolute, nil
}

func validateSafeExistingAncestors(path string) error {
	fd, err := unix.Open(
		string(filepath.Separator), safeDirectoryOpenFlags, 0,
	)
	if err != nil {
		return err
	}
	defer func() {
		_ = unix.Close(fd)
	}()
	for _, component := range strings.Split(
		strings.TrimPrefix(path, string(filepath.Separator)),
		string(filepath.Separator),
	) {
		if component == "" {
			continue
		}
		next, openErr := unix.Openat(
			fd, component, safeDirectoryOpenFlags, 0,
		)
		if errors.Is(openErr, unix.ENOENT) {
			return nil
		}
		if openErr != nil {
			return fmt.Errorf(
				"store path %s contains a symlink or non-directory component %q: %w",
				path, component, openErr,
			)
		}
		if err := unix.Close(fd); err != nil {
			unix.Close(next)
			return err
		}
		fd = next
	}
	return nil
}

func openOrCreateSafeDirectory(path string) (*safeDirectory, error) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path {
		return nil, fmt.Errorf(
			"safe directory path must be absolute and clean: %s", path,
		)
	}
	root, err := openSafeDirectory(string(filepath.Separator))
	if err != nil {
		return nil, err
	}
	relativePath := strings.TrimPrefix(path, string(filepath.Separator))
	if relativePath == "" {
		return root, nil
	}
	directory, err := root.openDirectory(relativePath, true)
	closeErr := root.close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		directory.close()
		return nil, closeErr
	}
	return directory, nil
}

func (store *Store) openSessionsDirectory() (*safeDirectory, error) {
	return store.openPinnedDirectory(store.sessionsDir)
}

func (store *Store) openPinnedDirectory(
	path string,
) (*safeDirectory, error) {
	directory, err := openSafeDirectory(path)
	if err != nil {
		return nil, err
	}
	identity, err := directory.identity()
	if err != nil {
		directory.close()
		return nil, err
	}
	if err := store.pinDirectoryIdentity(path, identity); err != nil {
		directory.close()
		return nil, err
	}
	return directory, nil
}

func (store *Store) pinOpenedDirectory(
	directory *safeDirectory,
) error {
	identity, err := directory.identity()
	if err != nil {
		return err
	}
	return store.pinDirectoryIdentity(directory.path, identity)
}

func (store *Store) pinDirectoryIdentity(
	path string,
	identity safeFileIdentity,
) error {
	store.directoryIdentityMu.Lock()
	defer store.directoryIdentityMu.Unlock()
	expected, pinned := store.directoryIdentities[path]
	if !pinned {
		store.directoryIdentities[path] = identity
		return nil
	}
	if expected != identity {
		return fmt.Errorf("store directory %s changed identity", path)
	}
	return nil
}

func (store *Store) forgetDirectoryIdentity(path string) {
	store.directoryIdentityMu.Lock()
	delete(store.directoryIdentities, path)
	store.directoryIdentityMu.Unlock()
}

func (store *Store) pinLockIdentity(
	path string,
	file *os.File,
) error {
	var stat unix.Stat_t
	if file == nil {
		return fmt.Errorf("lock file %s is unavailable", path)
	}
	if err := unix.Fstat(int(file.Fd()), &stat); err != nil {
		return err
	}
	if uint32(stat.Mode)&unix.S_IFMT != unix.S_IFREG ||
		uint64(stat.Nlink) != 1 {
		return fmt.Errorf(
			"lock path %s must be a single-link regular file", path,
		)
	}
	identity := safeFileIdentity{
		dev: uint64(stat.Dev),
		ino: uint64(stat.Ino),
	}
	store.lockIdentityMu.Lock()
	defer store.lockIdentityMu.Unlock()
	expected, pinned := store.lockIdentities[path]
	if !pinned {
		store.lockIdentities[path] = identity
		return nil
	}
	if expected != identity {
		return fmt.Errorf("lock path %s changed pinned process identity", path)
	}
	return nil
}

func (store *Store) ensurePinnedRoot(
	path string,
) (*safeDirectory, error) {
	directory, err := openOrCreateSafeDirectory(path)
	if err != nil {
		return nil, err
	}
	if err := store.pinOpenedDirectory(directory); err != nil {
		directory.close()
		return nil, err
	}
	return directory, nil
}

func (store *Store) openSessionRoot(
	sessionID string,
) (*safeDirectory, error) {
	sessions, err := store.openSessionsDirectory()
	if err != nil {
		return nil, err
	}
	defer sessions.close()
	root, err := sessions.openDirectory(sessionID, false)
	if err != nil {
		return nil, err
	}
	if err := store.pinOpenedDirectory(root); err != nil {
		root.close()
		return nil, err
	}
	return root, nil
}

func (store *Store) sessionDirectoryEntries(
	sessionID string,
	relativePath string,
) ([]safeDirectoryEntry, error) {
	root, err := store.openSessionRoot(sessionID)
	if err != nil {
		return nil, err
	}
	directory, err := root.openDirectory(relativePath, false)
	closeErr := root.close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		directory.close()
		return nil, closeErr
	}
	entries, err := directory.entries()
	closeErr = directory.close()
	if err != nil {
		return nil, err
	}
	if closeErr != nil {
		return nil, closeErr
	}
	return entries, nil
}

func (store *Store) createSessionRoot(
	sessionID string,
	nonce string,
) (*safeDirectory, string, error) {
	sessions, err := store.openSessionsDirectory()
	if err != nil {
		return nil, "", err
	}
	defer sessions.close()
	if err := safeName(sessionID); err != nil {
		return nil, "", err
	}
	tempName := mutationRootTempName(nonce)
	if err := safeName(tempName); err != nil {
		return nil, "", err
	}
	if _, err := sessions.statEntry(sessionID); err == nil {
		return nil, "", fmt.Errorf("Session root %s already exists", sessionID)
	} else if !errors.Is(err, os.ErrNotExist) {
		return nil, "", err
	}
	if err := unix.Mkdirat(sessions.fd, tempName, 0o700); err != nil {
		return nil, "", err
	}
	entry, err := sessions.statEntry(tempName)
	if err != nil {
		return nil, "", err
	}
	if !entry.isDirectory() {
		return nil, "", fmt.Errorf(
			"temporary Session root %s is not a directory", tempName,
		)
	}
	expectedIdentity := entry.identity()
	if err := sessions.sync(); err != nil {
		return nil, "", err
	}
	store.hitMutationFailpoint(
		"after_mutation_root_mkdir_before_open", "",
	)
	if err := store.hitMutationErrorpoint(
		"after_mutation_root_mkdir_before_open", "",
	); err != nil {
		return nil, "", err
	}
	root, err := sessions.openDirectory(tempName, false)
	if err != nil {
		return nil, "", err
	}
	openedIdentity, err := root.identity()
	if err != nil {
		root.close()
		return nil, "", err
	}
	if openedIdentity != expectedIdentity {
		root.close()
		return nil, "", fmt.Errorf(
			"temporary Session root changed identity before open",
		)
	}
	if err := store.pinOpenedDirectory(root); err != nil {
		root.close()
		return nil, "", err
	}
	return root, tempName, nil
}

func mutationRootTempName(nonce string) string {
	return ".runtime-session-" + nonce + ".tmp"
}

func (store *Store) publishSessionRoot(
	sessionID string,
	tempName string,
	expected safeFileIdentity,
) error {
	sessions, err := store.openSessionsDirectory()
	if err != nil {
		return err
	}
	defer sessions.close()
	tempEntry, err := sessions.statEntry(tempName)
	if err != nil {
		return err
	}
	if !tempEntry.isDirectory() || !tempEntry.sameIdentity(expected) {
		return fmt.Errorf("temporary Session root changed before publication")
	}
	store.hitMutationFailpoint(
		"before_mutation_root_publish", "",
	)
	if err := store.hitMutationErrorpoint(
		"before_mutation_root_publish", "",
	); err != nil {
		return err
	}
	if err := renameNoReplaceAt(
		sessions.fd, tempName, sessions.fd, sessionID,
	); err != nil {
		return err
	}
	if err := sessions.sync(); err != nil {
		return err
	}
	visible, err := sessions.openDirectory(sessionID, false)
	if err != nil {
		return err
	}
	defer visible.close()
	visibleIdentity, err := visible.identity()
	if err != nil {
		return err
	}
	if visibleIdentity != expected {
		restoreErr := renameNoReplaceAt(
			sessions.fd, sessionID, sessions.fd, tempName,
		)
		syncErr := sessions.sync()
		return errors.Join(
			fmt.Errorf("published Session root changed identity"),
			restoreErr, syncErr,
		)
	}
	store.forgetDirectoryIdentity(
		filepath.Join(store.sessionsDir, tempName),
	)
	return store.pinOpenedDirectory(visible)
}

type mutationTargetExpectation struct {
	missing  bool
	identity *safeFileIdentity
}

func (store *Store) mutationTargetExpectation(
	sessionID, relativePath string,
) (mutationTargetExpectation, error) {
	store.mutationMu.Lock()
	defer store.mutationMu.Unlock()
	mutation := store.activeMutations[sessionID]
	if mutation == nil {
		return mutationTargetExpectation{}, fmt.Errorf(
			"Session fact mutation for %s requires the Session lock",
			sessionID,
		)
	}
	for _, entry := range mutation.journal.Entries {
		if entry.RelativePath == relativePath {
			if written, alreadyWritten := mutation.written[relativePath]; alreadyWritten {
				identity := written.safeIdentity()
				return mutationTargetExpectation{identity: &identity}, nil
			}
			if !entry.Existed {
				return mutationTargetExpectation{missing: true}, nil
			}
			identity := safeFileIdentity{
				dev: entry.Device,
				ino: entry.Inode,
			}
			return mutationTargetExpectation{identity: &identity}, nil
		}
	}
	return mutationTargetExpectation{}, fmt.Errorf(
		"Session mutation target %q has no durable backup", relativePath,
	)
}

func (store *Store) markMutationTargetWritten(
	sessionID, relativePath string,
	identity safeFileIdentity,
) error {
	store.mutationMu.Lock()
	defer store.mutationMu.Unlock()
	mutation := store.activeMutations[sessionID]
	if mutation == nil {
		return fmt.Errorf(
			"Session fact mutation for %s requires the Session lock",
			sessionID,
		)
	}
	mutation.written[relativePath] = persistentMutationIdentity(identity)
	return nil
}

func (store *Store) recordOwnedMutationIdentity(
	sessionID, relativePath string,
	identity safeFileIdentity,
) error {
	persistent := persistentMutationIdentity(identity)
	store.mutationMu.Lock()
	mutation := store.activeMutations[sessionID]
	if mutation == nil {
		store.mutationMu.Unlock()
		return fmt.Errorf(
			"Session fact mutation for %s requires the Session lock",
			sessionID,
		)
	}
	entryIndex := -1
	for index := range mutation.journal.Entries {
		if mutation.journal.Entries[index].RelativePath == relativePath {
			entryIndex = index
			break
		}
	}
	if entryIndex < 0 {
		store.mutationMu.Unlock()
		return fmt.Errorf(
			"Session mutation target %q has no durable backup", relativePath,
		)
	}
	for _, current := range mutation.journal.Entries[entryIndex].Owned {
		if current == persistent {
			store.mutationMu.Unlock()
			return nil
		}
	}
	mutation.journal.Entries[entryIndex].Owned = append(
		mutation.journal.Entries[entryIndex].Owned, persistent,
	)
	journal := mutation.journal
	store.mutationMu.Unlock()
	if err := store.persistMutationJournal(journal); err != nil {
		store.mutationMu.Lock()
		current := store.activeMutations[sessionID]
		if current != nil &&
			entryIndex < len(current.journal.Entries) {
			owned := current.journal.Entries[entryIndex].Owned
			if len(owned) > 0 && owned[len(owned)-1] == persistent {
				current.journal.Entries[entryIndex].Owned = owned[:len(owned)-1]
			}
		}
		store.mutationMu.Unlock()
		return err
	}
	return nil
}

func (store *Store) openActiveMutationRoot(
	sessionID string,
) (*safeDirectory, error) {
	store.mutationMu.Lock()
	mutation := store.activeMutations[sessionID]
	if mutation == nil {
		store.mutationMu.Unlock()
		return nil, fmt.Errorf(
			"Session fact mutation for %s requires the Session lock",
			sessionID,
		)
	}
	journal := mutation.journal
	store.mutationMu.Unlock()
	root, err := store.openSessionRoot(sessionID)
	if err != nil {
		return nil, err
	}
	if err := validateMutationRootIdentity(journal, root); err != nil {
		root.close()
		return nil, err
	}
	return root, nil
}

func (store *Store) atomicMutationJSON(
	sessionID, relativePath string,
	value any,
) error {
	data, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return err
	}
	data = append(data, '\n')
	expectation, err := store.mutationTargetExpectation(
		sessionID, relativePath,
	)
	if err != nil {
		return err
	}
	store.hitMutationFailpoint("before_target_open", relativePath)
	root, err := store.openActiveMutationRoot(sessionID)
	if err != nil {
		return err
	}
	defer root.close()
	identity, err := root.atomicWrite(
		relativePath,
		data,
		0o600,
		expectation.missing,
		expectation.identity,
		func(identity safeFileIdentity) error {
			return store.recordOwnedMutationIdentity(
				sessionID, relativePath, identity,
			)
		},
		func() error {
			store.hitMutationFailpoint(
				"before_target_publish", relativePath,
			)
			return store.hitMutationErrorpoint(
				"before_target_publish", relativePath,
			)
		},
		nil,
	)
	if err != nil {
		return err
	}
	return store.markMutationTargetWritten(
		sessionID, relativePath, identity,
	)
}

func (store *Store) appendMutationJSONLine(
	sessionID, relativePath string,
	value any,
) error {
	expectation, err := store.mutationTargetExpectation(
		sessionID, relativePath,
	)
	if err != nil {
		return err
	}
	store.hitMutationFailpoint("before_target_open", relativePath)
	root, err := store.openActiveMutationRoot(sessionID)
	if err != nil {
		return err
	}
	defer root.close()
	var existing []byte
	if !expectation.missing {
		var existingEntry safeDirectoryEntry
		existing, existingEntry, err = root.readRegularFact(
			relativePath, maxFactFileBytes,
		)
		if err != nil {
			return err
		}
		if expectation.identity == nil ||
			!existingEntry.sameIdentity(*expectation.identity) {
			return fmt.Errorf(
				"append target %q changed after mutation backup",
				relativePath,
			)
		}
	}
	line, err := json.Marshal(value)
	if err != nil {
		return err
	}
	if len(line) > maxFactLineBytes {
		return fmt.Errorf("fact exceeds %d bytes", maxFactLineBytes)
	}
	if len(existing)+len(line)+1 > maxFactFileBytes {
		return fmt.Errorf(
			"%s exceeds %d bytes",
			filepath.Join(root.path, relativePath), maxFactFileBytes,
		)
	}
	data := make([]byte, 0, len(existing)+len(line)+1)
	data = append(data, existing...)
	data = append(data, line...)
	data = append(data, '\n')
	identity, err := root.atomicWrite(
		relativePath,
		data,
		0o600,
		expectation.missing,
		expectation.identity,
		func(identity safeFileIdentity) error {
			return store.recordOwnedMutationIdentity(
				sessionID, relativePath, identity,
			)
		},
		func() error {
			store.hitMutationFailpoint(
				"before_target_publish", relativePath,
			)
			return store.hitMutationErrorpoint(
				"before_target_publish", relativePath,
			)
		},
		nil,
	)
	if err != nil {
		return err
	}
	return store.markMutationTargetWritten(
		sessionID, relativePath, identity,
	)
}
