//go:build !darwin && !linux

package activation

import "fmt"

func assertNoTargetProcesses(
	_ []processTarget,
	_ map[int]processExclusion,
) error {
	return fmt.Errorf("upgrade activation is unsupported on this operating system")
}

func requireTargetCLIProcess(_ int, _ processTarget) error {
	return fmt.Errorf("upgrade activation is unsupported on this operating system")
}

func processStartToken(_ int) (string, error) {
	return "", fmt.Errorf("upgrade activation is unsupported on this operating system")
}
