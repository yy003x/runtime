//go:build darwin || linux

package executor

import (
	"os/signal"
	"syscall"

	"golang.org/x/sys/unix"
)

func isTerminal(fd uintptr) bool {
	_, err := unix.IoctlGetInt(int(fd), unix.TIOCGPGRP)
	return err == nil
}

func setForegroundPgid(fd uintptr, pid int) error {
	pgid, err := syscall.Getpgid(pid)
	if err != nil {
		return err
	}
	signal.Ignore(syscall.SIGTTOU)
	defer signal.Reset(syscall.SIGTTOU)
	return unix.IoctlSetPointerInt(int(fd), unix.TIOCSPGRP, pgid)
}
