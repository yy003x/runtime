package layout

import (
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
	CommandDir        string
	RuntimeConfigFile string
	ResourcesDir      string
	SchemaDir         string
	SessionsDir       string
	StateDir          string
	RunDBFile         string
	ServerPIDFile     string
	ServerLogFile     string
	ServerLeaseFile   string
	ServerLockFile    string
	UpdateStateFile   string
	TmpDir            string
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
	resourcesDir := filepath.Join(absolute, "resources")
	stateDir := filepath.Join(absolute, "state")
	return Paths{
		Home: absolute, BinDir: filepath.Join(absolute, "bin"),
		Binary:            filepath.Join(absolute, "bin", "sn-cli"),
		ServerBinary:      filepath.Join(absolute, "bin", "sn-server"),
		ConfigDir:         filepath.Join(absolute, "configs"),
		CommandDir:        filepath.Join(absolute, "commands"),
		RuntimeConfigFile: filepath.Join(absolute, "runtime.json"),
		ResourcesDir:      resourcesDir,
		SchemaDir:         filepath.Join(resourcesDir, "schema"),
		SessionsDir:       filepath.Join(absolute, "sessions"),
		StateDir:          stateDir,
		RunDBFile:         filepath.Join(stateDir, "runtime.db"),
		ServerPIDFile:     filepath.Join(stateDir, "sn-server.pid"),
		ServerLogFile:     filepath.Join(stateDir, "sn-server.log"),
		ServerLeaseFile:   filepath.Join(stateDir, "sn-server.lease.lock"),
		ServerLockFile:    filepath.Join(stateDir, "sn-server.lifecycle.lock"),
		UpdateStateFile:   filepath.Join(stateDir, "update.json"),
		TmpDir:            filepath.Join(absolute, "tmp"),
	}, nil
}

func (p Paths) Ensure() error {
	for _, dir := range []string{
		p.Home, p.BinDir, p.ConfigDir, p.CommandDir, p.ResourcesDir,
		p.SchemaDir, p.SessionsDir, p.StateDir, p.TmpDir,
	} {
		if err := os.MkdirAll(dir, 0o700); err != nil {
			return fmt.Errorf("create runtime directory %s: %w", dir, err)
		}
	}
	return nil
}
