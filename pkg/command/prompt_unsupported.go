//go:build !darwin && !linux

package command

import "os"

func openPromptFileNoFollow(path string) (*os.File, error) {
	return os.Open(path)
}
