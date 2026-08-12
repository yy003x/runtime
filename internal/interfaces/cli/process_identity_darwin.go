//go:build darwin

package cli

import (
	"bytes"
	"fmt"

	"golang.org/x/sys/unix"
)

func processIdentityForPID(pid int) (processIdentity, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return processIdentity{}, err
	}
	if info.Proc.P_pid != int32(pid) {
		return processIdentity{}, fmt.Errorf("pid %d is not running", pid)
	}
	executable := string(bytes.TrimRight(info.Proc.P_comm[:], "\x00"))
	if executable == "" {
		return processIdentity{}, fmt.Errorf("pid %d has no executable identity", pid)
	}
	return processIdentity{
		StartToken: fmt.Sprintf(
			"%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec,
		),
		Executable: executable,
	}, nil
}
