//go:build linux

package activation

import "golang.org/x/sys/unix"

func renameAtNoReplace(
	fromFD int,
	from string,
	toFD int,
	to string,
) error {
	return unix.Renameat2(
		fromFD, from, toFD, to, unix.RENAME_NOREPLACE,
	)
}
