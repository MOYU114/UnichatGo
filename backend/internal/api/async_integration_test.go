package api

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"strings"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/go-sql-driver/mysql"
	"github.com/google/uuid"
	redisclient "github.com/redis/go-redis/v9"
	"unichatgo/internal/auth"
	"unichatgo/internal/config"
	"unichatgo/internal/execution"
	"unichatgo/internal/service/assistant"
	"unichatgo/internal/storage"
)

type asyncAPIFixture struct {
	db      *sql.DB
	repo    *execution.Repository
	service *assistant.Service
	auth    *auth.Service
	cfg     *config.Config
	router  *gin.Engine
	user    int64
	token   string
}

func newAsyncAPIFixture(t *testing.T) *asyncAPIFixture {
	t.Helper()
	dsn := os.Getenv("UNICHATGO_TEST_MYSQL_DSN")
	if dsn == "" {
		t.Skip("real MySQL not executed: UNICHATGO_TEST_MYSQL_DSN unset")
	}
	t.Setenv("UNICHATGO_APIKEY_KEY", "0123456789abcdef0123456789abcdef")
	parsed, err := mysql.ParseDSN(dsn)
	if err != nil {
		t.Fatal(err)
	}
	admin, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	schema := "unichatgo_api_test_" + strings.ReplaceAll(uuid.NewString(), "-", "")
	if _, err = admin.Exec(`CREATE DATABASE ` + schema); err != nil {
		admin.Close()
		t.Fatal(err)
	}
	parsed.DBName = schema
	db, err := sql.Open("mysql", parsed.FormatDSN())
	if err != nil {
		t.Fatal(err)
	}
	db.SetMaxOpenConns(24)
	t.Cleanup(func() { db.Close(); admin.Exec(`DROP DATABASE ` + schema); admin.Close() })
	if err = storage.Migrate(db, "mysql"); err != nil {
		t.Fatal(err)
	}
	if err = execution.Migrate(context.Background(), db); err != nil {
		t.Fatal(err)
	}
	svc, err := assistant.NewService(db)
	if err != nil {
		t.Fatal(err)
	}
	f := &asyncAPIFixture{db: db, repo: execution.NewRepository(db, execution.Limits{}), service: svc, auth: auth.NewService(db, nil, time.Hour), cfg: &config.Config{Async: config.AsyncConfig{Enabled: true}, Providers: map[string]config.ProviderConfig{"openai": {Model: "mock-model"}}}}
	f.router = f.newRouter()
	username := "async-api-" + uuid.NewString()
	res := f.request(context.Background(), f.router, http.MethodPost, "/api/users/register", map[string]any{"username": username, "password": "isolated-test-password"}, false)
	assertStatus(t, res, 201)
	var user struct {
		ID int64 `json:"id"`
	}
	decodeJSON(t, res.Body.Bytes(), &user)
	f.user = user.ID
	t.Cleanup(func() {
		db.Exec(`DELETE q FROM execution_quota_calls q JOIN execution_evidence e ON e.id=q.request_id WHERE e.user_id=?`, f.user)
		db.Exec(`DELETE p FROM execution_attachment_pins p JOIN execution_evidence e ON e.id=p.request_id WHERE e.user_id=?`, f.user)
		db.Exec(`DELETE FROM users WHERE id=?`, f.user)
	})
	res = f.request(context.Background(), f.router, http.MethodPost, "/api/users/login", map[string]any{"username": username, "password": "isolated-test-password"}, false)
	assertStatus(t, res, 200)
	var login struct {
		Token string `json:"auth_token"`
	}
	decodeJSON(t, res.Body.Bytes(), &login)
	f.token = login.Token
	res = f.request(context.Background(), f.router, http.MethodPost, fmt.Sprintf("/api/users/%d/token", f.user), map[string]any{"provider": "openai", "token": "isolated-not-paid-token"}, true)
	assertStatus(t, res, 204)
	return f
}
func (f *asyncAPIFixture) newRouter() *gin.Engine {
	router := gin.New()
	NewAsyncHandler(f.service, f.auth, f.repo, f.cfg).RegisterRoutes(router)
	return router
}
func (f *asyncAPIFixture) request(ctx context.Context, router *gin.Engine, method, path string, payload any, authenticated bool) *httptest.ResponseRecorder {
	body, _ := json.Marshal(payload)
	req := httptest.NewRequest(method, path, bytes.NewReader(body)).WithContext(ctx)
	req.Header.Set("Content-Type", "application/json")
	if authenticated {
		req.Header.Set("Authorization", "Bearer "+f.token)
	}
	res := httptest.NewRecorder()
	router.ServeHTTP(res, req)
	return res
}
func (f *asyncAPIFixture) session(t *testing.T) int64 {
	t.Helper()
	res := f.request(context.Background(), f.router, http.MethodPost, fmt.Sprintf("/api/users/%d/conversation/start", f.user), map[string]any{"session_id": 0}, true)
	assertStatus(t, res, 202)
	var body struct {
		ID int64 `json:"sessionId"`
	}
	decodeJSON(t, res.Body.Bytes(), &body)
	return body.ID
}
func (f *asyncAPIFixture) input(sid int64, key, text string) map[string]any {
	return map[string]any{"session_id": sid, "client_msg_id": key, "content": text, "provider": "openai"}
}
func (f *asyncAPIFixture) messagePath() string {
	return fmt.Sprintf("/api/users/%d/conversation/msg", f.user)
}
func (f *asyncAPIFixture) historyPath(sid int64) string {
	return fmt.Sprintf("/api/users/%d/conversation/sessions/%d/messages", f.user, sid)
}

