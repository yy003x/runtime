//go:build !darwin && !linux

package command

func directoryEnterable(string) error { return nil }
