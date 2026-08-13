package activation

import (
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/internal/infrastructure/layout"
)

const (
	runtimeStateResetMarkerName = ".owner"
	runtimeStateResetPayload    = "payload"
	runtimeStateResetSchema     = 1
)

type runtimeStateEntryKind uint8

const (
	runtimeStateDirectory runtimeStateEntryKind = iota + 1
	runtimeStateRegularFile
)

type runtimeStateEntry struct {
	name string
	kind runtimeStateEntryKind
}

type runtimeStateResetOwner struct {
	SchemaVersion int    `json:"schema_version"`
	Name          string `json:"name"`
	Kind          string `json:"kind"`
	Device        uint64 `json:"device"`
	Inode         uint64 `json:"inode"`
}

// runtimeStateResetTestHook is intentionally package-private. Tests use it to
// prove that path swaps and interrupted tombstone cleanup fail closed.
var runtimeStateResetTestHook func(string) error

// resetRuntimeState removes only the existing Session and durable Run
// state owned by Runtime. Every mutation is relative to pinned home/state
// directory descriptors. An entry is first renamed into an owned tombstone,
// then cleaned without following symlinks. A committed activation can
// therefore resume safely after a crash at any point in this step.
func resetRuntimeState(target string) error {
	paths, err := runtimeStateResetPaths(target)
	if err != nil {
		return err
	}
	homeFD, stateFD, err := openRuntimeStateRoots(paths)
	if err != nil {
		return err
	}
	defer unix.Close(homeFD)  //nolint:errcheck
	defer unix.Close(stateFD) //nolint:errcheck

	if err := runRuntimeStateResetHook("roots_opened"); err != nil {
		return err
	}
	if err := verifyRuntimeStateRoots(paths, homeFD, stateFD); err != nil {
		return err
	}
	if err := resetRuntimeStateEntryAt(
		homeFD,
		runtimeStateEntry{
			name: filepath.Base(paths.SessionsDir),
			kind: runtimeStateDirectory,
		},
	); err != nil {
		return err
	}
	for _, entry := range []runtimeStateEntry{
		{name: "session-locks", kind: runtimeStateDirectory},
		{name: "session-invocations", kind: runtimeStateDirectory},
		{name: "session-mutations", kind: runtimeStateDirectory},
		{name: "session-trash-moves", kind: runtimeStateDirectory},
		{name: "native-tui-invocations", kind: runtimeStateDirectory},
		{name: "runtime.db-wal", kind: runtimeStateRegularFile},
		{name: "runtime.db-shm", kind: runtimeStateRegularFile},
		{name: "runtime.db-journal", kind: runtimeStateRegularFile},
		{name: "runtime.db", kind: runtimeStateRegularFile},
	} {
		if err := resetRuntimeStateEntryAt(stateFD, entry); err != nil {
			return err
		}
	}
	return verifyRuntimeStateRoots(paths, homeFD, stateFD)
}

func validateRuntimeStateReset(target string) error {
	paths, err := runtimeStateResetPaths(target)
	if err != nil {
		return err
	}
	homeFD, stateFD, err := openRuntimeStateRoots(paths)
	if err != nil {
		return err
	}
	defer unix.Close(homeFD)  //nolint:errcheck
	defer unix.Close(stateFD) //nolint:errcheck
	if err := verifyRuntimeStateRoots(paths, homeFD, stateFD); err != nil {
		return err
	}
	if err := validateRuntimeStateEntryAt(
		homeFD,
		runtimeStateEntry{
			name: filepath.Base(paths.SessionsDir),
			kind: runtimeStateDirectory,
		},
	); err != nil {
		return err
	}
	for _, entry := range []runtimeStateEntry{
		{name: "session-locks", kind: runtimeStateDirectory},
		{name: "session-invocations", kind: runtimeStateDirectory},
		{name: "session-mutations", kind: runtimeStateDirectory},
		{name: "session-trash-moves", kind: runtimeStateDirectory},
		{name: "native-tui-invocations", kind: runtimeStateDirectory},
		{name: "runtime.db-wal", kind: runtimeStateRegularFile},
		{name: "runtime.db-shm", kind: runtimeStateRegularFile},
		{name: "runtime.db-journal", kind: runtimeStateRegularFile},
		{name: "runtime.db", kind: runtimeStateRegularFile},
	} {
		if err := validateRuntimeStateEntryAt(stateFD, entry); err != nil {
			return err
		}
	}
	return nil
}

