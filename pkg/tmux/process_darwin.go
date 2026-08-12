//go:build darwin

package tmux

import (
	"bytes"
	"encoding/binary"
	"fmt"

	"golang.org/x/sys/unix"
)

func lookupProcessIdentity(pid int) (ProcessIdentity, error) {
	info, err := unix.SysctlKinfoProc("kern.proc.pid", pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	if info.Proc.P_pid != int32(pid) {
		return ProcessIdentity{}, fmt.Errorf("pid %d is not running", pid)
	}
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return ProcessIdentity{}, err
	}
	if len(raw) <= 4 ||
		int(int32(binary.LittleEndian.Uint32(raw[:4]))) <= 0 {
		return ProcessIdentity{}, fmt.Errorf(
			"pid %d has malformed executable metadata", pid,
		)
	}
	end := bytes.IndexByte(raw[4:], 0)
	if end <= 0 {
		return ProcessIdentity{}, fmt.Errorf(
			"pid %d has no executable path", pid,
		)
	}
	executable, executableIdentity, err := executableIdentity(
		string(raw[4 : 4+end]),
	)
	if err != nil {
		return ProcessIdentity{}, fmt.Errorf(
			"identify pid %d executable: %w", pid, err,
		)
	}
	return ProcessIdentity{
		StartToken: fmt.Sprintf(
			"%d:%d", info.Proc.P_starttime.Sec, info.Proc.P_starttime.Usec,
		),
		Executable: executable, ExecutableIdentity: executableIdentity,
	}, nil
}
