package worker

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"sync"
	"sync/atomic"
	"time"

	"unichatgo/internal/models"
	"unichatgo/internal/service/ai"
	"unichatgo/internal/service/assistant"

	"github.com/cloudwego/eino-ext/components/document/loader/file"
	"github.com/cloudwego/eino/components/document"
	"github.com/cloudwego/eino/components/document/parser"
)

type SessionRequest struct {
	Context   context.Context
	UserID    int64
	SessionID int64
	Provider  string
	Model     string
	Token     string
	Files     []*models.TempFile
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
	Stop   JobType = "stop"
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
	ListSessionTempFiles(ctx context.Context, userID, sessionID int64) ([]*models.TempFile, error)
	AddMessage(ctx context.Context, msg models.Message) (*models.Message, error)
	UpdateTempFileSummary(ctx context.Context, fileID int64, summary string, messageID int64) error
}

type Manager struct {
	mu             sync.Mutex
	dispatcher     *Dispatcher
	state          map[int64]*userState
	asst           Assistant
	fileLoader     *file.FileLoader
	enqueueTimeout time.Duration
}

var pendingSeq int64

var ErrDispatcherBusy = errors.New("dispatcher is busy")

// for mock test
var (
	aiFactory = func(provider, model, token string) (AICalling, error) {
		return ai.NewAiService(provider, model, token)
	}
	titleFactory = func(provider, model, token string) (AsCalling, error) {
		return assistant.NewAssistantService(provider, model, token)
	}
)

type DispatcherConfig struct {
	MinWorkers        int
	MaxWorkers        int
	QueueSize         int
	WorkerIdleTimeout time.Duration
	EnqueueTimeout    time.Duration
}

const (
	defaultMinWorkers    = 3
	defaultMaxWorkers    = 10
	defaultQueueSize     = 100
	defaultEnqueueTimout = time.Second
)

func NewManager(asst Assistant, cfg DispatcherConfig) *Manager {
	if cfg.MinWorkers <= 0 {
		cfg.MinWorkers = defaultMinWorkers
	}
	if cfg.MaxWorkers <= 0 {
		cfg.MaxWorkers = defaultMaxWorkers
	}
	if cfg.MaxWorkers < cfg.MinWorkers {
		cfg.MaxWorkers = cfg.MinWorkers
	}
	if cfg.EnqueueTimeout <= 0 {
		cfg.EnqueueTimeout = defaultEnqueueTimout
	}
	if cfg.QueueSize <= 0 {
		cfg.QueueSize = defaultQueueSize
	}
	if cfg.WorkerIdleTimeout <= 0 {
		cfg.WorkerIdleTimeout = defaultWorkerIdle
	}

	extParser, err := parser.NewExtParser(context.Background(), &parser.ExtParserConfig{
		FallbackParser: parser.TextParser{},
	})
	if err != nil {
		panic(err)
	}

	fileLoader, err := file.NewFileLoader(context.Background(), &file.FileLoaderConfig{
		UseNameAsID: true,
		Parser:      extParser,
	})
	if err != nil {
		panic(err)
	}

	m := &Manager{
		state:          make(map[int64]*userState),
		asst:           asst,
		fileLoader:     fileLoader,
		enqueueTimeout: cfg.EnqueueTimeout,
	}
	// cfg.WorkerIdleTimeout check in pool.go
	m.dispatcher = NewDispatcher(cfg.MinWorkers, cfg.MaxWorkers, cfg.QueueSize, m, cfg.WorkerIdleTimeout)
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

func (m *Manager) enqueueJob(job Job) error {
	if m.enqueueTimeout <= 0 {
		select {
		case m.dispatcher.JobQueue <- job:
			return nil
		default:
			return ErrDispatcherBusy
		}
	}
	timer := time.NewTimer(m.enqueueTimeout)
	defer timer.Stop()
	select {
	case m.dispatcher.JobQueue <- job:
		return nil
	case <-timer.C:
		return ErrDispatcherBusy
	}
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

	if err := m.enqueueJob(job); err != nil {
		return nil, err
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
	if err := m.enqueueJob(job); err != nil {
		return nil, "", err
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
	files, err := m.asst.ListSessionTempFiles(ctx, req.UserID, req.SessionID)
	if err != nil {
		if task.resultCh != nil {
			task.resultCh <- workerReturn{err: err}
		}
		return
	}
	state.setFiles(req.SessionID, files)
	req.Files = files
	if len(files) > 0 {
		ctx = ai.WithTempFiles(ctx, files)
	}
	ctx = ai.WithToolSession(ctx, req.UserID, req.SessionID)
	res, err := m.ensureResources(state, req.SessionRequest)
	if err != nil {
		if task.resultCh != nil {
			task.resultCh <- workerReturn{err: err}
		}
		return
	}

	history := state.getHistory(req.SessionID)
	var title string
	if !hasUserMessage(history) {
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

	if len(req.Files) > 0 {
		if res.as == nil {
			if task.resultCh != nil {
				task.resultCh <- workerReturn{err: errors.New("summarizer unavailable")}
			}
			return
		}
		if err := m.attachFileSummaries(ctx, state, req, res, &history); err != nil {
			if task.resultCh != nil {
				task.resultCh <- workerReturn{err: err}
			}
			return
		}
		state.setHistory(req.SessionID, history)
		state.setFiles(req.SessionID, req.Files)
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

func hasUserMessage(history []*models.Message) bool {
	for _, msg := range history {
		if msg != nil && msg.Role == models.RoleUser {
			return true
		}
	}
	return false
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

func (m *Manager) attachFileSummaries(ctx context.Context, state *userState, req StreamRequest, res *sessionResources, history *[]*models.Message) error {
	for _, tempFile := range req.Files {
		if tempFile == nil || tempFile.StoredPath == "" {
			continue
		}
		if tempFile.Summary != "" {
			continue
		}
		summary, err := m.generateFileSummary(ctx, res, tempFile)
		if err != nil {
			return err
		}
		if summary == "" {
			continue
		}
		msg, err := m.asst.AddMessage(ctx, models.Message{
			UserID:    req.UserID,
			SessionID: req.SessionID,
			Role:      models.RoleSystem,
			Content:   fmt.Sprintf("Summary of %s (file_id=%d):\n%s", tempFile.FileName, tempFile.ID, summary),
		})
		if err != nil {
			return err
		}
		if err := m.asst.UpdateTempFileSummary(ctx, tempFile.ID, summary, msg.ID); err != nil {
			return err
		}
		tempFile.Summary = summary
		tempFile.SummaryMessageID = msg.ID
		state.appendHistory(req.SessionID, msg)
		*history = append(*history, msg)
	}
	return nil
}

func (m *Manager) generateFileSummary(ctx context.Context, res *sessionResources, file *models.TempFile) (string, error) {
	docs, err := m.fileLoader.Load(ctx, document.Source{URI: file.StoredPath})
	if err != nil {
		return "", err
	}
	var builder strings.Builder
	builder.WriteString(fmt.Sprintf("File name: %s\n\n", file.FileName))
	for _, doc := range docs {
		content := strings.TrimSpace(doc.Content)
		if content == "" {
			continue
		}
		builder.WriteString(content)
		builder.WriteString("\n\n")
	}
	payload := strings.TrimSpace(builder.String())
	if payload == "" {
		return "", errors.New("file content empty")
	}
	messages := []*models.Message{
		{
			Role:    models.RoleUser,
			Content: payload,
		},
	}
	return res.as.SummarizeFile(ctx, messages)
}
