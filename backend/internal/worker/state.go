package worker

import (
	"context"
	"sync"
	"unichatgo/internal/models"
)

type userState struct {
	mu        sync.RWMutex
	ready     map[int64]int64
	sessions  map[int64]*models.Session
	history   map[int64][]*models.Message
	resources map[int64]*sessionResources
}

type AsCalling interface {
	GenerateTitle(ctx context.Context, messages []*models.Message) (string, error)
}

type AICalling interface {
	StreamChat(ctx context.Context, message *models.Message, prevHistory []*models.Message, callback func(string) error) (*models.Message, error)
}
type sessionResources struct {
	ai       AICalling
	as       AsCalling
	provider string
	model    string
	token    string
}

func newUserState() *userState {
	return &userState{
		ready:     make(map[int64]int64),
		sessions:  make(map[int64]*models.Session),
		history:   make(map[int64][]*models.Message),
		resources: make(map[int64]*sessionResources),
	}
}

func (s *userState) isReady(sessionID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.ready[sessionID]
	return ok
}

func (s *userState) markReady(sessionID int64) {
	s.mu.Lock()
	s.ready[sessionID] = sessionID
	s.mu.Unlock()
}

func (s *userState) promoteSession(pendingID, realID int64) {
	s.mu.Lock()
	if pendingID != realID {
		if se, ok := s.sessions[pendingID]; ok {
			delete(s.sessions, pendingID)
			s.sessions[realID] = se
		}
		if history, ok := s.history[pendingID]; ok {
			delete(s.history, pendingID)
			s.history[realID] = history
		}
		delete(s.ready, pendingID)
	}
	s.mu.Unlock()
}

func (s *userState) setSession(session *models.Session) {
	if session == nil {
		return
	}
	s.mu.Lock()
	s.sessions[session.ID] = session
	s.mu.Unlock()
}

func (s *userState) getSession(sessionID int64) *models.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[sessionID]
}

func (s *userState) setHistory(sessionID int64, history []*models.Message) {
	s.mu.Lock()
	s.history[sessionID] = history
	s.mu.Unlock()
}

func (s *userState) appendHistory(sessionID int64, msg *models.Message) {
	if msg == nil {
		return
	}
	s.mu.Lock()
	s.history[sessionID] = append(s.history[sessionID], msg)
	s.mu.Unlock()
}

func (s *userState) getHistory(sessionID int64) []*models.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.history[sessionID]
}

func (s *userState) purgeCache(sessionID int64) {
	s.mu.Lock()
	delete(s.ready, sessionID)
	delete(s.sessions, sessionID)
	delete(s.history, sessionID)
	delete(s.resources, sessionID)
	s.mu.Unlock()
}

func (s *userState) reset() {
	s.mu.Lock()
	s.ready = make(map[int64]int64)
	s.sessions = make(map[int64]*models.Session)
	s.history = make(map[int64][]*models.Message)
	s.resources = make(map[int64]*sessionResources)
	s.mu.Unlock()
}

func (s *userState) setResources(sessionID int64, res *sessionResources) {
	if res == nil {
		return
	}
	s.mu.Lock()
	s.resources[sessionID] = res
	s.mu.Unlock()
}

func (s *userState) getResources(sessionID int64) *sessionResources {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resources[sessionID]
}
