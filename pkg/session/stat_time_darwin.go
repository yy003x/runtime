//go:build darwin

package session

import (
	"time"

	"golang.org/x/sys/unix"
)

func statModifiedTime(stat unix.Stat_t) time.Time {
	return time.Unix(stat.Mtim.Sec, stat.Mtim.Nsec)
}
