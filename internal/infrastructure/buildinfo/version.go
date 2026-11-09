// Package buildinfo 保存共享构建元数据，产品名称由入口传入。
package buildinfo

import (
	"runtime/debug"
	"strings"
)

var (
	Version   = "v0.0.0-dev"
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
	if Version == "" || Version == "v0.0.0-dev" {
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

func String(product string) string {
	return product + " " + Summary()
}

// Summary 返回不含产品名的构建信息，供协议握手使用。
func Summary() string {
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
		return Version
	}
	return Version + " (" + strings.Join(parts, ", ") + ")"
}
