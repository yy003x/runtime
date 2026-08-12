package layout

import (
	"crypto/sha256"
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const HomeEnv = "SN_CLI_HOME"

type Paths struct {
	Home              string
	BinDir            string
	Binary            string
	ServerBinary      string
	ConfigDir         string
	ToolsDir          string
	RuntimeConfigFile string
	ResourcesDir      string
	SchemaDir         string
	SessionsDir       string
	LogsDir           string
	StateDir          string
	RunDBFile         string
	ServerPIDFile     string
	ServerLogFile     string
	ServerLeaseFile   string
	ServerLockFile    string
	UpdateStateFile   string
	TmpDir            string
	TmuxLockFile      string
	TmuxManifestDir   string
	TmuxConfigFile    string
	TmuxSocketDir     string
	TmuxSocketFile    string
}

func Resolve() (Paths, error) {
	home := strings.TrimSpace(os.Getenv(HomeEnv))
	if home == "" {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("resolve user home: %w", err)
		}
		home = filepath.Join(userHome, ".sn")
	}
	return FromHome(home)
}

func FromHome(home string) (Paths, error) {
	home = strings.TrimSpace(home)
	if home == "" {
		return Paths{}, fmt.Errorf("runtime home is required")
	}
	if home == "~" || strings.HasPrefix(home, "~/") {
		userHome, err := os.UserHomeDir()
		if err != nil {
			return Paths{}, fmt.Errorf("resolve user home: %w", err)
		}
		if home == "~" {
			home = userHome
		} else {
			home = filepath.Join(userHome, strings.TrimPrefix(home, "~/"))
		}
	}
	absolute, err := CanonicalHome(home)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve runtime home %q: %w", home, err)
	}
	resourcesDir := filepath.Join(absolute, "resources")
	stateDir := filepath.Join(absolute, "state")
	tmuxSocketDir := filepath.Join(
		"/tmp", fmt.Sprintf("sn-cli-tmux-%d", os.Getuid()),
	)
	homeDigest := fmt.Sprintf("%x", sha256.Sum256([]byte(absolute)))
	return Paths{
		Home: absolute, BinDir: filepath.Join(absolute, "bin"),
		Binary:            filepath.Join(absolute, "bin", "sn-cli"),
		ServerBinary:      filepath.Join(absolute, "bin", "sn-server"),
		ConfigDir:         filepath.Join(absolute, "configs"),
		ToolsDir:          filepath.Join(absolute, "tools"),
		RuntimeConfigFile: filepath.Join(absolute, "runtime.json"),
		ResourcesDir:      resourcesDir,
		SchemaDir:         filepath.Join(resourcesDir, "schema"),
		SessionsDir:       filepath.Join(absolute, "sessions"),
		LogsDir:           filepath.Join(absolute, "logs"),
		StateDir:          stateDir,
		RunDBFile:         filepath.Join(stateDir, "runtime.db"),
		ServerPIDFile:     filepath.Join(stateDir, "sn-server.pid"),
		ServerLogFile:     filepath.Join(absolute, "logs", "sn-server.log"),
		ServerLeaseFile:   filepath.Join(stateDir, "sn-server.lease.lock"),
		ServerLockFile:    filepath.Join(stateDir, "sn-server.lifecycle.lock"),
		UpdateStateFile:   filepath.Join(stateDir, "update.json"),
		TmpDir:            filepath.Join(absolute, "tmp"),
		TmuxLockFile:      filepath.Join(stateDir, "tmux.lock"),
		TmuxManifestDir:   filepath.Join(absolute, "tmp", "tmux"),
		TmuxConfigFile:    filepath.Join(resourcesDir, "tmux.conf"),
		TmuxSocketDir:     tmuxSocketDir,
		TmuxSocketFile: filepath.Join(
			tmuxSocketDir, homeDigest[:16]+".sock",
		),
	}, nil
}

// CanonicalHome resolves an existing Runtime home (or its nearest existing
// ancestor) before appending a missing suffix. It rejects a home that is
// itself a symlink so every process derives the same state and Tmux identity
// without allowing a caller to redirect the managed root.
func CanonicalHome(home string) (string, error) {
	home = strings.TrimSpace(home)
	if home == "" {
		return "", fmt.Errorf("runtime home is required")
	}
	absolute, err := filepath.Abs(home)
	if err != nil {
		return "", err
	}
	absolute = filepath.Clean(absolute)
	if info, statErr := os.Lstat(absolute); statErr == nil {
		if info.Mode()&os.ModeSymlink != 0 {
			return "", fmt.Errorf("runtime home must not be a symlink")
		}
		resolved, resolveErr := filepath.EvalSymlinks(absolute)
		if resolveErr != nil {
			return "", resolveErr
		}
		return filepath.Clean(resolved), nil
	} else if !os.IsNotExist(statErr) {
		return "", statErr
	}
	var suffix []string
	ancestor := absolute
	for {
		parent := filepath.Dir(ancestor)
		if parent == ancestor {
			return "", fmt.Errorf("runtime home has no existing ancestor")
		}
		suffix = append(suffix, filepath.Base(ancestor))
		ancestor = parent
		info, statErr := os.Lstat(ancestor)
		if statErr == nil {
			if info.Mode()&os.ModeSymlink == 0 && !info.IsDir() {
				return "", fmt.Errorf(
					"runtime home parent is not a directory: %s", ancestor,
				)
			}
			resolved, resolveErr := filepath.EvalSymlinks(ancestor)
			if resolveErr != nil {
				return "", resolveErr
			}
			resolvedInfo, statErr := os.Stat(resolved)
			if statErr != nil || !resolvedInfo.IsDir() {
				return "", fmt.Errorf(
					"runtime home parent is not a directory: %s", resolved,
				)
			}
			ancestor = resolved
			break
		}
		if !os.IsNotExist(statErr) {
			return "", statErr
		}
	}
	for index := len(suffix) - 1; index >= 0; index-- {
		ancestor = filepath.Join(ancestor, suffix[index])
	}
	return filepath.Clean(ancestor), nil
}

func (p Paths) Ensure() error {
	for _, dir := range []string{
		p.Home, p.BinDir, p.ConfigDir, p.ToolsDir, p.ResourcesDir,
		p.SchemaDir, p.SessionsDir, p.LogsDir, p.StateDir, p.TmpDir,
		p.TmuxManifestDir,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create runtime directory %s: %w", dir, err)
		}
	}
	return nil
}
