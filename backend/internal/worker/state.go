package worker

import (
	"sync"

	"unichatgo/internal/models"
)

type workerState struct {
	stopCh  chan struct{}
	initCh  chan sessionTask
	taskCh  chan streamTask
	purgeCh chan int64

	mu        sync.RWMutex
	ready     map[int64]int64
	waiters   map[int64][]chan workerReturn
	sessions  map[int64]*models.Session
	history   map[int64][]*models.Message
	resources map[int64]*sessionResources
}

type workerReturn struct {
	session   *models.Session
	aiMessage *models.Message
	title     string
	err       error
}

type sessionResources struct {
	ai       AICalling
	as       AsCalling
	provider string
	model    string
	token    string
}

func newWorkerState() *workerState {
	return &workerState{
		stopCh:    make(chan struct{}),
		initCh:    make(chan sessionTask, queueLen),
		taskCh:    make(chan streamTask, queueLen),
		purgeCh:   make(chan int64, queueLen),
		ready:     make(map[int64]int64),
		waiters:   make(map[int64][]chan workerReturn),
		sessions:  make(map[int64]*models.Session),
		history:   make(map[int64][]*models.Message),
		resources: make(map[int64]*sessionResources),
	}
}

func (s *workerState) isReady(sessionID int64) bool {
	s.mu.RLock()
	defer s.mu.RUnlock()
	_, ok := s.ready[sessionID]
	return ok
}

func (s *workerState) markReady(sessionID int64) {
	s.mu.Lock()
	s.ready[sessionID] = sessionID
	s.mu.Unlock()
}

func (s *workerState) addWaiter(sessionID int64, ch chan workerReturn) {
	s.mu.Lock()
	s.waiters[sessionID] = append(s.waiters[sessionID], ch)
	s.mu.Unlock()
}

func (s *workerState) drainWaiters(sessionID int64, ret workerReturn) {
	s.mu.Lock()
	waiters := s.waiters[sessionID]
	delete(s.waiters, sessionID)
	s.mu.Unlock()
	for _, ch := range waiters {
		ch <- ret
	}
}

func (s *workerState) promoteSession(pendingID, realID int64) {
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
		if waiters, ok := s.waiters[pendingID]; ok {
			s.waiters[realID] = append(s.waiters[realID], waiters...)
			delete(s.waiters, pendingID)
		}
		delete(s.ready, pendingID)
	}
	s.mu.Unlock()
}

func (s *workerState) setSession(session *models.Session) {
	if session == nil {
		return
	}
	s.mu.Lock()
	s.sessions[session.ID] = session
	s.mu.Unlock()
}

func (s *workerState) getSession(sessionID int64) *models.Session {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.sessions[sessionID]
}

func (s *workerState) setHistory(sessionID int64, history []*models.Message) {
	s.mu.Lock()
	s.history[sessionID] = history
	s.mu.Unlock()
}

func (s *workerState) appendHistory(sessionID int64, msg *models.Message) {
	if msg == nil {
		return
	}
	s.mu.Lock()
	s.history[sessionID] = append(s.history[sessionID], msg)
	s.mu.Unlock()
}

func (s *workerState) getHistory(sessionID int64) []*models.Message {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.history[sessionID]
}

func (s *workerState) purgeCache(sessionID int64) {
	s.mu.Lock()
	delete(s.ready, sessionID)
	delete(s.waiters, sessionID)
	delete(s.sessions, sessionID)
	delete(s.history, sessionID)
	delete(s.resources, sessionID)
	s.mu.Unlock()
}

func (s *workerState) reset() {
	s.mu.Lock()
	s.ready = make(map[int64]int64)
	s.waiters = make(map[int64][]chan workerReturn)
	s.sessions = make(map[int64]*models.Session)
	s.history = make(map[int64][]*models.Message)
	s.resources = make(map[int64]*sessionResources)
	s.mu.Unlock()
}

func (s *workerState) setResources(sessionID int64, res *sessionResources) {
	if res == nil {
		return
	}
	s.mu.Lock()
	s.resources[sessionID] = res
	s.mu.Unlock()
}

func (s *workerState) getResources(sessionID int64) *sessionResources {
	s.mu.RLock()
	defer s.mu.RUnlock()
	return s.resources[sessionID]
}
