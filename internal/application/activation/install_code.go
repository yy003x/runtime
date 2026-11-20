package activation

import (
	"errors"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"strings"

	"github.com/yy003x/sn-code/internal/infrastructure/activationgate"
	"github.com/yy003x/sn-code/internal/infrastructure/layout"
)

// InstallCodeBinary 仅供源码安装脚本使用。Runtime 激活会替换整个 bin 目录，
// 因此独立安装必须先取得同一把维护锁。
func InstallCodeBinary(source, home, commands string) (string, error) {
	home, err := layout.CanonicalHome(home)
	if err != nil {
		return "", err
	}
	commands, err = layout.CanonicalHome(commands)
	if err != nil {
		return "", err
	}
	if home == "/" || commands == "/" {
		return "", errors.New("installation paths must not be /")
	}
	for parent := commands; parent != "/"; parent = filepath.Dir(parent) {
		if strings.EqualFold(parent, home) {
			return "", errors.New("command directory must be outside the Runtime home")
		}
	}
	info, err := os.Lstat(source)
	if err != nil || !info.Mode().IsRegular() || info.Mode().Perm()&0111 == 0 {
		return "", fmt.Errorf("source must be an executable regular file: %s", source)
	}
	stateDir := filepath.Join(home, "state")
	for _, directory := range []string{home, stateDir} {
		if err := ensureCodeInstallDirectory(directory); err != nil {
			return "", err
		}
	}
	lock, err := acquireUpgradeLock(filepath.Join(stateDir, maintenanceLockName))
	if err != nil {
		return "", err
	}
	defer lock.Close()
	if err := activationgate.RequireOpen(stateDir); err != nil {
		return "", err
	}
	bin := filepath.Join(home, "bin")
	if err := ensureCodeInstallDirectory(bin); err != nil {
		return "", err
	}
	target := filepath.Join(bin, "sn-code")
	info, err = os.Lstat(target)
	if err == nil && !info.Mode().IsRegular() {
		return "", fmt.Errorf("binary target must be a regular file: %s", target)
	}
	if err != nil && !errors.Is(err, os.ErrNotExist) {
		return "", err
	}
	stage, err := os.MkdirTemp(bin, ".sn-code-install-")
	if err != nil {
		return "", err
	}
	defer os.RemoveAll(stage)
	candidate := filepath.Join(stage, "sn-code")
	if err := copyRegular(source, candidate, 0755); err != nil {
		return "", err
	}
	version, err := exec.Command(candidate, "--version").Output()
	if err != nil {
		return "", fmt.Errorf("check sn-code version: %w", err)
	}
	if !strings.HasPrefix(string(version), "sn-code ") {
		return "", errors.New("source binary does not identify itself as sn-code")
	}
	if err := os.MkdirAll(commands, 0755); err != nil {
		return "", err
	}
	link := filepath.Join(commands, "sn-code")
	err = os.Symlink(target, link)
	created := err == nil
	if errors.Is(err, os.ErrExist) {
		err = ValidateCommandLink(link, target)
	}
	if err != nil {
		return "", err
	}
	if err := os.Rename(candidate, target); err != nil {
		if dest, readErr := os.Readlink(link); created && readErr == nil && dest == target {
			err = errors.Join(err, os.Remove(link))
		}
		return "", err
	}
	return fmt.Sprintf("installed binary: %s\ninstalled command: %s\n%s", target, link, version), nil
}

func ensureCodeInstallDirectory(path string) error {
	if err := os.MkdirAll(path, 0700); err != nil {
		return err
	}
	info, err := os.Lstat(path)
	if err != nil {
		return err
	}
	if !info.IsDir() {
		return fmt.Errorf("expected a directory at %s", path)
	}
	return nil
}
