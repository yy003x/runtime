//go:build linux

package session

import "golang.org/x/sys/unix"

func renameNoReplaceAt(
	fromDirectory int,
	from string,
	toDirectory int,
	to string,
) error {
	return unix.Renameat2(
		fromDirectory, from, toDirectory, to, unix.RENAME_NOREPLACE,
	)
}

func renameExchangeAt(
	leftDirectory int,
	left string,
	rightDirectory int,
	right string,
) error {
	return unix.Renameat2(
		leftDirectory, left, rightDirectory, right, unix.RENAME_EXCHANGE,
	)
}
