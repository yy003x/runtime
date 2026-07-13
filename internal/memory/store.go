package memory

import (
	"context"
	"fmt"
	"sync"
)

type Store interface {
	Get(ctx context.Context, sessionID string) (*SessionMemory, error)
	Save(ctx context.Context, session *SessionMemory) error
}

type InMemoryStore struct {
	mu       sync.RWMutex
	sessions map[string]*SessionMemory
}

func NewInMemoryStore() *InMemoryStore {
	return &InMemoryStore{sessions: make(map[string]*SessionMemory)}
}

func (s *InMemoryStore) Get(_ context.Context, sessionID string) (*SessionMemory, error) {
	s.mu.RLock()
	defer s.mu.RUnlock()

	session, ok := s.sessions[sessionID]
	if !ok {
		return nil, fmt.Errorf("session %q not found", sessionID)
	}

	cloned := *session
	cloned.Turns = append([]Turn(nil), session.Turns...)
	cloned.Retrieved = append([]RetrievedMemory(nil), session.Retrieved...)
	if session.Summary != nil {
		summary := *session.Summary
		summary.Bullets = append([]string(nil), session.Summary.Bullets...)
		cloned.Summary = &summary
	}

	return &cloned, nil
}

func (s *InMemoryStore) Save(_ context.Context, session *SessionMemory) error {
	s.mu.Lock()
	defer s.mu.Unlock()

	cloned := *session
	cloned.Turns = append([]Turn(nil), session.Turns...)
	cloned.Retrieved = append([]RetrievedMemory(nil), session.Retrieved...)
	if session.Summary != nil {
		summary := *session.Summary
		summary.Bullets = append([]string(nil), session.Summary.Bullets...)
		cloned.Summary = &summary
	}
	s.sessions[session.SessionID] = &cloned
	return nil
}
