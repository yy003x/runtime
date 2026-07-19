package daemon

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"syscall"
	"time"
)

type Config struct {
	Home        string
	Dir         string
	LogFile     string
	Version     string
	Executable  string
	IdleTimeout time.Duration
	Busy        func() bool
}

func (c Config) normalized() Config {
	if c.Dir == "" {
		c.Dir = filepath.Join(c.Home, "daemon")
	}
	if c.LogFile == "" {
		c.LogFile = filepath.Join(c.Home, "logs", "daemon.log")
	}
	if c.Version == "" {
		c.Version = "dev"
	}
	if c.IdleTimeout <= 0 {
		c.IdleTimeout = 10 * time.Minute
	}
	return c
}

func (c Config) SocketPath() string {
	path := filepath.Join(c.normalized().Dir, "runtime.sock")
	if len(path) <= 96 {
		return path
	}
	hash := sha256.Sum256([]byte(c.normalized().Dir))
	return filepath.Join("/tmp", fmt.Sprintf("agent-runtime-%x.sock", hash[:8]))
}
func (c Config) PIDPath() string      { return filepath.Join(c.normalized().Dir, "runtime.pid") }
func (c Config) TokenPath() string    { return filepath.Join(c.normalized().Dir, "runtime.token") }
func (c Config) LogPath() string      { return c.normalized().LogFile }
func (c Config) RegistryPath() string { return filepath.Join(c.normalized().Dir, "processes.json") }

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
