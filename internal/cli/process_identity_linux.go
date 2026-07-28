//go:build linux

package cli

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func processIdentityForPID(pid int) (processIdentity, error) {
	procDir := filepath.Join("/proc", strconv.Itoa(pid))
	stat, err := os.ReadFile(filepath.Join(procDir, "stat"))
	if err != nil {
		return processIdentity{}, err
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 {
		return processIdentity{}, fmt.Errorf("pid %d has malformed proc stat", pid)
	}
	fields := strings.Fields(string(stat[closeParen+1:]))
	// After the command name, index 0 is field 3; process starttime is field 22.
	if len(fields) <= 19 || fields[19] == "" {
		return processIdentity{}, fmt.Errorf("pid %d has no process start token", pid)
	}
	executable, err := os.Readlink(filepath.Join(procDir, "exe"))
	if err != nil {
		return processIdentity{}, err
	}
	executable = strings.TrimSuffix(executable, " (deleted)")
	return processIdentity{
		StartToken: fields[19],
		Executable: executable,
	}, nil
}
