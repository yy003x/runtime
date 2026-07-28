//go:build !darwin && !linux

package command

import (
	"fmt"
	"runtime"
)

func replaceProcess(Invocation) error {
	return fmt.Errorf("command bridge is unsupported on %s", runtime.GOOS)
}

func replaceProcessWithInput(Invocation, string) error {
	return fmt.Errorf("command bridge is unsupported on %s", runtime.GOOS)
}