func runtimeStateResetPaths(target string) (layout.Paths, error) {
	paths, err := layout.FromHome(target)
	if err != nil {
		return layout.Paths{}, err
	}
	if paths.Home != target {
		return layout.Paths{}, fmt.Errorf(
			"runtime state reset target is not canonical",
		)
	}
	return paths, nil
}

func openRuntimeStateRoots(paths layout.Paths) (int, int, error) {
	homeFD, err := openDirectoryNoFollow(paths.Home)
	if err != nil {
		return -1, -1, fmt.Errorf("open Runtime home for state reset: %w", err)
	}
	stateFD, err := openDirectoryAt(homeFD, filepath.Base(paths.StateDir))
	if err != nil {
		_ = unix.Close(homeFD)
		return -1, -1, fmt.Errorf(
			"open Runtime state directory for reset: %w", err,
		)
	}
	return homeFD, stateFD, nil
}

func openDirectoryAt(parentFD int, name string) (int, error) {
	return unix.Openat(
		parentFD, name,
		unix.O_RDONLY|unix.O_DIRECTORY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
}

func verifyRuntimeStateRoots(
	paths layout.Paths,
	homeFD int,
	stateFD int,
) error {
	if err := verifyPinnedDirectory(paths.Home, homeFD); err != nil {
		return fmt.Errorf("Runtime home changed during state reset: %w", err)
	}
	if err := verifyPinnedDirectory(paths.StateDir, stateFD); err != nil {
		return fmt.Errorf("Runtime state directory changed during reset: %w", err)
	}
	return nil
}

func verifyPinnedDirectory(path string, pinnedFD int) error {
	currentFD, err := openDirectoryNoFollow(path)
	if err != nil {
		return err
	}
	defer unix.Close(currentFD) //nolint:errcheck
	var pinned unix.Stat_t
	if err := unix.Fstat(pinnedFD, &pinned); err != nil {
		return err
	}
	var current unix.Stat_t
	if err := unix.Fstat(currentFD, &current); err != nil {
		return err
	}
	if !sameRuntimeStateIdentity(pinned, current) {
		return fmt.Errorf("directory identity changed")
	}
	return nil
}

func validateRuntimeStateEntryAt(
	parentFD int,
	entry runtimeStateEntry,
) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(
		parentFD, entry.name, &stat, unix.AT_SYMLINK_NOFOLLOW,
	); errors.Is(err, unix.ENOENT) {
		return nil
	} else if err != nil {
		return err
	}
	return validateRuntimeStateKind(entry, stat)
}

func validateRuntimeStateKind(
	entry runtimeStateEntry,
	stat unix.Stat_t,
) error {
	actual := stat.Mode & unix.S_IFMT
	switch entry.kind {
	case runtimeStateDirectory:
		if actual != unix.S_IFDIR {
			return fmt.Errorf(
				"runtime state path must be a directory, not a symlink: %s",
				entry.name,
			)
		}
	case runtimeStateRegularFile:
		if actual != unix.S_IFREG {
			return fmt.Errorf(
				"runtime state path must be a regular file, not a symlink: %s",
				entry.name,
			)
		}
	default:
		return fmt.Errorf("unsupported runtime state entry kind")
	}
	return nil
}

func runtimeStateKindName(kind runtimeStateEntryKind) string {
	switch kind {
	case runtimeStateDirectory:
		return "directory"
	case runtimeStateRegularFile:
		return "regular_file"
	default:
		return "unsupported"
	}
}

