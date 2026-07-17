package layout

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

const HomeEnv = "SN_CLI_HOME"

type Paths struct {
	Home                 string
	BinDir               string
	Binary               string
	ConfigDir            string
	PersonaDir           string
	SkillsDir            string
	ToolsDir             string
	RunsDir              string
	SessionsDir          string
	HistoryDir           string
	DaemonDir            string
	DaemonLog            string
	StateDir             string
	MemoryFile           string
	MemoryDir            string
	MemoryCandidatesFile string
	SessionStateDir      string
	UpdateStateFile      string
	LogsDir              string
	CacheDir             string
	TmpDir               string
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
	absolute, err := filepath.Abs(home)
	if err != nil {
		return Paths{}, fmt.Errorf("resolve runtime home %q: %w", home, err)
	}
	configDir := filepath.Join(absolute, "configs")
	stateDir := filepath.Join(absolute, "state")
	logsDir := filepath.Join(absolute, "logs")
	return Paths{
		Home: absolute, BinDir: filepath.Join(absolute, "bin"), Binary: filepath.Join(absolute, "bin", "sn-cli"),
		ConfigDir: configDir, PersonaDir: filepath.Join(configDir, "personas"), SkillsDir: filepath.Join(configDir, "skills"), ToolsDir: filepath.Join(configDir, "tools"),
		RunsDir: filepath.Join(absolute, "runs"), SessionsDir: filepath.Join(absolute, "sessions"), HistoryDir: filepath.Join(absolute, "history"),
		DaemonDir: filepath.Join(absolute, "daemon"), DaemonLog: filepath.Join(logsDir, "daemon.log"),
		StateDir: stateDir, SessionStateDir: filepath.Join(stateDir, "sessions"),
		MemoryDir: filepath.Join(absolute, "memory"), MemoryFile: filepath.Join(absolute, "memory", "durable.json"),
		MemoryCandidatesFile: filepath.Join(absolute, "memory", "candidates.json"), UpdateStateFile: filepath.Join(stateDir, "update.json"),
		LogsDir: logsDir, CacheDir: filepath.Join(absolute, "cache"), TmpDir: filepath.Join(absolute, "tmp"),
	}, nil
}

func (p Paths) Ensure() error {
	for _, dir := range []string{
		p.Home, p.BinDir, p.ConfigDir, p.PersonaDir, p.SkillsDir, p.ToolsDir,
		p.RunsDir, p.SessionsDir, p.HistoryDir, p.DaemonDir, p.StateDir, p.SessionStateDir,
		p.MemoryDir, p.LogsDir, p.CacheDir, p.TmpDir,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create runtime directory %s: %w", dir, err)
		}
	}
	return nil
}
