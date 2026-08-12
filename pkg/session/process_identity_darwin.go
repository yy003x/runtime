//go:build darwin

package session

import (
	"fmt"

	"golang.org/x/sys/unix"
)

func processStartToken(pid int) (string, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return "", err
	}
	if info.Proc.P_pid != int32(pid) {
		return "", fmt.Errorf("pid %d is not running", pid)
	}
	return fmt.Sprintf(
		"%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec,
	), nil
}
