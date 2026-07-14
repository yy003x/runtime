//go:build !darwin && !linux

package executor

func isTerminal(uintptr) bool { return false }

func setForegroundPgid(uintptr, int) error { return nil }
