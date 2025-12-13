package worker

import (
	"context"
	"sync"
	"testing"
	"time"

	"unichatgo/internal/models"
)

func TestWorkerStateCacheOperations(t *testing.T) {
	state := newUserState()

	session := &models.Session{ID: 1, Title: "pending"}
	state.setSession(session)
	if got := state.getSession(1); got == nil || got.Title != "pending" {
		t.Fatalf("getSession mismatch: %#v", got)
	}

	state.setHistory(1, []*models.Message{{ID: 10}})
	state.appendHistory(1, &models.Message{ID: 11})
	if hist := state.getHistory(1); len(hist) != 2 || hist[1].ID != 11 {
		t.Fatalf("history not updated: %#v", hist)
	}

	state.setResources(1, &sessionResources{provider: "p", model: "m"})
	if res := state.getResources(1); res == nil || res.provider != "p" {
		t.Fatalf("resources not stored: %#v", res)
	}

	state.markReady(1)
	if !state.isReady(1) {
		t.Fatalf("session should be ready")
	}

	pendingID := int64(-2)
	state.sessions[pendingID] = &models.Session{ID: pendingID, Title: "pending"}
	state.history[pendingID] = []*models.Message{{ID: 20}}
	state.promoteSession(pendingID, 2)
	if state.getSession(2) == nil {
		t.Fatalf("session not promoted")
	}

	state.purgeCache(2)
	if state.getSession(2) != nil || state.getResources(2) != nil {
		t.Fatalf("purgeCache failed to clear entries")
	}

	state.reset()
	if len(state.sessions) != 0 || len(state.history) != 0 {
		t.Fatalf("reset did not clear caches")
	}
}

func TestManagerPurgeAndStop(t *testing.T) {
	manager := NewManager(newMockAssistant(), DispatcherConfig{MinWorkers: 2, MaxWorkers: 2, QueueSize: 10})
	state := manager.getState(42)

	state.setSession(&models.Session{ID: 99, Title: "cached"})
	state.setHistory(99, []*models.Message{{ID: 1}})
	state.setResources(99, &sessionResources{provider: "p", model: "m", token: "t"})
	state.markReady(99)

	manager.Purge(42, 99)
	if state.getSession(99) != nil || state.getResources(99) != nil || state.isReady(99) {
		t.Fatalf("purge did not clear cached session")
	}

	manager.ResetUser(42)
	manager.mu.Lock()
	if _, ok := manager.state[42]; ok {
		t.Fatalf("user state not removed after reset")
	}
	manager.mu.Unlock()

	// Ensure calling Purge after ResetUser is a no-op.
	manager.Purge(42, 99)
}

func TestDispatcherInitAndStream(t *testing.T) {
	mockAsst := newMockAssistant()
	manager := NewManager(mockAsst, DispatcherConfig{MinWorkers: 2, MaxWorkers: 2, QueueSize: 10})

	origAI := aiFactory
	origTitle := titleFactory
	defer func() {
		aiFactory = origAI
		titleFactory = origTitle
	}()
	aiFactory = func(provider, model, token string) (AICalling, error) {
		return &fakeAI{}, nil
	}
	titleFactory = func(provider, model, token string) (AsCalling, error) {
		return &fakeAS{}, nil
	}

	session, err := manager.InitSession(SessionRequest{
		UserID:   1,
		Provider: "mock",
		Model:    "m1",
		Token:    "tok",
	})
	if err != nil {
		t.Fatalf("InitSession error: %v", err)
	}
	if session == nil || session.ID == 0 {
		t.Fatalf("expected session to be created")
	}

	msg, title, err := manager.Stream(StreamRequest{
		SessionRequest: SessionRequest{
			Context:   context.Background(),
			UserID:    1,
			SessionID: session.ID,
			Provider:  "mock",
			Model:     "m1",
			Token:     "tok",
			Message:   &models.Message{Content: "hello"},
		},
	})
	if err != nil {
		t.Fatalf("Stream error: %v", err)
	}
	if msg == nil || msg.Content != "ai: hello" {
		t.Fatalf("unexpected stream response: %#v", msg)
	}
	if title != "fake-title" {
		t.Fatalf("unexpected title: %s", title)
	}
}

