package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/gin-gonic/gin"

	"unichatgo/internal/auth"
	"unichatgo/internal/models"
	"unichatgo/internal/service/assistant"
	"unichatgo/internal/storage"
	"unichatgo/internal/worker"
)

func TestHandlersEndToEndFlow(t *testing.T) {
	router, db, handler := newTestServer(t)
	defer db.Close()

	username := fmt.Sprintf("tester_%d", time.Now().UnixNano())
	password := "pass123"
	provider := "openai"

	// Register a user.
	regResp := doJSONRequest(t, router, http.MethodPost, "/api/users/register", map[string]string{
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
	loginResp := doJSONRequest(t, router, http.MethodPost, "/api/users/login", map[string]string{
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
	authHeader := map[string]string{"Authorization": fmt.Sprintf("Bearer %s", loginBody.AuthToken)}

	// Store provider token.
	tokenResp := doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/token", regBody.ID),
		map[string]string{"provider": provider, "token": "mock"},
		authHeader)
	assertStatus(t, tokenResp, http.StatusNoContent)

	// Start a new conversation (session_id == 0).
	startResp := doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", regBody.ID),
		map[string]any{"provider": provider, "session_id": 0, "model_type": "gpt-5-nano"},
		authHeader)
	assertStatus(t, startResp, http.StatusAccepted)
	var startBody struct {
		SessionID int64 `json:"sessionId"`
	}
	decodeJSON(t, startResp.Body.Bytes(), &startBody)
	if startBody.SessionID <= 0 {
		t.Fatalf("expected positive session id")
	}

	firstMessage := "Hello, remember my name is Bob."
	sendResp := postSSE(t, router,
		fmt.Sprintf("/api/users/%d/conversation/msg", regBody.ID),
		map[string]any{
			"session_id": startBody.SessionID,
			"content":    firstMessage,
			"provider":   provider,
			"model_type": "gpt-5-nano",
		},
		authHeader,
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
	logoutResp := doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/logout", regBody.ID), nil, authHeader)
	assertStatus(t, logoutResp, http.StatusNoContent)

	// Login again for a new token.
	loginResp2 := doJSONRequest(t, router, http.MethodPost, "/api/users/login", map[string]string{
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
	authHeader = map[string]string{"Authorization": fmt.Sprintf("Bearer %s", loginBody2.AuthToken)}

	// Reopen the existing session.
	reopenResp := doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", regBody.ID),
		map[string]any{"provider": provider, "session_id": startBody.SessionID, "model_type": "gpt-5-mini"},
		authHeader)
	assertStatus(t, reopenResp, http.StatusAccepted)

	secondMessage := "What was my name?"
	sendResp2 := postSSE(t, router,
		fmt.Sprintf("/api/users/%d/conversation/msg", regBody.ID),
		map[string]any{
			"session_id": startBody.SessionID,
			"content":    secondMessage,
			"provider":   provider,
			"model_type": "gpt-5-mini",
		},
		authHeader,
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
	delResp := doJSONRequest(t, router, http.MethodDelete,
		fmt.Sprintf("/api/users/%d", regBody.ID), nil, authHeader)
	assertStatus(t, delResp, http.StatusNoContent)

	// Ensure login now fails.
	failLogin := doJSONRequest(t, router, http.MethodPost, "/api/users/login", map[string]string{
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

	username := fmt.Sprintf("tester_%d", time.Now().UnixNano())
	password := "pass123"
	regResp := doJSONRequest(t, router, http.MethodPost, "/api/users/register", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	assertStatus(t, regResp, http.StatusCreated)
	var regBody struct {
		ID int64 `json:"id"`
	}
	decodeJSON(t, regResp.Body.Bytes(), &regBody)
	loginResp := doJSONRequest(t, router, http.MethodPost, "/api/users/login", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	assertStatus(t, loginResp, http.StatusOK)
	var loginBody struct {
		AuthToken string `json:"auth_token"`
	}
	decodeJSON(t, loginResp.Body.Bytes(), &loginBody)
	authHeader := map[string]string{"Authorization": fmt.Sprintf("Bearer %s", loginBody.AuthToken)}

	resp := doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", regBody.ID),
		map[string]any{"provider": "", "session_id": 0, "model_type": "gpt-5-nano"},
		authHeader)
	assertStatus(t, resp, http.StatusBadRequest)
}

func TestStartConversationDuplicateRequests(t *testing.T) {
	router, db, _ := newTestServer(t)
	defer db.Close()

	userID, authHeader := registerAndLogin(t, router)
	setTokenResp := doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/token", userID),
		map[string]string{"provider": "openai", "token": "mock"},
		authHeader)
	assertStatus(t, setTokenResp, http.StatusNoContent)

	newSession := func(sessionID int64) int64 {
		resp := doJSONRequest(t, router, http.MethodPost,
			fmt.Sprintf("/api/users/%d/conversation/start", userID),
			map[string]any{"provider": "openai", "session_id": sessionID, "model_type": "gpt-5-nano"},
			authHeader)
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

func TestCaptureInputValidation(t *testing.T) {
	router, db, handler := newTestServer(t)
	defer db.Close()

	userID, authHeader := registerAndLogin(t, router)
	setTokenResp := doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/token", userID),
		map[string]string{"provider": "openai", "token": "mock"},
		authHeader)
	assertStatus(t, setTokenResp, http.StatusNoContent)

	startResp := doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", userID),
		map[string]any{"provider": "openai", "session_id": 0, "model_type": "gpt-5-nano"},
		authHeader)
	assertStatus(t, startResp, http.StatusAccepted)
	var body struct {
		SessionID int64 `json:"sessionId"`
	}
	decodeJSON(t, startResp.Body.Bytes(), &body)

	// Missing session id
	resp := doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/msg", userID),
		map[string]any{"session_id": 0, "content": "hi", "provider": "openai", "model_type": "gpt"},
		authHeader)
	assertStatus(t, resp, http.StatusBadRequest)

	// Empty content
	resp = doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/msg", userID),
		map[string]any{"session_id": body.SessionID, "content": "   ", "provider": "openai", "model_type": "gpt"},
		authHeader)
	assertStatus(t, resp, http.StatusBadRequest)

	_ = handler
}

func TestStartConversationRequiresToken(t *testing.T) {
	router, db, _ := newTestServer(t)
	defer db.Close()

	userID, authHeader := registerAndLogin(t, router)
	resp := doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", userID),
		map[string]any{"provider": "openai", "session_id": 0, "model_type": "gpt"},
		authHeader)
	assertStatus(t, resp, http.StatusBadRequest)
	if !strings.Contains(resp.Body.String(), "api token not configured") {
		t.Fatalf("expected error about missing token, got %s", resp.Body.String())
	}
}

func TestCaptureInputSSEError(t *testing.T) {
	router, db, handler := newTestServer(t)
	defer db.Close()
	userID, authHeader := registerAndLogin(t, router)

	setTokenResp := doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/token", userID),
		map[string]string{"provider": "openai", "token": "mock"},
		authHeader)
	assertStatus(t, setTokenResp, http.StatusNoContent)

	startResp := doJSONRequest(t, router, http.MethodPost,
		fmt.Sprintf("/api/users/%d/conversation/start", userID),
		map[string]any{"provider": "openai", "session_id": 0, "model_type": "gpt"},
		authHeader)
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

	resp := postSSE(t, router,
		fmt.Sprintf("/api/users/%d/conversation/msg", userID),
		map[string]any{
			"session_id": body.SessionID,
			"content":    "hello",
			"provider":   "openai",
			"model_type": "gpt",
		},
		authHeader,
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

	db, err := storage.Open(":memory:")
	if err != nil {
		t.Fatalf("open db: %v", err)
	}
	if err := storage.Migrate(db); err != nil {
		t.Fatalf("migrate db: %v", err)
	}
	asst := assistant.NewService(db)
	authSvc := auth.NewService(db, time.Hour)
	handler := NewHandler(asst, authSvc)
	handler.workers = newMockWorker(asst)

	router := gin.New()
	handler.RegisterRoutes(router)
	return router, db, handler
}

func doJSONRequest(t *testing.T, router *gin.Engine, method, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	var buf bytes.Buffer
	if body != nil {
		if err := json.NewEncoder(&buf).Encode(body); err != nil {
			t.Fatalf("encode body: %v", err)
		}
	}
	req := httptest.NewRequest(method, path, &buf)
	req.Header.Set("Content-Type", "application/json")
	for k, v := range headers {
		req.Header.Set(k, v)
	}
	rec := httptest.NewRecorder()
	router.ServeHTTP(rec, req)
	return rec
}

func postSSE(t *testing.T, router *gin.Engine, path string, body interface{}, headers map[string]string) *httptest.ResponseRecorder {
	t.Helper()
	return doJSONRequest(t, router, http.MethodPost, path, body, headers)
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

func registerAndLogin(t *testing.T, router *gin.Engine) (int64, map[string]string) {
	t.Helper()
	username := fmt.Sprintf("tester_%d", time.Now().UnixNano())
	password := "pass123"
	regResp := doJSONRequest(t, router, http.MethodPost, "/api/users/register", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	assertStatus(t, regResp, http.StatusCreated)
	var regBody struct {
		ID int64 `json:"id"`
	}
	decodeJSON(t, regResp.Body.Bytes(), &regBody)

	loginResp := doJSONRequest(t, router, http.MethodPost, "/api/users/login", map[string]string{
		"username": username,
		"password": password,
	}, nil)
	assertStatus(t, loginResp, http.StatusOK)
	var loginBody struct {
		AuthToken string `json:"auth_token"`
	}
	decodeJSON(t, loginResp.Body.Bytes(), &loginBody)
	if loginBody.AuthToken == "" {
		t.Fatalf("expected auth token after login")
	}
	authHeader := map[string]string{"Authorization": fmt.Sprintf("Bearer %s", loginBody.AuthToken)}
	return regBody.ID, authHeader
}

type mockWorker struct {
	assistant *assistant.Service
	streamErr error
}

func newMockWorker(asst *assistant.Service) *mockWorker {
	return &mockWorker{assistant: asst}
}

func (m *mockWorker) EnsureSession(req worker.SessionRequest) (*models.Session, error) {
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

func (m *mockWorker) Stop(int64)         {}
func (m *mockWorker) Purge(int64, int64) {}
