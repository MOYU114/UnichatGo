package worker

import (
	"context"
	"errors"
	"log"
	"sync"
	"sync/atomic"

	"unichatgo/internal/models"
	"unichatgo/internal/service/ai"
	"unichatgo/internal/service/assistant"
)

const queueLen = 16

type SessionRequest struct {
	Context   context.Context
	UserID    int64
	SessionID int64
	Provider  string
	Model     string
	Token     string
	Message   *models.Message
}

type StreamRequest struct {
	SessionRequest
	ChunkFn func(string) error
}

type AsCalling interface {
	GenerateTitle(ctx context.Context, messages []*models.Message) (string, error)
}

type AICalling interface {
	StreamChat(ctx context.Context, message *models.Message, prevHistory []*models.Message, callback func(string) error) (*models.Message, error)
}

type Manager struct {
	assistant *assistant.Service
	mu        sync.Mutex

	workers map[int64]*workerState
}

type sessionTask struct {
	req SessionRequest
}

type streamTask struct {
	req      StreamRequest
	resultCh chan workerReturn
}

var pendingSeq int64

func NewManager(asst *assistant.Service) *Manager {
	return &Manager{
		assistant: asst,
		workers:   make(map[int64]*workerState),
	}
}

func (m *Manager) EnsureSession(req SessionRequest) (*models.Session, error) {
	state := m.ensureWorker(req.UserID)

	if req.SessionID == 0 { // use negative integer to create temp session id
		req.SessionID = -atomic.AddInt64(&pendingSeq, 1)
	}

	if req.SessionID != 0 && state.isReady(req.SessionID) {
		if se := state.getSession(req.SessionID); se != nil {
			return se, nil
		}
	}

	waitCh := make(chan workerReturn, 1)
	state.addWaiter(req.SessionID, waitCh)

	select {
	case state.initCh <- sessionTask{req: req}:
	default:
		state.drainWaiters(req.SessionID, workerReturn{err: errors.New("init queue full")})
		return nil, errors.New("init queue full")
	}

	ret := <-waitCh
	return ret.session, ret.err
}

func (m *Manager) Stream(req StreamRequest) (*models.Message, string, error) {
	state := m.ensureWorker(req.UserID)
	if !state.isReady(req.SessionID) {
		if _, err := m.EnsureSession(req.SessionRequest); err != nil {
			return nil, "", err
		}
	}

	resultCh := make(chan workerReturn, 1)
	select {
	case state.taskCh <- streamTask{req: req, resultCh: resultCh}:
	default:
		return nil, "", errors.New("task queue full")
	}

	ret := <-resultCh
	return ret.aiMessage, ret.title, ret.err
}

func (m *Manager) Purge(userID, sessionID int64) {
	state := m.getWorker(userID)
	if state == nil {
		return
	}
	state.purgeCache(sessionID)
	select {
	case state.purgeCh <- sessionID:
	default:
	}
}

func (m *Manager) Stop(userID int64) {
	m.mu.Lock()
	if state, ok := m.workers[userID]; ok {
		state.reset()
		close(state.stopCh)
	}
	m.mu.Unlock()
}

func (m *Manager) ensureWorker(userID int64) *workerState {
	m.mu.Lock()
	defer m.mu.Unlock()

	if state, ok := m.workers[userID]; ok {
		return state
	}

	state := newWorkerState()
	m.workers[userID] = state
	go m.runWorker(userID, state)
	return state
}

func (m *Manager) getWorker(userID int64) *workerState {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.workers[userID]
}

func (m *Manager) runWorker(userID int64, state *workerState) {
	defer func() {
		m.mu.Lock()
		delete(m.workers, userID)
		m.mu.Unlock()
	}()

	for {
		select {
		case <-state.stopCh:
			log.Printf("ai worker for user %d stopped", userID)
			return
		case task := <-state.initCh:
			m.handleInit(task, state)
		case task := <-state.taskCh:
			m.handleStream(task, state)
		case sessionID := <-state.purgeCh:
			state.purgeCache(sessionID)
		}
	}
}

