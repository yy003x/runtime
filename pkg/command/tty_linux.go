//go:build linux

package command

import "golang.org/x/sys/unix"

func isTerminalFD(fd int) bool {
	_, err := unix.IoctlGetTermios(fd, unix.TCGETS)
	return err == nil
}
