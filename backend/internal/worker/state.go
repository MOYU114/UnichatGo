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
	files     map[int64][]*models.TempFile
}

type AsCalling interface {
	GenerateTitle(ctx context.Context, messages []*models.Message) (string, error)
	SummarizeFile(ctx context.Context, content []*models.Message) (string, error)
}

type AICalling interface {
	StreamChat(ctx context.Context, message *models.Message, prevHistory []*models.Message, imageFiles []*models.TempFile, callback func(string) error) (*models.Message, error)
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
		files:     make(map[int64][]*models.TempFile),
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
		if files, ok := s.files[pendingID]; ok {
			delete(s.files, pendingID)
			s.files[realID] = files
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
	delete(s.files, sessionID)
	s.mu.Unlock()
}

func (s *userState) reset() {
	s.mu.Lock()
	s.ready = make(map[int64]int64)
	s.sessions = make(map[int64]*models.Session)
	s.history = make(map[int64][]*models.Message)
	s.resources = make(map[int64]*sessionResources)
	s.files = make(map[int64][]*models.TempFile)
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

func (s *userState) setFiles(sessionID int64, files []*models.TempFile) {
	s.mu.Lock()
	if files == nil {
		delete(s.files, sessionID)
	} else {
		s.files[sessionID] = files
	}
	s.mu.Unlock()
}

func (s *userState) getFiles(sessionID int64) []*models.TempFile {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.files[sessionID]
}

func (s *userState) clearFiles(sessionID int64) {
	s.mu.Lock()
	delete(s.files, sessionID)
	s.mu.Unlock()
}
