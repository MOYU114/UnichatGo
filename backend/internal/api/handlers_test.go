package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"mime/multipart"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strconv"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"unichatgo/internal/auth"
	"unichatgo/internal/config"
	"unichatgo/internal/models"
	"unichatgo/internal/service/assistant"
	"unichatgo/internal/storage"
	"unichatgo/internal/worker"
)

func TestHandlersEndToEndFlow(t *testing.T) {
	router, db, handler := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)

	username := fmt.Sprintf("tester_%d", time.Now().UnixNano())
	password := "pass123"
	provider := "openai"

	// Register a user.
	regResp := client.DoJSON(http.MethodPost, "/api/users/register", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	assertStatus(t, regResp, http.StatusCreated)
	var regBody struct {
		ID int64 `json:"id"`
	}
	decodeJSON(t, regResp.Body.Bytes(), &regBody)
	if regBody.ID == 0 {
		t.Fatalf("expected user id in register response")
	}

	// Login to fetch auth token.
	loginResp := client.DoJSON(http.MethodPost, "/api/users/login", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	assertStatus(t, loginResp, http.StatusOK)
	var loginBody struct {
		AuthToken string `json:"auth_token"`
	}
	decodeJSON(t, loginResp.Body.Bytes(), &loginBody)
	if loginBody.AuthToken == "" {
		t.Fatalf("expected auth token from login")
	}

	// Store provider token.
	tokenResp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/token", regBody.ID),
		map[string]string{"provider": provider, "token": "mock"},
		nil)
	assertStatus(t, tokenResp, http.StatusNoContent)

	// Start a new conversation (session_id == 0).
	startResp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", regBody.ID),
		map[string]any{"provider": provider, "session_id": 0, "model_type": "gpt-5-nano"},
		nil)
	assertStatus(t, startResp, http.StatusAccepted)
	var startBody struct {
		SessionID int64 `json:"sessionId"`
	}
	decodeJSON(t, startResp.Body.Bytes(), &startBody)
	if startBody.SessionID <= 0 {
		t.Fatalf("expected positive session id")
	}

	firstMessage := "Hello, remember my name is Bob."
	sendResp := client.PostSSE(
		fmt.Sprintf("/api/users/%d/conversation/msg", regBody.ID),
		map[string]any{
			"session_id": startBody.SessionID,
			"content":    firstMessage,
			"provider":   provider,
			"model_type": "gpt-5-nano",
			"client_msg_id": "client-msg-1",
		},
		nil,
	)
	assertStatus(t, sendResp, http.StatusOK)
	events := parseSSE(t, sendResp.Body.String())
	if len(events) != 3 {
		t.Fatalf("expected 3 SSE events, got %d", len(events))
	}
	if events[0].Name != "ack" {
		t.Fatalf("expected first SSE event to be ack, got %s", events[0].Name)
	}
	var ackPayload struct {
		Message struct {
			Content string `json:"content"`
		} `json:"message"`
	}
	decodeJSON(t, []byte(events[0].Data), &ackPayload)
	if ackPayload.Message.Content != firstMessage {
		t.Fatalf("ack payload mismatch, want %q got %q", firstMessage, ackPayload.Message.Content)
	}
	if events[1].Name != "stream" {
		t.Fatalf("expected stream event, got %s", events[1].Name)
	}
	if events[2].Name != "done" {
		t.Fatalf("expected done event, got %s", events[2].Name)
	}
	var donePayload struct {
		Title string `json:"title"`
		AI    struct {
			Content string `json:"content"`
		} `json:"ai_message"`
	}
	decodeJSON(t, []byte(events[2].Data), &donePayload)
	if donePayload.Title == "" || donePayload.AI.Content == "" {
		t.Fatalf("done payload missing title or ai content")
	}

	msgCount := countMessages(t, db, startBody.SessionID)
	if msgCount != 2 {
		t.Fatalf("expected 2 messages, got %d", msgCount)
	}

	// Logout revokes token but keeps session history.
	logoutResp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/logout", regBody.ID), nil, nil)
	assertStatus(t, logoutResp, http.StatusNoContent)

	// Login again for a new token.
	loginResp2 := client.DoJSON(http.MethodPost, "/api/users/login", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	assertStatus(t, loginResp2, http.StatusOK)
	var loginBody2 struct {
		AuthToken string `json:"auth_token"`
	}
	decodeJSON(t, loginResp2.Body.Bytes(), &loginBody2)
	if loginBody2.AuthToken == "" {
		t.Fatalf("expected auth token after relogin")
	}

	// Reopen the existing session.
	reopenResp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", regBody.ID),
		map[string]any{"provider": provider, "session_id": startBody.SessionID, "model_type": "gpt-5-mini"},
		nil)
	assertStatus(t, reopenResp, http.StatusAccepted)

	secondMessage := "What was my name?"
	sendResp2 := client.PostSSE(
		fmt.Sprintf("/api/users/%d/conversation/msg", regBody.ID),
		map[string]any{
			"session_id": startBody.SessionID,
			"content":    secondMessage,
			"provider":   provider,
			"model_type": "gpt-5-mini",
			"client_msg_id": "client-msg-2",
		},
		nil,
	)
	assertStatus(t, sendResp2, http.StatusOK)
	events = parseSSE(t, sendResp2.Body.String())
	if len(events) != 3 || events[0].Name != "ack" || events[2].Name != "done" {
		t.Fatalf("unexpected SSE sequence for second message: %#v", events)
	}

	msgCount = countMessages(t, db, startBody.SessionID)
	if msgCount != 4 {
		t.Fatalf("expected 4 messages after second exchange, got %d", msgCount)
	}

	// Finally, delete the account.
	delResp := client.DoJSON(http.MethodDelete,
		fmt.Sprintf("/api/users/%d", regBody.ID), nil, nil)
	assertStatus(t, delResp, http.StatusNoContent)

	// Ensure login now fails.
	failLogin := client.DoJSON(http.MethodPost, "/api/users/login", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	if failLogin.Code == http.StatusOK {
		t.Fatalf("expected login to fail after user deletion")
	}
	_ = handler
}