func resetRuntimeStateEntryAt(
	parentFD int,
	entry runtimeStateEntry,
) error {
	tombstone := ".sn-reset-" + entry.name
	if err := recoverRuntimeStateTombstone(
		parentFD, tombstone, entry,
	); err != nil {
		return err
	}

	var before unix.Stat_t
	if err := unix.Fstatat(
		parentFD, entry.name, &before, unix.AT_SYMLINK_NOFOLLOW,
	); errors.Is(err, unix.ENOENT) {
		return nil
	} else if err != nil {
		return err
	}
	if err := validateRuntimeStateKind(entry, before); err != nil {
		return err
	}

	pinnedFD, err := openRuntimeStateEntryAt(parentFD, entry)
	if err != nil {
		return err
	}
	defer unix.Close(pinnedFD) //nolint:errcheck
	var pinned unix.Stat_t
	if err := unix.Fstat(pinnedFD, &pinned); err != nil {
		return err
	}
	if !sameRuntimeStateIdentity(before, pinned) {
		return fmt.Errorf(
			"runtime state path %s changed while it was opened", entry.name,
		)
	}

	tombstoneFD, err := createRuntimeStateTombstone(
		parentFD, tombstone, entry, pinned,
	)
	if err != nil {
		return err
	}
	defer unix.Close(tombstoneFD) //nolint:errcheck
	if err := runRuntimeStateResetHook(
		"before_rename:" + entry.name,
	); err != nil {
		return err
	}
	if err := unix.Renameat(
		parentFD, entry.name, tombstoneFD, runtimeStateResetPayload,
	); err != nil {
		return fmt.Errorf(
			"move runtime state %s into reset tombstone: %w",
			entry.name, err,
		)
	}
	if err := unix.Fsync(parentFD); err != nil {
		return fmt.Errorf("persist runtime state tombstone rename: %w", err)
	}
	if err := unix.Fsync(tombstoneFD); err != nil {
		return fmt.Errorf("persist runtime state tombstone payload: %w", err)
	}
	if err := runRuntimeStateResetHook(
		"after_rename:" + entry.name,
	); err != nil {
		return err
	}
	var moved unix.Stat_t
	if err := unix.Fstatat(
		tombstoneFD, runtimeStateResetPayload, &moved,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return err
	}
	if !sameRuntimeStateIdentity(pinned, moved) {
		return fmt.Errorf(
			"runtime state path %s changed before tombstoning", entry.name,
		)
	}
	if err := removePinnedRuntimeStatePayload(
		tombstoneFD, pinnedFD, entry,
	); err != nil {
		return err
	}
	if err := finishRuntimeStateTombstone(
		parentFD, tombstoneFD, tombstone,
	); err != nil {
		return err
	}
	return nil
}

func openRuntimeStateEntryAt(
	parentFD int,
	entry runtimeStateEntry,
) (int, error) {
	flags := unix.O_RDONLY | unix.O_CLOEXEC | unix.O_NOFOLLOW
	if entry.kind == runtimeStateDirectory {
		flags |= unix.O_DIRECTORY
	}
	fd, err := unix.Openat(parentFD, entry.name, flags, 0)
	if err != nil {
		return -1, fmt.Errorf(
			"open runtime state path %s without following links: %w",
			entry.name, err,
		)
	}
	return fd, nil
}

