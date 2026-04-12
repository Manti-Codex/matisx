package mcp

import (
	"strings"
	"sync"
	"time"
)

type SessionStore interface {
	Get(sessionKey string) (*SessionState, bool)
	Put(sessionKey string, state *SessionState)
	Delete(sessionKey string)
	ListApprovals(sessionKey string) []ApprovalRequest
	CleanupExpired(sessionTTL time.Duration)
}

type InMemorySessionStore struct {
	mu    sync.Mutex
	items map[string]*SessionState
}

func NewInMemorySessionStore() *InMemorySessionStore {
	return &InMemorySessionStore{items: map[string]*SessionState{}}
}

func (s *InMemorySessionStore) Get(sessionKey string) (*SessionState, bool) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.TrimSpace(sessionKey)
	v, ok := s.items[key]
	return v, ok
}

func (s *InMemorySessionStore) Put(sessionKey string, state *SessionState) {
	if state == nil {
		return
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.TrimSpace(sessionKey)
	s.items[key] = state
}

func (s *InMemorySessionStore) Delete(sessionKey string) {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.TrimSpace(sessionKey)
	delete(s.items, key)
}

func (s *InMemorySessionStore) ListApprovals(sessionKey string) []ApprovalRequest {
	s.mu.Lock()
	defer s.mu.Unlock()
	key := strings.TrimSpace(sessionKey)
	st := s.items[key]
	if st == nil {
		return nil
	}
	out := make([]ApprovalRequest, 0, len(st.Approvals))
	for _, v := range st.Approvals {
		out = append(out, v)
	}
	return out
}

func (s *InMemorySessionStore) CleanupExpired(sessionTTL time.Duration) {
	if sessionTTL <= 0 {
		return
	}
	cutoff := time.Now().Add(-sessionTTL)
	s.mu.Lock()
	defer s.mu.Unlock()
	for k, v := range s.items {
		if v == nil {
			delete(s.items, k)
			continue
		}
		if !v.LastActivity.IsZero() && v.LastActivity.Before(cutoff) {
			delete(s.items, k)
		}
	}
}
