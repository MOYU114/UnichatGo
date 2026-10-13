package api

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"image"
	"image/png"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	redisclient "github.com/redis/go-redis/v9"
	"unichatgo/internal/execution"
	"unichatgo/internal/models"
)

func TestLegacyEmptyHistoryIsArrayAndFirstSendWorks(t *testing.T) {
	router, db, h := newTestServer(t)
	defer db.Close()
	client := newAPITestClient(t, router)
	uid, _ := registerAndLogin(t, client)
	s, err := h.assistant.CreateSession(context.Background(), uid, "empty")
	if err != nil {
		t.Fatal(err)
	}
	res := client.DoJSON(http.MethodGet, fmt.Sprintf("/api/users/%d/conversation/sessions/%d/messages", uid, s.ID), nil, nil)
	assertStatus(t, res, 200)
	var history map[string]json.RawMessage
	decodeJSON(t, res.Body.Bytes(), &history)
	if string(history["messages"]) != "[]" {
		t.Fatalf("empty history must be [], got %s", history["messages"])
	}
	res = client.DoJSON(http.MethodPost, fmt.Sprintf("/api/users/%d/token", uid), map[string]string{"provider": "openai", "token": "mock"}, nil)
	assertStatus(t, res, 204)
	res = client.PostSSE(fmt.Sprintf("/api/users/%d/conversation/msg", uid), inputRequest{SessionID: s.ID, ClientMsgID: "first", Content: "hello", Provider: "openai", ModelType: "mock"}, nil)
	assertStatus(t, res, 200)
	if events := parseSSE(t, res.Body.String()); len(events) != 3 || events[2].Name != "done" {
		t.Fatalf("first message failed: %s", res.Body.String())
	}
	if count := countMessages(t, db, s.ID); count != 2 {
		t.Fatalf("first turn rows=%d", count)
	}
}

func reviewImage(t *testing.T, width, height int) []byte {
	t.Helper()
	var b bytes.Buffer
	if err := png.Encode(&b, image.NewGray(image.Rect(0, 0, width, height))); err != nil {
		t.Fatal(err)
	}
	return b.Bytes()
}

