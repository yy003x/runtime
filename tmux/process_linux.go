//go:build linux

package tmux

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"syscall"
)

func lookupProcessIdentity(pid int) (ProcessIdentity, error) {
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return ProcessIdentity{}, err
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 {
		return ProcessIdentity{}, fmt.Errorf("pid %d has malformed proc stat", pid)
	}
	fields := strings.Fields(string(stat[closeParen+1:]))
	if len(fields) <= 19 || fields[19] == "" {
		return ProcessIdentity{}, fmt.Errorf("pid %d has no process start token", pid)
	}
	executable, err := os.Readlink(filepath.Join(procDir, "exe"))
	if err != nil {
		return ProcessIdentity{}, err
	}
	executable = strings.TrimSuffix(executable, " (deleted)")
	info, err := os.Stat(filepath.Join(procDir, "exe"))
	if err != nil {
		return ProcessIdentity{}, err
	}
	system, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return ProcessIdentity{}, fmt.Errorf(
			"cannot identify pid %d executable", pid,
		)
	}
	return ProcessIdentity{
		StartToken: fields[19], Executable: executable,
		ExecutableIdentity: formatExecutableIdentity(
			executable, info, system,
		),
	}, nil
}
