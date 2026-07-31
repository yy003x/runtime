package activation

import (
	"bytes"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"strings"

	"golang.org/x/sys/unix"

	"github.com/yy003x/runtime/internal/strictjson"
)

const commandLinkOwnerSchema = 1

type commandLinkOwner struct {
	SchemaVersion int    `json:"schema_version"`
	LinkName      string `json:"link_name"`
	Target        string `json:"target"`
}

// CommandLinkReservation pins both the visible command-link parent and a
// persistent owner sidecar. The sidecar survives SIGKILL, serializes
// cooperating installers with flock, and lets a retry distinguish a managed
// dangling link from an unrelated pre-existing entry.
type CommandLinkReservation struct {
	fd         int
	ownerFD    int
	linkPath   string
	name       string
	ownerName  string
	target     string
	parentStat unix.Stat_t
	ownerStat  unix.Stat_t
	linkStat   unix.Stat_t
	closed     bool
}

// commandLinkReservationTestHook lets tests interrupt durable owner/link
// publication. Production has no hook.
var commandLinkReservationTestHook func(string) error

// EnsureCommandLink creates linkPath without replacing an existing filesystem
// entry. An existing symlink is accepted only when it already has target as its
// exact payload. The parent directory is opened without following a symlink so
// validation and creation use one stable directory descriptor.
func EnsureCommandLink(linkPath string, target string) error {
	reservation, err := ReserveCommandLink(linkPath, target)
	if err != nil {
		return err
	}
	return reservation.Commit()
}

// ReserveCommandLink durably claims the link name, then validates or creates
// the exact symlink. The final name is never deleted on a failure path:
// Release only verifies and closes the reservation. A retry reopens the
// persistent owner and safely reuses the same exact (possibly dangling) link.
func ReserveCommandLink(
	linkPath string,
	target string,
) (*CommandLinkReservation, error) {
	fd, name, err := openCommandLinkParent(linkPath, target)
	if err != nil {
		return nil, err
	}
	var parentStat unix.Stat_t
	if err := unix.Fstat(fd, &parentStat); err != nil {
		_ = unix.Close(fd)
		return nil, fmt.Errorf("identify command link directory: %w", err)
	}
	ownerName := commandLinkOwnerName(name)
	ownerFD, err := acquireCommandLinkOwner(fd, ownerName, name, target)
	if err != nil {
		_ = unix.Close(fd)
		return nil, err
	}
	closeHandles := func() {
		_ = unix.Flock(ownerFD, unix.LOCK_UN)
		_ = unix.Close(ownerFD)
		_ = unix.Close(fd)
	}
	if commandLinkReservationTestHook != nil {
		if err := commandLinkReservationTestHook(
			"after_owner_persisted",
		); err != nil {
			closeHandles()
			return nil, err
		}
	}
	if err := verifyCommandLinkAt(fd, name, target); err != nil {
		if !errors.Is(err, unix.ENOENT) {
			closeHandles()
			return nil, err
		}
		if commandLinkReservationTestHook != nil {
			if err := commandLinkReservationTestHook(
				"before_link_create",
			); err != nil {
				closeHandles()
				return nil, err
			}
		}
		if err := unix.Symlinkat(target, fd, name); err != nil {
			if errors.Is(err, unix.EEXIST) {
				err = verifyCommandLinkAt(fd, name, target)
			}
			if err != nil {
				closeHandles()
				return nil, fmt.Errorf("create command link: %w", err)
			}
		}
		if err := unix.Fsync(fd); err != nil {
			closeHandles()
			return nil, fmt.Errorf(
				"persist command link publication: %w", err,
			)
		}
	}
	if err := verifyCommandLinkAt(fd, name, target); err != nil {
		closeHandles()
		return nil, err
	}
	var linkStat unix.Stat_t
	if err := unix.Fstatat(
		fd, name, &linkStat, unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		closeHandles()
		return nil, fmt.Errorf("identify command link reservation: %w", err)
	}
	var ownerStat unix.Stat_t
	if err := unix.Fstat(ownerFD, &ownerStat); err != nil {
		closeHandles()
		return nil, fmt.Errorf("identify command link owner: %w", err)
	}
	return &CommandLinkReservation{
		fd: fd, ownerFD: ownerFD,
		linkPath: linkPath, name: name,
		ownerName: ownerName, target: target,
		parentStat: parentStat, ownerStat: ownerStat, linkStat: linkStat,
	}, nil
}