func TestStartConversationValidation(t *testing.T) {
	router, db, _ := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)

	userID, _ := registerAndLogin(t, client)

	resp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", userID),
		map[string]any{"provider": "", "session_id": 0, "model_type": "gpt-5-nano"},
		nil)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestStartConversationDuplicateRequests(t *testing.T) {
	router, db, _ := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)

	userID, _ := registerAndLogin(t, client)
	setTokenResp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/token", userID),
		map[string]string{"provider": "openai", "token": "mock"},
		nil)
	assertStatus(t, setTokenResp, http.StatusNoContent)

	newSession := func(sessionID int64) int64 {
		resp := client.DoJSON(http.MethodPost,
			fmt.Sprintf("/api/users/%d/conversation/start", userID),
			map[string]any{"provider": "openai", "session_id": sessionID, "model_type": "gpt-5-nano"},
			nil)
		assertStatus(t, resp, http.StatusAccepted)
		var body struct {
			SessionID int64 `json:"sessionId"`
		}
		decodeJSON(t, resp.Body.Bytes(), &body)
		if body.SessionID <= 0 {
			t.Fatalf("expected positive session id, got %d", body.SessionID)
		}
		return body.SessionID
	}

	firstID := newSession(0)
	secondID := newSession(0)
	if firstID == secondID {
		t.Fatalf("expected distinct sessions when starting twice with session_id=0")
	}

	thirdID := newSession(firstID)
	if thirdID != firstID {
		t.Fatalf("expected reopening existing session to return same id, got %d vs %d", thirdID, firstID)
	}
}

func TestStartConversationBackpressure(t *testing.T) {
	router, db, handler := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)

	userID, _ := registerAndLogin(t, client)
	setTokenResp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/token", userID),
		map[string]string{"provider": "openai", "token": "mock"},
		nil)
	assertStatus(t, setTokenResp, http.StatusNoContent)

	handler.workers.(*mockWorker).initErr = worker.ErrDispatcherBusy
	resp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", userID),
		map[string]any{"provider": "openai", "session_id": 0, "model_type": "gpt-5-nano"},
		nil)
	assertStatus(t, resp, http.StatusTooManyRequests)
}

