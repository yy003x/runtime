package agentrun

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"
)

type Paths struct {
	RunDir      string
	RequestFile string
	StatusFile  string
	EventsFile  string
	OutputLog   string
	ResultFile  string
	DoneFile    string
}

func RunPaths(runsDir, runType, runID string) (Paths, error) {
	if !validRunType(runType) {
		return Paths{}, fmt.Errorf("未知 run_type: %s", runType)
	}
	if err := validateRunID(runID); err != nil {
		return Paths{}, err
	}
	runDir := filepath.Join(runsDir, runType, dateFromRunID(runID), runID)
	return Paths{
		RunDir:      runDir,
		RequestFile: filepath.Join(runDir, "request.json"),
		StatusFile:  filepath.Join(runDir, "status.json"),
		EventsFile:  filepath.Join(runDir, "events.jsonl"),
		OutputLog:   filepath.Join(runDir, "output.log"),
		ResultFile:  filepath.Join(runDir, "result.json"),
		DoneFile:    filepath.Join(runDir, "done"),
	}, nil
}

func validateRunID(runID string) error {
	if strings.TrimSpace(runID) != runID || runID == "" || len(runID) > 128 || runID == "." || runID == ".." {
		return fmt.Errorf("invalid run_id: %q", runID)
	}
	if filepath.Base(runID) != runID || strings.ContainsAny(runID, `/\\`) || strings.Contains(runID, "..") {
		return fmt.Errorf("invalid run_id: %q", runID)
	}
	for index, value := range []byte(runID) {
		valid := value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9' || value == '-' || value == '_' || value == '.'
		if !valid || index == 0 && !(value >= 'a' && value <= 'z' || value >= 'A' && value <= 'Z' || value >= '0' && value <= '9') {
			return fmt.Errorf("invalid run_id: %q", runID)
		}
	}
	return nil
}

func (p Paths) Ensure() error {
	if err := os.MkdirAll(p.RunDir, 0o700); err != nil {
		return fmt.Errorf("create run dir: %w", err)
	}
	return nil
}

func dateFromRunID(runID string) string {
	parts := strings.Split(runID, "-")
	if len(parts) >= 2 && len(parts[1]) == 8 {
		if parsed, err := time.Parse("20060102", parts[1]); err == nil {
			return parsed.Format("2006-01-02")
		}
	}
	return time.Now().Format("2006-01-02")
}

func validRunType(value string) bool {
	return value == RunSession || value == RunTurn || value == RunTask || value == RunCommand
}
