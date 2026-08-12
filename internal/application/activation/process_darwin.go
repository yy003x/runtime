//go:build darwin

package activation

import (
	"bytes"
	"encoding/binary"
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"syscall"
	"time"

	"golang.org/x/sys/unix"
)

func assertNoTargetProcesses(
	targets []processTarget,
	excludedPIDs map[int]processExclusion,
) error {
	if len(targets) == 0 {
		return nil
	}
	targetBase := make(map[string]struct{}, len(targets))
	for _, target := range targets {
		targetBase[filepath.Base(target.Path)] = struct{}{}
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.all")
	if err != nil {
		return fmt.Errorf("inspect process table: %w", err)
	}
processLoop:
	for _, process := range processes {
		pid := int(process.Proc.P_pid)
		if pid <= 0 {
			continue
		}
		if excluded, ok := excludedPIDs[pid]; ok {
			token := darwinStartToken(process)
			if token != excluded.StartToken {
				return fmt.Errorf(
					"excluded process pid=%d changed identity", pid,
				)
			}
			continue
		}
		executable, err := darwinExecutablePath(pid)
		if err != nil {
			name := string(bytes.TrimRight(process.Proc.P_comm[:], "\x00"))
			if _, relevant := targetBase[filepath.Base(name)]; !relevant {
				continue
			}
			return fmt.Errorf(
				"cannot identify possibly relevant process pid=%d: %w", pid, err,
			)
		}
		executableInfo, statErr := os.Stat(executable)
		if statErr != nil {
			_, relevant := targetBase[filepath.Base(executable)]
			if errors.Is(statErr, os.ErrNotExist) ||
				errors.Is(statErr, unix.ENOTDIR) {
				if !relevant {
					continue
				}
				predatesTargets, identityErr :=
					darwinProcessPredatesNamedTargets(
						process, executable, targets,
					)
				if identityErr != nil {
					return fmt.Errorf(
						"compare unresolved process executable identity pid=%d: %w",
						pid, identityErr,
					)
				}
				if predatesTargets {
					continue
				}
			}
			return fmt.Errorf(
				"identify process executable pid=%d: %w", pid, statErr,
			)
		}
		for _, target := range targets {
			if os.SameFile(executableInfo, target.Info) {
				predatesTarget, identityErr := darwinProcessPredatesFile(
					process, target.Info,
				)
				if identityErr != nil {
					return fmt.Errorf(
						"compare process executable identity pid=%d: %w",
						pid, identityErr,
					)
				}
				if predatesTarget {
					// kern.procargs2 exposes the path used at exec time.
					// After an atomic install that path can resolve to a
					// newer vnode while the process still executes the old
					// unlinked vnode. A process that started before the
					// current target vnode was born cannot own that vnode.
					continue processLoop
				}
				return fmt.Errorf(
					"target Runtime binary is still running: pid=%d executable=%s",
					pid, executable,
				)
			}
		}
	}
	return nil
}

func darwinProcessPredatesNamedTargets(
	process unix.KinfoProc,
	executable string,
	targets []processTarget,
) (bool, error) {
	base := filepath.Base(executable)
	matched := false
	for _, target := range targets {
		if filepath.Base(target.Path) != base {
			continue
		}
		matched = true
		predates, err := darwinProcessPredatesFile(process, target.Info)
		if err != nil {
			return false, err
		}
		if !predates {
			return false, nil
		}
	}
	return matched, nil
}

func requireTargetCLIProcess(pid int, target processTarget) error {
	executable, err := darwinExecutablePath(pid)
	if err != nil {
		return fmt.Errorf("identify activation coordinator pid=%d: %w", pid, err)
	}
	executableInfo, err := os.Stat(executable)
	if err != nil {
		return fmt.Errorf("stat activation coordinator pid=%d: %w", pid, err)
	}
	if !os.SameFile(executableInfo, target.Info) {
		return fmt.Errorf(
			"activation coordinator pid=%d is %s, expected %s",
			pid, executable, target.Path,
		)
	}
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		return fmt.Errorf("inspect activation coordinator pid=%d: %w", pid, err)
	}
	if len(processes) != 1 || int(processes[0].Proc.P_pid) != pid {
		return fmt.Errorf("activation coordinator pid=%d no longer exists", pid)
	}
	predatesTarget, err := darwinProcessPredatesFile(
		processes[0], target.Info,
	)
	if err != nil {
		return fmt.Errorf(
			"compare activation coordinator pid=%d identity: %w", pid, err,
		)
	}
	if predatesTarget {
		return fmt.Errorf(
			"activation coordinator pid=%d predates target executable %s",
			pid, target.Path,
		)
	}
	return nil
}

func darwinProcessPredatesFile(
	process unix.KinfoProc,
	info os.FileInfo,
) (bool, error) {
	stat, ok := info.Sys().(*syscall.Stat_t)
	if !ok {
		return false, fmt.Errorf("target file has unsupported stat identity")
	}
	if stat.Birthtimespec.Sec <= 0 {
		return false, fmt.Errorf("target file birth time is unavailable")
	}
	started := time.Unix(
		process.Proc.P_starttime.Sec,
		int64(process.Proc.P_starttime.Usec)*int64(time.Microsecond),
	)
	born := time.Unix(
		stat.Birthtimespec.Sec, stat.Birthtimespec.Nsec,
	)
	return started.Before(born), nil
}

func processStartToken(pid int) (string, error) {
	processes, err := unix.SysctlKinfoProcSlice("kern.proc.pid", pid)
	if err != nil {
		if errors.Is(err, unix.ESRCH) {
			return "", os.ErrNotExist
		}
		return "", err
	}
	if len(processes) != 1 || int(processes[0].Proc.P_pid) != pid {
		return "", os.ErrNotExist
	}
	return darwinStartToken(processes[0]), nil
}

func darwinStartToken(process unix.KinfoProc) string {
	return strconv.FormatInt(process.Proc.P_starttime.Sec, 10) + ":" +
		strconv.FormatInt(int64(process.Proc.P_starttime.Usec), 10)
}

func darwinExecutablePath(pid int) (string, error) {
	raw, err := unix.SysctlRaw("kern.procargs2", pid)
	if err != nil {
		return "", err
	}
	if len(raw) <= 4 {
		return "", fmt.Errorf("kern.procargs2 is truncated")
	}
	argc := int(int32(binary.LittleEndian.Uint32(raw[:4])))
	if argc <= 0 {
		return "", fmt.Errorf("kern.procargs2 has invalid argc")
	}
	value := raw[4:]
	end := bytes.IndexByte(value, 0)
	if end <= 0 {
		return "", fmt.Errorf("kern.procargs2 has no executable path")
	}
	return string(value[:end]), nil
}
