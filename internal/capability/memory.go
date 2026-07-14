package capability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"time"
)

type MemoryItem struct {
	ID        string    `json:"id"`
	Type      string    `json:"type"`
	Content   string    `json:"content"`
	Source    string    `json:"source"`
	CreatedAt time.Time `json:"created_at"`
}

type Memory struct {
	Path  string
	items map[string]MemoryItem
}

func OpenMemory(path string) (*Memory, error) {
	memory := &Memory{Path: path, items: make(map[string]MemoryItem)}
	if path == "" {
		return memory, nil
	}
	data, err := os.ReadFile(path)
	if err == nil {
		var items []MemoryItem
		if err := json.Unmarshal(data, &items); err != nil {
			return nil, err
		}
		for _, item := range items {
			memory.items[item.ID] = item
		}
	} else if !os.IsNotExist(err) {
		return nil, err
	}
	return memory, nil
}

func (m *Memory) Write(items []MemoryItem) error {
	for _, item := range items {
		if item.CreatedAt.IsZero() {
			item.CreatedAt = time.Now().UTC()
		}
		m.items[item.ID] = item
	}
	return m.save()
}

func (m *Memory) Recall(query, kind string, topK int) []MemoryItem {
	if topK <= 0 {
		topK = 5
	}
	lower := strings.ToLower(query)
	var out []MemoryItem
	for _, item := range m.items {
		if strings.Contains(strings.ToLower(item.Content), lower) && (kind == "" || item.Type == kind) {
			out = append(out, item)
		}
	}
	sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.Before(out[j].CreatedAt) })
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}

func (m *Memory) Forget(ids []string) error {
	for _, id := range ids {
		delete(m.items, id)
	}
	return m.save()
}

func (m *Memory) Sources() []map[string]any {
	counts := make(map[string]int)
	for _, item := range m.items {
		counts[item.Source]++
	}
	var names []string
	for name := range counts {
		names = append(names, name)
	}
	sort.Strings(names)
	out := make([]map[string]any, 0, len(names))
	for _, name := range names {
		out = append(out, map[string]any{"source": name, "count": counts[name]})
	}
	return out
}

func (m *Memory) save() error {
	if m.Path == "" {
		return nil
	}
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o700); err != nil {
		return err
	}
	items := make([]MemoryItem, 0, len(m.items))
	for _, item := range m.items {
		items = append(items, item)
	}
	data, err := json.MarshalIndent(items, "", "  ")
	if err != nil {
		return err
	}
	temporary, err := os.CreateTemp(filepath.Dir(m.Path), ".memory-*.json")
	if err != nil {
		return err
	}
	temporaryPath := temporary.Name()
	defer os.Remove(temporaryPath)
	if _, err := temporary.Write(append(data, '\n')); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Chmod(0o644); err != nil {
		_ = temporary.Close()
		return err
	}
	if err := temporary.Close(); err != nil {
		return err
	}
	return os.Rename(temporaryPath, m.Path)
}
