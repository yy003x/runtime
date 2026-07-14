package daemon

import (
	"crypto/sha256"
	"fmt"
	"path/filepath"
	"syscall"
	"time"
)

type Config struct {
	Root        string
	Dir         string
	Version     string
	Executable  string
	IdleTimeout time.Duration
}

func (c Config) normalized() Config {
	if c.Dir == "" {
		c.Dir = filepath.Join(c.Root, "runs", "global", "runtime", "daemon")
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
func (c Config) LogPath() string      { return filepath.Join(c.normalized().Dir, "runtime.log") }
func (c Config) RegistryPath() string { return filepath.Join(c.normalized().Dir, "processes.json") }

func processAlive(pid int) bool {
	if pid <= 0 {
		return false
	}
	return syscall.Kill(pid, 0) == nil
}