func TestDispatcherJobOrder(t *testing.T) {
	mockAsst := newMockAssistant()
	manager := NewManager(mockAsst, DispatcherConfig{MinWorkers: 2, MaxWorkers: 2, QueueSize: 10})

	origAI := aiFactory
	origTitle := titleFactory
	defer func() {
		aiFactory = origAI
		titleFactory = origTitle
	}()

	var mu sync.Mutex
	order := make([]string, 0, 2)
	aiFactory = func(provider, model, token string) (AICalling, error) {
		return &labeledAI{onRun: func(label string) {
			mu.Lock()
			order = append(order, label)
			mu.Unlock()
		}}, nil
	}
	titleFactory = func(provider, model, token string) (AsCalling, error) {
		return &fakeAS{}, nil
	}

	session, err := manager.InitSession(SessionRequest{UserID: 11, Provider: "mock", Model: "m", Token: "tok"})
	if err != nil {
		t.Fatalf("InitSession error: %v", err)
	}

	if _, _, err := manager.Stream(StreamRequest{
		SessionRequest: SessionRequest{
			Context:   context.Background(),
			UserID:    11,
			SessionID: session.ID,
			Provider:  "mock",
			Model:     "m",
			Token:     "tok",
			Message:   &models.Message{Content: "first"},
		},
	}); err != nil {
		t.Fatalf("Stream (first) error: %v", err)
	}
	if _, _, err := manager.Stream(StreamRequest{
		SessionRequest: SessionRequest{
			Context:   context.Background(),
			UserID:    11,
			SessionID: session.ID,
			Provider:  "mock",
			Model:     "m",
			Token:     "tok",
			Message:   &models.Message{Content: "second"},
		},
	}); err != nil {
		t.Fatalf("Stream (second) error: %v", err)
	}

	mu.Lock()
	defer mu.Unlock()
	if len(order) != 2 || order[0] != "first" || order[1] != "second" {
		t.Fatalf("expected execution order [first second], got %v", order)
	}
}

func TestDispatcherQueuesWhenWorkerBusy(t *testing.T) {
	mockAsst := newMockAssistant()
	manager := NewManager(mockAsst, DispatcherConfig{MinWorkers: 2, MaxWorkers: 2, QueueSize: 10})

	origAI := aiFactory
	origTitle := titleFactory
	defer func() {
		aiFactory = origAI
		titleFactory = origTitle
	}()

	block := make(chan struct{})
	started := make(chan struct{})
	aiFactory = func(provider, model, token string) (AICalling, error) {
		return &fakeBlockingAI{block: block, started: started}, nil
	}
	titleFactory = func(provider, model, token string) (AsCalling, error) {
		return &fakeAS{}, nil
	}

	session, err := manager.InitSession(SessionRequest{UserID: 21, Provider: "mock", Model: "m", Token: "tok"})
	if err != nil {
		t.Fatalf("InitSession error: %v", err)
	}

	done1 := make(chan struct{})
	done2 := make(chan struct{})

	go func() {
		_, _, _ = manager.Stream(StreamRequest{
			SessionRequest: SessionRequest{
				Context:   context.Background(),
				UserID:    21,
				SessionID: session.ID,
				Provider:  "mock",
				Model:     "m",
				Token:     "tok",
				Message:   &models.Message{Content: "first"},
			},
		})
		close(done1)
	}()

	go func() {
		_, _, _ = manager.Stream(StreamRequest{
			SessionRequest: SessionRequest{
				Context:   context.Background(),
				UserID:    21,
				SessionID: session.ID,
				Provider:  "mock",
				Model:     "m",
				Token:     "tok",
				Message:   &models.Message{Content: "second"},
			},
		})
		close(done2)
	}()

	select {
	case <-started:
		close(block)
	case <-time.After(time.Second):
		t.Fatalf("first job did not start")
	}
	select {
	case <-done1:
	case <-time.After(time.Second):
		t.Fatalf("first job did not complete after unblocking")
	}

	select {
	case <-done2:
	case <-time.After(time.Second):
		t.Fatalf("second job did not complete after first")
	}
}

