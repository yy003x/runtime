//go:build !darwin && !linux

package command

func isTerminalFD(int) bool { return false }