func TestAsyncMySQLHTTPIdentityOwnershipAndHistory(t *testing.T) {
	f := newAsyncAPIFixture(t)
	ctx := context.Background()
	sid := f.session(t)
	empty := f.request(ctx, f.router, http.MethodGet, f.historyPath(sid), nil, true)
	assertStatus(t, empty, 200)
	var raw map[string]json.RawMessage
	decodeJSON(t, empty.Body.Bytes(), &raw)
	if string(raw["messages"]) != "[]" || string(raw["requests"]) != "[]" {
		t.Fatalf("empty history must be arrays: %s", empty.Body.String())
	}
	otherRouter := f.newRouter()
	payload := f.input(sid, "same-browser-key", "hello")
	start := make(chan struct{})
	results := make(chan *httptest.ResponseRecorder, 12)
	var wg sync.WaitGroup
	for i := range 12 {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			<-start
			router := f.router
			if i%2 == 1 {
				router = otherRouter
			}
			results <- f.request(ctx, router, http.MethodPost, f.messagePath(), payload, true)
		}(i)
	}
	close(start)
	wg.Wait()
	close(results)
	var identity string
	accepted := 0
	for response := range results {
		if response.Code != 200 && response.Code != 202 {
			t.Fatalf("concurrent response %d %s", response.Code, response.Body.String())
		}
		if response.Code == 202 {
			accepted++
		}
		var receipt execution.Receipt
		decodeJSON(t, response.Body.Bytes(), &receipt)
		if identity == "" {
			identity = receipt.ID
		}
		if receipt.ID != identity || receipt.ClientID != "same-browser-key" {
			t.Fatalf("identity split %+v", receipt)
		}
	}
	if accepted != 1 {
		t.Fatalf("accepted %d times", accepted)
	}
	conflict := f.request(ctx, f.router, http.MethodPost, f.messagePath(), f.input(sid, "same-browser-key", "changed"), true)
	assertStatus(t, conflict, 409)
	pathMismatch := f.request(ctx, f.router, http.MethodPost, fmt.Sprintf("/api/users/%d/conversation/msg", f.user+1), payload, true)
	assertStatus(t, pathMismatch, 403)
	missingSession := f.request(ctx, f.router, http.MethodPost, f.messagePath(), f.input(sid+1000000, "same-browser-key", "hello"), true)
	assertStatus(t, missingSession, 404)
	// New acceptance cannot bypass the bounded same-session queue.
	f.repo = execution.NewRepository(f.db, execution.Limits{SessionQueue: 1})
	limited := f.newRouter()
	res := f.request(ctx, limited, http.MethodPost, f.messagePath(), f.input(sid, "another", "next"), true)
	assertStatus(t, res, 429)
	// An original key remains replayable after provider credentials are removed,
	// config changes, a restart, and acceptance is paused.
	if _, err := f.db.Exec(`DELETE FROM apiKeys WHERE user_id=?`, f.user); err != nil {
		t.Fatal(err)
	}
	f.cfg.Async.Paused = true
	f.cfg.Providers = map[string]config.ProviderConfig{}
	restart := f.newRouter()
	res = f.request(ctx, restart, http.MethodPost, f.messagePath(), payload, true)
	assertStatus(t, res, 200)
	var receipt execution.Receipt
	decodeJSON(t, res.Body.Bytes(), &receipt)
	if receipt.ID != identity {
		t.Fatal("replay after rotation changed identity")
	}
	res = f.request(ctx, restart, http.MethodGet, f.historyPath(sid), nil, true)
	assertStatus(t, res, 200)
	var history struct {
		Messages []map[string]any    `json:"messages"`
		Requests []execution.Receipt `json:"requests"`
	}
	decodeJSON(t, res.Body.Bytes(), &history)
	if len(history.Messages) != 1 || len(history.Requests) != 1 || history.Requests[0].State != execution.Queued {
		t.Fatalf("incorrect accepted history: %s", res.Body.String())
	}
}