func (m *Manager) handleInit(task sessionTask, state *workerState) {
	req := task.req
	pendingID := req.SessionID
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}

	var se *models.Session
	var err error
	var history []*models.Message

	if req.SessionID <= 0 {
		title := "New Conversation"
		se, err = m.assistant.CreateSession(ctx, req.UserID, title)
		if err != nil {
			state.drainWaiters(pendingID, workerReturn{err: err})
			return
		}
		history = make([]*models.Message, 0)
		req.SessionID = se.ID
	} else {
		se, history, err = m.assistant.GetSessionWithMessages(ctx, req.UserID, req.SessionID)
		if err != nil {
			state.drainWaiters(req.SessionID, workerReturn{err: err})
			return
		}
	}

	if _, err := m.ensureResources(state, req); err != nil {
		state.drainWaiters(req.SessionID, workerReturn{err: err})
		if pendingID != req.SessionID {
			state.drainWaiters(pendingID, workerReturn{err: err})
		}
		return
	}

	state.setSession(se)
	state.setHistory(se.ID, history)
	state.promoteSession(pendingID, se.ID) // change pending id into real one
	state.markReady(se.ID)
	state.drainWaiters(se.ID, workerReturn{session: se})
}

func (m *Manager) handleStream(task streamTask, state *workerState) {
	req := task.req
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	res, err := m.ensureResources(state, req.SessionRequest)
	if err != nil {
		if task.resultCh != nil {
			task.resultCh <- workerReturn{err: err}
		}
		return
	}
	history := state.getHistory(req.SessionID)
	var title string
	if len(history) == 0 {
		//generate title
		var titleMsgs []*models.Message
		if req.Message != nil {
			titleMsgs = []*models.Message{req.Message}
		}
		if res.as == nil {
			if task.resultCh != nil {
				task.resultCh <- workerReturn{err: errors.New("title generator unavailable")}
			}
			return
		}
		title, err = res.as.GenerateTitle(ctx, titleMsgs)
		if err != nil {
			task.resultCh <- workerReturn{err: err}
			return
		}
		if title != "" {
			if err := m.assistant.UpdateSessionTitle(ctx, req.UserID, req.SessionID, title); err != nil {
				task.resultCh <- workerReturn{err: err}
				return
			}
			// update cache
			if session := state.getSession(req.SessionID); session != nil {
				session.Title = title
				state.setSession(session)
			}
		}
	}
	if req.Message != nil {
		history = append(history, req.Message)
		state.setHistory(req.SessionID, history)
	}

	var cb func(string) error
	if req.ChunkFn != nil {
		cb = func(chunk string) error {
			return req.ChunkFn(chunk)
		}
	}
	if res.ai == nil {
		if task.resultCh != nil {
			task.resultCh <- workerReturn{err: errors.New("ai service unavailable")}
		}
		return
	}
	aiMsg, err := res.ai.StreamChat(ctx, req.Message, history, cb)
	if err != nil {
		if task.resultCh != nil {
			task.resultCh <- workerReturn{err: err}
		}
		return
	}

	state.appendHistory(req.SessionID, aiMsg)
	if task.resultCh != nil {
		task.resultCh <- workerReturn{aiMessage: aiMsg, title: title}
	}
}

// ensureResources Update as ai when model or provider changed.
func (m *Manager) ensureResources(state *workerState, req SessionRequest) (*sessionResources, error) {
	if state == nil {
		return nil, errors.New("worker state missing")
	}
	if req.SessionID <= 0 {
		return nil, errors.New("session id required")
	}
	res := state.getResources(req.SessionID)
	if res != nil && res.provider == req.Provider && res.model == req.Model && res.token == req.Token {
		return res, nil
	}
	aiSvc, err := ai.NewAiService(req.Provider, req.Model, req.Token)
	if err != nil {
		return nil, err
	}
	asSvc, err := assistant.NewAssistantService(req.Provider, req.Model, req.Token)
	if err != nil {
		return nil, err
	}
	res = &sessionResources{
		ai:       aiSvc,
		as:       asSvc,
		provider: req.Provider,
		model:    req.Model,
		token:    req.Token,
	}
	state.setResources(req.SessionID, res)
	return res, nil
}
