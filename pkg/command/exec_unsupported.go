//go:build !darwin && !linux

package command

import (
	"fmt"
	"runtime"
)

type StdinMode string

const (
	StdinInherit StdinMode = "inherit"
	StdinTTY     StdinMode = "tty"
	StdinNull    StdinMode = "null"
)

func ReplaceProcess(Invocation, StdinMode) error {
	return fmt.Errorf("command bridge is unsupported on %s", runtime.GOOS)
}