// Commit verifies that the reservation still owns the exact link and releases
// its pinned directory descriptor.
func (reservation *CommandLinkReservation) Commit() error {
	if reservation == nil || reservation.closed {
		return fmt.Errorf("command link reservation is closed")
	}
	verifyErr := reservation.verifyVisible()
	closeErr := reservation.close()
	if verifyErr != nil {
		return errors.Join(verifyErr, closeErr)
	}
	return closeErr
}

// Release deliberately never deletes the final link name or owner sidecar.
// POSIX has no inode-conditional unlink, so retaining the durable reservation
// is the only failure-path design that cannot unlink a last-instruction
// replacement. The next install safely reopens and verifies it.
func (reservation *CommandLinkReservation) Release() error {
	if reservation == nil || reservation.closed {
		return nil
	}
	if commandLinkReservationTestHook != nil {
		if err := commandLinkReservationTestHook(
			"before_release_verify",
		); err != nil {
			closeErr := reservation.close()
			return errors.Join(err, closeErr)
		}
	}
	verifyErr := reservation.verifyVisible()
	closeErr := reservation.close()
	if verifyErr != nil {
		return errors.Join(
			fmt.Errorf("release command link reservation: %w", verifyErr),
			closeErr,
		)
	}
	return closeErr
}

func (reservation *CommandLinkReservation) verifyPinned() error {
	if err := verifyCommandLinkAt(
		reservation.fd, reservation.name, reservation.target,
	); err != nil {
		return err
	}
	var current unix.Stat_t
	if err := unix.Fstatat(
		reservation.fd, reservation.name, &current,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return err
	}
	if !sameCommandLinkIdentity(current, reservation.linkStat) {
		return fmt.Errorf("command link reservation identity changed")
	}
	var owner unix.Stat_t
	if err := unix.Fstatat(
		reservation.fd, reservation.ownerName, &owner,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return err
	}
	if !sameCommandLinkIdentity(owner, reservation.ownerStat) {
		return fmt.Errorf("command link durable owner identity changed")
	}
	currentOwner, err := readCommandLinkOwner(reservation.ownerFD)
	if err != nil {
		return fmt.Errorf("revalidate command link durable owner: %w", err)
	}
	expectedOwner := commandLinkOwner{
		SchemaVersion: commandLinkOwnerSchema,
		LinkName:      reservation.name,
		Target:        reservation.target,
	}
	if currentOwner != expectedOwner {
		return fmt.Errorf("command link durable owner content changed")
	}
	return nil
}

func (reservation *CommandLinkReservation) verifyVisible() error {
	if err := reservation.verifyPinned(); err != nil {
		return err
	}
	visibleFD, visibleName, err := openCommandLinkParent(
		reservation.linkPath, reservation.target,
	)
	if err != nil {
		return fmt.Errorf("reopen visible command link parent: %w", err)
	}
	defer unix.Close(visibleFD) //nolint:errcheck
	if visibleName != reservation.name {
		return fmt.Errorf("visible command link name changed")
	}
	var visibleParent unix.Stat_t
	if err := unix.Fstat(visibleFD, &visibleParent); err != nil {
		return err
	}
	if !sameCommandLinkIdentity(visibleParent, reservation.parentStat) {
		return fmt.Errorf("visible command link parent identity changed")
	}
	if err := verifyCommandLinkAt(
		visibleFD, reservation.name, reservation.target,
	); err != nil {
		return err
	}
	var visibleLink unix.Stat_t
	if err := unix.Fstatat(
		visibleFD, reservation.name, &visibleLink,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return err
	}
	if !sameCommandLinkIdentity(visibleLink, reservation.linkStat) {
		return fmt.Errorf("visible command link identity changed")
	}
	var visibleOwner unix.Stat_t
	if err := unix.Fstatat(
		visibleFD, reservation.ownerName, &visibleOwner,
		unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return err
	}
	if !sameCommandLinkIdentity(visibleOwner, reservation.ownerStat) {
		return fmt.Errorf("visible command link owner identity changed")
	}
	return nil
}

func sameCommandLinkIdentity(left unix.Stat_t, right unix.Stat_t) bool {
	return left.Dev == right.Dev && left.Ino == right.Ino &&
		left.Mode&unix.S_IFMT == right.Mode&unix.S_IFMT
}

