package execution

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"os"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/google/uuid"
	"github.com/redis/go-redis/v9"
	"unichatgo/internal/storage"
)

func runtimeFixture(t *testing.T) (*Repository, *Runtime, int64, func() int64) {
	t.Helper()
	dsn := os.Getenv("UNICHATGO_TEST_MYSQL_DSN")
	addr := os.Getenv("UNICHATGO_TEST_REDIS_ADDR")
	if dsn == "" || addr == "" {
		t.Skip("real MySQL and Redis required; set UNICHATGO_TEST_MYSQL_DSN and UNICHATGO_TEST_REDIS_ADDR")
	}
	db, err := sql.Open("mysql", dsn)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { db.Close() })
	ctx := context.Background()
	if err = storage.Migrate(db, "mysql"); err != nil {
		t.Fatal(err)
	}
	if err = Migrate(ctx, db); err != nil {
		t.Fatal(err)
	}
	res, err := db.Exec(`INSERT INTO users(username,password_hash,created_at) VALUES(?,'test',NOW())`, uuid.NewString())
	if err != nil {
		t.Fatal(err)
	}
	uid, _ := res.LastInsertId()
	t.Cleanup(func() { _, _ = db.Exec(`DELETE FROM users WHERE id=?`, uid) })
	repo := NewRepository(db, Limits{})
	rc := redis.NewClient(&redis.Options{Addr: addr})
	t.Cleanup(func() { rc.Close() })
	if err = rc.Ping(ctx).Err(); err != nil {
		t.Fatal(err)
	}
	limit := QuotaLimit{Concurrency: 100, RPM: 100000, TPM: 1000000000}
	rt := &Runtime{Repo: repo, Redis: rc, Decrypt: func(s string) (string, error) { return s, nil }, Quota: QuotaPolicy{Platform: limit, User: limit, Credential: limit}, Lease: time.Minute}
	return repo, rt, uid, func() int64 {
		res, err := db.Exec(`INSERT INTO sessions(user_id,title,created_at,updated_at) VALUES(?,'test',NOW(),NOW())`, uid)
		if err != nil {
			t.Fatal(err)
		}
		sid, _ := res.LastInsertId()
		return sid
	}
}

func acceptRuntime(t *testing.T, r *Repository, uid, sid int64, content string) Receipt {
	t.Helper()
	s, _ := json.Marshal(Snapshot{Provider: "openai", Model: "mock", Token: "test", MaxOutputTokens: 100})
	a, _, err := r.Accept(context.Background(), Input{UserID: uid, SessionID: sid, ClientID: uuid.NewString(), PayloadHash: HashPayload([]byte(content)), Content: content, Provider: "openai", Model: "mock", Snapshot: string(s), QuotaScope: fmt.Sprint(uid)})
	if err != nil {
		t.Fatal(err)
	}
	return a
}

type controlledProvider struct {
	entered chan CallInput
	release chan struct{}
	calls   atomic.Int64
}

func (p *controlledProvider) Generate(ctx context.Context, in CallInput) (string, error) {
	p.calls.Add(1)
	p.entered <- in
	select {
	case <-ctx.Done():
		return "", ctx.Err()
	case <-p.release:
		return "answer", nil
	}
}