type apiBarrierProvider struct {
	entered chan struct{}
	release chan struct{}
	calls   atomic.Int64
}

func (p *apiBarrierProvider) Generate(ctx context.Context, in execution.CallInput) (string, error) {
	p.calls.Add(1)
	close(p.entered)
	select {
	case <-p.release:
		return "durable-background-answer", nil
	case <-ctx.Done():
		return "", ctx.Err()
	}
}

func TestAsyncMySQLHTTPDisconnectAndWorkerDrain(t *testing.T) {
	addr := os.Getenv("UNICHATGO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("real Redis not executed: UNICHATGO_TEST_REDIS_ADDR unset")
	}
	f := newAsyncAPIFixture(t)
	sid := f.session(t)
	requestCtx, cancelRequest := context.WithCancel(context.Background())
	res := f.request(requestCtx, f.router, http.MethodPost, f.messagePath(), f.input(sid, "disconnect-key", "run independently"), true)
	assertStatus(t, res, 202)
	cancelRequest()
	var receipt execution.Receipt
	decodeJSON(t, res.Body.Bytes(), &receipt)
	// Isolated Redis logical DB keeps this test separate from execution-package
	// stream-loss tests when go test runs packages concurrently.
	rc := redisclient.NewClient(&redisclient.Options{Addr: addr, DB: 12})
	defer rc.Close()
	if err := rc.Ping(context.Background()).Err(); err != nil {
		t.Fatal(err)
	}
	p := &apiBarrierProvider{entered: make(chan struct{}), release: make(chan struct{})}
	limit := execution.QuotaLimit{Concurrency: 1000, RPM: 100000, TPM: 100000000}
	rt := &execution.Runtime{Repo: f.repo, Redis: rc, Provider: p, Decrypt: f.service.OpenExecution, Workers: 1, Lease: time.Minute, Quota: execution.QuotaPolicy{Platform: limit, User: limit, Credential: limit}}
	workerCtx, stop := context.WithCancel(context.Background())
	done := make(chan struct{})
	go func() { defer close(done); rt.Run(workerCtx) }()
	var releaseOnce sync.Once
	release := func() { releaseOnce.Do(func() { close(p.release) }) }
	t.Cleanup(func() {
		stop()
		release()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("runtime cleanup did not drain")
		}
	})
	select {
	case <-p.entered:
	case <-time.After(10 * time.Second):
		t.Fatal("background worker did not execute disconnected acceptance")
	}
	stop()
	select {
	case <-done:
		t.Fatal("worker canceled task instead of draining")
	default:
	}
	release()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("worker failed to drain")
	}
	res = f.request(context.Background(), f.newRouter(), http.MethodGet, f.historyPath(sid), nil, true)
	assertStatus(t, res, 200)
	var history struct {
		Messages []struct {
			Content string `json:"content"`
		} `json:"messages"`
		Requests []execution.Receipt `json:"requests"`
	}
	decodeJSON(t, res.Body.Bytes(), &history)
	if len(history.Messages) != 2 || history.Messages[1].Content != "durable-background-answer" || len(history.Requests) != 1 || history.Requests[0].State != execution.Completed {
		t.Fatalf("history not restored after disconnect/drain: %s", res.Body.String())
	}
	res = f.request(context.Background(), f.newRouter(), http.MethodPost, f.messagePath(), f.input(sid, "disconnect-key", "run independently"), true)
	assertStatus(t, res, 200)
	if p.calls.Load() != 1 {
		t.Fatalf("HTTP restart duplicated provider: %d", p.calls.Load())
	}
	if err := f.repo.DeleteSession(context.Background(), f.user, sid); err != nil {
		t.Fatal(err)
	}
}