func TestManagerHighLoadAllowsOtherUsers(t *testing.T) {
	mockAsst := newMockAssistant()
	manager := NewManager(mockAsst, DispatcherConfig{MinWorkers: 1, MaxWorkers: 3, QueueSize: 10})

	block := make(chan struct{})
	started := make(chan struct{})

	origAI := aiFactory
	origTitle := titleFactory
	defer func() {
		aiFactory = origAI
		titleFactory = origTitle
	}()
	aiFactory = func(provider, model, token string) (AICalling, error) {
		if provider == "slow" {
			return &fakeBlockingAI{block: block, started: started}, nil
		}
		return &fakeAI{}, nil
	}
	titleFactory = func(provider, model, token string) (AsCalling, error) {
		return &fakeAS{}, nil
	}

	slowSession, err := manager.InitSession(SessionRequest{
		UserID:   1,
		Provider: "slow",
		Model:    "m",
		Token:    "tok",
	})
	if err != nil {
		t.Fatalf("slow session init: %v", err)
	}
	fastSession, err := manager.InitSession(SessionRequest{
		UserID:   2,
		Provider: "fast",
		Model:    "m",
		Token:    "tok",
	})
	if err != nil {
		t.Fatalf("fast session init: %v", err)
	}

	slowDone := make(chan error, 1)
	go func() {
		_, _, err := manager.Stream(StreamRequest{
			SessionRequest: SessionRequest{
				UserID:    1,
				SessionID: slowSession.ID,
				Provider:  "slow",
				Model:     "m",
				Token:     "tok",
				Message: &models.Message{
					UserID:    1,
					SessionID: slowSession.ID,
					Role:      models.RoleUser,
					Content:   "slow",
				},
			},
		})
		slowDone <- err
	}()

	select {
	case <-started:
	case <-time.After(time.Second):
		t.Fatalf("slow task did not start")
	}

	fastErr := make(chan error, 1)
	go func() {
		_, _, err := manager.Stream(StreamRequest{
			SessionRequest: SessionRequest{
				UserID:    2,
				SessionID: fastSession.ID,
				Provider:  "fast",
				Model:     "m",
				Token:     "tok",
				Message: &models.Message{
					UserID:    2,
					SessionID: fastSession.ID,
					Role:      models.RoleUser,
					Content:   "fast",
				},
			},
		})
		fastErr <- err
	}()

	select {
	case err := <-fastErr:
		if err != nil {
			t.Fatalf("fast stream error: %v", err)
		}
	case <-time.After(2 * time.Second):
		t.Fatalf("fast stream blocked but should complete")
	}

	close(block)
	if err := <-slowDone; err != nil {
		t.Fatalf("slow stream error: %v", err)
	}

	var wg sync.WaitGroup
	for i := 3; i <= 15; i++ {
		wg.Add(1)
		go func(uid int) {
			defer wg.Done()
			session, err := manager.InitSession(SessionRequest{
				UserID:   int64(uid),
				Provider: "fast",
				Model:    "m",
				Token:    "tok",
			})
			if err != nil {
				t.Errorf("init session user %d: %v", uid, err)
				return
			}
			if _, _, err := manager.Stream(StreamRequest{
				SessionRequest: SessionRequest{
					UserID:    int64(uid),
					SessionID: session.ID,
					Provider:  "fast",
					Model:     "m",
					Token:     "tok",
					Message: &models.Message{
						UserID:    int64(uid),
						SessionID: session.ID,
						Role:      models.RoleUser,
						Content:   "multi",
					},
				},
			}); err != nil {
				t.Errorf("stream user %d: %v", uid, err)
			}
		}(i)
	}
	wg.Wait()
}

// --- helpers ---

type mockAssistant struct {
	mu       sync.Mutex
	nextID   int64
	sessions map[int64]*models.Session
}

func newMockAssistant() *mockAssistant {
	return &mockAssistant{
		sessions: make(map[int64]*models.Session),
	}
}

func (m *mockAssistant) CreateSession(ctx context.Context, userID int64, title string) (*models.Session, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	m.nextID++
	session := &models.Session{ID: m.nextID, Title: title, UserID: userID}
	m.sessions[session.ID] = session
	return session, nil
}

func (m *mockAssistant) GetSessionWithMessages(ctx context.Context, userID, sessionID int64) (*models.Session, []*models.Message, error) {
	m.mu.Lock()
	defer m.mu.Unlock()
	return m.sessions[sessionID], nil, nil
}

func (m *mockAssistant) UpdateSessionTitle(ctx context.Context, userID, sessionID int64, title string) error {
	m.mu.Lock()
	defer m.mu.Unlock()
	if session, ok := m.sessions[sessionID]; ok {
		session.Title = title
	}
	return nil
}

type fakeAI struct{}

func (f *fakeAI) StreamChat(ctx context.Context, message *models.Message, prevHistory []*models.Message, callback func(string) error) (*models.Message, error) {
	if callback != nil {
		_ = callback("chunk")
	}
	return &models.Message{Content: "ai: " + message.Content}, nil
}

type fakeAS struct{}

func (f *fakeAS) GenerateTitle(ctx context.Context, messages []*models.Message) (string, error) {
	return "fake-title", nil
}

type fakeBlockingAI struct {
	block   chan struct{}
	started chan struct{}
	once    sync.Once
}

func (f *fakeBlockingAI) StreamChat(ctx context.Context, message *models.Message, prevHistory []*models.Message, callback func(string) error) (*models.Message, error) {
	f.once.Do(func() {
		if f.started != nil {
			close(f.started)
		}
	})
	<-f.block
	return &models.Message{Content: "ai: " + message.Content}, nil
}

type labeledAI struct {
	onRun func(label string)
}

func (f *labeledAI) StreamChat(ctx context.Context, message *models.Message, prevHistory []*models.Message, callback func(string) error) (*models.Message, error) {
	if f.onRun != nil {
		f.onRun(message.Content)
	}
	return &models.Message{Content: "ai: " + message.Content}, nil
}
