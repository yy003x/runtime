package native

import (
	"encoding/json"
	"fmt"
	"os"
	"path/filepath"
)

type FileStore struct {
	SnapshotPath string
	ControlPath  string
}

type Control struct {
	Action string       `json:"action"`
	Reason string       `json:"reason,omitempty"`
	Patch  ContextPatch `json:"patch,omitempty"`
}

func NewFileStore(snapshotPath string) *FileStore {
	return &FileStore{SnapshotPath: snapshotPath, ControlPath: snapshotPath + ".control"}
}

func (s *FileStore) Load() (Snapshot, error) {
	data, err := os.ReadFile(s.SnapshotPath)
	if err != nil {
		return Snapshot{}, err
	}
	var snapshot Snapshot
	if err := json.Unmarshal(data, &snapshot); err != nil {
		return Snapshot{}, fmt.Errorf("decode native snapshot: %w", err)
	}
	return snapshot, nil
}

func (s *FileStore) Save(snapshot Snapshot) error {
	if err := os.MkdirAll(filepath.Dir(s.SnapshotPath), 0o755); err != nil {
		return err
	}
	data, err := json.MarshalIndent(snapshot, "", "  ")
	if err != nil {
		return err
	}
	temporary := s.SnapshotPath + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o644); err != nil {
		return err
	}
	return os.Rename(temporary, s.SnapshotPath)
}

func (s *FileStore) WriteControl(control Control) error {
	data, err := json.Marshal(control)
	if err != nil {
		return err
	}
	temporary := s.ControlPath + ".tmp"
	if err := os.WriteFile(temporary, append(data, '\n'), 0o600); err != nil {
		return err
	}
	return os.Rename(temporary, s.ControlPath)
}

func (s *FileStore) TakeControl() (Control, bool, error) {
	data, err := os.ReadFile(s.ControlPath)
	if os.IsNotExist(err) {
		return Control{}, false, nil
	}
	if err != nil {
		return Control{}, false, err
	}
	var control Control
	if err := json.Unmarshal(data, &control); err != nil {
		return Control{}, false, err
	}
	if err := os.Remove(s.ControlPath); err != nil && !os.IsNotExist(err) {
		return Control{}, false, err
	}
	return control, true, nil
}