func TestCaptureInputValidation(t *testing.T) {
	router, db, handler := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)

	userID, _ := registerAndLogin(t, client)
	setTokenResp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/token", userID),
		map[string]string{"provider": "openai", "token": "mock"},
		nil)
	assertStatus(t, setTokenResp, http.StatusNoContent)

	startResp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", userID),
		map[string]any{"provider": "openai", "session_id": 0, "model_type": "gpt-5-nano"},
		nil)
	assertStatus(t, startResp, http.StatusAccepted)
	var body struct {
		SessionID int64 `json:"sessionId"`
	}
	decodeJSON(t, startResp.Body.Bytes(), &body)

	// Missing session id
	resp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/msg", userID),
		map[string]any{"session_id": 0, "content": "hi", "provider": "openai", "model_type": "gpt", "client_msg_id": "client-msg-3"},
		nil)
	assertStatus(t, resp, http.StatusBadRequest)

	// Empty content
	resp = client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/msg", userID),
		map[string]any{"session_id": body.SessionID, "content": "   ", "provider": "openai", "model_type": "gpt", "client_msg_id": "client-msg-4"},
		nil)
	assertStatus(t, resp, http.StatusBadRequest)

	_ = handler
}

func TestStartConversationRequiresToken(t *testing.T) {
	router, db, _ := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)

	userID, _ := registerAndLogin(t, client)
	resp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", userID),
		map[string]any{"provider": "openai", "session_id": 0, "model_type": "gpt"},
		nil)
	assertStatus(t, resp, http.StatusBadRequest)
	if !strings.Contains(resp.Body.String(), "api token not configured") {
		t.Fatalf("expected error about missing token, got %s", resp.Body.String())
	}
}

func TestCaptureInputSSEError(t *testing.T) {
	router, db, handler := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)
	userID, _ := registerAndLogin(t, client)

	setTokenResp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/token", userID),
		map[string]string{"provider": "openai", "token": "mock"},
		nil)
	assertStatus(t, setTokenResp, http.StatusNoContent)

	startResp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", userID),
		map[string]any{"provider": "openai", "session_id": 0, "model_type": "gpt"},
		nil)
	assertStatus(t, startResp, http.StatusAccepted)
	var body struct {
		SessionID int64 `json:"sessionId"`
	}
	decodeJSON(t, startResp.Body.Bytes(), &body)

	mw, ok := handler.workers.(*mockWorker)
	if !ok {
		t.Fatalf("expected mockWorker")
	}
	mw.streamErr = fmt.Errorf("mock failure")

	resp := client.PostSSE(
		fmt.Sprintf("/api/users/%d/conversation/msg", userID),
		map[string]any{
			"session_id": body.SessionID,
			"content":    "hello",
			"provider":   "openai",
			"model_type": "gpt",
			"client_msg_id": "client-msg-5",
		},
		nil,
	)
	assertStatus(t, resp, http.StatusOK)
	events := parseSSE(t, resp.Body.String())
	if len(events) != 2 {
		t.Fatalf("expected ack and error events, got %d: %#v", len(events), events)
	}
	if events[0].Name != "ack" || events[1].Name != "error" {
		t.Fatalf("unexpected SSE sequence: %#v", events)
	}
	if !strings.Contains(events[1].Data, "mock failure") {
		t.Fatalf("missing error payload: %s", events[1].Data)
	}
}

func TestCaptureInputBackpressure(t *testing.T) {
	router, db, handler := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)
	userID, _ := registerAndLogin(t, client)

	setTokenResp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/token", userID),
		map[string]string{"provider": "openai", "token": "mock"},
		nil)
	assertStatus(t, setTokenResp, http.StatusNoContent)

	startResp := client.DoJSON(http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", userID),
		map[string]any{"provider": "openai", "session_id": 0, "model_type": "gpt"},
		nil)
	assertStatus(t, startResp, http.StatusAccepted)
	var body struct {
		SessionID int64 `json:"sessionId"`
	}
	decodeJSON(t, startResp.Body.Bytes(), &body)

	mw := handler.workers.(*mockWorker)
	mw.streamErr = worker.ErrDispatcherBusy

	resp := client.PostSSE(
		fmt.Sprintf("/api/users/%d/conversation/msg", userID),
		map[string]any{
			"session_id": body.SessionID,
			"content":    "hello again",
			"provider":   "openai",
			"model_type": "gpt",
			"client_msg_id": "client-msg-6",
		},
		nil,
	)
	assertStatus(t, resp, http.StatusOK)
	events := parseSSE(t, resp.Body.String())
	if len(events) != 2 || events[1].Name != "error" {
		t.Fatalf("expected ack + error, got %#v", events)
	}
	if !strings.Contains(events[1].Data, "server is busy") {
		t.Fatalf("expected busy message, got %s", events[1].Data)
	}
}