func createRuntimeStateTombstone(
	parentFD int,
	name string,
	entry runtimeStateEntry,
	pinned unix.Stat_t,
) (int, error) {
	if err := unix.Mkdirat(parentFD, name, 0o700); err != nil {
		return -1, fmt.Errorf("reserve runtime state reset tombstone: %w", err)
	}
	fd, err := openDirectoryAt(parentFD, name)
	if err != nil {
		return -1, err
	}
	markerFD, err := unix.Openat(
		fd, runtimeStateResetMarkerName,
		unix.O_WRONLY|unix.O_CREAT|unix.O_EXCL|
			unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("create runtime state tombstone marker: %w", err)
	}
	marker, err := json.Marshal(runtimeStateResetOwner{
		SchemaVersion: runtimeStateResetSchema,
		Name:          entry.name,
		Kind:          runtimeStateKindName(entry.kind),
		Device:        uint64(pinned.Dev),
		Inode:         uint64(pinned.Ino),
	})
	if err != nil {
		_ = unix.Close(markerFD)
		_ = unix.Close(fd)
		return -1, fmt.Errorf("encode runtime state tombstone marker: %w", err)
	}
	marker = append(marker, '\n')
	written, err := unix.Write(markerFD, marker)
	if err != nil {
		_ = unix.Close(markerFD)
		_ = unix.Close(fd)
		return -1, fmt.Errorf("write runtime state tombstone marker: %w", err)
	}
	if written != len(marker) {
		_ = unix.Close(markerFD)
		_ = unix.Close(fd)
		return -1, fmt.Errorf("write complete runtime state tombstone marker")
	}
	if err := unix.Fsync(markerFD); err != nil {
		_ = unix.Close(markerFD)
		_ = unix.Close(fd)
		return -1, fmt.Errorf("persist runtime state tombstone marker: %w", err)
	}
	if err := unix.Close(markerFD); err != nil {
		_ = unix.Close(fd)
		return -1, err
	}
	if err := unix.Fsync(fd); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("persist runtime state tombstone: %w", err)
	}
	if err := unix.Fsync(parentFD); err != nil {
		_ = unix.Close(fd)
		return -1, fmt.Errorf("persist runtime state tombstone parent: %w", err)
	}
	return fd, nil
}

func recoverRuntimeStateTombstone(
	parentFD int,
	tombstone string,
	entry runtimeStateEntry,
) error {
	var stat unix.Stat_t
	if err := unix.Fstatat(
		parentFD, tombstone, &stat, unix.AT_SYMLINK_NOFOLLOW,
	); errors.Is(err, unix.ENOENT) {
		return nil
	} else if err != nil {
		return err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
		return fmt.Errorf(
			"runtime state reset tombstone is not an owned directory: %s",
			tombstone,
		)
	}
	fd, err := openDirectoryAt(parentFD, tombstone)
	if err != nil {
		return err
	}
	defer unix.Close(fd) //nolint:errcheck
	owner, ownerErr := readRuntimeStateTombstoneOwner(fd, entry)
	if ownerErr != nil {
		if cleanupErr := discardUncommittedRuntimeStateTombstone(
			parentFD, fd, tombstone,
		); cleanupErr == nil {
			return nil
		}
		return ownerErr
	}
	var payload unix.Stat_t
	if err := unix.Fstatat(
		fd, runtimeStateResetPayload, &payload, unix.AT_SYMLINK_NOFOLLOW,
	); err == nil {
		if err := validateRuntimeStateKind(entry, payload); err != nil {
			return fmt.Errorf(
				"invalid runtime state reset tombstone payload: %w", err,
			)
		}
		if owner.Device != uint64(payload.Dev) ||
			owner.Inode != uint64(payload.Ino) {
			return fmt.Errorf(
				"runtime state reset tombstone payload identity does not match its durable owner",
			)
		}
		payloadFD, openErr := openRuntimeStateEntryAt(
			fd,
			runtimeStateEntry{
				name: runtimeStateResetPayload,
				kind: entry.kind,
			},
		)
		if openErr != nil {
			return openErr
		}
		var opened unix.Stat_t
		if statErr := unix.Fstat(payloadFD, &opened); statErr != nil {
			_ = unix.Close(payloadFD)
			return statErr
		}
		if !sameRuntimeStateIdentity(payload, opened) {
			_ = unix.Close(payloadFD)
			return fmt.Errorf(
				"runtime state reset tombstone payload changed while opening",
			)
		}
		removeErr := removePinnedRuntimeStatePayload(
			fd, payloadFD, entry,
		)
		closeErr := unix.Close(payloadFD)
		if removeErr != nil {
			return removeErr
		}
		if closeErr != nil {
			return closeErr
		}
	} else if !errors.Is(err, unix.ENOENT) {
		return err
	}
	return finishRuntimeStateTombstone(parentFD, fd, tombstone)
}

