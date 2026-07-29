//go:build !darwin && !linux

package tmux

import "os"

func isTerminal(_ *os.File) bool {
	return false
}