func TestRuntimeRealServicesOrderingRedeliveryAndLoss(t *testing.T) {
	repo, rt, uid, newSession := runtimeFixture(t)
	ctx := context.Background()
	sid := newSession()
	first := acceptRuntime(t, repo, uid, sid, "first")
	second := acceptRuntime(t, repo, uid, sid, "second")
	other := acceptRuntime(t, repo, uid, newSession(), "parallel")
	p := &controlledProvider{entered: make(chan CallInput, 3), release: make(chan struct{})}
	rt.Provider = p
	var wg sync.WaitGroup
	invoke := func(id, owner string) {
		wg.Add(1)
		go func() {
			defer wg.Done()
			rt.deliver(ctx, owner, redis.XMessage{ID: "0-1", Values: map[string]any{"request_id": id}})
		}()
	}
	invoke(first.ID, "worker-a")
	select {
	case in := <-p.entered:
		if len(in.Messages) != 1 || in.Messages[0].Content != "first" {
			t.Fatalf("future or duplicate history: %#v", in.Messages)
		}
	case <-time.After(5 * time.Second):
		t.Fatal("first did not enter")
	}
	// The call is blocked by a channel; the synchronous second delivery MUST return
	// without entering the provider. This assertion does not infer order from sleep.
	rt.deliver(ctx, "worker-b", redis.XMessage{ID: "0-2", Values: map[string]any{"request_id": second.ID}})
	if p.calls.Load() != 1 {
		t.Fatal("same-session call overtook first")
	}
	invoke(other.ID, "worker-c")
	select {
	case <-p.entered:
	case <-time.After(5 * time.Second):
		t.Fatal("different session did not run concurrently")
	}
	// Losing the delivery stream must not reset SQL authority while calls run.
	if err := rt.Redis.Del(ctx, streamKey).Err(); err != nil {
		t.Fatal(err)
	}
	rt.deliver(ctx, "worker-after-redis-loss", redis.XMessage{ID: "0-3", Values: map[string]any{"request_id": first.ID}})
	if p.calls.Load() != 2 {
		t.Fatal("redis loss duplicated live call")
	}
	close(p.release)
	wg.Wait()
	e, err := repo.Get(ctx, first.ID)
	if err != nil || e.State != Completed {
		t.Fatalf("first state %s %v", e.State, err)
	}
	// Simulated result-commit/no-ACK followed by restart of runtime object.
	rt2 := &Runtime{Repo: repo, Redis: rt.Redis, Provider: p, Decrypt: rt.Decrypt, Quota: rt.Quota, Lease: rt.Lease}
	rt2.deliver(ctx, "replacement", redis.XMessage{ID: "0-4", Values: map[string]any{"request_id": first.ID}})
	if p.calls.Load() != 2 {
		t.Fatal("terminal redelivery called provider")
	}
	rt.deliver(ctx, "worker-b", redis.XMessage{ID: "0-5", Values: map[string]any{"request_id": second.ID}})
	if p.calls.Load() != 3 {
		t.Fatal("second never ran")
	}
	in := <-p.entered
	if len(in.Messages) != 3 || in.Messages[0].Content != "first" || in.Messages[1].Content != "answer" || in.Messages[2].Content != "second" {
		t.Fatalf("invalid next context: %#v", in.Messages)
	}
}

func TestRuntimeRealServicesOutboxPendingAndPoison(t *testing.T) {
	repo, rt, uid, newSession := runtimeFixture(t)
	ctx := context.Background()
	a := acceptRuntime(t, repo, uid, newSession(), "outbox")
	if err := rt.Redis.Del(ctx, streamKey).Err(); err != nil {
		t.Fatal(err)
	}
	if err := rt.Redis.XGroupCreateMkStream(ctx, streamKey, consumerGroup, "0").Err(); err != nil {
		t.Fatal(err)
	}
	if err := rt.PublishOnce(ctx); err != nil {
		t.Fatal(err)
	}
	streams, err := rt.Redis.XReadGroup(ctx, &redis.XReadGroupArgs{Group: consumerGroup, Consumer: "dead", Streams: []string{streamKey, ">"}, Count: 100, Block: -1}).Result()
	if err != nil {
		t.Fatal(err)
	}
	var delivered redis.XMessage
	for _, s := range streams {
		for _, m := range s.Messages {
			if m.Values["request_id"] == a.ID {
				delivered = m
			}
		}
	}
	if delivered.ID == "" {
		t.Fatal("outbox missing")
	}
	pending, _, err := rt.Redis.XAutoClaim(ctx, &redis.XAutoClaimArgs{Stream: streamKey, Group: consumerGroup, Consumer: "replacement", Start: "0-0", MinIdle: 0, Count: 100}).Result()
	if err != nil || len(pending) == 0 {
		t.Fatalf("pending not reclaimed: %v", err)
	}
	p := &controlledProvider{entered: make(chan CallInput, 10), release: make(chan struct{})}
	close(p.release)
	rt.Provider = p
	rt.deliver(ctx, "replacement", delivered)
	if p.calls.Load() != 1 {
		t.Fatal("reclaimed entry did not complete")
	}
	poison, err := rt.Redis.XAdd(ctx, &redis.XAddArgs{Stream: streamKey, Values: map[string]any{"wrong": "no-payload"}}).Result()
	if err != nil {
		t.Fatal(err)
	}
	rt.deliver(ctx, "replacement", redis.XMessage{ID: poison, Values: map[string]any{"wrong": "no-payload"}})
	var n int
	if err = repo.DB.QueryRow(`SELECT COUNT(*) FROM execution_quarantine WHERE stream_id=?`, poison).Scan(&n); err != nil || n != 1 {
		t.Fatalf("poison not durable: %d %v", n, err)
	}
}