func TestAsyncMySQLHTTPUncertainResolutionAndDeletion(t *testing.T) {
	f := newAsyncAPIFixture(t)
	ctx := context.Background()
	sid := f.session(t)
	res := f.request(ctx, f.router, http.MethodPost, f.messagePath(), f.input(sid, "uncertain", "unknown outcome"), true)
	assertStatus(t, res, 202)
	var receipt execution.Receipt
	decodeJSON(t, res.Body.Bytes(), &receipt)
	e, err := f.repo.Claim(ctx, receipt.ID, "isolated-test-worker", time.Minute)
	if err != nil {
		t.Fatal(err)
	}
	limit := execution.QuotaLimit{Concurrency: 100, RPM: 10000, TPM: 10000000}
	if err = f.repo.StartCall(ctx, e, execution.QuotaPolicy{Platform: limit, User: limit, Credential: limit}, 100); err != nil {
		t.Fatal(err)
	}
	if err = f.repo.Finish(ctx, e, execution.Uncertain, "", "upstream_unknown"); err != nil {
		t.Fatal(err)
	}
	res = f.request(ctx, f.router, http.MethodGet, f.historyPath(sid), nil, true)
	assertStatus(t, res, 200)
	var history struct {
		Requests []execution.Receipt `json:"requests"`
	}
	decodeJSON(t, res.Body.Bytes(), &history)
	if len(history.Requests) != 1 || history.Requests[0].State != execution.Uncertain {
		t.Fatalf("uncertain hidden: %s", res.Body.String())
	}
	res = f.request(ctx, f.router, http.MethodPost, f.messagePath(), f.input(sid, "later", "blocked"), true)
	assertStatus(t, res, 409)
	res = f.request(ctx, f.router, http.MethodDelete, fmt.Sprintf("/api/users/%d/conversation/sessions/%d", f.user, sid), nil, true)
	assertStatus(t, res, 409)
	path := fmt.Sprintf("/api/users/%d/conversation/requests/%s/resolve", f.user, receipt.ID)
	res = f.request(ctx, f.router, http.MethodPost, path, map[string]any{"action": "abandon", "reason": ""}, true)
	assertStatus(t, res, 400)
	res = f.request(ctx, f.router, http.MethodPost, path, map[string]any{"action": "abandon", "reason": "user explicitly accepts duplicate risk"}, true)
	assertStatus(t, res, 409)
	if _, err = f.db.Exec(`UPDATE execution_evidence SET lease_until_ms=0 WHERE id=?`, e.ID); err != nil {
		t.Fatal(err)
	}
	res = f.request(ctx, f.router, http.MethodPost, path, map[string]any{"action": "abandon", "reason": "user explicitly accepts duplicate risk"}, true)
	assertStatus(t, res, 200)
	res = f.request(ctx, f.router, http.MethodPost, f.messagePath(), f.input(sid, "later", "blocked"), true)
	assertStatus(t, res, 202)
}

func TestAsyncMySQLHTTPDatabaseUnavailableIs503(t *testing.T) {
	f := newAsyncAPIFixture(t)
	sid := f.session(t)
	if err := f.db.Close(); err != nil {
		t.Fatal(err)
	}
	res := f.request(context.Background(), f.router, http.MethodGet, f.historyPath(sid), nil, true)
	assertStatus(t, res, 503)
	var body map[string]any
	decodeJSON(t, res.Body.Bytes(), &body)
	if _, ok := body["requests"]; ok {
		t.Fatal("DB outage manufactured task status")
	}
	if strings.Contains(res.Body.String(), "sql:") {
		t.Fatal("raw DB details leaked")
	}
}

func TestAsyncMySQLHTTPBinaryClientKeysAndFrozenSecret(t *testing.T) {
	f := newAsyncAPIFixture(t)
	ctx := context.Background()
	sid := f.session(t)
	seen := map[string]bool{}
	for _, key := range []string{"Case-ID", "case-id", " Case-ID "} {
		res := f.request(ctx, f.router, http.MethodPost, f.messagePath(), f.input(sid, key, "same content"), true)
		assertStatus(t, res, 202)
		if strings.Contains(res.Body.String(), "isolated-not-paid-token") {
			t.Fatal("receipt leaked credential")
		}
		var receipt execution.Receipt
		decodeJSON(t, res.Body.Bytes(), &receipt)
		if receipt.ClientID != key || seen[receipt.ID] {
			t.Fatalf("binary idempotency key collapsed: %+v", receipt)
		}
		seen[receipt.ID] = true
		e, err := f.repo.Get(ctx, receipt.ID)
		if err != nil {
			t.Fatal(err)
		}
		if strings.Contains(e.Snapshot, "isolated-not-paid-token") {
			t.Fatal("SQL snapshot is plaintext")
		}
		plain, err := f.service.OpenExecution(e.Snapshot)
		if err != nil {
			t.Fatal(err)
		}
		var snapshot execution.Snapshot
		if err = json.Unmarshal([]byte(plain), &snapshot); err != nil {
			t.Fatal(err)
		}
		if snapshot.Token != "isolated-not-paid-token" || snapshot.Model != "mock-model" {
			t.Fatalf("snapshot not frozen: model=%s", snapshot.Model)
		}
	}
	res := f.request(ctx, f.router, http.MethodGet, f.historyPath(sid), nil, true)
	assertStatus(t, res, 200)
	for _, secret := range []string{"isolated-not-paid-token", "snapshot", "quota_scope", "lease_until", "fence"} {
		if strings.Contains(res.Body.String(), secret) {
			t.Fatalf("history exposed internal field %s", secret)
		}
	}
}
