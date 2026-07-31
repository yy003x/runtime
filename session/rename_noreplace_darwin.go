//go:build darwin

package session

import "golang.org/x/sys/unix"

func renameNoReplaceAt(
	fromDirectory int,
	from string,
	toDirectory int,
	to string,
) error {
	return unix.RenameatxNp(
		fromDirectory, from, toDirectory, to, unix.RENAME_EXCL,
	)
}

func renameExchangeAt(
	leftDirectory int,
	left string,
	rightDirectory int,
	right string,
) error {
	return unix.RenameatxNp(
		leftDirectory, left, rightDirectory, right, unix.RENAME_SWAP,
	)
}
