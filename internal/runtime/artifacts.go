package runtime

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type RunPaths struct {
	RunDir       string
	RequestPath  string
	ConfigPath   string
	EventsPath   string
	StdoutPath   string
	StderrPath   string
	OutputPath   string
	ResultPath   string
	ArtifactsDir string
}

func createRunPaths(root, artifactsRoot, runID string, startedAt time.Time) (RunPaths, error) {
	base, err := resolveArtifactsBase(root, artifactsRoot)
	if err != nil {
		return RunPaths{}, err
	}

	runDir := filepath.Join(base, "runs", startedAt.UTC().Format("2006-01-02"), runID)
	paths := RunPaths{
		RunDir:       runDir,
		RequestPath:  filepath.Join(runDir, "request.json"),
		ConfigPath:   filepath.Join(runDir, "resolved_config.json"),
		EventsPath:   filepath.Join(runDir, "events.jsonl"),
		StdoutPath:   filepath.Join(runDir, "stdout.log"),
		StderrPath:   filepath.Join(runDir, "stderr.log"),
		OutputPath:   filepath.Join(runDir, "output.txt"),
		ResultPath:   filepath.Join(runDir, "result.json"),
		ArtifactsDir: filepath.Join(runDir, "artifacts"),
	}
	if err := os.MkdirAll(paths.ArtifactsDir, 0o755); err != nil {
		return RunPaths{}, fmt.Errorf("create run artifacts dir: %w", err)
	}
	return paths, nil
}

func resolveArtifactsBase(root, artifactsRoot string) (string, error) {
	base := strings.TrimSpace(artifactsRoot)
	if base == "" {
		base = DefaultArtifactsRoot
	}
	if filepath.IsAbs(base) {
		return "", fmt.Errorf("artifacts.root must be relative under runs/: %s", artifactsRoot)
	}
	clean := filepath.Clean(base)
	if clean == "." || clean == ".." || strings.HasPrefix(clean, ".."+string(os.PathSeparator)) {
		return "", fmt.Errorf("artifacts.root must stay under runs/: %s", artifactsRoot)
	}
	if clean != "runs" && !strings.HasPrefix(clean, "runs"+string(os.PathSeparator)) {
		return "", fmt.Errorf("artifacts.root must stay under runs/: %s", artifactsRoot)
	}
	return filepath.Join(root, clean), nil
}

func writeRunArtifacts(paths RunPaths, req RunRequest, profile *Profile, events []Event, stdout, stderr string, result *RunResult) error {
	if err := writeJSON(paths.RequestPath, req); err != nil {
		return err
	}
	if profile != nil {
		if err := writeJSON(paths.ConfigPath, profile); err != nil {
			return err
		}
	}
	if err := writeEvents(paths.EventsPath, events); err != nil {
		return err
	}
	if err := os.WriteFile(paths.StdoutPath, []byte(stdout), 0o644); err != nil {
		return fmt.Errorf("write stdout log: %w", err)
	}
	if err := os.WriteFile(paths.StderrPath, []byte(stderr), 0o644); err != nil {
		return fmt.Errorf("write stderr log: %w", err)
	}
	if err := os.WriteFile(paths.OutputPath, []byte(result.FinalText), 0o644); err != nil {
		return fmt.Errorf("write output: %w", err)
	}
	if err := writeJSON(paths.ResultPath, result); err != nil {
		return err
	}
	return nil
}

func writeJSON(path string, value any) error {
	payload, err := json.MarshalIndent(value, "", "  ")
	if err != nil {
		return fmt.Errorf("marshal %s: %w", filepath.Base(path), err)
	}
	payload = append(payload, '\n')
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write %s: %w", filepath.Base(path), err)
	}
	return nil
}

func writeEvents(path string, events []Event) error {
	var payload []byte
	for _, event := range events {
		line, err := json.Marshal(event)
		if err != nil {
			return fmt.Errorf("marshal event: %w", err)
		}
		payload = append(payload, line...)
		payload = append(payload, '\n')
	}
	if err := os.WriteFile(path, payload, 0o644); err != nil {
		return fmt.Errorf("write events: %w", err)
	}
	return nil
}
