//go:build linux

package session

import (
	"fmt"
	"os"
	"path/filepath"
	"strconv"
	"strings"
)

func processStartToken(pid int) (string, error) {
	stat, err := os.ReadFile(
		filepath.Join("/proc", strconv.Itoa(pid), "stat"),
	)
	if err != nil {
		return "", err
	}
	closeParen := strings.LastIndexByte(string(stat), ')')
	if closeParen < 0 {
		return "", fmt.Errorf("pid %d has malformed proc stat", pid)
	}
	fields := strings.Fields(string(stat[closeParen+1:]))
	if len(fields) <= 19 || fields[19] == "" {
		return "", fmt.Errorf("pid %d has no process start token", pid)
	}
	return fields[19], nil
}