func TestCSRFMiddlewareRejectsMissingHeader(t *testing.T) {
	router, db, _ := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)

	userID, _ := registerAndLogin(t, client)

	resp := client.DoJSONNoCSRF(http.MethodPost,
		fmt.Sprintf("/api/users/%d/token", userID),
		map[string]string{"provider": "openai", "token": "mock"},
		nil)
	assertStatus(t, resp, http.StatusForbidden)
}

func TestGetSessionMessages(t *testing.T) {
	router, db, handler := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)

	userID, _ := registerAndLogin(t, client)
	session, err := handler.assistant.CreateSession(context.Background(), userID, "Test Session")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}
	msg, err := handler.assistant.AddMessage(context.Background(), models.Message{
		UserID:    userID,
		SessionID: session.ID,
		Role:      models.RoleUser,
		Content:   "Hello history",
	})
	if err != nil {
		t.Fatalf("add message: %v", err)
	}

	resp := client.DoJSON(http.MethodGet,
		fmt.Sprintf("/api/users/%d/conversation/sessions/%d/messages", userID, session.ID),
		nil,
		nil,
	)
	assertStatus(t, resp, http.StatusOK)
	var payload struct {
		Session  models.Session   `json:"session"`
		Messages []models.Message `json:"messages"`
	}
	decodeJSON(t, resp.Body.Bytes(), &payload)
	if payload.Session.ID != session.ID {
		t.Fatalf("expected session %d, got %d", session.ID, payload.Session.ID)
	}
	if len(payload.Messages) != 1 || payload.Messages[0].ID != msg.ID {
		t.Fatalf("expected single message in history")
	}
}

func TestFilesUploadSuccess(t *testing.T) {
	router, db, handler := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)

	userID, _ := registerAndLogin(t, client)
	session, err := handler.assistant.CreateSession(context.Background(), userID, "files")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	path := fmt.Sprintf("/api/users/%d/uploads", userID)
	resp := client.UploadFile(path, session.ID, "notes.txt", []byte("hello world"))
	assertStatus(t, resp, http.StatusCreated)

	var body struct {
		FileID   int64  `json:"file_id"`
		FileName string `json:"file_name"`
		Size     int64  `json:"size"`
	}
	decodeJSON(t, resp.Body.Bytes(), &body)
	if body.FileID <= 0 || body.FileName == "" || body.Size == 0 {
		t.Fatalf("invalid upload response: %+v", body)
	}

	expectedPath := filepath.Join(handler.fileBase, strconv.FormatInt(userID, 10), strconv.FormatInt(session.ID, 10), body.FileName)
	if _, err := os.Stat(expectedPath); err != nil {
		t.Fatalf("expected file at %s: %v", expectedPath, err)
	}
}

func TestFilesUploadRejectsInvalidInput(t *testing.T) {
	router, db, handler := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)

	userID, _ := registerAndLogin(t, client)
	session, err := handler.assistant.CreateSession(context.Background(), userID, "files")
	if err != nil {
		t.Fatalf("create session: %v", err)
	}

	path := fmt.Sprintf("/api/users/%d/uploads", userID)
	resp := client.UploadFile(path, session.ID, "data.bin", []byte{0x00, 0x01, 0x02, 0x03})
	if resp.Code != http.StatusBadRequest {
		t.Fatalf("expected 400 for unsupported file type, got %d", resp.Code)
	}

	big := bytes.Repeat([]byte("a"), maxUploadBytes+1)
	resp = client.UploadFile(path, session.ID, "big.txt", big)
	if resp.Code != http.StatusRequestEntityTooLarge {
		t.Fatalf("expected 413 for large file, got %d", resp.Code)
	}
}

type sseEvent struct {
	Name string
	Data string
}