func (reservation *CommandLinkReservation) close() error {
	if reservation.closed {
		return nil
	}
	reservation.closed = true
	unlockErr := unix.Flock(reservation.ownerFD, unix.LOCK_UN)
	ownerCloseErr := unix.Close(reservation.ownerFD)
	parentCloseErr := unix.Close(reservation.fd)
	return errors.Join(
		wrapCommandLinkCloseError("unlock command link owner", unlockErr),
		wrapCommandLinkCloseError("close command link owner", ownerCloseErr),
		wrapCommandLinkCloseError("close command link parent", parentCloseErr),
	)
}

func commandLinkOwnerName(linkName string) string {
	return ".sn-cli." + linkName + ".owner.json"
}

func acquireCommandLinkOwner(
	parentFD int,
	ownerName string,
	linkName string,
	target string,
) (int, error) {
	expected := commandLinkOwner{
		SchemaVersion: commandLinkOwnerSchema,
		LinkName:      linkName,
		Target:        target,
	}
	ownerFD, err := unix.Openat(
		parentFD, ownerName,
		unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0,
	)
	if errors.Is(err, unix.ENOENT) {
		return createCommandLinkOwner(
			parentFD, ownerName, expected,
		)
	}
	if err != nil {
		return -1, fmt.Errorf("open command link durable owner: %w", err)
	}
	if err := lockCommandLinkOwner(ownerFD); err != nil {
		_ = unix.Close(ownerFD)
		return -1, err
	}
	owner, readErr := readCommandLinkOwner(ownerFD)
	if readErr != nil {
		_ = unix.Flock(ownerFD, unix.LOCK_UN)
		_ = unix.Close(ownerFD)
		return -1, readErr
	}
	if owner != expected {
		_ = unix.Flock(ownerFD, unix.LOCK_UN)
		_ = unix.Close(ownerFD)
		return -1, fmt.Errorf(
			"command link durable owner belongs to link %q target %q",
			owner.LinkName, owner.Target,
		)
	}
	if err := verifyCommandLinkOwnerName(
		parentFD, ownerName, ownerFD,
	); err != nil {
		_ = unix.Flock(ownerFD, unix.LOCK_UN)
		_ = unix.Close(ownerFD)
		return -1, err
	}
	return ownerFD, nil
}

func createCommandLinkOwner(
	parentFD int,
	ownerName string,
	owner commandLinkOwner,
) (int, error) {
	nonce, err := randomNonce()
	if err != nil {
		return -1, fmt.Errorf("name command link owner temp file: %w", err)
	}
	tempName := ownerName + ".pending-" + nonce
	ownerFD, err := unix.Openat(
		parentFD, tempName,
		unix.O_RDWR|unix.O_CREAT|unix.O_EXCL|
			unix.O_CLOEXEC|unix.O_NOFOLLOW,
		0o600,
	)
	if err != nil {
		return -1, fmt.Errorf("create command link owner temp file: %w", err)
	}
	closeOwner := func() {
		_ = unix.Flock(ownerFD, unix.LOCK_UN)
		_ = unix.Close(ownerFD)
	}
	if err := lockCommandLinkOwner(ownerFD); err != nil {
		_ = unix.Close(ownerFD)
		return -1, err
	}
	if err := writeCommandLinkOwner(ownerFD, owner); err != nil {
		closeOwner()
		return -1, err
	}
	if err := unix.Fsync(parentFD); err != nil {
		closeOwner()
		return -1, fmt.Errorf(
			"persist command link owner temp entry: %w", err,
		)
	}
	if commandLinkReservationTestHook != nil {
		if err := commandLinkReservationTestHook(
			"after_owner_temp_persisted",
		); err != nil {
			closeOwner()
			return -1, err
		}
	}
	if err := renameAtNoReplace(
		parentFD, tempName, parentFD, ownerName,
	); err != nil {
		closeOwner()
		return -1, fmt.Errorf(
			"publish command link durable owner without replacement: %w", err,
		)
	}
	if err := unix.Fsync(parentFD); err != nil {
		closeOwner()
		return -1, fmt.Errorf(
			"persist command link durable owner publication: %w", err,
		)
	}
	if _, err := readCommandLinkOwner(ownerFD); err != nil {
		closeOwner()
		return -1, err
	}
	if err := verifyCommandLinkOwnerName(
		parentFD, ownerName, ownerFD,
	); err != nil {
		closeOwner()
		return -1, err
	}
	return ownerFD, nil
}

