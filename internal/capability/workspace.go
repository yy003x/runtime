package capability

import (
	"fmt"
	"os"
	"path/filepath"
	"strings"
)

type WorkspaceManager struct {
	Root string
}

func NewWorkspaceManager(root string) (*WorkspaceManager, error) {
	absolute, err := filepath.Abs(root)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(absolute, 0o755); err != nil {
		return nil, err
	}
	return &WorkspaceManager{Root: absolute}, nil
}

func (m *WorkspaceManager) Path(parts ...string) (string, error) {
	for _, part := range parts {
		for _, component := range strings.Split(filepath.ToSlash(part), "/") {
			if component == ".." {
				return "", fmt.Errorf("路径含 ..,拒绝: %v", parts)
			}
		}
	}
	resolved, err := filepath.Abs(filepath.Join(append([]string{m.Root}, parts...)...))
	if err != nil {
		return "", err
	}
	relative, err := filepath.Rel(m.Root, resolved)
	if err != nil || relative == ".." || strings.HasPrefix(relative, ".."+string(filepath.Separator)) {
		return "", fmt.Errorf("路径越界: %s", resolved)
	}
	return resolved, nil
}

func (m *WorkspaceManager) RunWorkspace(runID string) (string, error) {
	path, err := m.Path("work", runID)
	if err != nil {
		return "", err
	}
	if err := os.MkdirAll(path, 0o755); err != nil {
		return "", err
	}
	return path, nil
}

func (m *WorkspaceManager) GC(runID string) error {
	path, err := m.Path("work", runID)
	if err != nil {
		return err
	}
	return os.RemoveAll(path)
}