func parseSSE(t *testing.T, payload string) []sseEvent {
	t.Helper()
	payload = strings.TrimSpace(payload)
	if payload == "" {
		return nil
	}
	chunks := strings.Split(payload, "\n\n")
	var events []sseEvent
	for _, chunk := range chunks {
		lines := strings.Split(strings.TrimSpace(chunk), "\n")
		if len(lines) == 0 {
			continue
		}
		var evt sseEvent
		for _, line := range lines {
			switch {
			case strings.HasPrefix(line, "event:"):
				evt.Name = strings.TrimSpace(strings.TrimPrefix(line, "event:"))
			case strings.HasPrefix(line, "data:"):
				data := strings.TrimSpace(strings.TrimPrefix(line, "data:"))
				if evt.Data == "" {
					evt.Data = data
				} else {
					evt.Data += "\n" + data
				}
			}
		}
		events = append(events, evt)
	}
	return events
}

func newTestServer(t *testing.T) (*gin.Engine, *sql.DB, *Handler) {
	t.Helper()
	gin.SetMode(gin.TestMode)
	t.Setenv("UNICHATGO_APIKEY_KEY", strings.Repeat("k", 32))

	cfg := &config.Config{
		Databases: map[string]config.DatabaseConfig{
			"sqlite3": {DSN: ":memory:"},
		},
	}
	db, err := storage.Open("sqlite3", cfg)
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := storage.Migrate(db, "sqlite3"); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	asst, err := assistant.NewService(db)
	if err != nil {
		t.Fatalf("assistant service: %v", err)
	}
	authSvc := auth.NewService(db, nil, time.Hour)
	handler := NewHandler(asst, authSvc, worker.DispatcherConfig{MinWorkers: 2, MaxWorkers: 2, QueueSize: 10}, t.TempDir(), assistant.DefaultTempFileTTL, nil)
	handler.workers = newMockWorker(asst)

	router := gin.New()
	handler.RegisterRoutes(router)
	return router, db, handler
}

func decodeJSON(t *testing.T, data []byte, v interface{}) {
	t.Helper()
	if err := json.Unmarshal(data, v); err != nil {
		t.Fatalf("decode json: %v", err)
	}
}

func assertStatus(t *testing.T, rec *httptest.ResponseRecorder, want int) {
	t.Helper()
	if rec.Code != want {
		t.Fatalf("unexpected status %d, body: %s", rec.Code, rec.Body.String())
	}
}

func countMessages(t *testing.T, db *sql.DB, sessionID int64) int {
	t.Helper()
	var count int
	if err := db.QueryRow(`SELECT COUNT(*) FROM messages WHERE session_id = ?`, sessionID).Scan(&count); err != nil {
		t.Fatalf("count messages: %v", err)
	}
	return count
}