// discardUncommittedRuntimeStateTombstone handles a crash after reserving the
// deterministic tombstone directory but before its owner marker was durably
// completed. It removes only an empty tombstone or its lone marker; the
// presence of a payload or any other entry fails closed.
func discardUncommittedRuntimeStateTombstone(
	parentFD int,
	fd int,
	tombstone string,
) error {
	names, err := runtimeStateDirectoryNames(fd)
	if err != nil {
		return err
	}
	for _, name := range names {
		if name != runtimeStateResetMarkerName {
			return fmt.Errorf("unowned runtime state tombstone is not empty")
		}
		var stat unix.Stat_t
		if err := unix.Fstatat(
			fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW,
		); err != nil {
			return err
		}
		if stat.Mode&unix.S_IFMT != unix.S_IFREG {
			return fmt.Errorf("unowned runtime state tombstone marker is invalid")
		}
		if err := unix.Unlinkat(fd, name, 0); err != nil {
			return err
		}
	}
	if err := unix.Fsync(fd); err != nil {
		return err
	}
	if err := unix.Unlinkat(
		parentFD, tombstone, unix.AT_REMOVEDIR,
	); err != nil {
		return err
	}
	return unix.Fsync(parentFD)
}

func readRuntimeStateTombstoneOwner(
	fd int,
	entry runtimeStateEntry,
) (runtimeStateResetOwner, error) {
	markerFD, err := unix.Openat(
		fd, runtimeStateResetMarkerName,
		unix.O_RDONLY|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if err != nil {
		return runtimeStateResetOwner{}, fmt.Errorf(
			"open runtime state tombstone marker: %w", err,
		)
	}
	file := os.NewFile(uintptr(markerFD), runtimeStateResetMarkerName)
	if file == nil {
		_ = unix.Close(markerFD)
		return runtimeStateResetOwner{}, fmt.Errorf(
			"wrap runtime state tombstone marker",
		)
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 513))
	closeErr := file.Close()
	if readErr != nil {
		return runtimeStateResetOwner{}, readErr
	}
	if closeErr != nil {
		return runtimeStateResetOwner{}, closeErr
	}
	if len(data) > 512 {
		return runtimeStateResetOwner{}, fmt.Errorf(
			"runtime state tombstone marker is too large",
		)
	}
	var owner runtimeStateResetOwner
	if err := json.Unmarshal(data, &owner); err != nil {
		return runtimeStateResetOwner{}, fmt.Errorf(
			"decode runtime state tombstone owner: %w", err,
		)
	}
	if owner.SchemaVersion != runtimeStateResetSchema ||
		owner.Name != entry.name ||
		owner.Kind != runtimeStateKindName(entry.kind) ||
		owner.Inode == 0 {
		return runtimeStateResetOwner{}, fmt.Errorf(
			"runtime state reset tombstone ownership is invalid",
		)
	}
	return owner, nil
}

func removePinnedRuntimeStatePayload(
	tombstoneFD int,
	pinnedFD int,
	entry runtimeStateEntry,
) error {
	verifyPinned := func() error {
		var pinned unix.Stat_t
		if err := unix.Fstat(pinnedFD, &pinned); err != nil {
			return err
		}
		var current unix.Stat_t
		if err := unix.Fstatat(
			tombstoneFD, runtimeStateResetPayload, &current,
			unix.AT_SYMLINK_NOFOLLOW,
		); err != nil {
			return err
		}
		if !sameRuntimeStateIdentity(pinned, current) {
			return fmt.Errorf(
				"runtime state reset tombstone payload identity changed",
			)
		}
		return nil
	}
	if err := verifyPinned(); err != nil {
		return err
	}
	switch entry.kind {
	case runtimeStateDirectory:
		if err := emptyRuntimeStateDirectory(pinnedFD); err != nil {
			return fmt.Errorf(
				"clean runtime state directory %s: %w", entry.name, err,
			)
		}
		if err := verifyPinned(); err != nil {
			return err
		}
		if err := unix.Unlinkat(
			tombstoneFD, runtimeStateResetPayload, unix.AT_REMOVEDIR,
		); err != nil {
			return fmt.Errorf(
				"remove runtime state directory %s: %w", entry.name, err,
			)
		}
	case runtimeStateRegularFile:
		if err := verifyPinned(); err != nil {
			return err
		}
		if err := unix.Unlinkat(
			tombstoneFD, runtimeStateResetPayload, 0,
		); err != nil {
			return fmt.Errorf(
				"remove runtime state file %s: %w", entry.name, err,
			)
		}
	default:
		return fmt.Errorf("unsupported runtime state entry kind")
	}
	return unix.Fsync(tombstoneFD)
}

