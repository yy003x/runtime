package agentrun

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"syscall"
	"time"
)

type registryDocument struct {
	Runs map[string]registryEntry `json:"runs"`
}

type registryEntry struct {
	RunType   string    `json:"run_type"`
	ProjectID string    `json:"project_id"`
	RunDir    string    `json:"run_dir"`
	State     string    `json:"state"`
	UpdatedAt time.Time `json:"updated_at"`
}

// ResolveRunType returns the persisted run type for a public run ID. The
// registry and queue are the owners of this relationship; callers must not
// infer it from an ID prefix or reconstruct run paths themselves.
func (s *Service) ResolveRunType(runID string) (string, error) {
	if err := validateRunID(runID); err != nil {
		return "", err
	}
	resolved := ""
	if err := s.withQueue(false, func(document *queueDocument) error {
		for _, entry := range document.Entries {
			if entry.RunID != runID {
				continue
			}
			if resolved != "" && resolved != entry.RunType {
				return fmt.Errorf("run_id %s is registered with multiple run types", runID)
			}
			resolved = entry.RunType
		}
		return nil
	}); err != nil {
		return "", err
	}
	if err := s.withRegistry(false, func(document *registryDocument) {
		if entry, ok := document.Runs[runID]; ok {
			if resolved == "" || resolved == entry.RunType {
				resolved = entry.RunType
			} else {
				resolved = "conflict:" + entry.RunType
			}
		}
	}); err != nil {
		return "", err
	}
	if strings.HasPrefix(resolved, "conflict:") {
		return "", fmt.Errorf("run_id %s is registered with multiple run types", runID)
	}
	if resolved == "" {
		return "", fmt.Errorf("run not found: %s", runID)
	}
	return resolved, nil
}

func (s *Service) register(paths Paths, request Request, state string) {
	_ = s.withRegistry(true, func(document *registryDocument) {
		document.Runs[request.RunID] = registryEntry{RunType: request.RunType, ProjectID: request.ProjectID, RunDir: paths.RunDir, State: state, UpdatedAt: time.Now().UTC()}
	})
}

func (s *Service) updateRegistry(paths Paths, status Status) {
	_ = s.withRegistry(true, func(document *registryDocument) {
		entry := document.Runs[status.RunID]
		entry.RunType, entry.ProjectID, entry.RunDir = status.RunType, status.ProjectID, paths.RunDir
		entry.State, entry.UpdatedAt = status.State, time.Now().UTC()
		document.Runs[status.RunID] = entry
	})
}

func (s *Service) registerLoop(paths LoopPaths, request LoopRequest, state string) {
	_ = s.withRegistry(true, func(document *registryDocument) {
		document.Runs[request.LoopID] = registryEntry{RunType: "loop", ProjectID: request.ProjectID, RunDir: paths.LoopDir, State: state, UpdatedAt: time.Now().UTC()}
	})
}

func (s *Service) updateLoopRegistry(paths LoopPaths, status PersistentLoopStatus) {
	_ = s.withRegistry(true, func(document *registryDocument) {
		entry := document.Runs[status.LoopID]
		entry.RunType, entry.ProjectID, entry.RunDir = "loop", status.ProjectID, paths.LoopDir
		entry.State, entry.UpdatedAt = status.State, time.Now().UTC()
		document.Runs[status.LoopID] = entry
	})
}

func (s *Service) Prune(dryRun bool) (map[string]any, error) {
	removed := []string{}
	err := s.withRegistry(!dryRun, func(document *registryDocument) {
		for runID, entry := range document.Runs {
			var status Status
			if data, readErr := os.ReadFile(filepath.Join(entry.RunDir, "status.json")); readErr == nil {
				_ = json.Unmarshal(data, &status)
			}
			state := entry.State
			if status.State != "" {
				state = status.State
			}
			if terminalStateValue(state) {
				removed = append(removed, runID)
				if !dryRun {
					delete(document.Runs, runID)
				}
			}
		}
	})
	if err != nil {
		return nil, err
	}
	return map[string]any{"dry_run": dryRun, "removed": removed}, nil
}

func (s *Service) withRegistry(persist bool, update func(*registryDocument)) error {
	stateDir := filepath.Join(s.StateDir, "runs")
	if err := os.MkdirAll(stateDir, 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(filepath.Join(stateDir, "registry.lock"), os.O_CREATE|os.O_RDWR, 0o644)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	path := filepath.Join(stateDir, "registry.json")
	document := registryDocument{Runs: map[string]registryEntry{}}
	if data, readErr := os.ReadFile(path); readErr == nil {
		if err := json.Unmarshal(data, &document); err != nil {
			return fmt.Errorf("parse registry: %w", err)
		}
	}
	if document.Runs == nil {
		document.Runs = map[string]registryEntry{}
	}
	update(&document)
	if !persist {
		return nil
	}
	return writeJSONAtomic(path, document)
}