func registerAndLogin(t *testing.T, client *apiTestClient) (int64, string) {
	t.Helper()
	username := fmt.Sprintf("tester_%d", time.Now().UnixNano())
	password := "pass123"
	regResp := client.DoJSON(http.MethodPost, "/api/users/register", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	assertStatus(t, regResp, http.StatusCreated)
	var regBody struct {
		ID int64 `json:"id"`
	}
	decodeJSON(t, regResp.Body.Bytes(), &regBody)

	loginResp := client.DoJSON(http.MethodPost, "/api/users/login", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	assertStatus(t, loginResp, http.StatusOK)
	var loginBody struct {
		AuthToken string `json:"auth_token"`
	}
	decodeJSON(t, loginResp.Body.Bytes(), &loginBody)
	if loginBody.AuthToken == "" {
		t.Fatalf("expected auth token in login response")
	}
	return regBody.ID, loginBody.AuthToken
}

type mockWorker struct {
	assistant *assistant.Service
	streamErr error
	initErr   error
}

func newMockWorker(asst *assistant.Service) *mockWorker {
	return &mockWorker{assistant: asst}
}

func (m *mockWorker) InitSession(req worker.SessionRequest) (*models.Session, error) {
	if err := m.initErr; err != nil {
		m.initErr = nil
		return nil, err
	}
	ctx := req.Context
	if ctx == nil {
		ctx = context.Background()
	}
	if req.SessionID <= 0 {
		return m.assistant.CreateSession(ctx, req.UserID, "Mock Session")
	}
	session, _, err := m.assistant.GetSessionWithMessages(ctx, req.UserID, req.SessionID)
	return session, err
}

func (m *mockWorker) Stream(req worker.StreamRequest) (*models.Message, string, error) {
	if err := m.streamErr; err != nil {
		m.streamErr = nil
		return nil, "", err
	}
	if req.ChunkFn != nil {
		if err := req.ChunkFn("mock-chunk"); err != nil {
			return nil, "", err
		}
	}
	resp := &models.Message{
		UserID:    req.UserID,
		SessionID: req.SessionID,
		Role:      models.RoleAssistant,
		Content:   fmt.Sprintf("Mock response to %q", req.Message.Content),
	}
	return resp, "Mock Title", nil
}

func (m *mockWorker) ResetUser(int64)                  {}
func (m *mockWorker) Purge(int64, int64)               {}
func (m *mockWorker) InvalidateTempFiles(int64, int64) {}

type apiTestClient struct {
	t       *testing.T
	router  *gin.Engine
	cookies map[string]*http.Cookie
}

type requestConfig struct {
	skipCSRF bool
}

func newAPITestClient(t *testing.T, router *gin.Engine) *apiTestClient {
	t.Helper()
	return &apiTestClient{
		t:       t,
		router:  router,
		cookies: make(map[string]*http.Cookie),
	}
}

func (c *apiTestClient) DoJSON(method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	return c.doRequest(method, path, body, headers, requestConfig{})
}

func (c *apiTestClient) DoJSONNoCSRF(method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	return c.doRequest(method, path, body, headers, requestConfig{skipCSRF: true})
}

func (c *apiTestClient) PostSSE(path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	return c.doRequest(http.MethodPost, path, body, headers, requestConfig{})
}

func (c *apiTestClient) UploadFile(path string, sessionID int64, filename string, content []byte) *httptest.ResponseRecorder {
	c.t.Helper()
	var buf bytes.Buffer
	writer := multipart.NewWriter(&buf)
	if err := writer.WriteField("session_id", strconv.FormatInt(sessionID, 10)); err != nil {
		c.t.Fatalf("write session field: %v", err)
	}
	part, err := writer.CreateFormFile("file", filename)
	if err != nil {
		c.t.Fatalf("create form file: %v", err)
	}
	if _, err := part.Write(content); err != nil {
		c.t.Fatalf("write file content: %v", err)
	}
	writer.Close()

	req := httptest.NewRequest(http.MethodPost, path, &buf)
	req.Header.Set("Content-Type", writer.FormDataContentType())
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	if needsCSRFAttach(http.MethodPost) && req.Header.Get("X-CSRF-Token") == "" {
		if ck, ok := c.cookies["csrf_token"]; ok {
			req.Header.Set("X-CSRF-Token", ck.Value)
		}
	}
	rec := httptest.NewRecorder()
	c.router.ServeHTTP(rec, req)
	if resp := rec.Result(); resp != nil {
		c.captureCookies(resp)
		resp.Body.Close()
	}
	return rec
}

func (c *apiTestClient) doRequest(method, path string, body interface{}, headers map[string]string, cfg requestConfig) *httptest.ResponseRecorder {
	c.t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			c.t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	for _, ck := range c.cookies {
		req.AddCookie(ck)
	}
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	if !cfg.skipCSRF && needsCSRFAttach(method) {
		if req.Header.Get("X-CSRF-Token") == "" {
			if ck, ok := c.cookies["csrf_token"]; ok {
				req.Header.Set("X-CSRF-Token", ck.Value)
			}
		}
	}
	rec := httptest.NewRecorder()
	c.router.ServeHTTP(rec, req)
	if resp := rec.Result(); resp != nil {
		c.captureCookies(resp)
		resp.Body.Close()
	}
	return rec
}

func (c *apiTestClient) captureCookies(resp *http.Response) {
	for _, ck := range resp.Cookies() {
		if ck.Value == "" || ck.MaxAge < 0 {
			delete(c.cookies, ck.Name)
			continue
		}
		c.cookies[ck.Name] = ck
	}
}

func needsCSRFAttach(method string) bool {
	switch strings.ToUpper(method) {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return false
	default:
		return true
	}
}
