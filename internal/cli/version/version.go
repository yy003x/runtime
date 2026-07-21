package version

import (
	"runtime/debug"
	"strings"
)

var (
	Version   = "0.1.0"
	Commit    = ""
	BuildDate = ""
	Dirty     = "false"
)

var goVersion = ""

func init() {
	info, ok := debug.ReadBuildInfo()
	if !ok || info == nil {
		return
	}
	if Version == "" || Version == "0.1.0" {
		if v := info.Main.Version; v != "" && v != "(devel)" {
			Version = v
		}
	}
	goVersion = info.GoVersion
	for _, setting := range info.Settings {
		switch setting.Key {
		case "vcs.revision":
			if Commit == "" && len(setting.Value) >= 7 {
				Commit = setting.Value[:7]
			}
		case "vcs.time":
			if BuildDate == "" {
				BuildDate = setting.Value
			}
		case "vcs.modified":
			if Dirty == "" || Dirty == "false" {
				Dirty = setting.Value
			}
		}
	}
}

func String() string {
	parts := []string{}
	if Commit != "" {
		parts = append(parts, "commit "+Commit)
	}
	if BuildDate != "" {
		parts = append(parts, "built "+BuildDate)
	}
	if Dirty == "true" {
		parts = append(parts, "dirty")
	}
	if goVersion != "" {
		parts = append(parts, goVersion)
	}
	if len(parts) == 0 {
		return "sn-cli " + Version
	}
	return "sn-cli " + Version + " (" + strings.Join(parts, ", ") + ")"
}
