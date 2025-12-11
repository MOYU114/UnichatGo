package worker

import (
	"context"
	"errors"
	"sync"
	"sync/atomic"

	"unichatgo/internal/models"
	"unichatgo/internal/service/ai"
	"unichatgo/internal/service/assistant"
)

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

type sessionTask struct {
	req      SessionRequest
	resultCh chan workerReturn
}

type streamTask struct {
	req      StreamRequest
	resultCh chan workerReturn
}

type JobType string

const (
	Init   JobType = "init"
	Stream JobType = "stream"
)

type Job struct {
	Type        JobType
	SessionTask sessionTask
	StreamTask  streamTask
}

type Assistant interface {
	CreateSession(ctx context.Context, userID int64, title string) (*models.Session, error)
	GetSessionWithMessages(ctx context.Context, userID, sessionID int64) (*models.Session, []*models.Message, error)
	UpdateSessionTitle(ctx context.Context, userID, sessionID int64, title string) error
}

type Manager struct {
	mu         sync.Mutex
	dispatcher *Dispatcher
	state      map[int64]*userState
	asst       Assistant
}

var pendingSeq int64

var (
	aiFactory = func(provider, model, token string) (AICalling, error) {
		return ai.NewAiService(provider, model, token)
	}
	titleFactory = func(provider, model, token string) (AsCalling, error) {
		return assistant.NewAssistantService(provider, model, token)
	}
)

type DispatcherConfig struct {
	MaxWorkers int
	QueueSize  int
}

const (
	defaultWorkerCount = 10
	defaultQueueSize   = 100
)

func NewManager(asst Assistant, cfg DispatcherConfig) *Manager {
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = defaultWorkerCount
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	m := &Manager{
		state: make(map[int64]*userState),
		asst:  asst,
	}
	m.dispatcher = NewDispatcher(cfg.MaxWorkers, cfg.QueueSize, m)
	return m
}

func (m *Manager) getState(userID int64) *userState {
	m.mu.Lock()
	defer m.mu.Unlock()

	if state, ok := m.state[userID]; ok {
		return state
	}

	state := newUserState()
	m.state[userID] = state
	return state
}

func (m *Manager) InitSession(req SessionRequest) (*models.Session, error) {
	if req.SessionID == 0 { // use negative integer to create temp session id
		req.SessionID = -atomic.AddInt64(&pendingSeq, 1)
	}

	state := m.getState(req.UserID)
	if req.SessionID != 0 && state.isReady(req.SessionID) {
		if se := state.getSession(req.SessionID); se != nil {
			return se, nil
		}
	}

	waitCh := make(chan workerReturn, 1)
	job := Job{
		Type: Init,
		SessionTask: sessionTask{
			req:      req,
			resultCh: waitCh,
		},
	}

	select {
	case m.dispatcher.JobQueue <- job:
	default:
		return nil, errors.New("job queue full")
	}

	ret := <-waitCh
	return ret.session, ret.err
}

func (m *Manager) Stream(req StreamRequest) (*models.Message, string, error) {
	state := m.getState(req.UserID)
	if !state.isReady(req.SessionID) {
		if _, err := m.InitSession(req.SessionRequest); err != nil {
			return nil, "", err
		}
	}

	resultCh := make(chan workerReturn, 1)
	job := Job{
		Type: Stream,
		StreamTask: streamTask{
			req:      req,
			resultCh: resultCh,
		},
	}
	select {
	case m.dispatcher.JobQueue <- job:
	default:
		return nil, "", errors.New("task queue full")
	}

	ret := <-resultCh
	return ret.aiMessage, ret.title, ret.err
}

// Purge Clean one session of userX
func (m *Manager) Purge(userID, sessionID int64) {
	state := m.getState(userID)
	if state == nil {
		return
	}
	state.purgeCache(sessionID)
}

// ResetUser Reset all cache of userX
func (m *Manager) ResetUser(userID int64) {
	m.mu.Lock()
	if state, ok := m.state[userID]; ok {
		delete(m.state, userID)
		state.reset()
	}
	m.mu.Unlock()
	m.dispatcher.CancelUser(userID)
}

func (m *Manager) handleInit(task sessionTask) {
	req := task.req
	state := m.getState(req.UserID)
	pendingID := req.SessionID
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}

	var (
		session *models.Session
		history []*models.Message
		err     error
	)

	if req.SessionID <= 0 {
		title := "New Conversation"
		session, err = m.asst.CreateSession(ctx, req.UserID, title)
		if err != nil {
			if task.resultCh != nil {
				task.resultCh <- workerReturn{err: err}
			}
			return
		}
		history = make([]*models.Message, 0)
		req.SessionID = session.ID
	} else {
		session, history, err = m.asst.GetSessionWithMessages(ctx, req.UserID, req.SessionID)
		if err != nil {
			if task.resultCh != nil {
				task.resultCh <- workerReturn{err: err}
			}
			return
		}
	}

	if _, err := m.ensureResources(state, req); err != nil {
		if task.resultCh != nil {
			task.resultCh <- workerReturn{err: err}
		}
		return
	}

	state.setSession(session)
	state.setHistory(session.ID, history)
	state.promoteSession(pendingID, session.ID)
	state.markReady(session.ID)

	if task.resultCh != nil {
		task.resultCh <- workerReturn{session: session}
	}
}

func (m *Manager) handleStream(task streamTask) {
	req := task.req
	state := m.getState(req.UserID)

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
			if task.resultCh != nil {
				task.resultCh <- workerReturn{err: err}
			}
			return
		}
		if title != "" {
			if err := m.asst.UpdateSessionTitle(ctx, req.UserID, req.SessionID, title); err != nil {
				if task.resultCh != nil {
					task.resultCh <- workerReturn{err: err}
				}
				return
			}
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
		cb = func(chunk string) error { return req.ChunkFn(chunk) }
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

func (m *Manager) ensureResources(state *userState, req SessionRequest) (*sessionResources, error) {
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
	aiSvc, err := aiFactory(req.Provider, req.Model, req.Token)
	if err != nil {
		return nil, err
	}
	asSvc, err := titleFactory(req.Provider, req.Model, req.Token)
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