func TestAsyncMySQLImageBudgetPreflightAndDefaultExecution(t *testing.T) {
	addr := os.Getenv("UNICHATGO_TEST_REDIS_ADDR")
	if addr == "" {
		t.Skip("real isolated Redis required")
	}
	f := newAsyncAPIFixture(t)
	ctx := context.Background()
	sid := f.session(t)
	data := reviewImage(t, 512, 512)
	a, err := f.repo.Upload(ctx, f.user, sid, "ordinary.png", "image/png", data)
	if err != nil {
		t.Fatal(err)
	}
	in := execution.CallInput{Provider: "openai", Model: "mock", Messages: []models.Message{{Role: models.RoleUser, Content: "describe"}}, Attachments: []execution.Attachment{{Name: a.Name, MIME: a.MIME, Data: data, Size: int64(len(data))}}}
	tokens, err := execution.EstimateTokens(in)
	if err != nil {
		t.Fatal(err)
	}
	quota := f.cfg.Async.WithQuotaDefaults()
	if tokens > quota.TPM {
		t.Fatalf("ordinary PNG reservation %d > default %d", tokens, quota.TPM)
	}
	var calls atomic.Int64
	entered := make(chan []byte, 1)
	release := make(chan struct{})
	var once sync.Once
	unblock := func() { once.Do(func() { close(release) }) }
	mock := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		calls.Add(1)
		b, err := io.ReadAll(r.Body)
		if err != nil {
			t.Error(err)
		}
		select {
		case entered <- b:
		default:
			t.Error("unexpected duplicate provider call")
		}
		select {
		case <-release:
		case <-r.Context().Done():
			return
		}
		w.Header().Set("Content-Type", "application/json")
		_, _ = io.WriteString(w, `{"choices":[{"finish_reason":"stop","message":{"content":"image understood"}}]}`)
	}))
	defer mock.Close()
	defer unblock()
	p := f.cfg.Providers["openai"]
	p.BaseURL = mock.URL
	f.cfg.Providers["openai"] = p
	payload := f.input(sid, "ordinary-image", "describe")
	payload["attachment_namespace"] = "shared-v1"
	payload["file_ids"] = []int64{a.ID}
	res := f.request(ctx, f.router, http.MethodPost, f.messagePath(), payload, true)
	assertStatus(t, res, 202)
	var accepted execution.Receipt
	decodeJSON(t, res.Body.Bytes(), &accepted)
	rc := redisclient.NewClient(&redisclient.Options{Addr: addr, DB: 12})
	defer rc.Close()
	httpProvider := execution.NewHTTPProvider(10 * time.Second)
	defer httpProvider.Close()
	limit := execution.QuotaLimit{Concurrency: quota.Concurrency, RPM: quota.RPM, TPM: quota.TPM}
	rt := &execution.Runtime{Repo: f.repo, Redis: rc, Provider: httpProvider, Decrypt: f.service.OpenExecution, Workers: 1, Lease: time.Minute, Quota: execution.QuotaPolicy{Platform: limit, Credential: limit, User: execution.QuotaLimit{Concurrency: quota.UserConcurrency, RPM: quota.RPM, TPM: quota.TPM}}}
	workerCtx, stop := context.WithCancel(ctx)
	done := make(chan struct{})
	go func() { defer close(done); rt.Run(workerCtx) }()
	defer func() {
		stop()
		unblock()
		select {
		case <-done:
		case <-time.After(10 * time.Second):
			t.Error("image worker did not drain")
		}
	}()
	select {
	case body := <-entered:
		if !bytes.Contains(body, []byte("data:image/png;base64,")) {
			t.Fatal("image missing in provider request")
		}
	case <-time.After(10 * time.Second):
		t.Fatal("default image never entered provider")
	}
	stop()
	unblock()
	select {
	case <-done:
	case <-time.After(10 * time.Second):
		t.Fatal("image task did not finish")
	}
	e, err := f.repo.Get(ctx, accepted.ID)
	if err != nil || e.State != execution.Completed {
		t.Fatalf("default image state=%s err=%v", e.State, err)
	}
	if calls.Load() != 1 {
		t.Fatalf("provider calls=%d", calls.Load())
	}
	// Budget changes and missing dependencies cannot invalidate an existing key.
	f.cfg.Async.TPM = 1
	res = f.request(ctx, f.router, http.MethodPost, f.messagePath(), payload, true)
	assertStatus(t, res, 200)
	f.cfg.Async.TPM = 0
	// Two maximum-size images cannot fit the same default minute budget. Reject
	// before acceptance and leave sequence, messages, evidence and outbox untouched.
	large := reviewImage(t, 2048, 2048)
	sid2 := f.session(t)
	ids := []int64{}
	for i := 0; i < 2; i++ {
		a, err = f.repo.Upload(ctx, f.user, sid2, fmt.Sprintf("large-%d.png", i), "image/png", large)
		if err != nil {
			t.Fatal(err)
		}
		ids = append(ids, a.ID)
	}
	tooLarge := f.input(sid2, "over-budget", "describe")
	tooLarge["attachment_namespace"] = "shared-v1"
	tooLarge["file_ids"] = ids
	var admissionBefore, outboxBefore, pinsBefore int
	for query, target := range map[string]*int{
		`SELECT COUNT(*) FROM execution_admission_events WHERE user_id=?`:                                                   &admissionBefore,
		`SELECT COUNT(*) FROM execution_outbox o JOIN execution_evidence e ON o.request_id=e.id WHERE e.user_id=?`:          &outboxBefore,
		`SELECT COUNT(*) FROM execution_attachment_pins p JOIN execution_evidence e ON p.request_id=e.id WHERE e.user_id=?`: &pinsBefore,
	} {
		if err = f.db.QueryRow(query, f.user).Scan(target); err != nil {
			t.Fatal(err)
		}
	}
	res = f.request(ctx, f.router, http.MethodPost, f.messagePath(), tooLarge, true)
	assertStatus(t, res, 422)
	if !bytes.Contains(res.Body.Bytes(), []byte("token_budget_exceeded")) {
		t.Fatal(res.Body.String())
	}
	for _, query := range []string{`SELECT COUNT(*) FROM messages WHERE session_id=?`, `SELECT COUNT(*) FROM execution_evidence WHERE session_id=?`, `SELECT COUNT(*) FROM execution_sessions WHERE session_id=?`} {
		var count int
		if err = f.db.QueryRow(query, sid2).Scan(&count); err != nil || count != 0 {
			t.Fatalf("rejected input wrote state count=%d err=%v", count, err)
		}
	}
	for query, want := range map[string]int{
		`SELECT COUNT(*) FROM execution_admission_events WHERE user_id=?`:                                                   admissionBefore,
		`SELECT COUNT(*) FROM execution_outbox o JOIN execution_evidence e ON o.request_id=e.id WHERE e.user_id=?`:          outboxBefore,
		`SELECT COUNT(*) FROM execution_attachment_pins p JOIN execution_evidence e ON p.request_id=e.id WHERE e.user_id=?`: pinsBefore,
	} {
		var n int
		if err = f.db.QueryRow(query, f.user).Scan(&n); err != nil || n != want {
			t.Fatalf("rejected budget changed durable facts: n=%d want=%d err=%v", n, want, err)
		}
	}
	tooLarge["file_ids"] = ids[:1]
	res = f.request(ctx, f.router, http.MethodPost, f.messagePath(), tooLarge, true)
	assertStatus(t, res, 202)
	var one execution.Receipt
	decodeJSON(t, res.Body.Bytes(), &one)
	if one.Sequence != 1 {
		t.Fatalf("rejection consumed sequence: %d", one.Sequence)
	}
}

func TestAsyncMySQLPreflightIncludesLegacyContext(t *testing.T) {
	f := newAsyncAPIFixture(t)
	sid := f.session(t)
	ctx := context.Background()
	if _, err := f.service.AddMessage(ctx, models.Message{UserID: f.user, SessionID: sid, Role: models.RoleUser, Content: string(bytes.Repeat([]byte("a"), 200000))}); err != nil {
		t.Fatal(err)
	}
	res := f.request(ctx, f.router, http.MethodPost, f.messagePath(), f.input(sid, "legacy-budget", "short input"), true)
	assertStatus(t, res, 422)
	if count := countMessages(t, f.db, sid); count != 1 {
		t.Fatalf("rejected input added message: %d", count)
	}
}