func verifyCommandLinkOwnerName(
	parentFD int,
	ownerName string,
	ownerFD int,
) error {
	var pinned unix.Stat_t
	if err := unix.Fstat(ownerFD, &pinned); err != nil {
		return err
	}
	var visible unix.Stat_t
	if err := unix.Fstatat(
		parentFD, ownerName, &visible, unix.AT_SYMLINK_NOFOLLOW,
	); err != nil {
		return err
	}
	if !sameCommandLinkIdentity(pinned, visible) {
		return fmt.Errorf("command link durable owner name identity changed")
	}
	return nil
}

func lockCommandLinkOwner(fd int) error {
	if err := unix.Flock(fd, unix.LOCK_EX|unix.LOCK_NB); err != nil {
		if errors.Is(err, unix.EWOULDBLOCK) ||
			errors.Is(err, unix.EAGAIN) {
			return fmt.Errorf("another command link activation is in progress")
		}
		return fmt.Errorf("lock command link durable owner: %w", err)
	}
	return nil
}

func writeCommandLinkOwner(fd int, owner commandLinkOwner) error {
	data, err := json.Marshal(owner)
	if err != nil {
		return err
	}
	data = append(data, '\n')
	if err := unix.Ftruncate(fd, 0); err != nil {
		return fmt.Errorf("truncate command link durable owner: %w", err)
	}
	if _, err := unix.Seek(fd, 0, io.SeekStart); err != nil {
		return fmt.Errorf("rewind command link durable owner: %w", err)
	}
	for len(data) > 0 {
		written, writeErr := unix.Write(fd, data)
		if writeErr != nil {
			return fmt.Errorf("write command link durable owner: %w", writeErr)
		}
		if written == 0 {
			return fmt.Errorf("write complete command link durable owner")
		}
		data = data[written:]
	}
	if err := unix.Fsync(fd); err != nil {
		return fmt.Errorf("persist command link durable owner: %w", err)
	}
	return nil
}

func readCommandLinkOwner(fd int) (commandLinkOwner, error) {
	var stat unix.Stat_t
	if err := unix.Fstat(fd, &stat); err != nil {
		return commandLinkOwner{}, err
	}
	if stat.Mode&unix.S_IFMT != unix.S_IFREG ||
		stat.Mode&0o777 != 0o600 || stat.Nlink != 1 ||
		stat.Size <= 0 || stat.Size > 4096 {
		return commandLinkOwner{}, fmt.Errorf(
			"command link durable owner must be a mode 0600 single-link small regular file",
		)
	}
	duplicate, err := unix.Dup(fd)
	if err != nil {
		return commandLinkOwner{}, err
	}
	if _, err := unix.Seek(duplicate, 0, io.SeekStart); err != nil {
		_ = unix.Close(duplicate)
		return commandLinkOwner{}, err
	}
	file := os.NewFile(uintptr(duplicate), "command-link-owner")
	if file == nil {
		_ = unix.Close(duplicate)
		return commandLinkOwner{}, fmt.Errorf("wrap command link durable owner")
	}
	data, readErr := io.ReadAll(io.LimitReader(file, 4097))
	closeErr := file.Close()
	if readErr != nil {
		return commandLinkOwner{}, readErr
	}
	if closeErr != nil {
		return commandLinkOwner{}, closeErr
	}
	if len(data) > 4096 {
		return commandLinkOwner{}, fmt.Errorf(
			"command link durable owner is too large",
		)
	}
	var owner commandLinkOwner
	if err := strictjson.DecodeObjectNoNulls(
		bytes.NewReader(data), 4096, &owner,
	); err != nil {
		return commandLinkOwner{}, fmt.Errorf(
			"decode command link durable owner: %w", err,
		)
	}
	if owner.SchemaVersion != commandLinkOwnerSchema ||
		owner.LinkName == "" || owner.Target == "" {
		return commandLinkOwner{}, fmt.Errorf(
			"command link durable owner is invalid",
		)
	}
	return owner, nil
}

func wrapCommandLinkCloseError(operation string, err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%s: %w", operation, err)
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
