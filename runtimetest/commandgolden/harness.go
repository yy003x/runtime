// Package commandgolden contains test-only helpers for freezing the observable
// command profile behavior that Runtime vNext must preserve.
package commandgolden

import (
	"encoding/json"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path/filepath"
	"strings"
	"time"
)

// Baseline records where a command golden came from and the exact provider
// arguments/environment expected for each public command profile.
type Baseline struct {
	CapturedAt     string          `json:"captured_at"`
	SourceHead     string          `json:"source_head"`
	InstalledBuild string          `json:"installed_build"`
	Profiles       []ProfileGolden `json:"profiles"`
}

type ProfileGolden struct {
	ID   string            `json:"id"`
	Argv []string          `json:"argv"`
	Env  map[string]string `json:"env"`
}

// Capture is emitted by the fake command target.
type Capture struct {
	Argv    []string          `json:"argv"`
	Env     map[string]string `json:"env"`
	Missing []string          `json:"missing,omitempty"`
	TTY     TTYState          `json:"tty"`
	Stdin   string            `json:"stdin,omitempty"`
	Signals []string          `json:"signals,omitempty"`
}

type TTYState struct {
	Stdin  bool `json:"stdin"`
	Stdout bool `json:"stdout"`
	Stderr bool `json:"stderr"`
}

func LoadBaseline(path string) (Baseline, error) {
	file, err := os.Open(path)
	if err != nil {
		return Baseline{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var baseline Baseline
	if err := decoder.Decode(&baseline); err != nil {
		return Baseline{}, err
	}
	if baseline.SourceHead == "" || baseline.InstalledBuild == "" || len(baseline.Profiles) == 0 {
		return Baseline{}, fmt.Errorf("incomplete command golden baseline")
	}
	return baseline, nil
}

func ReadCapture(path string) (Capture, error) {
	file, err := os.Open(path)
	if err != nil {
		return Capture{}, err
	}
	defer file.Close()
	decoder := json.NewDecoder(io.LimitReader(file, 1<<20))
	decoder.DisallowUnknownFields()
	var capture Capture
	if err := decoder.Decode(&capture); err != nil {
		return Capture{}, err
	}
	return capture, nil
}

func Expand(value string, replacements map[string]string) string {
	for name, replacement := range replacements {
		value = strings.ReplaceAll(value, "${"+name+"}", replacement)
	}
	return value
}

func Build(repoRoot, output, packagePath string) error {
	command := exec.Command("go", "build", "-o", output, packagePath)
	command.Dir = repoRoot
	command.Stdout = os.Stdout
	command.Stderr = os.Stderr
	return command.Run()
}

func CopyFile(source, destination string, mode os.FileMode) error {
	value, err := os.ReadFile(source)
	if err != nil {
		return err
	}
	if err := os.MkdirAll(filepath.Dir(destination), 0o755); err != nil {
		return err
	}
	return os.WriteFile(destination, value, mode)
}

func WaitForFile(path string, timeout time.Duration) error {
	deadline := time.Now().Add(timeout)
	for {
		if _, err := os.Stat(path); err == nil {
			return nil
		} else if !os.IsNotExist(err) {
			return err
		}
		if time.Now().After(deadline) {
			return fmt.Errorf("timed out waiting for %s", path)
		}
		time.Sleep(20 * time.Millisecond)
	}
}
