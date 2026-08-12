//go:build darwin || linux

package tmux

import (
	"fmt"
	"os"

	"golang.org/x/sys/unix"
)

type fileLock struct {
	file *os.File
}

func acquireFileLock(path string, shared bool) (*fileLock, error) {
	if err := ensurePrivateDir(filepathDir(path), os.Getuid()); err != nil {
		return nil, err
	}
	fd, err := unix.Open(
		path, unix.O_CREAT|unix.O_RDWR|unix.O_CLOEXEC|unix.O_NOFOLLOW, 0o600,
	)
	if err != nil {
		return nil, fmt.Errorf("open tmux lock: %w", err)
	}
	file := os.NewFile(uintptr(fd), path)
	info, err := file.Stat()
	if err != nil {
		file.Close()
		return nil, err
	}
	if !info.Mode().IsRegular() || info.Mode().Perm() != 0o600 {
		file.Close()
		return nil, fmt.Errorf("tmux lock must be a private regular file")
	}
	if err := requireOwner(info, os.Getuid(), path); err != nil {
		file.Close()
		return nil, err
	}
	mode := unix.LOCK_EX
	if shared {
		mode = unix.LOCK_SH
	}
	if err := unix.Flock(fd, mode); err != nil {
		file.Close()
		return nil, fmt.Errorf("lock tmux lifecycle: %w", err)
	}
	return &fileLock{file: file}, nil
}

func (lock *fileLock) Close() {
	if lock == nil || lock.file == nil {
		return
	}
	_ = unix.Flock(int(lock.file.Fd()), unix.LOCK_UN)
	_ = lock.file.Close()
	lock.file = nil
}

func filepathDir(path string) string {
	index := len(path) - 1
	for index >= 0 && path[index] != '/' {
		index--
	}
	if index <= 0 {
		return "."
	}
	return path[:index]
}
