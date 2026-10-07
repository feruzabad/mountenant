package app

import (
	"context"
	"sync"
	"time"

	"github.com/feruzabad/mountenant/internal/identity/domain"
)

// memStore is an in-memory UserRepository and SessionRepository.
type memStore struct {
	mu       sync.Mutex
	users    map[domain.UserID]domain.User
	sessions map[domain.SessionID]domain.Session
}

func newMemStore() *memStore {
	return &memStore{users: map[domain.UserID]domain.User{}, sessions: map[domain.SessionID]domain.Session{}}
}

func (m *memStore) ByUsername(_ context.Context, n domain.Username) (domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range m.users {
		if u.Username == n {
			return u, nil
		}
	}
	return domain.User{}, domain.ErrUserNotFound
}

func (m *memStore) ByID(_ context.Context, id domain.UserID) (domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	u, ok := m.users[id]
	if !ok {
		return domain.User{}, domain.ErrUserNotFound
	}
	return u, nil
}

func (m *memStore) All(context.Context) ([]domain.User, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var out []domain.User
	for _, u := range m.users {
		out = append(out, u)
	}
	return out, nil
}

func (m *memStore) ApplySync(_ context.Context, p domain.SyncPlan) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	for _, u := range append(p.Create, p.Update...) {
		m.users[u.ID] = u
	}
	for _, id := range p.RevokeSessions {
		for k, s := range m.sessions {
			if s.UserID == id {
				delete(m.sessions, k)
			}
		}
	}
	return nil
}

func (m *memStore) Create(_ context.Context, s domain.Session) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.sessions[s.ID] = s
	return nil
}

func (m *memStore) Get(_ context.Context, id domain.SessionID) (domain.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	s, ok := m.sessions[id]
	if !ok {
		return domain.Session{}, domain.ErrSessionNotFound
	}
	return s, nil
}

func (m *memStore) Touch(_ context.Context, id domain.SessionID, at time.Time) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if s, ok := m.sessions[id]; ok {
		s.LastSeenAt = at
		m.sessions[id] = s
	}
	return nil
}

func (m *memStore) Delete(_ context.Context, id domain.SessionID) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	delete(m.sessions, id)
	return nil
}

func (m *memStore) DeleteInactive(_ context.Context, now, idleCutoff time.Time) (int64, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	var n int64
	for k, s := range m.sessions {
		if !now.Before(s.ExpiresAt) || !idleCutoff.Before(s.LastSeenAt) {
			delete(m.sessions, k)
			n++
		}
	}
	return n, nil
}

func (m *memStore) sessionCount() int {
	m.mu.Lock()
	defer m.mu.Unlock()
	return len(m.sessions)
}
