package capability

import (
	"encoding/json"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"sync"
	"syscall"
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
	mu    sync.RWMutex
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
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.withFileLock(func() error {
		if err := m.load(); err != nil {
			return err
		}
		for _, item := range items {
			if item.CreatedAt.IsZero() {
				item.CreatedAt = time.Now().UTC()
			}
			m.items[item.ID] = item
		}
		return m.save()
	})
}

func (m *Memory) Recall(query, kind string, topK int) []MemoryItem {
	m.mu.RLock()
	defer m.mu.RUnlock()
	if topK <= 0 {
		topK = 5
	}
	lower := strings.ToLower(strings.TrimSpace(query))
	terms := strings.Fields(lower)
	type scoredItem struct {
		item  MemoryItem
		score int
	}
	var matches []scoredItem
	var fallback []MemoryItem
	for _, item := range m.items {
		if kind != "" && item.Type != kind {
			continue
		}
		fallback = append(fallback, item)
		content := strings.ToLower(item.Content)
		score := 0
		if lower == "" || strings.Contains(content, lower) {
			score += 100
		}
		for _, term := range terms {
			if len([]rune(term)) >= 2 && strings.Contains(content, term) {
				score++
			}
		}
		if score > 0 {
			matches = append(matches, scoredItem{item: item, score: score})
		}
	}
	sort.Slice(matches, func(i, j int) bool {
		if matches[i].score != matches[j].score {
			return matches[i].score > matches[j].score
		}
		return matches[i].item.CreatedAt.After(matches[j].item.CreatedAt)
	})
	out := make([]MemoryItem, 0, len(matches))
	for _, match := range matches {
		out = append(out, match.item)
	}
	if len(out) == 0 {
		out = fallback
		sort.Slice(out, func(i, j int) bool { return out[i].CreatedAt.After(out[j].CreatedAt) })
	}
	if len(out) > topK {
		out = out[:topK]
	}
	return out
}

func (m *Memory) Forget(ids []string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.withFileLock(func() error {
		if err := m.load(); err != nil {
			return err
		}
		for _, id := range ids {
			delete(m.items, id)
		}
		return m.save()
	})
}

func (m *Memory) Sources() []map[string]any {
	m.mu.RLock()
	defer m.mu.RUnlock()
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

func (m *Memory) load() error {
	m.items = make(map[string]MemoryItem)
	if m.Path == "" {
		return nil
	}
	data, err := os.ReadFile(m.Path)
	if os.IsNotExist(err) {
		return nil
	}
	if err != nil {
		return err
	}
	var items []MemoryItem
	if err := json.Unmarshal(data, &items); err != nil {
		return err
	}
	for _, item := range items {
		m.items[item.ID] = item
	}
	return nil
}

func (m *Memory) withFileLock(operation func() error) error {
	if m.Path == "" {
		return operation()
	}
	if err := os.MkdirAll(filepath.Dir(m.Path), 0o700); err != nil {
		return err
	}
	lock, err := os.OpenFile(m.Path+".lock", os.O_CREATE|os.O_RDWR, 0o600)
	if err != nil {
		return err
	}
	defer lock.Close()
	if err := syscall.Flock(int(lock.Fd()), syscall.LOCK_EX); err != nil {
		return err
	}
	defer syscall.Flock(int(lock.Fd()), syscall.LOCK_UN) //nolint:errcheck
	return operation()
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