func emptyRuntimeStateDirectory(fd int) error {
	for attempts := 0; attempts < 16; attempts++ {
		names, err := runtimeStateDirectoryNames(fd)
		if err != nil {
			return err
		}
		if len(names) == 0 {
			return nil
		}
		for _, name := range names {
			var stat unix.Stat_t
			if err := unix.Fstatat(
				fd, name, &stat, unix.AT_SYMLINK_NOFOLLOW,
			); errors.Is(err, unix.ENOENT) {
				continue
			} else if err != nil {
				return err
			}
			if stat.Mode&unix.S_IFMT != unix.S_IFDIR {
				if err := unix.Unlinkat(fd, name, 0); err != nil &&
					!errors.Is(err, unix.ENOENT) {
					return err
				}
				continue
			}
			childFD, err := openDirectoryAt(fd, name)
			if errors.Is(err, unix.ENOENT) {
				continue
			}
			if err != nil {
				return err
			}
			var opened unix.Stat_t
			if err := unix.Fstat(childFD, &opened); err != nil {
				_ = unix.Close(childFD)
				return err
			}
			if !sameRuntimeStateIdentity(stat, opened) {
				_ = unix.Close(childFD)
				return fmt.Errorf(
					"runtime state directory entry %s changed while opening",
					name,
				)
			}
			removeErr := emptyRuntimeStateDirectory(childFD)
			closeErr := unix.Close(childFD)
			if removeErr != nil {
				return removeErr
			}
			if closeErr != nil {
				return closeErr
			}
			if err := unix.Unlinkat(
				fd, name, unix.AT_REMOVEDIR,
			); err != nil && !errors.Is(err, unix.ENOENT) {
				return err
			}
		}
	}
	return fmt.Errorf("runtime state directory changed during cleanup")
}

func runtimeStateDirectoryNames(fd int) ([]string, error) {
	scanFD, err := openDirectoryAt(fd, ".")
	if err != nil {
		return nil, err
	}
	file := os.NewFile(uintptr(scanFD), "runtime-state-directory")
	if file == nil {
		_ = unix.Close(scanFD)
		return nil, fmt.Errorf("wrap runtime state directory")
	}
	entries, readErr := file.ReadDir(-1)
	closeErr := file.Close()
	if readErr != nil {
		return nil, readErr
	}
	if closeErr != nil {
		return nil, closeErr
	}
	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if entry.Name() == "." || entry.Name() == ".." {
			continue
		}
		names = append(names, entry.Name())
	}
	return names, nil
}

func finishRuntimeStateTombstone(
	parentFD int,
	tombstoneFD int,
	tombstone string,
) error {
	if err := unix.Unlinkat(
		tombstoneFD, runtimeStateResetMarkerName, 0,
	); err != nil && !errors.Is(err, unix.ENOENT) {
		return err
	}
	if err := unix.Fsync(tombstoneFD); err != nil {
		return err
	}
	if err := unix.Unlinkat(
		parentFD, tombstone, unix.AT_REMOVEDIR,
	); err != nil {
		return err
	}
	return unix.Fsync(parentFD)
}

func sameRuntimeStateIdentity(left unix.Stat_t, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino &&
		left.Mode&unix.S_IFMT == right.Mode&unix.S_IFMT
}

func runRuntimeStateResetHook(phase string) error {
	if runtimeStateResetTestHook == nil {
		return nil
	}
	return runtimeStateResetTestHook(phase)
}
