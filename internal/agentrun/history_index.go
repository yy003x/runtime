package agentrun

import (
	"fmt"
	"os"
	"path/filepath"
	"sort"
	"time"
)

func (s *SessionStore) indexPath() string {
	return filepath.Join(s.historyDir, "index.json")
}

func (s *SessionStore) indexEntry(sessionID string) (SessionIndexEntry, bool, error) {
	index, err := s.loadIndex()
	if err != nil {
		return SessionIndexEntry{}, false, err
	}
	entry, ok := index.Sessions[sessionID]
	return entry, ok, nil
}

func (s *SessionStore) loadIndex() (SessionIndex, error) {
	index := SessionIndex{SchemaVersion: SessionSchemaVersion, Sessions: map[string]SessionIndexEntry{}}
	if err := readJSON(s.indexPath(), &index); err != nil {
		if os.IsNotExist(err) {
			return index, nil
		}
		return SessionIndex{}, err
	}
	if index.Sessions == nil {
		index.Sessions = map[string]SessionIndexEntry{}
	}
	for id, entry := range index.Sessions {
		entry.State = normalizeSessionState(entry.State)
		index.Sessions[id] = entry
	}
	return index, nil
}

func (s *SessionStore) updateIndex(record SessionRecord, dir string) error {
	if err := s.ensure(); err != nil {
		return err
	}
	return withAdvisoryFileLock(s.indexPath()+".lock", func() error {
		index, err := s.loadIndex()
		if err != nil {
			return err
		}
		index.SchemaVersion = SessionSchemaVersion
		index.UpdatedAt = time.Now().UTC()
		index.Sessions[record.SessionID] = SessionIndexEntry{
			SessionID: record.SessionID, ProjectID: record.ProjectID, State: record.State, Title: record.Title,
			Summary: record.Summary, SessionDir: dir, RecordMode: record.RecordMode, Retention: record.Retention,
			CaptureQuality: record.CaptureQuality, Runtime: record.Runtime, Profile: record.Profile, TurnCount: record.TurnCount, RunCount: record.RunCount,
			CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, Tags: append([]string(nil), record.Tags...),
		}
		return writeJSONAtomic(s.indexPath(), index)
	})
}

func (s *SessionStore) List(filter SessionFilter) ([]SessionIndexEntry, error) {
	index, err := s.loadIndex()
	if err != nil {
		return nil, err
	}
	values := make([]SessionIndexEntry, 0, len(index.Sessions))
	for _, entry := range index.Sessions {
		if filter.ProjectID != "" && entry.ProjectID != filter.ProjectID {
			continue
		}
		if filter.State != "" && entry.State != filter.State {
			continue
		}
		if filter.Retention != "" && entry.Retention != filter.Retention {
			continue
		}
		if !filter.FromTime.IsZero() && entry.UpdatedAt.Before(filter.FromTime) {
			continue
		}
		if !filter.ToTime.IsZero() && entry.UpdatedAt.After(filter.ToTime) {
			continue
		}
		if len(filter.Tags) > 0 && !hasAnySessionTag(entry.Tags, filter.Tags) {
			continue
		}
		values = append(values, entry)
	}
	sort.Slice(values, func(i, j int) bool {
		if values[i].UpdatedAt.Equal(values[j].UpdatedAt) {
			return values[i].SessionID < values[j].SessionID
		}
		return values[i].UpdatedAt.After(values[j].UpdatedAt)
	})
	if filter.Limit > 0 && len(values) > filter.Limit {
		values = values[:filter.Limit]
	}
	return values, nil
}

func (s *SessionStore) RebuildIndex() (SessionIndex, error) {
	if err := s.ensure(); err != nil {
		return SessionIndex{}, err
	}
	index := SessionIndex{SchemaVersion: SessionSchemaVersion, UpdatedAt: time.Now().UTC(), Sessions: map[string]SessionIndexEntry{}}
	err := filepath.WalkDir(s.sessionsDir, func(path string, entry os.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if entry.IsDir() || entry.Name() != "session.json" {
			return nil
		}
		var record SessionRecord
		if err := readJSON(path, &record); err != nil {
			return fmt.Errorf("rebuild session index from %s: %w", path, err)
		}
		record.State = normalizeSessionState(record.State)
		dir := filepath.Dir(path)
		index.Sessions[record.SessionID] = SessionIndexEntry{
			SessionID: record.SessionID, ProjectID: record.ProjectID, State: record.State, Title: record.Title,
			Summary: record.Summary, SessionDir: dir, RecordMode: record.RecordMode, Retention: record.Retention,
			CaptureQuality: record.CaptureQuality, Runtime: record.Runtime, Profile: record.Profile, TurnCount: record.TurnCount, RunCount: record.RunCount,
			CreatedAt: record.CreatedAt, UpdatedAt: record.UpdatedAt, Tags: append([]string(nil), record.Tags...),
		}
		return nil
	})
	if err != nil {
		return SessionIndex{}, err
	}
	err = withAdvisoryFileLock(s.indexPath()+".lock", func() error { return writeJSONAtomic(s.indexPath(), index) })
	return index, err
}

func hasAnySessionTag(values, required []string) bool {
	seen := make(map[string]struct{}, len(values))
	for _, value := range values {
		seen[value] = struct{}{}
	}
	for _, value := range required {
		if _, ok := seen[value]; ok {
			return true
		}
	}
	return false
}
