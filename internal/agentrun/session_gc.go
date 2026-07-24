package agentrun

import (
	"fmt"
	"time"
)

type SessionGCOptions struct {
	Before time.Time
	Limit  int
	Apply  bool
}

type SessionGCItem struct {
	SessionID string    `json:"session_id"`
	UpdatedAt time.Time `json:"updated_at"`
	TrashPath string    `json:"trash_path,omitempty"`
}

type SessionGCResult struct {
	Before     time.Time       `json:"before"`
	Apply      bool            `json:"apply"`
	Candidates int             `json:"candidates"`
	Processed  int             `json:"processed"`
	Items      []SessionGCItem `json:"items"`
}

func (s *SessionStore) GC(options SessionGCOptions) (SessionGCResult, error) {
	if options.Before.IsZero() {
		return SessionGCResult{}, fmt.Errorf("session GC requires before")
	}
	if options.Limit < 0 || options.Limit > 1000 {
		return SessionGCResult{}, fmt.Errorf("session GC limit must be between 0 and 1000")
	}
	values, err := s.List(SessionFilter{
		Retention: RetentionEphemeral, ToTime: options.Before,
	})
	if err != nil {
		return SessionGCResult{}, err
	}
	result := SessionGCResult{
		Before: options.Before, Apply: options.Apply, Items: []SessionGCItem{},
	}
	for _, value := range values {
		current, err := s.Get(value.SessionID)
		if err != nil {
			return result, err
		}
		if !gcEligibleSession(current, options.Before) {
			continue
		}
		result.Candidates++
		if options.Limit > 0 && len(result.Items) >= options.Limit {
			continue
		}
		item := SessionGCItem{SessionID: current.SessionID, UpdatedAt: current.UpdatedAt}
		if options.Apply {
			trashPath, trashed, err := s.trashSession(current.SessionID, func(record SessionRecord) bool {
				return gcEligibleSession(record, options.Before)
			})
			if err != nil {
				return result, err
			}
			if !trashed {
				continue
			}
			item.TrashPath = trashPath
			result.Processed++
		}
		result.Items = append(result.Items, item)
	}
	return result, nil
}

func gcEligibleSession(record SessionRecord, before time.Time) bool {
	return record.Retention == RetentionEphemeral &&
		!record.UpdatedAt.After(before) &&
		gcEligibleState(record.State)
}

func gcEligibleState(state string) bool {
	switch normalizeSessionState(state) {
	case SessionStateIdle, SessionStateArchived, StateDone, StateFailed, StateCancelled:
		return true
	default:
		return false
	}
}
