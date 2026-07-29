//go:build linux

package activation

import (
	"errors"
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
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
	entries, err := os.ReadDir("/proc")
	if err != nil {
		return fmt.Errorf("inspect process table: %w", err)
	}
	for _, entry := range entries {
		pid, err := strconv.Atoi(entry.Name())
		if err != nil {
			continue
		}
		if excluded, ok := excludedPIDs[pid]; ok {
			token, tokenErr := processStartToken(pid)
			if tokenErr != nil {
				if errors.Is(tokenErr, os.ErrNotExist) {
					continue
				}
				return fmt.Errorf(
					"revalidate excluded process pid=%d: %w", pid, tokenErr,
				)
			}
			if token != excluded.StartToken {
				return fmt.Errorf(
					"excluded process pid=%d changed identity", pid,
				)
			}
			continue
		}
		procDir := filepath.Join("/proc", entry.Name())
		executablePath := filepath.Join(procDir, "exe")
		executable, err := os.Readlink(executablePath)
		if err != nil {
			if errors.Is(err, os.ErrNotExist) {
				continue
			}
			name, readErr := os.ReadFile(filepath.Join(procDir, "comm"))
			if readErr == nil {
				if _, relevant := targetBase[strings.TrimSpace(string(name))]; !relevant {
					continue
				}
			}
			return fmt.Errorf(
				"cannot identify possibly relevant process pid=%d: %w", pid, err,
			)
		}
		executableInfo, statErr := os.Stat(executablePath)
		if statErr != nil {
			if _, relevant := targetBase[filepath.Base(
				strings.TrimSuffix(executable, " (deleted)"),
			)]; !relevant && errors.Is(statErr, os.ErrNotExist) {
				continue
			}
			return fmt.Errorf(
				"identify process executable pid=%d: %w", pid, statErr,
			)
		}
		for _, target := range targets {
			if os.SameFile(executableInfo, target.Info) {
				return fmt.Errorf(
					"target Runtime binary is still running: pid=%d executable=%s",
					pid, strings.TrimSuffix(executable, " (deleted)"),
				)
			}
		}
	}
	return nil
}

func requireTargetCLIProcess(pid int, target processTarget) error {
	executablePath := filepath.Join("/proc", strconv.Itoa(pid), "exe")
	executable, err := os.Readlink(executablePath)
	if err != nil {
		return fmt.Errorf("identify activation coordinator pid=%d: %w", pid, err)
	}
	executableInfo, err := os.Stat(executablePath)
	if err != nil {
		return fmt.Errorf("stat activation coordinator pid=%d: %w", pid, err)
	}
	if !os.SameFile(executableInfo, target.Info) {
		return fmt.Errorf(
			"activation coordinator pid=%d is %s, expected %s",
			pid, strings.TrimSuffix(executable, " (deleted)"), target.Path,
		)
	}
	return nil
}

func processStartToken(pid int) (string, error) {
	data, err := os.ReadFile(
		filepath.Join("/proc", strconv.Itoa(pid), "stat"),
	)
	if err != nil {
		return "", err
	}
	value := string(data)
	end := strings.LastIndex(value, ")")
	if end < 0 || end+1 >= len(value) {
		return "", fmt.Errorf("process stat is malformed")
	}
	fields := strings.Fields(value[end+1:])
	if len(fields) <= 19 {
		return "", fmt.Errorf("process stat is truncated")
	}
	return fields[19], nil
}
