//go:build darwin || linux

package command

import "golang.org/x/sys/unix"

func directoryEnterable(path string) error {
	return unix.Access(path, unix.X_OK)
}
